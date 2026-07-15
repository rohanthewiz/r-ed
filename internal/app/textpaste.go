// =============================================================================
// File: internal/app/textpaste.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-15
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// textpaste.go implements bracketed-paste handling for the editor buffer.
// The screen enables paste mode (see App.New's scr.EnablePaste), so a
// terminal paste arrives wrapped in an EventPaste{start} … content-as-
// key-events … EventPaste{end} sequence instead of a raw stream of
// keystrokes. When an editable tab is the paste target we accumulate the
// content verbatim and splice it in as ONE InsertString.
//
// That single verbatim insert is the whole point. Replaying a paste as
// keystrokes ran each pasted tab through handleKey's `KeyTab →
// tab.IndentUnit` branch (so pasted code lost its exact indentation when
// the buffer's unit was spaces) and pushed every rune through the
// leader/shortcut machinery. Inserting the accumulated string once keeps
// tabs and newlines byte-for-byte and collapses the paste into a single
// undo step.
//
// Only the editor is gated. When a modal, the find bar, the menu, or a
// focused terminal owns the keyboard, `pasting` stays false and the
// content flows through handleKey as ordinary key events — exactly the
// pre-bracketed-paste behavior, so those single-line inputs are
// unaffected and there's no regression. Terminals that don't understand
// the enable sequence never send the markers, so the paste also arrives
// as raw keys there: the fix degrades silently, same contract as the
// formatter and LSP layers.
//
// All handling runs on the main loop; there is no locking here.

package app

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/r-ed/internal/editor"
)

// handlePaste processes a bracketed-paste boundary event. A start marker
// arms accumulation only when an editable tab is the focus target;
// otherwise it leaves `pasting` false so the content flows as normal key
// events. An end marker flushes whatever was accumulated into that tab
// as a single insert.
func (a *App) handlePaste(ev *tcell.EventPaste) {
	if ev.Start() {
		a.pasting = a.editorPasteTarget() != nil
		a.pasteBuf = a.pasteBuf[:0]
		return
	}
	if !a.pasting {
		return
	}
	a.pasting = false
	if len(a.pasteBuf) == 0 {
		return
	}
	// Re-resolve the target rather than trust the one seen at start:
	// nothing can steal focus mid-paste (no events run between the
	// markers), but a nil-safe re-check means a flush never panics if
	// that assumption ever changes.
	if tab := a.editorPasteTarget(); tab != nil {
		tab.InsertString(string(a.pasteBuf))
	}
	a.pasteBuf = a.pasteBuf[:0]
}

// accumulatePaste appends one key event's literal content to the paste
// buffer while a paste is in flight. Enter becomes a newline and Tab a
// literal tab — the exact characters raw handleKey would otherwise
// destroy (IndentUnit substitution, leader routing). Non-text keys inside
// a paste (arrows, control chars, a stray Esc from the parser) carry no
// insertable content and are dropped.
func (a *App) accumulatePaste(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyRune:
		a.pasteBuf = append(a.pasteBuf, ev.Rune())
	case tcell.KeyEnter:
		a.pasteBuf = append(a.pasteBuf, '\n')
	case tcell.KeyTab:
		a.pasteBuf = append(a.pasteBuf, '\t')
	}
}

// editorPasteTarget returns the tab a paste should land in, or nil when
// some other surface (modal, find bar, menu, or a focused terminal) owns
// the keyboard or the active tab can't accept text (none open, or an
// image preview). It is the single source of truth for "is the editor
// the paste target", used both to arm accumulation and to flush it, so
// the two can never disagree.
func (a *App) editorPasteTarget() *editor.Tab {
	if a.modal != nil || a.findOpen || a.menuOpen {
		return nil
	}
	if a.term.open && a.term.focused {
		return nil
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return nil
	}
	return tab
}
