// =============================================================================
// File: internal/lsp/client_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer is an in-memory language server: it reads framed messages
// from the client and lets tests script responses, so the whole
// JSON-RPC layer is exercised without spawning a process.
type fakeServer struct {
	in  *bufio.Reader // client → server
	out io.Writer     // server → client
}

// pipeClient wires a Client to a fakeServer over two in-memory pipes.
func pipeClient(t *testing.T, onNotify func(string, json.RawMessage), onExit func(error)) (*Client, *fakeServer, func()) {
	t.Helper()
	cliR, srvW := io.Pipe() // server writes → client reads
	srvR, cliW := io.Pipe() // client writes → server reads
	c := NewClient(cliR, cliW, onNotify, onExit)
	srv := &fakeServer{in: bufio.NewReader(srvR), out: srvW}
	return c, srv, func() { _ = srvW.Close(); _ = cliW.Close() }
}

// read returns the next message the client sent.
func (s *fakeServer) read(t *testing.T) *message {
	t.Helper()
	m, err := readMessage(s.in)
	if err != nil {
		t.Fatalf("fake server read: %v", err)
	}
	return m
}

// write frames and sends a raw JSON body to the client.
func (s *fakeServer) write(t *testing.T, body string) {
	t.Helper()
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("fake server write: %v", err)
	}
}

// TestReadMessageFraming pins the header parser: a valid frame decodes,
// unknown headers are skipped, and a missing Content-Length is a hard
// error because the stream can't be resynchronised.
func TestReadMessageFraming(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"m"}`
	raw := fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	m, err := readMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if m.Method != "m" {
		t.Errorf("method = %q, want m", m.Method)
	}

	_, err = readMessage(bufio.NewReader(strings.NewReader("Content-Type: x\r\n\r\n{}")))
	if err == nil {
		t.Error("missing Content-Length should be an error")
	}
}

// TestCallRoundTrip drives a full request/response cycle: the call
// blocks, the fake server answers by id, and the result unmarshals into
// the caller's struct.
func TestCallRoundTrip(t *testing.T) {
	c, srv, done := pipeClient(t, nil, nil)
	defer done()

	type res struct{ OK bool }
	var got res
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call("test/echo", map[string]int{"x": 1}, &got) }()

	m := srv.read(t)
	if m.Method != "test/echo" || m.ID == nil {
		t.Fatalf("server saw %+v, want test/echo request", m)
	}
	srv.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"OK":true}}`, *m.ID))

	if err := <-errCh; err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !got.OK {
		t.Error("result not unmarshalled")
	}
}

// TestCallWithTimeout pins the two behaviors CallWithTimeout exists
// for: a response that arrives after the caller's deadline fails as a
// timeout (and unblocks — a wedged server can't hang the goroutine),
// while a longer deadline than callTimeout lets a slow-but-legitimate
// exchange (the Copilot device-flow confirmation) complete.
func TestCallWithTimeout(t *testing.T) {
	c, srv, done := pipeClient(t, nil, nil)
	defer done()

	// Short deadline, request delivered but never answered: must fail
	// fast rather than block. The call runs on a goroutine because the
	// in-memory pipe's write blocks until the fake server reads.
	start := time.Now()
	slowCh := make(chan error, 1)
	go func() { slowCh <- c.CallWithTimeout("test/slow", nil, nil, 50*time.Millisecond) }()
	_ = srv.read(t) // deliver it; deliberately no response
	err := <-slowCh
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("short-deadline call: err = %v, want timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timed-out call blocked far past its deadline")
	}

	// Response after a delay, but within a generous deadline: succeeds.
	errCh := make(chan error, 1)
	go func() { errCh <- c.CallWithTimeout("test/eventually", nil, nil, 5*time.Second) }()
	m := srv.read(t)
	time.Sleep(100 * time.Millisecond)
	srv.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":null}`, *m.ID))
	if err := <-errCh; err != nil {
		t.Fatalf("delayed-but-in-deadline call: %v", err)
	}
}

// TestCallServerError pins that a JSON-RPC error response surfaces as a
// Go error naming the method — the caller's only diagnostic.
func TestCallServerError(t *testing.T) {
	c, srv, done := pipeClient(t, nil, nil)
	defer done()

	errCh := make(chan error, 1)
	go func() { errCh <- c.Call("test/fail", nil, nil) }()

	m := srv.read(t)
	srv.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"nope"}}`, *m.ID))

	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Call error = %v, want server message surfaced", err)
	}
}

// TestServerNotificationDispatch pins that notifications reach onNotify
// with method and raw params intact — the diagnostics pipeline hangs
// off this path.
func TestServerNotificationDispatch(t *testing.T) {
	type note struct {
		method string
		params json.RawMessage
	}
	ch := make(chan note, 1)
	_, srv, done := pipeClient(t, func(m string, p json.RawMessage) {
		ch <- note{m, p}
	}, nil)
	defer done()

	srv.write(t, `{"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":"file:///x.go","diagnostics":[]}}`)

	select {
	case n := <-ch:
		if n.method != "textDocument/publishDiagnostics" {
			t.Errorf("method = %q", n.method)
		}
		var p PublishDiagnosticsParams
		if err := json.Unmarshal(n.params, &p); err != nil || p.URI != "file:///x.go" {
			t.Errorf("params = %s (err %v)", n.params, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification never dispatched")
	}
}

// TestServerRequestAutoReply pins the auto-responder: gopls's
// workspace/configuration request must be answered with one element
// per item or the server stalls, and unknown requests get null.
func TestServerRequestAutoReply(t *testing.T) {
	_, srv, done := pipeClient(t, nil, nil)
	defer done()

	srv.write(t, `{"jsonrpc":"2.0","id":7,"method":"workspace/configuration","params":{"items":[{},{}]}}`)
	resp := srv.read(t)
	if resp.ID == nil || *resp.ID != 7 {
		t.Fatalf("reply id = %v, want 7", resp.ID)
	}
	var arr []map[string]any
	if err := json.Unmarshal(resp.Result, &arr); err != nil || len(arr) != 2 {
		t.Errorf("configuration reply = %s, want 2-element array", resp.Result)
	}

	srv.write(t, `{"jsonrpc":"2.0","id":8,"method":"client/registerCapability","params":{}}`)
	resp = srv.read(t)
	if resp.ID == nil || *resp.ID != 8 || string(resp.Result) != "null" {
		t.Errorf("registerCapability reply = id %v result %s, want id 8 null", resp.ID, resp.Result)
	}
}

// TestPipeCloseFailsPendingCalls pins the shutdown contract: when the
// server side dies, in-flight Calls fail promptly (not after the 5s
// timeout) and onExit fires exactly once.
func TestPipeCloseFailsPendingCalls(t *testing.T) {
	exited := make(chan error, 1)
	c, srv, _ := pipeClient(t, nil, func(err error) { exited <- err })

	errCh := make(chan error, 1)
	go func() { errCh <- c.Call("test/hang", nil, nil) }()
	srv.read(t) // make sure the request is in flight

	// Server "crashes".
	_ = srv.out.(io.Closer).Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("pending Call should fail on connection close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Call still blocked after pipe close")
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("onExit never fired")
	}
}

// TestDefinitionShapes pins the response normalisation: single
// Location, array, and null all come back as a slice.
func TestDefinitionShapes(t *testing.T) {
	cases := []struct {
		name, response string
		wantLen        int
	}{
		{"array", `[{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}]`, 1},
		{"single object", `{"uri":"file:///a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}`, 1},
		{"null", `null`, 0},
	}
	for _, tc := range cases {
		c, srv, done := pipeClient(t, nil, nil)

		type defRes struct {
			locs []Location
			err  error
		}
		ch := make(chan defRes, 1)
		go func() {
			locs, err := c.Definition("/x.go", Position{Line: 3, Character: 4})
			ch <- defRes{locs, err}
		}()
		m := srv.read(t)
		srv.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *m.ID, tc.response))

		r := <-ch
		if r.err != nil {
			t.Errorf("%s: Definition err %v", tc.name, r.err)
		}
		if len(r.locs) != tc.wantLen {
			t.Errorf("%s: got %d locations, want %d", tc.name, len(r.locs), tc.wantLen)
		}
		done()
	}
}

// TestNotifyWireFormat pins what didChange actually puts on the wire —
// full-document sync means exactly one change event with no range.
func TestNotifyWireFormat(t *testing.T) {
	c, srv, done := pipeClient(t, nil, nil)
	defer done()

	go func() { _ = c.DidChange("/tmp/x.go", 4, "package x\n") }()
	m := srv.read(t)
	if m.Method != "textDocument/didChange" {
		t.Fatalf("method = %q", m.Method)
	}
	var p DidChangeParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.TextDocument.Version != 4 || p.TextDocument.URI != "file:///tmp/x.go" {
		t.Errorf("doc id = %+v", p.TextDocument)
	}
	if len(p.ContentChanges) != 1 || p.ContentChanges[0].Text != "package x\n" {
		t.Errorf("content changes = %+v, want one full-text change", p.ContentChanges)
	}
}

// TestGoplsEndToEnd exercises the full stack against a real gopls:
// spawn, initialize, didOpen a file with a type error, and wait for
// publishDiagnostics to report it. Skipped when gopls isn't on PATH
// (CI boxes without Go tooling) — same convention as the git
// integration tests. The lspServerBinary override env var lets local
// runs point at a scratch install.
func TestGoplsEndToEnd(t *testing.T) {
	bin := os.Getenv("R_ED_TEST_GOPLS")
	if bin == "" {
		bin = "gopls"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("gopls not installed")
	}

	dir := t.TempDir()
	// A module root makes gopls treat the dir as a real workspace.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module e2e\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}
	src := filepath.Join(dir, "main.go")
	// 'undefined: notDefined' — a guaranteed diagnostic.
	code := "package main\n\nfunc main() {\n\tnotDefined()\n}\n"
	if err := os.WriteFile(src, []byte(code), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}

	diagCh := make(chan PublishDiagnosticsParams, 8)
	onNotify := func(method string, params json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p PublishDiagnosticsParams
		if json.Unmarshal(params, &p) == nil {
			diagCh <- p
		}
	}
	c, err := Start(dir, bin, nil, onNotify, nil)
	if err != nil {
		t.Fatalf("start gopls: %v", err)
	}
	defer c.Close()
	if err := c.Initialize(dir); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.DidOpen(src, "go", 1, code); err != nil {
		t.Fatalf("didOpen: %v", err)
	}

	// gopls type-checks asynchronously; give a cold cache a generous
	// window but return the moment the diagnostic lands.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case p := <-diagCh:
			if URIToPath(p.URI) != src || len(p.Diagnostics) == 0 {
				continue
			}
			msg := p.Diagnostics[0].Message
			if !strings.Contains(msg, "notDefined") {
				t.Errorf("diagnostic = %q, want mention of notDefined", msg)
			}
			// Bonus round-trip: definition from inside main() should
			// resolve — proves requests work after the notification
			// stream is flowing.
			locs, err := c.Definition(src, Position{Line: 2, Character: 6})
			if err != nil {
				t.Errorf("definition: %v", err)
			}
			_ = locs // any non-error answer is fine; main has no def target beyond itself
			return
		case <-deadline:
			t.Fatal("no diagnostics from gopls within 60s")
		}
	}
}
