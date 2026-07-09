// =============================================================================
// File: internal/lsp/client.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// client.go is the JSON-RPC 2.0 transport under the LSP client: framing
// (Content-Length headers over stdio), request/response correlation,
// notification dispatch, and the handful of server→client requests a
// minimal client must answer for gopls not to stall.
//
//	main loop ──Call/Notify──► Client ──stdin──► gopls
//	    ▲                        │
//	    │   onNotify (goroutine) │◄──stdout── readLoop goroutine
//	    └── caller posts a tcell event; only the main loop touches App
//
// Thread model: Call and Notify are safe from any goroutine (writes are
// mutex-serialised). The read loop runs on its own goroutine and calls
// onNotify / resolves pending Calls from there — so onNotify must never
// touch editor state directly; the app layer posts custom tcell events
// instead, the same goroutine→main-loop bridge every background job in
// this codebase uses.

package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// callTimeout bounds how long a Call waits for the server's response.
// Definition and hover on a warm gopls answer in tens of milliseconds;
// five seconds covers a cold server still type-checking a big package
// without letting a wedged server leak goroutines forever.
const callTimeout = 5 * time.Second

// message is the JSON-RPC envelope, used for both directions. Which
// fields are set determines the shape: ID+Method = request,
// ID+Result/Error = response, Method alone = notification.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *respError      `json:"error,omitempty"`
}

// respError is the JSON-RPC error object of a failed response.
type respError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client is a JSON-RPC connection to one language server process.
type Client struct {
	writeMu sync.Mutex
	w       io.Writer
	r       *bufio.Reader

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *message
	closed  bool

	// onNotify receives server→client notifications (method + raw
	// params). Called on the read-loop goroutine — implementations must
	// hand off to their own event loop, not mutate shared state.
	onNotify func(method string, params json.RawMessage)

	// onExit fires once when the read loop ends (server exited, pipe
	// closed, or protocol error). Also called from the read-loop
	// goroutine.
	onExit func(err error)

	// cmd is the spawned server process when the client came from
	// Start; nil for clients built over arbitrary pipes (tests).
	cmd *exec.Cmd
}

// NewClient wraps an existing reader/writer pair (the server's stdout /
// stdin) and starts the read loop. Split from Start so tests can drive
// the protocol over in-memory pipes without spawning a process.
func NewClient(r io.Reader, w io.Writer, onNotify func(string, json.RawMessage), onExit func(error)) *Client {
	c := &Client{
		w:        w,
		r:        bufio.NewReader(r),
		pending:  map[int64]chan *message{},
		onNotify: onNotify,
		onExit:   onExit,
	}
	go c.readLoop()
	return c
}

// Start launches the server binary with args in dir and returns a
// Client wired to its stdio. The caller should have verified the
// binary exists (exec.LookPath) — a missing binary errors here too,
// but checking first keeps "gopls not installed" a silent no-op
// instead of a surfaced failure.
func Start(dir, bin string, args []string, onNotify func(string, json.RawMessage), onExit func(error)) (*Client, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// The server's stderr goes to our stderr, which tcell has taken
	// over — effectively /dev/null. Deliberate: gopls logs are noise
	// for an editor user, and capturing them would need another drain
	// goroutine for no user-visible benefit.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := NewClient(stdout, stdin, onNotify, onExit)
	c.cmd = cmd
	return c, nil
}

// Call sends a request and blocks until the response arrives, then
// unmarshals its result into result (skipped when result is nil).
// Times out after callTimeout so a wedged server can't hang the
// calling goroutine forever. Safe from any goroutine.
func (c *Client) Call(method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("lsp: connection closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(&message{JSONRPC: "2.0", ID: &id, Method: method, Params: marshalParams(params)}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return fmt.Errorf("lsp: connection closed")
		}
		if resp.Error != nil {
			return fmt.Errorf("lsp: %s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-time.After(callTimeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("lsp: %s timed out", method)
	}
}

// Notify sends a fire-and-forget notification. Safe from any goroutine.
func (c *Client) Notify(method string, params any) error {
	return c.send(&message{JSONRPC: "2.0", Method: method, Params: marshalParams(params)})
}

// Close tears the connection down: best-effort shutdown/exit handshake
// when a process is attached, then kill as a backstop so a deaf server
// can't outlive the editor. Idempotent.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	// Polite LSP goodbye. Fire-and-forget notifications only — a full
	// shutdown Call would block Close for callTimeout on a wedged
	// server, and the editor is exiting either way.
	_ = c.Notify("exit", nil)
	if wc, ok := c.w.(io.Closer); ok {
		_ = wc.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// Reap on a goroutine so Close never blocks on a slow exit;
		// kill after a grace period if the exit notification wasn't
		// enough.
		proc := c.cmd
		go func() {
			done := make(chan struct{})
			go func() { _ = proc.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = proc.Process.Kill()
				<-done
			}
		}()
	}
}

// send frames and writes one message. Serialised by writeMu so
// concurrent Calls/Notifies can't interleave their bytes.
func (c *Client) send(m *message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

// marshalParams pre-encodes params so the envelope marshal can't fail
// halfway. nil params stay nil (the field is omitempty).
func marshalParams(params any) json.RawMessage {
	if params == nil {
		return nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		// Params are always our own structs; a marshal failure is a
		// programming error. Sending null keeps the wire valid.
		return json.RawMessage("null")
	}
	return b
}

// readLoop drains server messages until the pipe closes, routing each
// to the pending Call it answers, the notification callback, or the
// server-request auto-responder.
func (c *Client) readLoop() {
	var loopErr error
	for {
		m, err := readMessage(c.r)
		if err != nil {
			if err != io.EOF {
				loopErr = err
			}
			break
		}
		switch {
		case m.ID != nil && m.Method != "":
			// Server→client request. A minimal client still has to
			// answer these or gopls blocks waiting (it really does —
			// workspace/configuration gates type-checking).
			c.respondToServer(m)
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		}
	}

	// Connection over — fail every in-flight Call so nothing blocks
	// until its timeout, and let the owner know.
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- nil
	}
	c.mu.Unlock()
	if c.onExit != nil {
		c.onExit(loopErr)
	}
}

// respondToServer answers a server→client request with the emptiest
// legal payload. workspace/configuration must echo one element per
// requested item (gopls waits on it); everything else — registration,
// progress-token creation, message requests — accepts a null result.
func (c *Client) respondToServer(m *message) {
	var result any
	if m.Method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(m.Params, &p)
		empties := make([]any, len(p.Items))
		for i := range empties {
			empties[i] = map[string]any{}
		}
		result = empties
	}
	raw, _ := json.Marshal(result)
	_ = c.send(&message{JSONRPC: "2.0", ID: m.ID, Result: raw})
}

// readMessage parses one Content-Length-framed JSON-RPC message.
// Unknown headers (Content-Type) are skipped; a missing or malformed
// Content-Length is a hard protocol error — there is no way to
// resynchronise a byte stream once framing is lost.
func readMessage(r *bufio.Reader) (*message, error) {
	contentLen := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line ends the header block
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", v)
			}
			contentLen = n
		}
	}
	if contentLen < 0 {
		return nil, fmt.Errorf("lsp: missing Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("lsp: bad message body: %w", err)
	}
	return &m, nil
}

// -----------------------------------------------------------------------------
// LSP-level convenience wrappers
// -----------------------------------------------------------------------------

// Initialize runs the initialize → initialized handshake for the
// workspace rooted at rootDir. The capability set is the minimal
// honest one: full-text sync, plaintext-preferred hover, and the
// publishDiagnostics / definition defaults.
func (c *Client) Initialize(rootDir string) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   PathToURI(rootDir),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{},
				"definition":         map[string]any{},
				"hover": map[string]any{
					// Plaintext first: the hover modal is a dumb text
					// box, and gopls honours the preference order.
					"contentFormat": []string{"plaintext", "markdown"},
				},
			},
		},
	}
	if err := c.Call("initialize", params, nil); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
}

// DidOpen announces a newly-opened document with its full text.
func (c *Client) DidOpen(path, languageID string, version int, text string) error {
	return c.Notify("textDocument/didOpen", DidOpenParams{
		TextDocument: TextDocumentItem{
			URI:        PathToURI(path),
			LanguageID: languageID,
			Version:    version,
			Text:       text,
		},
	})
}

// DidChange sends the document's full new text (see DidChangeParams
// for why full sync).
func (c *Client) DidChange(path string, version int, text string) error {
	return c.Notify("textDocument/didChange", DidChangeParams{
		TextDocument:   VersionedTextDocumentIdentifier{URI: PathToURI(path), Version: version},
		ContentChanges: []ContentChange{{Text: text}},
	})
}

// DidSave announces a document was written to disk.
func (c *Client) DidSave(path string) error {
	return c.Notify("textDocument/didSave", DidSaveParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
	})
}

// DidClose announces a document is no longer open in the editor.
func (c *Client) DidClose(path string) error {
	return c.Notify("textDocument/didClose", DidCloseParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
	})
}

// Definition asks where the symbol at pos is defined. Servers may
// answer with a single Location, an array, or null; all normalise to a
// (possibly empty) slice here so callers only handle one shape.
func (c *Client) Definition(path string, pos Position) ([]Location, error) {
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
		Position:     pos,
	}
	var raw json.RawMessage
	if err := c.Call("textDocument/definition", params, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var many []Location
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil {
		return []Location{one}, nil
	}
	return nil, nil
}

// HoverAt asks for hover documentation at pos. A nil result with nil
// error means "the server has nothing to say here".
func (c *Client) HoverAt(path string, pos Position) (*Hover, error) {
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
		Position:     pos,
	}
	var raw json.RawMessage
	if err := c.Call("textDocument/hover", params, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var h Hover
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
