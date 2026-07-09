// =============================================================================
// File: internal/lsp/types.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Package lsp is a minimal Language Server Protocol client, hand-rolled
// over JSON-RPC 2.0 on stdio with nothing beyond the standard library.
// The editor needs a tiny protocol subset — initialize, document sync,
// publishDiagnostics, definition, hover — so a dependency-free client is
// both smaller and easier to reason about than pulling in a full LSP
// framework (which would also fight the project's no-CGO / few-deps
// philosophy).
//
// types.go holds the wire structs for that subset plus the two
// coordinate systems the protocol forces on us:
//
//   - URIs: LSP identifies documents by file:// URI; the editor thinks
//     in absolute paths. PathToURI / URIToPath convert.
//   - UTF-16 columns: LSP character offsets count UTF-16 code units
//     (a JavaScript inheritance); the editor's Buffer counts runes.
//     UTF16Col / RuneCol convert per line. Getting this wrong shows up
//     as off-by-N diagnostics on any line containing non-BMP characters
//     (emoji in a string literal is the classic case).
package lsp

import (
	"encoding/json"
	"net/url"
	"unicode/utf16"
)

// Position is an LSP text position: zero-based line, zero-based
// character offset measured in UTF-16 code units.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open [Start, End) span of an LSP document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location names a range inside a document — the payload of a
// definition response.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic severities, per the LSP spec. Zero means the server
// omitted the field; the spec says to treat that as an error.
const (
	SeverityError   = 1
	SeverityWarning = 2
	SeverityInfo    = 3
	SeverityHint    = 4
)

// Diagnostic is one server-reported problem in a document.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// PublishDiagnosticsParams is the payload of the
// textDocument/publishDiagnostics notification.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// TextDocumentItem describes a document being opened.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentIdentifier names an already-open document.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier names a document plus the client's
// version counter — required by didChange so the server can order
// edits.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// DidOpenParams is the payload of textDocument/didOpen.
type DidOpenParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeParams is the payload of textDocument/didChange. The editor
// always sends one change event carrying the full document text (a
// change with no range means "replace everything" per the spec), which
// sidesteps incremental-edit bookkeeping entirely — the files this
// editor handles are small enough that full sync is cheap.
type DidChangeParams struct {
	TextDocument   VersionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []ContentChange                 `json:"contentChanges"`
}

// ContentChange is one edit in a didChange. With Range omitted it
// replaces the whole document.
type ContentChange struct {
	Text string `json:"text"`
}

// DidSaveParams is the payload of textDocument/didSave.
type DidSaveParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// DidCloseParams is the payload of textDocument/didClose.
type DidCloseParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// TextDocumentPositionParams is the shared request payload of
// definition and hover — a document plus a position in it.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// Hover is the response payload of textDocument/hover. Contents is
// left raw because servers are allowed to send several shapes
// (MarkupContent, MarkedString, arrays of either); HoverText flattens
// whichever arrives into plain lines.
type Hover struct {
	Contents json.RawMessage `json:"contents"`
	Range    *Range          `json:"range,omitempty"`
}

// markupContent is the modern hover payload: {kind, value}.
type markupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// HoverText flattens a hover's Contents into its text value. It
// tolerates the three shapes real servers send — MarkupContent
// ({kind, value}), a bare string, and an array of strings /
// {language, value} objects — and returns "" when the payload is
// empty or unrecognised, which callers treat as "nothing to show".
func (h Hover) HoverText() string {
	if len(h.Contents) == 0 {
		return ""
	}
	// MarkupContent — what gopls sends when the client advertises it.
	var mc markupContent
	if err := json.Unmarshal(h.Contents, &mc); err == nil && mc.Value != "" {
		return mc.Value
	}
	// Bare MarkedString.
	var s string
	if err := json.Unmarshal(h.Contents, &s); err == nil {
		return s
	}
	// Array of MarkedStrings (each a string or {language, value}).
	var arr []json.RawMessage
	if err := json.Unmarshal(h.Contents, &arr); err == nil {
		out := ""
		for _, el := range arr {
			var es string
			if json.Unmarshal(el, &es) == nil {
				if out != "" {
					out += "\n"
				}
				out += es
				continue
			}
			var emc markupContent
			if json.Unmarshal(el, &emc) == nil && emc.Value != "" {
				if out != "" {
					out += "\n"
				}
				out += emc.Value
			}
		}
		return out
	}
	return ""
}

// -----------------------------------------------------------------------------
// URI conversion
// -----------------------------------------------------------------------------

// PathToURI converts an absolute filesystem path to a file:// URI.
// Built via net/url so paths with spaces or other reserved characters
// escape correctly instead of producing URIs some servers reject.
func PathToURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

// URIToPath converts a file:// URI back to a filesystem path. Non-file
// or unparseable URIs return "" — the caller treats that as "not a
// document we can open" (e.g. a definition inside a zipped stdlib
// archive some servers report with custom schemes).
func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// -----------------------------------------------------------------------------
// UTF-16 ↔ rune column conversion
// -----------------------------------------------------------------------------

// UTF16Col converts a rune column in line to the UTF-16 code-unit
// column LSP expects. Runes outside the Basic Multilingual Plane
// (emoji, some CJK extensions) occupy two code units; everything else
// one. A runeCol past the end of the line clamps to the line's total
// UTF-16 length, matching the spec's "clamp to line length" rule.
func UTF16Col(line []rune, runeCol int) int {
	col := 0
	for i, r := range line {
		if i >= runeCol {
			break
		}
		col += utf16.RuneLen(r)
	}
	return col
}

// RuneCol converts a UTF-16 code-unit column from the server back to a
// rune index into line. Columns past the end clamp to len(line); a
// column landing on the second unit of a surrogate pair resolves to
// that rune's index (you can't address half a rune).
func RuneCol(line []rune, utf16Col int) int {
	col := 0
	for i, r := range line {
		// A target anywhere inside this rune's code units — including
		// the second half of a surrogate pair — addresses this rune.
		if utf16Col < col+utf16.RuneLen(r) {
			return i
		}
		col += utf16.RuneLen(r)
	}
	return len(line)
}
