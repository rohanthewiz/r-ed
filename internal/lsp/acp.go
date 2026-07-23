// =============================================================================
// File: internal/lsp/acp.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// acp.go adapts the JSON-RPC client to the Agent Client Protocol (ACP)
// — the protocol copilot-language-server speaks in --acp mode for chat.
// ACP reuses the JSON-RPC 2.0 envelope this package already implements;
// the only wire difference is framing (one JSON object per line instead
// of Content-Length headers), plus the fact that ACP agents send the
// client REAL requests — permission prompts, filesystem access — that
// need domain answers, not the LSP auto-responder's nulls. Hence the
// two knobs these constructors set: Client.ndjson and Client.onRequest.
//
// Kept inside internal/lsp on purpose: the transport is protocol-
// generic (the same Call/Notify/readLoop machinery serves gopls, the
// Copilot LSP sidecar, and now ACP), and a second framing package would
// just duplicate the correlation and lifecycle code.

package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

// NewClientACP wraps an existing reader/writer pair in an ACP-framed
// client (newline-delimited JSON) and starts the read loop. onRequest
// answers agent→client requests and runs on the read-loop goroutine —
// see the field docs on Client. Split from StartACP so tests can drive
// the protocol over in-memory pipes without spawning a process, same
// as NewClient.
func NewClientACP(r io.Reader, w io.Writer,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) *Client {
	c := &Client{
		w:         w,
		r:         bufio.NewReader(r),
		pending:   map[int64]chan *message{},
		ndjson:    true,
		onNotify:  onNotify,
		onRequest: onRequest,
		onExit:    onExit,
	}
	go c.readLoop()
	return c
}

// StartACP launches an ACP agent binary with args in dir and returns a
// Client wired to its stdio, the ACP twin of Start. The caller should
// have verified the binary exists (exec.LookPath) — same
// silent-degradation contract as the LSP starters.
func StartACP(dir, bin string, args []string,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) (*Client, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// stderr goes to our stderr (tcell owns the tty, so effectively
	// /dev/null) — deliberate, same as Start: agent logs are noise for
	// an editor user.
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
	c := NewClientACP(stdout, stdin, onNotify, onRequest, onExit)
	c.cmd = cmd
	return c, nil
}

// readLineMessage parses one newline-delimited JSON-RPC message — the
// ACP framing. Blank lines are skipped (harmless keep-alives some
// agents emit); a line that isn't valid JSON is a hard protocol error,
// same rationale as lost Content-Length framing — there's no way to
// resynchronise once a record boundary is wrong.
func readLineMessage(r *bufio.Reader) (*message, error) {
	for {
		line, err := r.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err // EOF (or pipe error) with nothing pending
			}
			continue
		}
		// A final record may arrive without its trailing newline right
		// before EOF — parse what we have rather than dropping it.
		var m message
		if uerr := json.Unmarshal(trimmed, &m); uerr != nil {
			return nil, fmt.Errorf("lsp: bad ndjson message: %w", uerr)
		}
		return &m, nil
	}
}
