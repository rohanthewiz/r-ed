// =============================================================================
// File: internal/app/palette_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/r-ed/internal/customactions"
	"github.com/rohanthewiz/r-ed/internal/finder"
)

// paletteOf returns the active modal as a *paletteModal, or nil when
// the palette isn't the open modal — same typed-accessor pattern the
// other modal tests use so assertions read cleanly.
func paletteOf(a *App) *paletteModal {
	m, _ := a.modal.(*paletteModal)
	return m
}

// typePalette feeds a string through the palette's key handler one
// rune at a time, the same path real keystrokes take.
func typePalette(a *App, s string) {
	for _, r := range s {
		paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
}

// TestOpenPalette_GathersEnabledActions pins the inventory contract:
// with no tab open, tab-dependent actions ("Save") must not be
// listed, always-available ones ("Quit editor") must be, and the
// palette must never list itself.
func TestOpenPalette_GathersEnabledActions(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()

	m := paletteOf(a)
	if m == nil {
		t.Fatal("openPalette should install the palette modal")
	}
	labels := map[string]bool{}
	for _, it := range m.items {
		labels[it.label] = true
	}
	if labels["Save"] {
		t.Fatal("'Save' should be excluded with no savable tab")
	}
	if !labels["Quit editor"] {
		t.Fatal("'Quit editor' should always be listed")
	}
	if labels[paletteMenuLabel] {
		t.Fatal("the palette must not list itself")
	}
}

// TestOpenPalette_EmptyQueryKeepsMenuOrder pins the browse mode: with
// no query, every item matches and the order mirrors the action menu
// so the palette doubles as a readable menu.
func TestOpenPalette_EmptyQueryKeepsMenuOrder(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()

	m := paletteOf(a)
	if len(m.matches) != len(m.items) {
		t.Fatalf("empty query should match everything: %d matches for %d items", len(m.matches), len(m.items))
	}
	for i := range m.items {
		if m.matches[i].item.label != m.items[i].label {
			t.Fatalf("row %d: got %q, want %q (source order)", i, m.matches[i].item.label, m.items[i].label)
		}
	}
}

// TestPaletteKey_TypingFilters walks the keystroke loop: a query must
// narrow the list and rank the obvious target on top, so Enter-after-
// typing does what the user expects.
func TestPaletteKey_TypingFilters(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	typePalette(a, "quit")

	m := paletteOf(a)
	if len(m.matches) == 0 {
		t.Fatal("expected matches for 'quit'")
	}
	if got := m.matches[0].item.label; got != "Quit editor" {
		t.Fatalf("top match: got %q, want %q", got, "Quit editor")
	}
	if len(m.matches) >= len(m.items) {
		t.Fatal("query should narrow the match list")
	}
}

// TestPaletteKey_EnterRunsAction is the headline outcome: Enter on
// the top match actually fires the action and dismisses the modal.
// Uses the sidebar toggle because its effect (sidebarShown flipping)
// is directly observable on App.
func TestPaletteKey_EnterRunsAction(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	typePalette(a, "hide file")
	if got := paletteOf(a).matches[0].item.label; got != "Hide file explorer" {
		t.Fatalf("top match: got %q, want %q", got, "Hide file explorer")
	}

	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if paletteOf(a) != nil {
		t.Fatal("modal should close after Enter")
	}
	if a.sidebarShown {
		t.Fatal("running 'Hide file explorer' should hide the sidebar")
	}
}

// TestPaletteKey_EnterOnEmptyIsNoop guards the degenerate path: Enter
// with zero matches must neither run anything nor panic.
func TestPaletteKey_EnterOnEmptyIsNoop(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	typePalette(a, "zzzzzzzz no such action")
	if len(paletteOf(a).matches) != 0 {
		t.Fatal("expected zero matches for gibberish query")
	}

	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if paletteOf(a) == nil {
		t.Fatal("Enter on empty match list should keep the modal open")
	}
	if a.quit {
		t.Fatal("no action should have run")
	}
}

// TestPaletteKey_ArrowsMoveSelection pins the navigation contract so
// Enter can't silently run the wrong row: ↓ advances, ↑ retreats,
// neither runs off the ends.
func TestPaletteKey_ArrowsMoveSelection(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()

	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if paletteOf(a).selected != 1 {
		t.Fatalf("selected after ↓: got %d, want 1", paletteOf(a).selected)
	}
	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if paletteOf(a).selected != 0 {
		t.Fatalf("selected after ↑: got %d, want 0", paletteOf(a).selected)
	}
	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if paletteOf(a).selected != 0 {
		t.Fatalf("selected at top after ↑: got %d, want 0", paletteOf(a).selected)
	}
}

// TestPaletteKey_EscCloses pins the cancel path and that a reopen
// starts fresh — the discarded modal's query and selection must never
// resurrect.
func TestPaletteKey_EscCloses(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	typePalette(a, "x")
	paletteOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	if paletteOf(a) != nil {
		t.Fatal("Esc should close the palette")
	}
	a.openPalette()
	if got := paletteOf(a).field.String(); got != "" {
		t.Fatalf("reopened palette should start with an empty query, got %q", got)
	}
	if paletteOf(a).selected != 0 {
		t.Fatalf("reopened palette should start at selection 0, got %d", paletteOf(a).selected)
	}
}

// TestPaletteMouse_ClickRunsRow walks the click path: clicking a row
// selects and runs it. Row geometry comes from the same rect helper
// the renderer uses so the math can't drift.
func TestPaletteMouse_ClickRunsRow(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	typePalette(a, "hide file")
	mx, my, _, _ := paletteOf(a).rect(a)

	paletteOf(a).handleMouse(a, mx+5, my+4, tcell.Button1)

	if paletteOf(a) != nil {
		t.Fatal("modal should close after click-run")
	}
	if a.sidebarShown {
		t.Fatal("clicking 'Hide file explorer' should hide the sidebar")
	}
}

// TestPaletteMouse_ClickOutsideCloses guards the dismiss path: a
// stray click outside the modal closes it without running anything.
func TestPaletteMouse_ClickOutsideCloses(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()

	paletteOf(a).handleMouse(a, 0, 0, tcell.Button1)

	if paletteOf(a) != nil {
		t.Fatal("modal should close on outside click")
	}
	if a.quit || !a.sidebarShown {
		t.Fatal("outside click must not run any action")
	}
}

// TestPalette_CustomActionsListed pins the second half of the
// inventory: user-configured actions from actions.json flow through
// menuLayout into the palette alongside the built-ins.
func TestPalette_CustomActionsListed(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.customActions = []customactions.Action{{Label: "Deploy to prod", Command: "true"}}
	a.openPalette()

	found := false
	for _, it := range paletteOf(a).items {
		if it.label == "Deploy to prod" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom action should be listed in the palette")
	}
}

// TestLeader_AFiresPalette pins the Esc-a binding so a leader-table
// refactor can't quietly drop the palette shortcut.
func TestLeader_AFiresPalette(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	action := leaderActionFor('a')
	if action == nil {
		t.Fatal("Esc-a has no leader binding")
	}
	action(a)
	if paletteOf(a) == nil {
		t.Fatal("Esc-a should open the palette")
	}
}

// TestMenu_CommandPaletteEntry pins the mouse-first rule: the palette
// must be reachable from the ≡ menu, not just the leader key.
func TestMenu_CommandPaletteEntry(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	for _, it := range items {
		if it.label == paletteMenuLabel {
			it.action(a)
			if paletteOf(a) == nil {
				t.Fatal("menu entry should open the palette")
			}
			return
		}
	}
	t.Fatalf("no %q entry in the action menu", paletteMenuLabel)
}

// TestPaletteDraw_RendersTitleAndRows smoke-tests the renderer on a
// simulation screen: the frame title and the first action label must
// land in the drawn cells. Catches geometry regressions (off-by-one
// row starts) that the logic tests can't see.
func TestPaletteDraw_RendersTitleAndRows(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPalette()
	paletteOf(a).draw(a)
	a.screen.Show()

	content := screenText(a)
	if !strings.Contains(content, paletteMenuLabel) {
		t.Fatalf("drawn screen should contain the title %q", paletteMenuLabel)
	}
	first := paletteOf(a).matches[0].item.label
	if !strings.Contains(content, first) {
		t.Fatalf("drawn screen should contain the first row %q", first)
	}
}

// screenText flattens the simulation screen into one string per row,
// joined by newlines, so tests can substring-match drawn output
// without caring about styles.
func screenText(a *App) string {
	scr := a.screen.(tcell.SimulationScreen)
	cells, w, h := scr.GetContents()
	out := make([]rune, 0, (w+1)*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) > 0 {
				out = append(out, c.Runes[0])
			} else {
				out = append(out, ' ')
			}
		}
		out = append(out, '\n')
	}
	return string(out)
}

// buildFinderT wires a ready file index onto the app, blocking until
// the background build lands so tests see a deterministic path list.
func buildFinderT(t *testing.T, a *App) {
	t.Helper()
	a.finder = finder.New(a.rootDir)
	done := make(chan struct{})
	a.finder.Rebuild(func() { close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("finder build did not finish within 3s")
	}
}

// TestOpenPalette_IncludesFileItems pins the second source: with a
// ready index, project files list after the actions (empty query =
// source order, actions first) and Enter on a file row opens it.
func TestOpenPalette_IncludesFileItems(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(dir, "alpha.go"), "package a\n")
	writeFileT(t, filepath.Join(sub, "beta.txt"), "b\n")

	a := newTestApp(t, dir)
	buildFinderT(t, a)
	a.openPalette()
	m := paletteOf(a)

	labels := map[string]int{}
	for i, it := range m.items {
		labels[it.label] = i
	}
	alphaIdx, ok := labels["alpha.go"]
	if !ok {
		t.Fatalf("palette items missing alpha.go: %v", labels)
	}
	if _, ok := labels[filepath.Join("sub", "beta.txt")]; !ok {
		t.Fatalf("palette items missing sub/beta.txt: %v", labels)
	}
	if quitIdx := labels["Quit editor"]; quitIdx > alphaIdx {
		t.Fatal("actions must list before files on an empty query")
	}

	// Filter down to the file and run it — the tab must open.
	typePalette(a, "alpha")
	m.runSelected(a)
	if got := a.activeTabPtr(); got == nil || filepath.Base(got.Path) != "alpha.go" {
		t.Fatalf("selecting a file row should open it; active tab = %+v", got)
	}
}

// TestPalette_RecollectsOnIndexRebuild verifies the streaming-in path:
// a palette opened before the first index build shows no file rows,
// and the finderRebuiltEvent posted by the build re-collects sources
// so the files appear without reopening the modal.
func TestPalette_RecollectsOnIndexRebuild(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "alpha.go"), "package a\n")

	a := newTestApp(t, dir)
	a.finder = finder.New(a.rootDir) // idle — openPalette kicks the build
	a.openPalette()

	m := paletteOf(a)
	for _, it := range m.items {
		if it.label == "alpha.go" {
			t.Fatal("file rows must not exist before the index is built")
		}
	}

	// Pump the rebuilt event the build posts and expect re-collection.
	pumpAppEvents(t, a, func() bool {
		for _, it := range paletteOf(a).items {
			if it.label == "alpha.go" {
				return true
			}
		}
		return false
	})
}

// TestOpenPicker_TitleAndCallerItems pins the picker mode: the given
// title heads the frame, the caller's items are taken verbatim (no
// sources), and Enter runs the chosen item's action.
func TestOpenPicker_TitleAndCallerItems(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	ran := ""
	a.openPicker("Pick a thing", []paletteItem{
		{label: "one", run: func(*App) { ran = "one" }},
		{label: "two", run: func(*App) { ran = "two" }},
	})

	m := paletteOf(a)
	if m == nil || m.sourced {
		t.Fatalf("picker should be a non-sourced paletteModal, got %T", a.modal)
	}
	if len(m.items) != 2 {
		t.Fatalf("picker items = %d, want the 2 given", len(m.items))
	}

	m.draw(a)
	a.screen.Show()
	if !strings.Contains(screenText(a), "Pick a thing") {
		t.Fatal("picker should draw its own title")
	}

	typePalette(a, "two")
	m.runSelected(a)
	if ran != "two" {
		t.Fatalf("ran = %q, want the filtered selection to fire", ran)
	}
	if a.modal != nil {
		t.Fatal("running a picker item should close the modal")
	}
}
