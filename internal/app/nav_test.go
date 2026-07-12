// =============================================================================
// File: internal/app/nav_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
)

// seedNavFiles writes n small fixture files and returns their absolute
// paths, giving nav tests distinct destinations to hop between.
func seedNavFiles(t *testing.T, dir string, n int) []string {
	t.Helper()
	paths := make([]string, n)
	for i := range paths {
		p := filepath.Join(dir, string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte("line one\nline two\nline three\n"), 0644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		paths[i] = p
	}
	return paths
}

// TestOpenFileRecordsHistory pins the central recording contract: every
// cross-file openFile pushes the departure point (path + cursor), and
// same-path re-opens don't record — clicking the active file in the
// tree must not cost a Go back press.
func TestOpenFileRecordsHistory(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	if len(a.nav.back) != 0 {
		t.Fatal("first open has no departure point — nothing to record")
	}
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 2, Col: 3}, false)

	a.openFile(paths[1])
	if len(a.nav.back) != 1 {
		t.Fatalf("back stack len = %d, want 1", len(a.nav.back))
	}
	if got := a.nav.back[0]; got.path != paths[0] || got.pos != (editor.Position{Line: 2, Col: 3}) {
		t.Errorf("recorded %+v, want %s at 2:3", got, paths[0])
	}

	a.openFile(paths[1]) // same path — not a navigation
	if len(a.nav.back) != 1 {
		t.Error("same-path open must not record")
	}
}

// TestNavBackForwardRoundTrip drives the full browser cycle: back
// restores the previous file and cursor and arms forward; forward
// re-advances and restores the back entry.
func TestNavBackForwardRoundTrip(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 1, Col: 4}, false)
	a.openFile(paths[1])
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 2, Col: 0}, false)

	a.navBack()
	if tab := a.activeTabPtr(); tab.Path != paths[0] || tab.Cursor != (editor.Position{Line: 1, Col: 4}) {
		t.Fatalf("back landed at %s %+v, want %s 1:4", tab.Path, tab.Cursor, paths[0])
	}
	if len(a.nav.back) != 0 || len(a.nav.fwd) != 1 {
		t.Fatalf("stacks back=%d fwd=%d, want 0/1", len(a.nav.back), len(a.nav.fwd))
	}

	a.navForward()
	if tab := a.activeTabPtr(); tab.Path != paths[1] || tab.Cursor != (editor.Position{Line: 2, Col: 0}) {
		t.Fatalf("forward landed at %s %+v, want %s 2:0", tab.Path, tab.Cursor, paths[1])
	}
	if len(a.nav.back) != 1 || len(a.nav.fwd) != 0 {
		t.Errorf("stacks back=%d fwd=%d, want 1/0", len(a.nav.back), len(a.nav.fwd))
	}
}

// TestNavRetraceDoesNotRecord pins the suppress contract: the openFile
// that navBack itself performs must not feed the history, or going back
// twice would bounce between two entries forever instead of walking
// deeper into the trail.
func TestNavRetraceDoesNotRecord(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 3)
	a := newTestApp(t, dir)

	for _, p := range paths {
		a.openFile(p)
	}
	// Trail is a→b→c with back=[a,b]. Two backs must walk c→b→a.
	a.navBack()
	a.navBack()
	if tab := a.activeTabPtr(); tab.Path != paths[0] {
		t.Fatalf("two backs landed at %s, want %s", tab.Path, paths[0])
	}
	if len(a.nav.back) != 0 {
		t.Errorf("back stack len = %d, want 0 — retrace recorded into itself", len(a.nav.back))
	}
}

// TestNavNewNavigationClearsForward pins the browser rule: going back
// and then navigating somewhere NEW abandons the forward trail.
func TestNavNewNavigationClearsForward(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 3)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	a.openFile(paths[1])
	a.navBack() // fwd now holds b
	if len(a.nav.fwd) != 1 {
		t.Fatal("back should have armed the forward stack")
	}
	a.openFile(paths[2]) // fresh navigation from a
	if len(a.nav.fwd) != 0 {
		t.Error("new navigation must clear the forward stack")
	}
}

// TestNavStackCap pins the cap: overflow drops the oldest entries, not
// the newest, and the stack never outgrows navStackMax.
func TestNavStackCap(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	for i := 0; i < navStackMax+10; i++ {
		a.recordNav(navLoc{pos: editor.Position{Line: i}})
	}
	if len(a.nav.back) != navStackMax {
		t.Fatalf("stack len = %d, want %d", len(a.nav.back), navStackMax)
	}
	if got := a.nav.back[len(a.nav.back)-1].pos.Line; got != navStackMax+9 {
		t.Errorf("newest entry line = %d, want %d", got, navStackMax+9)
	}
}

// TestNavBackEmptyFlashes pins the no-history UX for both directions: a
// flash, no crash, no tab change.
func TestNavBackEmptyFlashes(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.navBack()
	if a.statusMsg != "Nowhere to go back to" {
		t.Errorf("flash = %q", a.statusMsg)
	}
	a.navForward()
	if a.statusMsg != "Nowhere to go forward to" {
		t.Errorf("flash = %q", a.statusMsg)
	}
}

// TestNavBackDropsDeletedFile pins the stale-entry policy: an entry
// whose file has been deleted flashes the open error and is dropped
// from the stack rather than left to fail on every subsequent press.
func TestNavBackDropsDeletedFile(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	a.openFile(paths[1])
	a.closeTab(0) // drop a's tab so back must reopen from disk
	if err := os.Remove(paths[0]); err != nil {
		t.Fatalf("remove: %v", err)
	}

	a.navBack()
	if tab := a.activeTabPtr(); tab == nil || tab.Path != paths[1] {
		t.Fatal("failed back should leave the current tab in place")
	}
	if len(a.nav.back) != 0 {
		t.Error("unopenable entry should be dropped from the stack")
	}
	if len(a.nav.fwd) != 0 {
		t.Error("failed back must not arm the forward stack")
	}
}

// TestTabBarClickRecordsHistory pins that switching files via the tab
// bar — which bypasses openFile — still feeds the history.
func TestTabBarClickRecordsHistory(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	a.openFile(paths[1])
	a.nav.back = nil // isolate: only the tab click below should record

	a.drawTabBar() // populate lastTabRects for the hit test
	r := a.lastTabRects[0]
	a.tabBarClick(r.X, 0)
	if a.activeTab != 0 {
		t.Fatalf("activeTab = %d, want 0", a.activeTab)
	}
	if len(a.nav.back) != 1 || a.nav.back[0].path != paths[1] {
		t.Errorf("back stack = %+v, want departure from %s", a.nav.back, paths[1])
	}

	// Clicking the already-active tab is not a navigation.
	a.tabBarClick(r.X, 0)
	if len(a.nav.back) != 1 {
		t.Error("same-tab click must not record")
	}
}

// TestAltArrowsNavigate pins the Alt+Left / Alt+Right key path through
// handleKey — the arrow twins of Esc-o / Esc-O.
func TestAltArrowsNavigate(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	a.openFile(paths[0])
	a.openFile(paths[1])

	a.handleKey(tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModAlt))
	if tab := a.activeTabPtr(); tab.Path != paths[0] {
		t.Fatalf("Alt+Left landed at %s, want %s", tab.Path, paths[0])
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModAlt))
	if tab := a.activeTabPtr(); tab.Path != paths[1] {
		t.Fatalf("Alt+Right landed at %s, want %s", tab.Path, paths[1])
	}
}

// TestNavLeaderBindings pins the o/O leader pair onto the history
// actions — inside tmux these arrive as Alt+o / Alt+O, so the runes
// must stay in the leader table.
func TestNavLeaderBindings(t *testing.T) {
	for _, key := range []rune{'o', 'O'} {
		if leaderActionFor(key) == nil {
			t.Errorf("leader %q not bound", key)
		}
	}
}

// TestMenuNavRows pins the ≡ menu rows and their enablement predicates:
// both present, both dimmed without history, per the house rule that
// every action is reachable from the main menu.
func TestMenuNavRows(t *testing.T) {
	dir := t.TempDir()
	paths := seedNavFiles(t, dir, 2)
	a := newTestApp(t, dir)

	back := menuItemByLabel(t, a, "Go back")
	fwd := menuItemByLabel(t, a, "Go forward")
	if back.enabled(a) || fwd.enabled(a) {
		t.Error("no history — both rows should be disabled")
	}

	a.openFile(paths[0])
	a.openFile(paths[1])
	if back = menuItemByLabel(t, a, "Go back"); !back.enabled(a) {
		t.Error("history exists — Go back should be enabled")
	}
	a.navBack()
	if fwd = menuItemByLabel(t, a, "Go forward"); !fwd.enabled(a) {
		t.Error("after a back — Go forward should be enabled")
	}
}

// TestMenuShortcutHints pins the accelerator column: rows with a
// binding carry the hint string that drawMenu renders, and the hint
// actually lands right-aligned in the drawn modal.
func TestMenuShortcutHints(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	if sc := menuItemByLabel(t, a, "Save").shortcut; sc != "esc s" {
		t.Errorf("Save shortcut = %q, want %q", sc, "esc s")
	}
	if sc := menuItemByLabel(t, a, "Go back").shortcut; sc != "esc o / alt+←" {
		t.Errorf("Go back shortcut = %q, want %q", sc, "esc o / alt+←")
	}

	a.openMenu()
	a.draw()
	scr := a.screen.(tcell.SimulationScreen)
	scr.Show() // SimulationScreen serves GetContents from the *front* buffer.
	cells, w, _ := scr.GetContents()

	// The Save row: find its label, then assert its hint sits at the
	// right edge of the same row.
	mx, my, mw, _ := a.menuModalRect()
	items, _, _ := a.menuLayout()
	var saveRelY int
	for _, it := range items {
		if it.label == "Save" {
			saveRelY = it.relY
		}
	}
	row := my + saveRelY - a.menuScrollOffset()
	hint := "esc s"
	start := mx + mw - 2 - len([]rune(hint))
	for i, r := range []rune(hint) {
		c := cells[row*w+start+i]
		if len(c.Runes) == 0 || c.Runes[0] != r {
			t.Fatalf("hint cell %d = %v, want %q", i, c.Runes, r)
		}
	}
}
