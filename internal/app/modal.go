// =============================================================================
// File: internal/app/modal.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// modal.go defines the single-slot modal abstraction every secondary
// overlay (prompt, confirm, dirty-close, form, tree context menu, file
// finder) implements. Exactly one modal can be up at a time — App.modal
// holds it, nil means none — so the event router needs one dispatch
// instead of a per-modal precedence chain, and adding a new modal means
// implementing the interface rather than threading new fields through
// App, handleKey, handleMouse, closeAllModals, and draw in lockstep.
//
// Each modal struct owns its own state, so "capture the callback before
// closing" gymnastics are unnecessary: closing just clears App.modal
// while the method's receiver keeps the fields alive for the duration
// of the call.
//
// The file also hosts two small shared building blocks:
//
//   - btnRect: a clickable button rectangle. Draw code and mouse
//     hit-tests consume the same btnRect, which kills the old failure
//     mode where a button's drawn columns and its click columns were
//     hard-coded separately and drifted apart.
//   - textField: single-line text input state (value, caret, hscroll)
//     with shared key handling, click-to-position, and rendering.
//     Previously triplicated across the prompt, form, and finder.

package app

import "github.com/gdamore/tcell/v2"

// modal is the contract every secondary overlay implements. While a
// modal is open it owns the keyboard and the mouse: handleKey and
// handleMouse receive every event, and by convention Esc cancels and a
// click outside the modal's rectangle dismisses it. draw paints the
// modal above everything else; it runs last in App.draw.
type modal interface {
	handleKey(a *App, ev *tcell.EventKey)
	handleMouse(a *App, x, y int, btn tcell.ButtonMask)
	draw(a *App)
}

// openModal installs m as the active modal, dismissing any other modal
// (and the main menu / find bar) first so overlays stay mutually
// exclusive — the invariant the old per-modal open helpers each
// enforced by hand.
func (a *App) openModal(m modal) {
	a.closeAllModals()
	a.modal = m
}

// closeModal dismisses the active modal without running any callback.
// Kept tiny and symmetrical with openModal so intent reads clearly at
// call sites ("dismiss" vs "the modal finished its job").
func (a *App) closeModal() {
	a.modal = nil
}

// -----------------------------------------------------------------------------
// Buttons
// -----------------------------------------------------------------------------

// btnRect is a one-row button: origin plus width in cells. Modals
// compute their button rects in a single method that both draw and
// mouse hit-testing call, so geometry can't drift between the two.
type btnRect struct {
	x, y, w int
}

// contains reports whether the screen cell (x, y) falls inside the button.
func (b btnRect) contains(x, y int) bool {
	return y == b.y && x >= b.x && x < b.x+b.w
}

// -----------------------------------------------------------------------------
// Shared single-line text field
// -----------------------------------------------------------------------------

// textField is the state of a single-line text input: the value, the
// caret position (rune index), and the horizontal scroll offset that
// keeps the caret visible when the value outgrows the field.
type textField struct {
	value  []rune
	cursor int
	scroll int
}

// newTextField builds a field pre-filled with initial, caret at the end
// — the convention every input in the editor uses so the user can
// immediately append or select-all-and-retype.
func newTextField(initial string) textField {
	v := []rune(initial)
	return textField{value: v, cursor: len(v)}
}

// String returns the field's current value.
func (f *textField) String() string { return string(f.value) }

// handleKey applies one keystroke of standard single-line editing:
// Left/Right/Home/End caret motion, Backspace/Delete, printable rune
// insertion. Esc and Enter are deliberately not handled — those mean
// different things to different modals, so callers route them first.
// Returns handled=true when the key was consumed and edited=true when
// the value changed, so callers can refresh dependent state (e.g. the
// finder re-runs its query only on real edits).
func (f *textField) handleKey(ev *tcell.EventKey) (handled, edited bool) {
	switch ev.Key() {
	case tcell.KeyLeft:
		if f.cursor > 0 {
			f.cursor--
		}
		return true, false
	case tcell.KeyRight:
		if f.cursor < len(f.value) {
			f.cursor++
		}
		return true, false
	case tcell.KeyHome:
		f.cursor = 0
		return true, false
	case tcell.KeyEnd:
		f.cursor = len(f.value)
		return true, false
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if f.cursor > 0 {
			f.value = append(f.value[:f.cursor-1], f.value[f.cursor:]...)
			f.cursor--
			return true, true
		}
		return true, false
	case tcell.KeyDelete:
		if f.cursor < len(f.value) {
			f.value = append(f.value[:f.cursor], f.value[f.cursor+1:]...)
			return true, true
		}
		return true, false
	case tcell.KeyRune:
		r := ev.Rune()
		if r < 0x20 {
			return true, false
		}
		next := make([]rune, 0, len(f.value)+1)
		next = append(next, f.value[:f.cursor]...)
		next = append(next, r)
		next = append(next, f.value[f.cursor:]...)
		f.value = next
		f.cursor++
		return true, true
	}
	return false, false
}

// clickAt moves the caret to the rune under a click at screen column x,
// given the field's visible span [fieldStart, fieldEnd). Clicks past
// the end of the value park the caret at the end.
func (f *textField) clickAt(fieldStart, fieldEnd, x int) {
	if x < fieldStart || x >= fieldEnd {
		return
	}
	target := f.scroll + (x - fieldStart)
	if target < 0 {
		target = 0
	}
	if target > len(f.value) {
		target = len(f.value)
	}
	f.cursor = target
}

// adjustScroll slides the horizontal scroll window so the caret stays
// visible within a field of the given width.
func (f *textField) adjustScroll(width int) {
	if width <= 0 {
		f.scroll = 0
		return
	}
	if f.cursor < f.scroll {
		f.scroll = f.cursor
	}
	if f.cursor >= f.scroll+width {
		f.scroll = f.cursor - width + 1
	}
	if f.scroll < 0 {
		f.scroll = 0
	}
}

// draw renders the field on row y across [fieldStart, fieldEnd),
// including the one-cell inset on each side that every modal input
// paints, and places the terminal caret when showCaret is set (the
// focused field owns the real cursor; unfocused form rows don't).
func (f *textField) draw(scr tcell.Screen, y, fieldStart, fieldEnd int, style tcell.Style, showCaret bool) {
	width := fieldEnd - fieldStart
	f.adjustScroll(width)
	for cx := fieldStart - 1; cx <= fieldEnd; cx++ {
		scr.SetContent(cx, y, ' ', nil, style)
	}
	for i := 0; i < width; i++ {
		idx := f.scroll + i
		if idx >= len(f.value) {
			break
		}
		scr.SetContent(fieldStart+i, y, f.value[idx], nil, style)
	}
	if showCaret {
		caret := fieldStart + (f.cursor - f.scroll)
		if caret >= fieldStart && caret <= fieldEnd {
			scr.ShowCursor(caret, y)
		}
	}
}
