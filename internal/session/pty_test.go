package session

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/saltylin/hopskip/internal/store"
)

func TestPTYSessionLifecycle(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewManager(st)
	if err := m.Available(); err != nil {
		t.Skip("no shell available: " + err.Error())
	}

	// open a PTY session
	ss, err := m.Open("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ss.Status != "open" {
		t.Fatalf("expected open, got %s", ss.Status)
	}
	defer m.Close(ss.ID)

	// send_keys with the done-sentinel: a command whose output we can see
	screen, code, completed, err := m.SendKeys(ss.ID, "echo hello-pty-world\n", true, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatalf("command did not complete (sentinel not seen); screen:\n%s", screen)
	}
	if code == nil || *code != 0 {
		t.Fatalf("expected exit code 0, got %v", code)
	}
	if !strings.Contains(screen, "hello-pty-world") {
		t.Fatalf("expected command output on screen, got:\n%s", screen)
	}
	if strings.Contains(screen, "__HOPSKIP_DONE_") {
		t.Fatalf("sentinel line should be stripped, got:\n%s", screen)
	}

	// a non-zero exit code is captured
	_, code2, ok2, _ := m.SendKeys(ss.ID, "false\n", true, 8000)
	if !ok2 || code2 == nil || *code2 == 0 {
		t.Fatalf("expected a non-zero exit code from `false`, got ok=%v code=%v", ok2, code2)
	}

	// read_screen returns the current rendered screen
	rs, err := m.Capture(ss.ID, 0)
	if err != nil || rs == "" {
		t.Fatalf("read_screen failed: err=%v screen=%q", err, rs)
	}

	// attach replays recent output (the browser sees the current screen)
	a, err := m.Attach(ss.ID, 50, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	buf := make([]byte, 64*1024)
	n, _ := a.Read(buf) // replay backlog
	if n == 0 || !strings.Contains(string(buf[:n]), "hello-pty-world") {
		t.Fatalf("attach replay should contain prior output; got %d bytes", n)
	}

	// live output reaches the attach
	donec := make(chan string, 1)
	go func() {
		b := make([]byte, 64*1024)
		acc := ""
		for {
			k, err := a.Read(b)
			acc += string(b[:k])
			if strings.Contains(acc, "live-marker-xyz") || err != nil {
				donec <- acc
				return
			}
		}
	}()
	_, _, _, _ = m.SendKeys(ss.ID, "echo live-marker-xyz\n", false, 0)
	select {
	case got := <-donec:
		if !strings.Contains(got, "live-marker-xyz") {
			t.Fatalf("attach did not receive live output: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for live output on attach")
	}

	// closing the session ends the attach (Read -> EOF) and marks it closed in DB
	m.Close(ss.ID)
	time.Sleep(100 * time.Millisecond)
	if got, _ := st.GetSession(ss.ID); got.Status != "closed" {
		t.Fatalf("session should be closed in DB, got %s", got.Status)
	}
	for {
		if _, err := a.Read(buf); err == io.EOF {
			break
		} else if err != nil {
			break
		}
	}
}
