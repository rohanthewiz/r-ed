// =============================================================================
// File: internal/editor/ghost_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the ghost-text overlay (ghost.go): the splice math that
// inserts an inline suggestion into a render row, and the end-to-end
// paint through Tab.Render on a simulation screen.

package editor

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/theme"
)

// ghostTestTab builds an in-memory tab (no file) with content and the
// cursor parked at pos, ready for overlay assertions.
func ghostTestTab(content string, pos Position) *Tab {
	t := &Tab{Buffer: NewBuffer(content), StyleStale: true}
	t.Cursor = pos
	t.Anchor = pos
	return t
}

// evenStyles returns n copies of a base style — stand-ins for the
// resolved per-rune styles Render would hand the overlay.
func evenStyles(n int, st tcell.Style) []tcell.Style {
	out := make([]tcell.Style, n)
	for i := range out {
		out[i] = st
	}
	return out
}

// TestGhostOverlay_SpliceAtCursor pins the core splice: the proposal's
// runes appear exactly at the caret column with the ghost style, the
// line's tail shifts right intact, and runes/styles stay in lockstep —
// the invariant the paint walk depends on.
func TestGhostOverlay_SpliceAtCursor(t *testing.T) {
	tab := ghostTestTab("func ma() {", Position{Line: 0, Col: 7})
	tab.Ghost = &GhostText{Pos: Position{Line: 0, Col: 7}, Text: "in"}

	base := tcell.StyleDefault.Foreground(tcell.ColorWhite)
	ghost := tcell.StyleDefault.Foreground(tcell.ColorGray)
	runes := []rune("func ma() {")
	styles := evenStyles(len(runes), base)

	outR, outS := tab.ghostOverlay(0, runes, styles, ghost)
	if got := string(outR); got != "func main() {" {
		t.Errorf("spliced runes = %q, want %q", got, "func main() {")
	}
	if len(outR) != len(outS) {
		t.Fatalf("runes/styles diverged: %d runes vs %d styles", len(outR), len(outS))
	}
	// The two inserted cells carry the ghost style; neighbors keep base.
	if outS[7] != ghost || outS[8] != ghost {
		t.Error("inserted cells did not receive the ghost style")
	}
	if outS[6] != base || outS[9] != base {
		t.Error("cells around the splice lost their original style")
	}
}

// TestGhostOverlay_IgnoredWhenStale verifies the overlay's refusal
// paths: no ghost, a ghost for another line, and — the important one —
// a ghost whose anchor no longer matches the live cursor. A stale
// ghost painting one keystroke behind the caret is the bug this guards.
func TestGhostOverlay_IgnoredWhenStale(t *testing.T) {
	base := tcell.StyleDefault
	runes := []rune("hello")
	styles := evenStyles(len(runes), base)

	// No ghost at all.
	tab := ghostTestTab("hello", Position{Line: 0, Col: 5})
	if outR, _ := tab.ghostOverlay(0, runes, styles, base); len(outR) != len(runes) {
		t.Error("nil ghost should leave the row unchanged")
	}

	// Ghost anchored on a different line than the one being painted.
	tab.Ghost = &GhostText{Pos: Position{Line: 2, Col: 0}, Text: "x"}
	tab.Cursor = Position{Line: 2, Col: 0}
	if outR, _ := tab.ghostOverlay(0, runes, styles, base); len(outR) != len(runes) {
		t.Error("ghost for another line should not splice into this row")
	}

	// Anchor drifted from the cursor (cursor moved, app hasn't cleared yet).
	tab.Ghost = &GhostText{Pos: Position{Line: 0, Col: 5}, Text: "x"}
	tab.Cursor = Position{Line: 0, Col: 3}
	if outR, _ := tab.ghostOverlay(0, runes, styles, base); len(outR) != len(runes) {
		t.Error("ghost with a stale anchor must not paint")
	}
}

// TestGhostDisplayRunes_MultiLineMarker pins the multi-line summary:
// only the first line renders inline, and the ⋯+N tail advertises the
// rest so the user knows Tab accepts more than what's visible.
func TestGhostDisplayRunes_MultiLineMarker(t *testing.T) {
	g := &GhostText{Text: "return nil", MoreLines: 2}
	if got := string(g.displayRunes()); got != "return nil ⋯+2" {
		t.Errorf("displayRunes = %q, want %q", got, "return nil ⋯+2")
	}
	// Single-line proposals get no marker — the common case stays clean.
	g = &GhostText{Text: "return nil"}
	if got := string(g.displayRunes()); got != "return nil" {
		t.Errorf("displayRunes = %q, want %q", got, "return nil")
	}
}

// TestRender_GhostPainted drives the full paint path: with a ghost at
// the caret, the suggestion's characters appear on screen after the
// typed prefix — and the buffer itself stays untouched (the overlay is
// display-only; accepting is the app layer's job).
func TestRender_GhostPainted(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(60, 10)

	tab := ghostTestTab("func ma", Position{Line: 0, Col: 7})
	tab.Ghost = &GhostText{Pos: Position{Line: 0, Col: 7}, Text: "in() {}"}
	tab.Render(scr, theme.Default(), 0, 0, 60, 10)
	scr.Show()

	if got := screenRow(scr, 0); !strings.Contains(got, "func main() {}") {
		t.Errorf("row 0 = %q, want it to contain the spliced %q", got, "func main() {}")
	}
	if got := tab.Buffer.String(); got != "func ma" {
		t.Errorf("buffer mutated by render: %q", got)
	}
}

// screenRow flattens one row of the simulation screen into a string,
// skipping combining-rune cells the same way GetContents reports them.
func screenRow(scr tcell.SimulationScreen, row int) string {
	cells, w, _ := scr.GetContents()
	var b strings.Builder
	for x := 0; x < w; x++ {
		c := cells[row*w+x]
		if len(c.Runes) > 0 {
			b.WriteRune(c.Runes[0])
		}
	}
	return b.String()
}
