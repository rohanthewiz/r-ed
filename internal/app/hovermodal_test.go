// =============================================================================
// File: internal/app/hovermodal_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
)

// hoverTestApp opens a tall Go file so cursor-anchoring tests can park
// the caret anywhere in the viewport.
func hoverTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	content := "package main\n" + strings.Repeat("// filler line\n", 60)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(path)
	a.draw() // resolve scroll/EnsureVisible so CursorScreenCell is live
	return a
}

// TestHoverModalRectBelowCursor pins the tooltip convention: with room
// underneath, the popup's top row sits directly below the caret's row.
func TestHoverModalRectBelowCursor(t *testing.T) {
	a := hoverTestApp(t)
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 3, Col: 0}, false)
	a.draw()

	m := &hoverModal{lines: []string{"one", "two"}}
	_, y, _, h := m.rect(a)

	ex, ey, ew, eh := a.editorRect()
	_ = ex
	dx, dy, ok := a.activeTabPtr().CursorScreenCell(ew, eh)
	_ = dx
	if !ok {
		t.Fatal("cursor should be on screen")
	}
	if want := ey + dy + 1; y != want {
		t.Errorf("popup y = %d, want %d (one row below caret)", y, want)
	}
	if h != 4 { // 2 lines + 2 border rows
		t.Errorf("popup h = %d, want 4", h)
	}
}

// TestHoverModalRectFlipsAbove pins the bottom-edge behavior: when the
// popup would clip the status bar it flips to sit above the caret.
func TestHoverModalRectFlipsAbove(t *testing.T) {
	a := hoverTestApp(t)
	tab := a.activeTabPtr()
	// Park the caret on the last visible row.
	_, _, _, eh := a.editorRect()
	tab.MoveCursorTo(editor.Position{Line: eh - 1, Col: 0}, false)
	a.draw()

	m := &hoverModal{lines: []string{"a", "b", "c"}}
	_, y, _, h := m.rect(a)

	ex, ey, ew, ehh := a.editorRect()
	_ = ex
	_, dy, ok := tab.CursorScreenCell(ew, ehh)
	if !ok {
		t.Fatal("cursor should be on screen")
	}
	cy := ey + dy
	if y+h > cy+1 {
		t.Errorf("popup [%d,%d) overlaps or passes the caret row %d — should flip above", y, y+h, cy)
	}
}

// TestHoverModalRectCenteredFallback pins the degenerate case: with the
// caret scrolled off-screen the popup falls back to center rather than
// anchoring to a stale cell.
func TestHoverModalRectCenteredFallback(t *testing.T) {
	a := hoverTestApp(t)
	a.activeTabPtr().Scroll(40) // caret at top, viewport far below
	a.draw()

	m := &hoverModal{lines: []string{"x"}}
	x, y, w, h := m.rect(a)
	cx, cy, _, _ := a.centeredRect(w, h)
	if x != cx || y != cy {
		t.Errorf("popup at (%d,%d), want centered (%d,%d)", x, y, cx, cy)
	}
}

// TestHoverModalDismissal pins the trigger-happy close contract: any
// key closes, any button click closes, wheel and motion do not.
func TestHoverModalDismissal(t *testing.T) {
	a := hoverTestApp(t)

	a.openModal(&hoverModal{lines: []string{"x"}})
	a.modal.handleKey(a, tcell.NewEventKey(tcell.KeyRune, 'z', tcell.ModNone))
	if a.modal != nil {
		t.Error("any key should dismiss the hover popup")
	}

	a.openModal(&hoverModal{lines: []string{"x"}})
	a.modal.handleMouse(a, 0, 0, tcell.WheelDown)
	if a.modal == nil {
		t.Error("wheel must not dismiss — the user may be reading")
	}
	a.modal.handleMouse(a, 0, 0, tcell.Button1)
	if a.modal != nil {
		t.Error("click should dismiss")
	}
}

// TestHoverModalDrawContent renders onto the sim screen and asserts
// the text lands inside the popup, with over-wide lines truncated to
// an ellipsis instead of bleeding through the border.
func TestHoverModalDrawContent(t *testing.T) {
	a := hoverTestApp(t)
	long := strings.Repeat("w", hoverModalMaxWidth*2)
	m := &hoverModal{lines: []string{"short line", long}}
	a.openModal(m)
	a.draw()
	a.screen.Show()

	mx, my, mw, _ := m.rect(a)
	scr := a.screen.(tcell.SimulationScreen)
	cells, w, _ := scr.GetContents()

	row := func(y int) string {
		var b strings.Builder
		for x := mx; x < mx+mw; x++ {
			c := cells[y*w+x]
			if len(c.Runes) > 0 {
				b.WriteRune(c.Runes[0])
			}
		}
		return b.String()
	}
	if !strings.Contains(row(my+1), "short line") {
		t.Errorf("first body row = %q, want the hover text", row(my+1))
	}
	second := row(my + 2)
	if !strings.Contains(second, "…") {
		t.Errorf("over-wide line not truncated with ellipsis: %q", second)
	}
	if strings.Contains(second, strings.Repeat("w", mw)) {
		t.Error("over-wide line bled past the popup border")
	}
}
