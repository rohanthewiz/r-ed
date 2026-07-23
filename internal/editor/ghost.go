// =============================================================================
// File: internal/editor/ghost.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// ghost.go is the editor-side half of inline completions (Copilot
// "ghost text"): a proposed insertion painted dimmed at the caret,
// accepted with Tab or discarded by any other movement/edit.
//
// Ghost text is deliberately NOT a DecorationSource. Decorations can
// only restyle cells the buffer already owns; a suggestion adds cells
// that aren't in the buffer at all, shifting the tail of the cursor
// line right. So the mechanism is a splice: Render hands the cursor
// row's runes+styles through ghostOverlay, which inserts the proposal
// at the caret column, and the existing paint walk (tab stops, ScrollX,
// overflow arrows) handles the widened row with no ghost awareness.
//
//	buffer row:   func ma│in()          │ = caret
//	spliced row:  func ma┊in() {┊in()   ┊…┊ = dimmed ghost cells
//
// The overlay never mutates the buffer — accepting is the app layer's
// job (it owns the LSP-side range/insertText bookkeeping). The Tab just
// carries the display form and paints it.

package editor

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
)

// GhostText is the display form of one inline suggestion, anchored at
// the caret. Only the first line is painted inline; further lines are
// summarised by a "⋯+N" marker so the render path never has to invent
// virtual rows (which would ripple through scrolling, hit-testing, and
// every rect helper for a feature the user may discard with one key).
type GhostText struct {
	// Pos is the buffer position the suggestion was computed for. The
	// overlay refuses to paint unless it still equals the live cursor —
	// a stale ghost drawn one keystroke behind the caret reads as the
	// editor hallucinating.
	Pos Position

	// Text is the first line of the proposal — exactly what would be
	// inserted at Pos, minus any prefix the user already typed.
	Text string

	// MoreLines counts the proposal's lines beyond the first. Zero for
	// the common single-line completion.
	MoreLines int
}

// displayRunes returns the cells the overlay paints: the inline first
// line plus, for multi-line proposals, a compact "⋯+N" tail so the user
// knows Tab accepts more than what's visible.
func (g *GhostText) displayRunes() []rune {
	s := g.Text
	if g.MoreLines > 0 {
		s += fmt.Sprintf(" ⋯+%d", g.MoreLines)
	}
	return []rune(s)
}

// ghostOverlay splices the tab's ghost text into one row's runes and
// per-rune styles, returning the inputs unchanged when no ghost applies
// to lineIdx (no ghost set, ghost anchored elsewhere, or the anchor has
// drifted from the live cursor). Called by Render after decoration
// spans resolve, so precedence stays: syntax < decorations < ghost —
// and the paint walk downstream needs no changes at all.
func (t *Tab) ghostOverlay(lineIdx int, runes []rune, styles []tcell.Style, ghostStyle tcell.Style) ([]rune, []tcell.Style) {
	g := t.Ghost
	if g == nil || g.Pos.Line != lineIdx || g.Pos != t.Cursor || g.Pos.Col > len(runes) {
		return runes, styles
	}
	gr := g.displayRunes()
	if len(gr) == 0 {
		return runes, styles
	}
	col := g.Pos.Col
	outR := make([]rune, 0, len(runes)+len(gr))
	outR = append(outR, runes[:col]...)
	outR = append(outR, gr...)
	outR = append(outR, runes[col:]...)
	outS := make([]tcell.Style, 0, len(styles)+len(gr))
	outS = append(outS, styles[:col]...)
	for range gr {
		outS = append(outS, ghostStyle)
	}
	outS = append(outS, styles[col:]...)
	return outR, outS
}
