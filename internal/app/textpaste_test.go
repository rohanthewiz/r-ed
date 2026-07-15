// =============================================================================
// File: internal/app/textpaste_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-15
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// feedPaste drives a bracketed paste through the real event path: the
// start marker, one key event per rune (newlines as KeyEnter, tabs as
// KeyTab — exactly what tcell reports for pasted content), then the end
// marker. It goes through handleKey so the `pasting` gate is exercised,
// not bypassed.
func feedPaste(a *App, text string) {
	a.handlePaste(tcell.NewEventPaste(true))
	for _, r := range text {
		switch r {
		case '\n':
			a.handleKey(tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone))
		case '\t':
			a.handleKey(tcell.NewEventKey(tcell.KeyTab, '\t', tcell.ModNone))
		default:
			a.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
	}
	a.handlePaste(tcell.NewEventPaste(false))
}

// openBlankTab seeds and opens an empty text file, returning the app with
// that tab active — the common fixture for the paste tests.
func openBlankTab(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "paste.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	if a.activeTabPtr() == nil {
		t.Fatal("expected an active tab after openFile")
	}
	return a
}

// TestPaste_PreservesLineFormatting is the regression pin for the report
// that pasted text lost its line formatting: a multi-line, indented blob
// must land in the buffer byte-for-byte, newlines and leading tabs intact.
func TestPaste_PreservesLineFormatting(t *testing.T) {
	a := openBlankTab(t)
	src := "func main() {\n\tfmt.Println(\"hi\")\n\tif x {\n\t\ty()\n\t}\n}"

	feedPaste(a, src)

	if got := a.activeTabPtr().Buffer.String(); got != src {
		t.Fatalf("paste mangled formatting:\n got %q\nwant %q", got, src)
	}
}

// TestPaste_TabStaysLiteral pins the core bug: a pasted tab must be
// inserted verbatim, NOT expanded to the buffer's IndentUnit. The
// contrast keystroke shows the old, non-paste path still expands a Tab to
// IndentUnit — so the difference is genuinely the paste gate, not a
// coincidence.
func TestPaste_TabStaysLiteral(t *testing.T) {
	a := openBlankTab(t)
	a.activeTabPtr().IndentUnit = "    " // 4 spaces

	feedPaste(a, "\tx")
	if got := a.activeTabPtr().Buffer.String(); got != "\tx" {
		t.Fatalf("pasted tab should stay literal; got %q, want %q", got, "\tx")
	}

	// A Tab typed OUTSIDE a paste still expands to IndentUnit — proves the
	// literal-tab behavior above is the paste path doing its job.
	a.handleKey(tcell.NewEventKey(tcell.KeyTab, '\t', tcell.ModNone))
	if got := a.activeTabPtr().Buffer.String(); got != "\tx    " {
		t.Fatalf("typed Tab should expand to IndentUnit; got %q", got)
	}
}

// TestPaste_SingleUndoStep verifies a whole paste collapses into one undo
// step (one InsertString), not one per character — undoing once must clear
// the entire paste.
func TestPaste_SingleUndoStep(t *testing.T) {
	a := openBlankTab(t)

	feedPaste(a, "line one\nline two\nline three")
	if a.activeTabPtr().Buffer.String() == "" {
		t.Fatal("paste inserted nothing")
	}

	a.activeTabPtr().Undo()
	if got := a.activeTabPtr().Buffer.String(); got != "" {
		t.Fatalf("one Undo should clear the whole paste; got %q", got)
	}
}

// TestPaste_EmptyPasteNoop confirms a paste that carries no content
// leaves the buffer and paste state untouched.
func TestPaste_EmptyPasteNoop(t *testing.T) {
	a := openBlankTab(t)
	a.activeTabPtr().InsertString("seed")

	a.handlePaste(tcell.NewEventPaste(true))
	a.handlePaste(tcell.NewEventPaste(false))

	if a.pasting {
		t.Fatal("pasting flag should be cleared after an empty paste")
	}
	if got := a.activeTabPtr().Buffer.String(); got != "seed" {
		t.Fatalf("empty paste changed the buffer: %q", got)
	}
}

// TestEditorPasteTarget_Gating checks that the paste target resolves to
// the active tab only when the editor truly owns the keyboard, and to nil
// when a modal, a focused terminal, or an empty workspace should swallow
// the paste instead.
func TestEditorPasteTarget_Gating(t *testing.T) {
	a := openBlankTab(t)
	tab := a.activeTabPtr()

	if a.editorPasteTarget() != tab {
		t.Fatal("editor should be the paste target with a plain active tab")
	}

	// A focused terminal owns the keyboard — paste must not divert here.
	a.term.open, a.term.focused = true, true
	if a.editorPasteTarget() != nil {
		t.Fatal("focused terminal should suppress the editor paste target")
	}
	a.term.open, a.term.focused = false, false

	// A modal owns the keyboard too.
	a.modal = &confirmModal{}
	if a.editorPasteTarget() != nil {
		t.Fatal("open modal should suppress the editor paste target")
	}
	a.modal = nil

	// No open tab: nothing to paste into.
	a.tabs = nil
	a.activeTab = -1
	if a.editorPasteTarget() != nil {
		t.Fatal("no active tab should yield a nil paste target")
	}
}

// TestPaste_NotGatedWhenModalOpen verifies that starting a paste while a
// modal owns the keyboard leaves `pasting` false, so the content is NOT
// diverted into the editor buffer (it flows to the modal as normal keys).
func TestPaste_NotGatedWhenModalOpen(t *testing.T) {
	a := openBlankTab(t)
	a.modal = &confirmModal{}

	a.handlePaste(tcell.NewEventPaste(true))
	if a.pasting {
		t.Fatal("paste must not arm accumulation while a modal is open")
	}
}
