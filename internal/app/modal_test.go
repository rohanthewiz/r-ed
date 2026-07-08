// =============================================================================
// File: internal/app/modal_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the single-slot modal abstraction and its shared building
// blocks (textField, btnRect), plus the typed modal accessors the rest
// of the test suite uses to reach into whichever modal is active.

package app

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// -----------------------------------------------------------------------------
// Typed accessors — shared by every modal-poking test in the package.
// Each returns the active modal when it is of that type, or nil, so
// tests can both assert openness (`promptOf(a) != nil`) and reach state
// (`promptOf(a).field.value`).
// -----------------------------------------------------------------------------

func promptOf(a *App) *promptModal {
	m, _ := a.modal.(*promptModal)
	return m
}

func confirmOf(a *App) *confirmModal {
	m, _ := a.modal.(*confirmModal)
	return m
}

func dirtyOf(a *App) *dirtyModal {
	m, _ := a.modal.(*dirtyModal)
	return m
}

func formOf(a *App) *formModal {
	m, _ := a.modal.(*formModal)
	return m
}

func contextOf(a *App) *contextModal {
	m, _ := a.modal.(*contextModal)
	return m
}

func finderOf(a *App) *finderModal {
	m, _ := a.modal.(*finderModal)
	return m
}

// keyEvent builds a plain key event for direct handler calls.
func keyEvent(key tcell.Key, r rune) *tcell.EventKey {
	return tcell.NewEventKey(key, r, tcell.ModNone)
}

// -----------------------------------------------------------------------------
// openModal / closeAllModals
// -----------------------------------------------------------------------------

// TestOpenModalIsExclusive pins the core invariant of the single-slot
// design: opening any modal displaces whatever overlay was up before —
// another modal, the main menu, or the find bar — so two overlays can
// never fight over input.
func TestOpenModalIsExclusive(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	a.openPrompt("New File", "", "", nil)
	if promptOf(a) == nil {
		t.Fatalf("prompt should be the active modal after openPrompt")
	}

	a.openConfirm("Delete?", "sure?", nil)
	if promptOf(a) != nil {
		t.Fatalf("prompt should have been displaced by the confirm modal")
	}
	if confirmOf(a) == nil {
		t.Fatalf("confirm should be the active modal after openConfirm")
	}

	a.menuOpen = true
	a.openPrompt("Rename", "", "x", nil)
	if a.menuOpen {
		t.Fatalf("opening a modal must close the main menu")
	}
}

// TestCloseAllModalsClearsSlot verifies the one-shot dismiss clears the
// modal slot alongside the menu / find-bar state it always cleared.
func TestCloseAllModalsClearsSlot(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPrompt("New File", "", "", nil)
	a.closeAllModals()
	if a.modal != nil {
		t.Fatalf("closeAllModals should nil the modal slot")
	}
	if a.anyModalOpen() {
		t.Fatalf("anyModalOpen should be false after closeAllModals")
	}
}

// -----------------------------------------------------------------------------
// textField
// -----------------------------------------------------------------------------

// TestTextFieldEditing walks the shared single-line editing behaviors:
// rune insertion at the caret, Backspace/Delete, and the edited flag
// that tells callers when dependent state (finder results, form values)
// needs a refresh.
func TestTextFieldEditing(t *testing.T) {
	f := newTextField("ab")
	if f.cursor != 2 {
		t.Fatalf("caret should start at the end, got %d", f.cursor)
	}

	if _, edited := f.handleKey(keyEvent(tcell.KeyRune, 'c')); !edited {
		t.Fatalf("rune insert should report edited")
	}
	if f.String() != "abc" {
		t.Fatalf("value = %q, want abc", f.String())
	}

	f.handleKey(keyEvent(tcell.KeyHome, 0))
	if _, edited := f.handleKey(keyEvent(tcell.KeyBackspace2, 0)); edited {
		t.Fatalf("backspace at column 0 must not report edited")
	}
	if _, edited := f.handleKey(keyEvent(tcell.KeyDelete, 0)); !edited || f.String() != "bc" {
		t.Fatalf("delete at column 0 should remove the first rune, got %q", f.String())
	}

	f.handleKey(keyEvent(tcell.KeyEnd, 0))
	if _, edited := f.handleKey(keyEvent(tcell.KeyDelete, 0)); edited {
		t.Fatalf("delete at the end must not report edited")
	}

	// Control runes below 0x20 are swallowed, never inserted.
	if _, edited := f.handleKey(keyEvent(tcell.KeyRune, 0x01)); edited || f.String() != "bc" {
		t.Fatalf("control rune should be ignored, got %q", f.String())
	}
}

// TestTextFieldClickAt pins click-to-position: clicks map through the
// scroll offset, clamp past-the-end clicks to the value length, and
// ignore clicks outside the field span.
func TestTextFieldClickAt(t *testing.T) {
	f := newTextField("hello")
	f.clickAt(10, 20, 12)
	if f.cursor != 2 {
		t.Fatalf("click at col 12 of field starting 10 should set caret 2, got %d", f.cursor)
	}
	f.clickAt(10, 20, 19)
	if f.cursor != 5 {
		t.Fatalf("click past the value should clamp to len, got %d", f.cursor)
	}
	f.clickAt(10, 20, 25)
	if f.cursor != 5 {
		t.Fatalf("click outside the span should be ignored, got %d", f.cursor)
	}
}

// TestTextFieldAdjustScroll verifies the caret is kept inside the
// visible window when it walks off either edge.
func TestTextFieldAdjustScroll(t *testing.T) {
	f := newTextField("0123456789")
	f.adjustScroll(4)
	if f.scroll != 7 {
		t.Fatalf("caret at 10 in a 4-wide field needs scroll 7, got %d", f.scroll)
	}
	f.cursor = 0
	f.adjustScroll(4)
	if f.scroll != 0 {
		t.Fatalf("caret at 0 needs scroll 0, got %d", f.scroll)
	}
	f.adjustScroll(0)
	if f.scroll != 0 {
		t.Fatalf("zero-width field must park scroll at 0, got %d", f.scroll)
	}
}

// -----------------------------------------------------------------------------
// btnRect
// -----------------------------------------------------------------------------

// TestBtnRectContains pins the half-open hit box: [x, x+w) on exactly
// row y — the contract draw code and mouse handlers share.
func TestBtnRectContains(t *testing.T) {
	b := btnRect{x: 10, y: 5, w: 8}
	if !b.contains(10, 5) || !b.contains(17, 5) {
		t.Fatalf("both ends of the button should hit")
	}
	if b.contains(18, 5) {
		t.Fatalf("one past the width must miss")
	}
	if b.contains(12, 6) {
		t.Fatalf("wrong row must miss")
	}
}
