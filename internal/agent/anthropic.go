package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/saltylin/hopskip/internal/store"
)

// anthropicRunner drives the loop against the Anthropic Messages API.
type anthropicRunner struct {
	agent  *Agent
	client anthropic.Client
	model  string
	disp   *Dispatcher
	msgs   []anthropic.MessageParam
}

func newAnthropicRunner(a *Agent, cred store.Credential) *anthropicRunner {
	model := cred.Model
	if model == "" {
		model = string(anthropic.ModelClaudeOpus4_8)
	}
	client := anthropic.NewClient(
		option.WithAPIKey(cred.Token),
		option.WithHTTPClient(a.httpClient), // routed through the operator's network proxy
	)
	return &anthropicRunner{agent: a, client: client, model: model, disp: a.disp}
}

func anthropicTools() []anthropic.ToolUnionParam {
	specs := toolSpecs()
	out := make([]anthropic.ToolUnionParam, 0, len(specs))
	for _, t := range specs {
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description:  anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: t.Properties, Required: t.Required},
		}})
	}
	return out
}

// adaptive thinking is supported on the 4.6+ opus/sonnet families.
func anthropicThinks(model string) bool {
	return strings.Contains(model, "opus") || strings.Contains(model, "sonnet")
}

func (r *anthropicRunner) run(ctx context.Context, userMessage string, emit func(Event)) {
	r.msgs = append(r.msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)))
	tools := anthropicTools()
	steps := r.agent.MaxSteps()
	sys := systemPrompt + r.disp.knowledgePrompt() + r.disp.inventoryPrompt() // knowledge + fleet notes, fresh each turn

	for step := 0; step < steps; step++ {
		cctx, cancel := context.WithTimeout(ctx, turnTimeout)
		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(r.model),
			MaxTokens: 16000,
			System:    []anthropic.TextBlockParam{{Text: sys}},
			Messages:  r.msgs,
			Tools:     tools,
		}
		if anthropicThinks(r.model) {
			params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
		}

		emit(Event{Kind: "thinking"})
		stream := r.client.Messages.NewStreaming(cctx, params, option.WithMaxRetries(r.agent.MaxRetries()))
		msg := anthropic.Message{}
		for stream.Next() {
			ev := stream.Current()
			_ = msg.Accumulate(ev)
			if cb, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if td, ok := cb.Delta.AsAny().(anthropic.TextDelta); ok && td.Text != "" {
					emit(Event{Kind: "token", Text: td.Text})
				}
			}
		}
		err := stream.Err()
		cancel()
		if err != nil {
			if stoppedByOperator(ctx) {
				emit(Event{Kind: "notice", Text: "⏹ Stopped."})
				emit(Event{Kind: "done"})
				return
			}
			emit(Event{Kind: "error", Text: friendlyErr(err)})
			emit(Event{Kind: "done"})
			return
		}
		emit(Event{Kind: "usage", In: int(msg.Usage.InputTokens), Out: int(msg.Usage.OutputTokens)})

		r.msgs = append(r.msgs, msg.ToParam())
		if msg.StopReason != anthropic.StopReasonToolUse {
			emit(Event{Kind: "done"})
			return
		}

		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			tu, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			emit(Event{Kind: "tool_use", Text: tu.Name + "  " + string(tu.Input)})
			out, isErr := r.disp.Dispatch(tu.Name, tu.Input)
			emit(Event{Kind: "tool_result", Text: clip(out, toolResultDisplayLimit)})
			results = append(results, anthropic.NewToolResultBlock(tu.ID, out, isErr))
		}
		r.msgs = append(r.msgs, anthropic.NewUserMessage(results...))
	}

	emit(Event{Kind: "notice", Text: "(stopped: reached the " + strconv.Itoa(steps) + "-step budget)"})
	emit(Event{Kind: "done"})
}
