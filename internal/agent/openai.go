package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"github.com/saltylin/hopskip/internal/store"
)

// openaiRunner drives the agent loop against OpenAI. It speaks the Chat
// Completions API by default, but transparently switches to the Responses API
// (/v1/responses) for models that OpenAI only serves there — the "pro"/reasoning
// models (e.g. gpt-5.5-pro), which reject Chat Completions with a 404
// "not a chat model". The switch is auto-detected on that error and remembered
// for the rest of this runner's life, so regular chat models keep streaming.
type openaiRunner struct {
	agent   *Agent
	client  openai.Client
	model   string
	disp    *Dispatcher
	msgs    []openai.ChatCompletionMessageParamUnion
	started bool

	// Responses-API mode (set once a chat-completions call reports the model is
	// not a chat model). prevRespID chains turns server-side so the model's own
	// reasoning + function_call items are retained without us replaying them.
	useResponses bool
	prevRespID   string
}

func newOpenAIRunner(a *Agent, cred store.Credential) *openaiRunner {
	model := cred.Model
	if model == "" {
		model = "gpt-4o"
	}
	client := openai.NewClient(
		option.WithAPIKey(cred.Token),
		option.WithHTTPClient(a.httpClient), // routed through the operator's network proxy
	)
	return &openaiRunner{agent: a, client: client, model: model, disp: a.disp}
}

func openaiTools() []openai.ChatCompletionToolParam {
	specs := toolSpecs()
	out := make([]openai.ChatCompletionToolParam, 0, len(specs))
	for _, t := range specs {
		schema := map[string]any{"type": "object", "properties": t.Properties}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}
		out = append(out, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  schema,
			},
		})
	}
	return out
}

// openaiResponsesTools mirrors openaiTools for the Responses API shape.
func openaiResponsesTools() []responses.ToolUnionParam {
	specs := toolSpecs()
	out := make([]responses.ToolUnionParam, 0, len(specs))
	for _, t := range specs {
		schema := map[string]any{"type": "object", "properties": t.Properties}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}
		fn := responses.FunctionToolParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  schema,
			Strict:      openai.Bool(false), // our schemas aren't strict-mode compliant
		}
		out = append(out, responses.ToolUnionParam{OfFunction: &fn})
	}
	return out
}

// isNotChatModelErr detects OpenAI's 404 for a Responses-API-only model hitting
// the Chat Completions endpoint ("This is not a chat model ... Did you mean to
// use v1/completions?" / a pointer to v1/responses).
func isNotChatModelErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not a chat model") ||
		strings.Contains(s, "v1/responses") ||
		strings.Contains(s, "v1/completions")
}

func (r *openaiRunner) run(ctx context.Context, userMessage string, emit func(Event)) {
	if r.useResponses {
		r.runResponses(ctx, userMessage, emit)
		return
	}

	if !r.started {
		r.msgs = append(r.msgs, openai.SystemMessage(systemPrompt+r.disp.knowledgePrompt()+r.disp.inventoryPrompt()))
		r.started = true
	}
	r.msgs = append(r.msgs, openai.UserMessage(userMessage))
	tools := openaiTools()
	steps := r.agent.MaxSteps()

	for step := 0; step < steps; step++ {
		cctx, cancel := context.WithTimeout(ctx, turnTimeout)
		params := openai.ChatCompletionNewParams{
			Model:               shared.ChatModel(r.model),
			Messages:            r.msgs,
			Tools:               tools,
			MaxCompletionTokens: openai.Int(16000),
			StreamOptions:       openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
		}

		emit(Event{Kind: "thinking"})
		stream := r.client.Chat.Completions.NewStreaming(cctx, params, option.WithMaxRetries(r.agent.MaxRetries()))
		acc := openai.ChatCompletionAccumulator{}
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				emit(Event{Kind: "token", Text: chunk.Choices[0].Delta.Content})
			}
		}
		err := stream.Err()
		cancel()
		if err != nil {
			// A Responses-API-only model rejects chat completions on the very
			// first call (before any tokens). Switch this runner over and retry
			// the same turn through /v1/responses.
			if isNotChatModelErr(err) && step == 0 {
				// Silently switch this token to the Responses API and retry.
				r.useResponses = true
				r.runResponses(ctx, userMessage, emit)
				return
			}
			if stoppedByOperator(ctx) {
				emit(Event{Kind: "notice", Text: "⏹ Stopped."})
				emit(Event{Kind: "done"})
				return
			}
			emit(Event{Kind: "error", Text: friendlyErr(err)})
			emit(Event{Kind: "done"})
			return
		}
		if len(acc.Choices) == 0 {
			emit(Event{Kind: "error", Text: "empty response from provider"})
			emit(Event{Kind: "done"})
			return
		}
		emit(Event{Kind: "usage", In: int(acc.Usage.PromptTokens), Out: int(acc.Usage.CompletionTokens)})

		choice := acc.Choices[0]
		r.msgs = append(r.msgs, choice.Message.ToParam())

		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			emit(Event{Kind: "done"})
			return
		}

		for _, tc := range choice.Message.ToolCalls {
			emit(Event{Kind: "tool_use", Text: tc.Function.Name + "  " + tc.Function.Arguments})
			out, _ := r.disp.Dispatch(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			emit(Event{Kind: "tool_result", Text: clip(out, toolResultDisplayLimit)})
			r.msgs = append(r.msgs, openai.ToolMessage(out, tc.ID))
		}
	}

	emit(Event{Kind: "notice", Text: "(stopped: reached the " + strconv.Itoa(steps) + "-step budget)"})
	emit(Event{Kind: "done"})
}

// runResponses drives the loop against the Responses API. It is non-streaming
// (the SDK ships no stream accumulator for responses; pro models are slow,
// deliberate, low-volume calls where one chunk of text per turn is fine). Turns
// are chained with previous_response_id so the model's reasoning and prior
// function_call items live server-side and tool call_ids resolve without us
// replaying encrypted reasoning blocks. Instructions are re-sent every turn (the
// API does not carry them across previous_response_id), so knowledge edits apply
// mid-conversation, exactly like the Anthropic runner.
func (r *openaiRunner) runResponses(ctx context.Context, userMessage string, emit func(Event)) {
	instructions := systemPrompt + r.disp.knowledgePrompt() + r.disp.inventoryPrompt()
	tools := openaiResponsesTools()
	steps := r.agent.MaxSteps()

	// First request this turn carries the user message; later iterations carry
	// only the tool outputs (the rest is chained via previous_response_id).
	input := responses.ResponseInputParam{
		responses.ResponseInputItemParamOfMessage(userMessage, responses.EasyInputMessageRoleUser),
	}

	for step := 0; step < steps; step++ {
		cctx, cancel := context.WithTimeout(ctx, turnTimeout)
		params := responses.ResponseNewParams{
			Model:        shared.ResponsesModel(r.model),
			Instructions: openai.String(instructions),
			Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
			Tools:        tools,
		}
		if r.prevRespID != "" {
			params.PreviousResponseID = openai.String(r.prevRespID)
		}

		emit(Event{Kind: "thinking"})
		resp, err := r.client.Responses.New(cctx, params, option.WithMaxRetries(r.agent.MaxRetries()))
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
		r.prevRespID = resp.ID
		emit(Event{Kind: "usage", In: int(resp.Usage.InputTokens), Out: int(resp.Usage.OutputTokens)})

		if text := resp.OutputText(); text != "" {
			emit(Event{Kind: "token", Text: text})
		}

		var calls []responses.ResponseFunctionToolCall
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				calls = append(calls, item.AsFunctionCall())
			}
		}
		if len(calls) == 0 {
			emit(Event{Kind: "done"})
			return
		}

		// Next turn sends just the tool outputs; the function_call items they
		// answer are retained server-side via previous_response_id.
		input = responses.ResponseInputParam{}
		for _, tc := range calls {
			emit(Event{Kind: "tool_use", Text: tc.Name + "  " + tc.Arguments})
			out, _ := r.disp.Dispatch(tc.Name, json.RawMessage(tc.Arguments))
			emit(Event{Kind: "tool_result", Text: clip(out, toolResultDisplayLimit)})
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(tc.CallID, out))
		}
	}

	emit(Event{Kind: "notice", Text: "(stopped: reached the " + strconv.Itoa(steps) + "-step budget)"})
	emit(Event{Kind: "done"})
}
