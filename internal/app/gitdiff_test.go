// =============================================================================
// File: internal/app/gitdiff_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/theme"
)

// TestParseHunkHeader_Forms pins the header scanner against every
// shape git emits: full "-a,b +c,d", the ",1" shorthand omitted on
// either side, and junk that must be rejected rather than guessed at.
func TestParseHunkHeader_Forms(t *testing.T) {
	cases := []struct {
		line           string
		os, oc, ns, nc int
		ok             bool
	}{
		{"@@ -1,2 +3,4 @@", 1, 2, 3, 4, true},
		{"@@ -5 +7 @@", 5, 1, 7, 1, true},                // counts omitted → 1
		{"@@ -0,0 +1,3 @@", 0, 0, 1, 3, true},            // pure addition at top
		{"@@ -4,2 +3,0 @@ func foo()", 4, 2, 3, 0, true}, // trailing context
		{"@@ garbage @@", 0, 0, 0, 0, false},
		{"@@ -x,1 +2 @@", 0, 0, 0, 0, false},
		{"not a header", 0, 0, 0, 0, false},
	}
	for _, c := range cases {
		os_, oc, ns, nc, ok := parseHunkHeader(c.line)
		if ok != c.ok || os_ != c.os || oc != c.oc || ns != c.ns || nc != c.nc {
			t.Errorf("%q: got (%d,%d,%d,%d,%v), want (%d,%d,%d,%d,%v)",
				c.line, os_, oc, ns, nc, ok, c.os, c.oc, c.ns, c.nc, c.ok)
		}
	}
}

// TestParseUnifiedDiff_Kinds walks the three hunk classifications and
// the 1-based → 0-based line conversion — the exact arithmetic the
// gutter's correctness rides on.
func TestParseUnifiedDiff_Kinds(t *testing.T) {
	diff := []byte(`diff --git a/f.txt b/f.txt
index 000..111 100644
--- a/f.txt
+++ b/f.txt
@@ -0,0 +1,2 @@
+new line one
+new line two
@@ -5,1 +7,1 @@
-old
+new
@@ -10,3 +11,0 @@
-gone
-gone
-gone
`)
	hunks := parseUnifiedDiff(diff)
	want := []diffHunk{
		{Start: 0, End: 1, Kind: diffAdded},     // +1,2 → lines 0-1
		{Start: 6, End: 6, Kind: diffModified},  // +7,1 → line 6
		{Start: 10, End: 10, Kind: diffDeleted}, // +11,0 → boundary line 10
	}
	if len(hunks) != len(want) {
		t.Fatalf("hunk count: got %d (%+v), want %d", len(hunks), hunks, len(want))
	}
	for i, w := range want {
		if hunks[i] != w {
			t.Errorf("hunk %d: got %+v, want %+v", i, hunks[i], w)
		}
	}
}

// TestParseUnifiedDiff_TopOfFileDeletion pins the one genuinely tricky
// coordinate: deleting the first lines of a file reports "+0,0", and
// the boundary mark must clamp to line 0, not go negative.
func TestParseUnifiedDiff_TopOfFileDeletion(t *testing.T) {
	hunks := parseUnifiedDiff([]byte("@@ -1,2 +0,0 @@\n-a\n-b\n"))
	if len(hunks) != 1 {
		t.Fatalf("hunk count: got %d, want 1", len(hunks))
	}
	if hunks[0] != (diffHunk{Start: 0, End: 0, Kind: diffDeleted}) {
		t.Fatalf("got %+v, want boundary at line 0", hunks[0])
	}
}

// TestParseUnifiedDiff_EmptyAndJunk guards the degenerate inputs: no
// output (clean file) and non-diff noise both yield zero hunks.
func TestParseUnifiedDiff_EmptyAndJunk(t *testing.T) {
	if h := parseUnifiedDiff(nil); h != nil {
		t.Fatalf("nil input should give nil hunks, got %+v", h)
	}
	if h := parseUnifiedDiff([]byte("random\ntext\n@@ bad @@\n")); h != nil {
		t.Fatalf("junk input should give nil hunks, got %+v", h)
	}
}

// gitAvailable reports whether the git binary can run here; the
// end-to-end tests skip (not fail) without it, per the "skip only on
// hard environment requirements" rule.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// initTestRepo builds a real repo in a tempdir with one committed
// file, returning its root and the file path. The identity flags keep
// commit from failing on CI boxes with no global git config.
func initTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("one\ntwo\nthree\nfour\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	return dir, file
}

// TestLoadFileDiff_EndToEnd exercises the real git path: a committed
// file gets edited three ways (modify, append, delete) and the parsed
// hunks must classify each. This is the integration test for the
// whole shell-out → parse pipeline.
func TestLoadFileDiff_EndToEnd(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	dir, file := initTestRepo(t)

	// Modify line 2, delete line 3, append a line at the end:
	// one → one / two → TWO / three deleted / four → four + five.
	if err := os.WriteFile(file, []byte("one\nTWO\nfour\nfive\n"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	hunks := loadFileDiff(dir, file)
	if len(hunks) == 0 {
		t.Fatal("expected hunks for an edited tracked file")
	}
	kinds := map[diffKind]bool{}
	for _, h := range hunks {
		kinds[h.Kind] = true
	}
	if !kinds[diffModified] || !kinds[diffAdded] {
		t.Fatalf("expected modified+added hunks, got %+v", hunks)
	}
}

// TestLoadFileDiff_CleanUntrackedAndNonRepo pins the three best-effort
// nil paths: an unedited tracked file, an untracked file, and a
// directory that isn't a repo at all. All must stay silent.
func TestLoadFileDiff_CleanUntrackedAndNonRepo(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	dir, file := initTestRepo(t)
	if h := loadFileDiff(dir, file); h != nil {
		t.Fatalf("clean file should have no hunks, got %+v", h)
	}
	untracked := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(untracked, []byte("x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if h := loadFileDiff(dir, untracked); h != nil {
		t.Fatalf("untracked file should have no hunks, got %+v", h)
	}
	plain := t.TempDir()
	if h := loadFileDiff(plain, filepath.Join(plain, "a.txt")); h != nil {
		t.Fatalf("non-repo should have no hunks, got %+v", h)
	}
}

// TestHandleGitDiff_StoresAndClears pins the main-loop half of the
// async pipeline: results land keyed by path, the map lazy-inits, and
// an empty result clears a stale entry (the gutter must empty out
// after the user commits).
func TestHandleGitDiff_StoresAndClears(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	hunks := []diffHunk{{Start: 0, End: 1, Kind: diffAdded}}

	a.handleGitDiff(&gitDiffEvent{when: time.Now(), path: "/p/f.go", hunks: hunks})
	if got := a.fileDiffs["/p/f.go"]; len(got) != 1 {
		t.Fatalf("diff not stored: %+v", a.fileDiffs)
	}
	a.handleGitDiff(&gitDiffEvent{when: time.Now(), path: "/p/f.go", hunks: nil})
	if _, ok := a.fileDiffs["/p/f.go"]; ok {
		t.Fatal("empty result should clear the stale entry")
	}
}

// TestHandleEvent_RoutesGitDiff pins the event-loop wiring: a
// gitDiffEvent arriving through the generic dispatcher must land in
// fileDiffs — a missing switch case would compile fine and silently
// drop every diff.
func TestHandleEvent_RoutesGitDiff(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.handleEvent(&gitDiffEvent{
		when:  time.Now(),
		path:  "/p/f.go",
		hunks: []diffHunk{{Start: 3, End: 3, Kind: diffModified}},
	})
	if len(a.fileDiffs["/p/f.go"]) != 1 {
		t.Fatal("gitDiffEvent should route to handleGitDiff")
	}
}

// TestRequestFileDiff_AsyncRoundTrip drives the full production
// pipeline against a real repo: openFile fires the background diff,
// the goroutine posts its event, and handling it populates the gutter
// state. Catches any break in the goroutine → event → handler chain
// that the unit tests around each piece can't see.
func TestRequestFileDiff_AsyncRoundTrip(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	dir, file := initTestRepo(t)
	if err := os.WriteFile(file, []byte("one\nCHANGED\nthree\nfour\n"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(file) // registers the source and requests the diff

	// Pump the screen's event queue until the diff event arrives; the
	// poll runs on a goroutine so a lost event fails the test in 2s
	// instead of hanging it.
	events := make(chan tcell.Event, 8)
	go func() {
		for {
			ev := a.screen.PollEvent()
			if ev == nil {
				return
			}
			events <- ev
		}
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			a.handleEvent(ev)
			if len(a.fileDiffs[file]) > 0 {
				if a.fileDiffs[file][0].Kind != diffModified {
					t.Fatalf("expected a modified hunk, got %+v", a.fileDiffs[file])
				}
				return
			}
		case <-deadline:
			t.Fatal("no gitDiffEvent arrived within 2s")
		}
	}
}

// TestGitDiffSource_MarksAndCulling verifies the decoration adapter:
// per-kind glyph/color mapping, expansion of multi-line hunks into
// per-line marks, and viewport culling.
func TestGitDiffSource_MarksAndCulling(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	th := theme.Default()
	tab, _ := editor.NewTab("")
	tab.Path = "/p/f.go"
	a.fileDiffs = map[string][]diffHunk{"/p/f.go": {
		{Start: 1, End: 2, Kind: diffAdded},
		{Start: 5, End: 5, Kind: diffModified},
		{Start: 9, End: 9, Kind: diffDeleted},
	}}
	src := gitDiffSource{app: a}

	_, marks := src.Decorations(tab, th, 0, 20)
	if len(marks) != 4 {
		t.Fatalf("mark count: got %d, want 4 (2 added + 1 modified + 1 deleted)", len(marks))
	}
	if marks[0].Glyph != glyphChanged || marks[0].FG != th.GitAdded || marks[0].Line != 1 {
		t.Fatalf("added mark wrong: %+v", marks[0])
	}
	if marks[2].FG != th.GitModified {
		t.Fatalf("modified mark wrong: %+v", marks[2])
	}
	if marks[3].Glyph != glyphDeleted || marks[3].FG != th.GitDeleted {
		t.Fatalf("deleted mark wrong: %+v", marks[3])
	}

	// Window covering only lines 4..6 must keep just the modified mark.
	_, culled := src.Decorations(tab, th, 4, 6)
	if len(culled) != 1 || culled[0].Line != 5 {
		t.Fatalf("culling failed: %+v", culled)
	}

	// A tab with no cached diff draws nothing.
	other, _ := editor.NewTab("")
	other.Path = "/p/clean.go"
	if _, m := src.Decorations(other, th, 0, 20); m != nil {
		t.Fatalf("clean tab should have no marks, got %+v", m)
	}
}

// withDiffedTab wires an App with one open tab and a canned diff so
// the navigation tests read as scenarios, not setup.
func withDiffedTab(t *testing.T) (*App, *editor.Tab) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	content := ""
	for i := 0; i < 30; i++ {
		content += "line\n"
	}
	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(file)
	a.fileDiffs = map[string][]diffHunk{file: {
		{Start: 4, End: 5, Kind: diffModified},
		{Start: 12, End: 12, Kind: diffAdded},
		{Start: 20, End: 20, Kind: diffDeleted},
	}}
	return a, a.activeTabPtr()
}

// TestJumpHunk_NextAndWrap pins the forward walk: from the top the
// cursor visits each hunk start in order, then wraps back to the
// first — the same contract find-next has.
func TestJumpHunk_NextAndWrap(t *testing.T) {
	a, tab := withDiffedTab(t)
	wantLines := []int{4, 12, 20, 4} // three hunks then wrap
	for _, want := range wantLines {
		a.menuNextHunk()
		if tab.Cursor.Line != want {
			t.Fatalf("cursor at %d, want %d", tab.Cursor.Line, want)
		}
	}
}

// TestJumpHunk_PrevAndWrap pins the backward walk including the wrap
// from above-the-first-hunk to the last hunk.
func TestJumpHunk_PrevAndWrap(t *testing.T) {
	a, tab := withDiffedTab(t)
	tab.MoveCursorTo(editor.Position{Line: 13, Col: 0}, false)
	a.menuPrevHunk()
	if tab.Cursor.Line != 12 {
		t.Fatalf("prev from 13: got %d, want 12", tab.Cursor.Line)
	}
	a.menuPrevHunk()
	if tab.Cursor.Line != 4 {
		t.Fatalf("prev from 12: got %d, want 4", tab.Cursor.Line)
	}
	a.menuPrevHunk() // above every hunk → wrap to the last
	if tab.Cursor.Line != 20 {
		t.Fatalf("prev wrap: got %d, want 20", tab.Cursor.Line)
	}
}

// TestJumpHunk_NoHunksNoop guards the empty case: no diff, no cursor
// movement, no panic — the menu rows are disabled then, but the
// leader keys can still fire the method.
func TestJumpHunk_NoHunksNoop(t *testing.T) {
	a, tab := withDiffedTab(t)
	a.fileDiffs = nil
	tab.MoveCursorTo(editor.Position{Line: 7, Col: 3}, false)
	a.menuNextHunk()
	if tab.Cursor.Line != 7 {
		t.Fatalf("cursor moved with no hunks: %d", tab.Cursor.Line)
	}
}

// TestHasDiffHunks_Predicate pins the menu-row enablement: false with
// no tab, false with a clean tab, true once hunks land.
func TestHasDiffHunks_Predicate(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if a.hasDiffHunks() {
		t.Fatal("no tab → predicate must be false")
	}
	a2, _ := withDiffedTab(t)
	if !a2.hasDiffHunks() {
		t.Fatal("tab with hunks → predicate must be true")
	}
	a2.fileDiffs = nil
	if a2.hasDiffHunks() {
		t.Fatal("clean tab → predicate must be false")
	}
}

// TestCloseTab_DropsCachedDiff prevents a slow leak and stale marks:
// closing a tab must evict its diff so a later re-open starts fresh
// from the async request, not from a stale cache.
func TestCloseTab_DropsCachedDiff(t *testing.T) {
	a, tab := withDiffedTab(t)
	path := tab.Path
	a.closeTab(0)
	if _, ok := a.fileDiffs[path]; ok {
		t.Fatal("closed tab's diff should be evicted")
	}
}

// TestOpenFile_RegistersDiffSource pins the wiring: every file tab
// gets the gitDiffSource decoration source, so marks appear the
// moment its diff lands — no separate registration step to forget.
func TestOpenFile_RegistersDiffSource(t *testing.T) {
	a, tab := withDiffedTab(t)
	found := false
	for _, s := range tab.DecoSources {
		if gs, ok := s.(gitDiffSource); ok && gs.app == a {
			found = true
		}
	}
	if !found {
		t.Fatal("openFile should register gitDiffSource on the tab")
	}
}

// TestGitDiff_RendersGutterMarks is the end-to-end draw test: an open
// tab with cached hunks renders the changed-glyph in the gutter mark
// column via the decoration layer.
func TestGitDiff_RendersGutterMarks(t *testing.T) {
	a, tab := withDiffedTab(t)
	tab.Render(a.screen, a.theme, 0, 0, 60, 25)
	a.screen.Show()

	scr := a.screen.(tcell.SimulationScreen)
	cells, w, _ := scr.GetContents()
	// Hunk 1 covers lines 4-5 (modified) — expect the bar glyph in the
	// mark column (x = gutterWidth = 6 per the editor's layout).
	cell := cells[4*w+6]
	if len(cell.Runes) == 0 || cell.Runes[0] != glyphChanged {
		t.Fatalf("mark cell at line 4: got %v, want %q", cell.Runes, glyphChanged)
	}
	fg, _, _ := cell.Style.Decompose()
	if fg != a.theme.GitModified {
		t.Fatalf("mark fg: got %v, want GitModified", fg)
	}
}

// TestLeader_HunkBindings pins Esc-h / Esc-H so a leader refactor
// can't drop hunk navigation.
func TestLeader_HunkBindings(t *testing.T) {
	a, tab := withDiffedTab(t)
	if action := leaderActionFor('h'); action == nil {
		t.Fatal("Esc-h has no binding")
	} else {
		action(a)
	}
	if tab.Cursor.Line != 4 {
		t.Fatalf("Esc-h should jump to first hunk, cursor at %d", tab.Cursor.Line)
	}
	if action := leaderActionFor('H'); action == nil {
		t.Fatal("Esc-H has no binding")
	}
}

// TestMenu_GitChangeRows pins the mouse-first rule for phase 4: both
// hunk actions must be reachable from the ≡ menu.
func TestMenu_GitChangeRows(t *testing.T) {
	a, tab := withDiffedTab(t)
	next := menuItemByLabel(t, a, "Next change")
	prev := menuItemByLabel(t, a, "Previous change")
	if !next.enabled(a) || !prev.enabled(a) {
		t.Fatal("hunk rows should be enabled with hunks present")
	}
	next.action(a)
	if tab.Cursor.Line != 4 {
		t.Fatalf("menu Next change should jump to line 4, got %d", tab.Cursor.Line)
	}
}
