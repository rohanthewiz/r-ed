// =============================================================================
// File: internal/app/hovermodal.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// hovermodal.go is the LSP hover popup — a borderless-feeling tooltip
// that anchors to the caret instead of centering like every other
// modal. It still implements the standard single-slot modal interface
// so openModal's mutual-exclusion and the key/mouse routing all apply
// unchanged; only the geometry is special.
//
// Dismissal is deliberately trigger-happy: ANY key and any click
// dismiss it. Hover is a glance, not a workspace — the user's next
// action is always "back to editing", and eating that keystroke to
// make them close a tooltip first would feel broken.

package app

import (
	"github.com/gdamore/tcell/v2"
)

// hoverModalMaxWidth caps the popup so a long single-line signature
// wraps the server's problem, not ours — we truncate with an ellipsis
// instead of wrapping, because hover text is a preview, not a reader.
const hoverModalMaxWidth = 66

// hoverModal shows flattened hover text near the cursor.
type hoverModal struct {
	lines []string
}

// handleKey dismisses on any key — see the file comment for why.
func (m *hoverModal) handleKey(a *App, _ *tcell.EventKey) {
	a.closeModal()
}

// handleMouse dismisses on any button press, inside or out. Wheel and
// pure motion events pass through silently so an accidental scroll
// doesn't close the popup before it's been read.
func (m *hoverModal) handleMouse(a *App, _, _ int, btn tcell.ButtonMask) {
	if btn&(tcell.Button1|tcell.Button2|tcell.Button3) != 0 {
		a.closeModal()
	}
}

// rect computes the popup rectangle anchored to the caret: preferred
// position is one row below the cursor (tooltip convention); when the
// bottom of the window would clip it, it flips above. X follows the
// caret but clamps into the window. A cursor that's scrolled offscreen
// falls back to the centered position every other modal uses.
func (m *hoverModal) rect(a *App) (x, y, w, h int) {
	w = 4 // border + one cell padding each side
	for _, ln := range m.lines {
		if lw := runeLen(ln) + 4; lw > w {
			w = lw
		}
	}
	if w > hoverModalMaxWidth {
		w = hoverModalMaxWidth
	}
	if w > a.width {
		w = a.width
	}
	h = len(m.lines) + 2 // top and bottom border rows

	ex, ey, ew, eh := a.editorRect()
	t := a.activeTabPtr()
	if t == nil {
		return a.centeredRect(w, h)
	}
	dx, dy, ok := t.CursorScreenCell(ew, eh)
	if !ok {
		return a.centeredRect(w, h)
	}
	cx, cy := ex+dx, ey+dy

	x = cx
	if x+w > a.width {
		x = a.width - w
	}
	if x < 0 {
		x = 0
	}
	y = cy + 1 // below the caret
	if y+h > a.height-1 {
		y = cy - h // flip above
	}
	if y < 0 {
		y = 0
	}
	return x, y, w, h
}

// draw paints the popup: plain bordered box, no title row — a tooltip
// with a "Hover   esc" header would be all chrome and no content.
func (m *hoverModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	fillRect(a.screen, mx, my, mw, mh, c.bgSt)
	drawBorder(a.screen, mx, my, mw, mh, c.border)

	for i, ln := range m.lines {
		if runeLen(ln) > mw-4 {
			ln = string([]rune(ln)[:mw-5]) + "…"
		}
		drawAt(a.screen, mx+2, my+1+i, ln, c.body)
	}
	a.screen.HideCursor()
}
