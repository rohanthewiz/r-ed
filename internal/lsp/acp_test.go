// =============================================================================
// File: internal/lsp/acp_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the ACP adaptation of the JSON-RPC client: newline-
// delimited framing in both directions and the onRequest hook that
// answers agent→client requests. Driven over in-memory pipes with a
// fake ndjson agent, the same isolation as client_test's fakeServer.

package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeAgent is an in-memory ACP agent: it reads newline-framed
// messages from the client and lets tests script line responses.
type fakeAgent struct {
	in  *bufio.Reader // client → agent
	out io.Writer     // agent → client
}

// pipeClientACP wires an ndjson Client to a fakeAgent over two
// in-memory pipes.
func pipeClientACP(t *testing.T, onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) (*Client, *fakeAgent, func()) {
	t.Helper()
	cliR, agtW := io.Pipe() // agent writes → client reads
	agtR, cliW := io.Pipe() // client writes → agent reads
	c := NewClientACP(cliR, cliW, onNotify, onRequest, onExit)
	agt := &fakeAgent{in: bufio.NewReader(agtR), out: agtW}
	return c, agt, func() { _ = agtW.Close(); _ = cliW.Close() }
}

// read returns the next message the client sent, decoded from one
// ndjson line — failing loudly if the client leaked LSP framing.
func (s *fakeAgent) read(t *testing.T) *message {
	t.Helper()
	line, err := s.in.ReadString('\n')
	if err != nil {
		t.Fatalf("fake agent read: %v", err)
	}
	if strings.HasPrefix(line, "Content-Length:") {
		t.Fatalf("client sent LSP framing on an ACP connection: %q", line)
	}
	var m message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("fake agent decode %q: %v", line, err)
	}
	return &m
}

// write sends one raw JSON body to the client as an ndjson line.
func (s *fakeAgent) write(t *testing.T, body string) {
	t.Helper()
	if _, err := fmt.Fprintf(s.out, "%s\n", body); err != nil {
		t.Fatalf("fake agent write: %v", err)
	}
}

// TestReadLineMessage pins the ndjson parser: a framed line decodes,
// blank lines are skipped, a final unterminated record before EOF still
// parses, and a garbage line is a hard error (framing can't resync).
func TestReadLineMessage(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n{\"jsonrpc\":\"2.0\",\"method\":\"m\"}\n"))
	m, err := readLineMessage(r)
	if err != nil {
		t.Fatalf("readLineMessage: %v", err)
	}
	if m.Method != "m" {
		t.Errorf("method = %q, want m", m.Method)
	}

	// Unterminated final record: parse it rather than dropping it.
	r = bufio.NewReader(strings.NewReader(`{"jsonrpc":"2.0","method":"tail"}`))
	m, err = readLineMessage(r)
	if err != nil {
		t.Fatalf("unterminated record: %v", err)
	}
	if m.Method != "tail" {
		t.Errorf("unterminated method = %q, want tail", m.Method)
	}

	if _, err := readLineMessage(bufio.NewReader(strings.NewReader("not json\n"))); err == nil {
		t.Error("garbage line should be a hard error")
	}
}

// TestACPCallRoundTrip drives a full request/response over ndjson
// framing: the call's wire form is one JSON line, and the agent's line
// response resolves it.
func TestACPCallRoundTrip(t *testing.T) {
	c, agt, done := pipeClientACP(t, nil, nil, nil)
	defer done()

	type res struct {
		SessionID string `json:"sessionId"`
	}
	var got res
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call("session/new", map[string]string{"cwd": "/tmp"}, &got) }()

	m := agt.read(t)
	if m.Method != "session/new" || m.ID == nil {
		t.Fatalf("agent saw method=%q id=%v", m.Method, m.ID)
	}
	agt.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":"s-1"}}`, *m.ID))

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not resolve")
	}
	if got.SessionID != "s-1" {
		t.Errorf("sessionId = %q, want s-1", got.SessionID)
	}
}

// TestACPNotifyIsOneLine pins the outbound notification framing: a
// single well-formed JSON line, no Content-Length header.
func TestACPNotifyIsOneLine(t *testing.T) {
	c, agt, done := pipeClientACP(t, nil, nil, nil)
	defer done()

	// io.Pipe writes are synchronous, so the notify must run off the
	// test goroutine or it deadlocks against our own read below.
	errCh := make(chan error, 1)
	go func() { errCh <- c.Notify("session/cancel", map[string]string{"sessionId": "s-1"}) }()
	m := agt.read(t)
	if err := <-errCh; err != nil {
		t.Fatalf("notify: %v", err)
	}
	if m.Method != "session/cancel" || m.ID != nil {
		t.Errorf("agent saw method=%q id=%v, want notification session/cancel", m.Method, m.ID)
	}
}

// TestACPOnRequestAnswers pins the hook contract: an agent→client
// request routes through onRequest, whose return value becomes the
// response result — this is how session/request_permission gets a real
// answer instead of the LSP auto-responder's null.
func TestACPOnRequestAnswers(t *testing.T) {
	onRequest := func(method string, params json.RawMessage) (any, error) {
		if method != "session/request_permission" {
			return nil, fmt.Errorf("unexpected method %s", method)
		}
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	_, agt, done := pipeClientACP(t, nil, onRequest, nil)
	defer done()

	agt.write(t, `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{}}`)
	resp := agt.read(t)
	if resp.ID == nil || *resp.ID != 7 {
		t.Fatalf("response id = %v, want 7", resp.ID)
	}
	if !strings.Contains(string(resp.Result), "cancelled") {
		t.Errorf("result = %s, want the hook's cancelled outcome", resp.Result)
	}
}

// TestACPOnRequestError pins the failure side: a hook error becomes a
// JSON-RPC error response (not a dropped request the agent would block
// on forever).
func TestACPOnRequestError(t *testing.T) {
	onRequest := func(method string, params json.RawMessage) (any, error) {
		return nil, fmt.Errorf("r-ed does not handle %s", method)
	}
	_, agt, done := pipeClientACP(t, nil, onRequest, nil)
	defer done()

	agt.write(t, `{"jsonrpc":"2.0","id":9,"method":"fs/read_text_file","params":{}}`)
	resp := agt.read(t)
	if resp.ID == nil || *resp.ID != 9 {
		t.Fatalf("response id = %v, want 9", resp.ID)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "fs/read_text_file") {
		t.Errorf("error = %+v, want method-not-found naming the method", resp.Error)
	}
}

// TestACPNotificationDispatch pins that agent notifications reach
// onNotify with method and raw params — the session/update path.
func TestACPNotificationDispatch(t *testing.T) {
	got := make(chan string, 1)
	onNotify := func(method string, params json.RawMessage) {
		got <- method + ":" + string(params)
	}
	_, agt, done := pipeClientACP(t, onNotify, nil, nil)
	defer done()

	agt.write(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s-1"}}`)
	select {
	case s := <-got:
		if !strings.HasPrefix(s, "session/update:") || !strings.Contains(s, "s-1") {
			t.Errorf("onNotify saw %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification never dispatched")
	}
}
