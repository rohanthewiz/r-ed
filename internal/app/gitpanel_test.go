// =============================================================================
// File: internal/app/gitpanel_test.go
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
	"time"

	"github.com/gdamore/tcell/v2"
)

// panelRepo builds a repo with one committed file, returning both
// paths — the shared fixture most panel tests start from.
func panelRepo(t *testing.T) (repo, file string) {
	t.Helper()
	repo = initRepo(t)
	file = filepath.Join(repo, "f.txt")
	writeFileT(t, file, "one\ntwo\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	return repo, file
}

// TestMenuToggleGitPanel pins the toggle contract: opening loads the
// changed-file list immediately (not on the next tick), collapsing
// keeps state, and the menu label flips with the panel.
func TestMenuToggleGitPanel(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo, file := panelRepo(t)
	writeFileT(t, file, "one\nCHANGED\n")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	if a.gitPanelToggleLabel() != "Show git panel" {
		t.Fatalf("label = %q", a.gitPanelToggleLabel())
	}

	a.menuToggleGitPanel()
	if !a.gitPanel.open {
		t.Fatal("toggle should open the panel")
	}
	if len(a.gitPanel.files) != 1 || a.gitPanel.files[0].Rel != "f.txt" {
		t.Fatalf("panel files = %+v, want f.txt", a.gitPanel.files)
	}
	if a.gitPanelToggleLabel() != "Hide git panel" {
		t.Fatalf("open label = %q", a.gitPanelToggleLabel())
	}

	a.menuToggleGitPanel()
	if a.gitPanel.open {
		t.Fatal("second toggle should collapse")
	}
}

// TestEditorRect_ShrinksForGitPanel pins the layout contract: the
// panel's rows come out of the editor, and the panel sits directly
// above the status bar (above the find bar too when that's open).
func TestEditorRect_ShrinksForGitPanel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	_, _, _, hBefore := a.editorRect()

	a.gitPanel.open = true
	_, _, _, hAfter := a.editorRect()
	if hAfter != hBefore-a.gitPanelHeight() {
		t.Fatalf("editor h = %d, want %d", hAfter, hBefore-a.gitPanelHeight())
	}

	_, py, _, ph := a.gitPanelRect()
	if py+ph != a.height-1 {
		t.Fatalf("panel bottom = %d, want flush against status bar at %d", py+ph, a.height-1)
	}

	a.findOpen = true
	_, py, _, ph = a.gitPanelRect()
	if py+ph != a.height-1-findBarHeight {
		t.Fatalf("with find bar: panel bottom = %d, want %d", py+ph, a.height-1-findBarHeight)
	}
	_, _, _, hFind := a.editorRect()
	if hFind != hAfter-findBarHeight {
		t.Fatalf("editor h with find bar = %d, want %d", hFind, hAfter-findBarHeight)
	}
}

// TestLoadGitStatusFiles_CodesAndScope verifies the list parse: codes
// survive, renames keep the new path, and entries outside the
// editor's root are filtered even when the repo is bigger.
func TestLoadGitStatusFiles_CodesAndScope(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(repo, "outside.txt"), "x\n")
	writeFileT(t, filepath.Join(sub, "tracked.txt"), "x\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	writeFileT(t, filepath.Join(repo, "outside.txt"), "changed\n") // outside sub
	writeFileT(t, filepath.Join(sub, "tracked.txt"), "changed\n")  // modified
	writeFileT(t, filepath.Join(sub, "brand-new.txt"), "fresh\n")  // untracked

	files := loadGitStatusFiles(sub)
	if len(files) != 2 {
		t.Fatalf("files = %+v, want 2 entries scoped to sub/", files)
	}
	byRel := map[string]gitPanelFile{}
	for _, f := range files {
		byRel[f.Rel] = f
	}
	if f, ok := byRel["tracked.txt"]; !ok || strings.TrimSpace(f.Code) != "M" {
		t.Fatalf("tracked.txt = %+v, want code M", byRel)
	}
	if f, ok := byRel["brand-new.txt"]; !ok || f.Code != "??" {
		t.Fatalf("brand-new.txt = %+v, want code ??", byRel)
	}

	if got := loadGitStatusFiles(t.TempDir()); got != nil {
		t.Fatalf("non-repo = %+v, want nil", got)
	}
}

// TestLoadGitPanelDiff_ModifiedAndUntracked exercises the two fetch
// paths: a tracked edit produces a real unified diff, and an untracked
// file falls back to synthesized all-added lines.
func TestLoadGitPanelDiff_ModifiedAndUntracked(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo, file := panelRepo(t)
	writeFileT(t, file, "one\nCHANGED\n")

	lines := loadGitPanelDiff(repo, gitPanelFile{Path: file, Rel: "f.txt", Code: " M"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-two") || !strings.Contains(joined, "+CHANGED") {
		t.Fatalf("diff missing change lines:\n%s", joined)
	}

	newFile := filepath.Join(repo, "fresh.txt")
	writeFileT(t, newFile, "alpha\nbeta\n")
	lines = loadGitPanelDiff(repo, gitPanelFile{Path: newFile, Rel: "fresh.txt", Code: "??"})
	joined = strings.Join(lines, "\n")
	if !strings.Contains(joined, "+alpha") || !strings.Contains(joined, "+beta") {
		t.Fatalf("untracked fallback missing +lines:\n%s", joined)
	}
	if !strings.Contains(joined, "untracked") {
		t.Fatalf("untracked fallback should label itself:\n%s", joined)
	}
}

// TestGitPanel_DiffAsyncRoundTrip drives the production pipeline:
// opening the panel requests the selected file's diff on a goroutine,
// the event lands, and the diff text is ready to draw.
func TestGitPanel_DiffAsyncRoundTrip(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo, file := panelRepo(t)
	writeFileT(t, file, "one\nCHANGED\n")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuToggleGitPanel()

	pumpAppEvents(t, a, func() bool {
		return strings.Contains(strings.Join(a.gitPanel.diffLines, "\n"), "+CHANGED")
	})
	if a.gitPanel.diffPath != file {
		t.Fatalf("diffPath = %q, want %q", a.gitPanel.diffPath, file)
	}
}

// TestHandleGitPanelDiff_DropsStaleResults pins the staleness rule: a
// diff arriving for a file the user already clicked away from must
// not overwrite the current selection's pane.
func TestHandleGitPanelDiff_DropsStaleResults(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	a.gitPanel.files = []gitPanelFile{
		{Path: "/p/a.txt", Rel: "a.txt", Code: " M"},
		{Path: "/p/b.txt", Rel: "b.txt", Code: " M"},
	}
	a.gitPanel.selected = 1

	a.handleGitPanelDiff(&gitPanelDiffEvent{when: time.Now(), path: "/p/a.txt", lines: []string{"+stale"}})
	if len(a.gitPanel.diffLines) != 0 {
		t.Fatalf("stale diff stored: %v", a.gitPanel.diffLines)
	}

	a.handleGitPanelDiff(&gitPanelDiffEvent{when: time.Now(), path: "/p/b.txt", lines: []string{"+fresh"}})
	if len(a.gitPanel.diffLines) != 1 || a.gitPanel.diffLines[0] != "+fresh" {
		t.Fatalf("selected diff not stored: %v", a.gitPanel.diffLines)
	}
}

// TestGitPanelClick_SelectsAndCloses covers the mouse contract: a
// click on a list row selects it, the header ✕ collapses the panel,
// and clicks on the diff side change nothing.
func TestGitPanelClick_SelectsAndCloses(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	a.gitPanel.files = []gitPanelFile{
		{Path: "/p/a.txt", Rel: "a.txt", Code: " M"},
		{Path: "/p/b.txt", Rel: "b.txt", Code: " M"},
	}

	px, py, pw, _ := a.gitPanelRect()
	// Click past the checkbox gutter — the gutter toggles staging
	// instead of selecting (covered by its own test).
	a.gitPanelClick(px+gitPanelCheckboxW+1, py+2) // second list row
	if a.gitPanel.selected != 1 {
		t.Fatalf("selected = %d, want 1", a.gitPanel.selected)
	}

	a.gitPanelClick(px+pw-2, py+3) // diff pane — inert
	if a.gitPanel.selected != 1 || !a.gitPanel.open {
		t.Fatal("diff-pane click must not change state")
	}

	btn := a.gitPanelCloseRect()
	a.gitPanelClick(btn.x+1, btn.y)
	if a.gitPanel.open {
		t.Fatal("✕ click should collapse the panel")
	}
}

// TestGitPanelScroll_SplitsByColumn pins wheel routing inside the
// panel: the list half scrolls the list, the diff half scrolls the
// diff, and both clamp to their content.
func TestGitPanelScroll_SplitsByColumn(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	for i := 0; i < 40; i++ {
		a.gitPanel.files = append(a.gitPanel.files,
			gitPanelFile{Path: "/p/f", Rel: "f", Code: " M"})
		a.gitPanel.diffLines = append(a.gitPanel.diffLines, "+x")
	}

	px, py, pw, _ := a.gitPanelRect()
	a.gitPanelScroll(px+1, py+2, 5)
	if a.gitPanel.listScroll != 5 || a.gitPanel.diffScroll != 0 {
		t.Fatalf("list wheel: list=%d diff=%d", a.gitPanel.listScroll, a.gitPanel.diffScroll)
	}
	a.gitPanelScroll(px+pw-2, py+2, 7)
	if a.gitPanel.diffScroll != 7 {
		t.Fatalf("diff wheel: diff=%d", a.gitPanel.diffScroll)
	}
	a.gitPanelScroll(px+pw-2, py+2, 999)
	if max := len(a.gitPanel.diffLines) - (a.gitPanelHeight() - 1); a.gitPanel.diffScroll != max {
		t.Fatalf("diff scroll = %d, want clamped to %d", a.gitPanel.diffScroll, max)
	}
	a.gitPanelScroll(px+1, py+2, -999)
	if a.gitPanel.listScroll != 0 {
		t.Fatalf("list scroll = %d, want clamped to 0", a.gitPanel.listScroll)
	}
}

// TestDrawGitPanel_Smoke renders the panel on the simulation screen
// and asserts the load-bearing strings land: header title, file row,
// and a diff line. Catches row-offset regressions the logic tests
// can't see.
func TestDrawGitPanel_Smoke(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	a.gitPanel.files = []gitPanelFile{{Path: "/p/main.go", Rel: "main.go", Code: " M"}}
	a.gitPanel.diffLines = []string{"@@ -1 +1 @@", "-old line", "+new line"}

	a.draw()
	a.screen.Show()
	content := screenText(a)
	// "[ ]" is the unstaged checkbox for the " M" row.
	for _, want := range []string{"Git changes", "[ ]", "main.go", "+new line", "-old line", "✕"} {
		if !strings.Contains(content, want) {
			t.Fatalf("drawn panel missing %q:\n%s", want, content)
		}
	}
}

// TestGitPanel_RefreshPreservesSelectionByPath pins the identity rule
// borrowed from the tree refresh: a status reload keeps the selected
// file selected even when its index shifts.
func TestGitPanel_RefreshPreservesSelectionByPath(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo, file := panelRepo(t)
	writeFileT(t, file, "one\nCHANGED\n")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuToggleGitPanel()
	if len(a.gitPanel.files) != 1 {
		t.Fatalf("files = %+v", a.gitPanel.files)
	}

	// A new untracked file sorts before f.txt in porcelain order —
	// the selection must follow f.txt to its new index.
	writeFileT(t, filepath.Join(repo, "aaa.txt"), "x\n")
	a.refreshGitStatus()
	if len(a.gitPanel.files) != 2 {
		t.Fatalf("files after refresh = %+v", a.gitPanel.files)
	}
	sel, ok := a.gitPanelSelectedFile()
	if !ok || sel.Path != file {
		t.Fatalf("selection = %+v, want it pinned to %s", sel, file)
	}
}

// TestLeader_GTogglesGitPanel pins the Esc-g binding to the panel
// toggle — and the leader contract that it silently no-ops outside a
// git repository instead of opening a panel with nothing to show.
func TestLeader_GTogglesGitPanel(t *testing.T) {
	action := leaderActionFor('g')
	if action == nil {
		t.Fatal("Esc-g should be bound")
	}
	a := newTestApp(t, t.TempDir())
	action(a)
	if a.gitPanel.open {
		t.Fatal("Esc-g outside a repo should be a no-op")
	}
	a.gitIsRepo = true
	action(a)
	if !a.gitPanel.open {
		t.Fatal("Esc-g should open the git panel inside a repo")
	}
	action(a)
	if a.gitPanel.open {
		t.Fatal("Esc-g should collapse the open panel")
	}
}

// TestDiffTargetLine pins the diff-row → buffer-line mapping across
// every row kind: file headers have no target, hunk headers land on
// the hunk start, added/context rows track the new-side counter,
// deleted rows land on the boundary line, and the synthetic untracked
// view maps rows 1:1.
func TestDiffTargetLine(t *testing.T) {
	lines := []string{
		"diff --git a/f.txt b/f.txt", // 0
		"index 1234567..89abcde 100644",
		"--- a/f.txt",
		"+++ b/f.txt",
		"@@ -1,3 +1,4 @@",   // 4
		" one",              // 5 → line 0
		"+NEW",              // 6 → line 1
		" two",              // 7 → line 2
		"-gone",             // 8 → boundary: line 3 (the row after the deletion)
		" three",            // 9 → line 3
		"@@ -10,2 +12,2 @@", // 10 → line 11
		" ctx",              // 11 → line 11
	}
	cases := []struct {
		idx    int
		want   int
		wantOK bool
	}{
		{0, 0, false}, {1, 0, false}, {2, 0, false}, {3, 0, false},
		{4, 0, true},
		{5, 0, true},
		{6, 1, true},
		{7, 2, true},
		{8, 3, true},
		{9, 3, true},
		{10, 11, true},
		{11, 11, true},
		{99, 0, false},
		{-1, 0, false},
	}
	for _, tc := range cases {
		got, ok := diffTargetLine(lines, tc.idx)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("idx %d (%q): got (%d,%v), want (%d,%v)",
				tc.idx, lines[max(0, min(tc.idx, len(lines)-1))], got, ok, tc.want, tc.wantOK)
		}
	}

	synth := []string{gitPanelUntrackedHeader, "+alpha", "+beta"}
	if _, ok := diffTargetLine(synth, 0); ok {
		t.Error("untracked label row must have no target")
	}
	if got, ok := diffTargetLine(synth, 2); !ok || got != 1 {
		t.Errorf("untracked row 2: got (%d,%v), want (1,true)", got, ok)
	}
	if _, ok := diffTargetLine(nil, 0); ok {
		t.Error("empty pane must have no target")
	}
}

// TestGitPanelDoubleClick_JumpsToEditor drives the gesture end to end:
// a single click on a diff row is inert, the second click within the
// double-click window opens the file and parks the cursor on the line
// the diff row describes.
func TestGitPanelDoubleClick_JumpsToEditor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	writeFileT(t, file, "one\nNEW\ntwo\nthree\n")

	a := newTestApp(t, dir)
	a.gitPanel.open = true
	a.gitPanel.files = []gitPanelFile{{Path: file, Rel: "f.txt", Code: " M"}}
	a.gitPanel.diffLines = []string{
		"@@ -1,3 +1,4 @@",
		" one",
		"+NEW",
		" two",
	}

	px, py, pw, _ := a.gitPanelRect()
	// Diff row index 2 ("+NEW") renders at panel row 3 (header + 2).
	cx, cy := px+a.gitPanelListW(pw)+3, py+3

	a.gitPanelClick(cx, cy)
	if a.activeTabPtr() != nil {
		t.Fatal("single click must not open anything")
	}
	a.gitPanelClick(cx, cy)
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != file {
		t.Fatalf("double click should open %s, active = %+v", file, tab)
	}
	if tab.Cursor.Line != 1 || tab.Cursor.Col != 0 {
		t.Fatalf("cursor = %+v, want line 1 col 0", tab.Cursor)
	}
	if !a.gitPanel.open {
		t.Fatal("jumping must keep the panel open — the user is mid-review")
	}
}

// TestResizeGitPanel_ClampsAndOverridesAuto pins the height rules: a
// user height beats auto mode, both ends clamp, and a terminal shrink
// re-clamps a remembered height instead of squeezing the editor out.
func TestResizeGitPanel_ClampsAndOverridesAuto(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true

	auto := a.gitPanelHeight()
	if auto != 13 { // 40/3, within [6,18]
		t.Fatalf("auto height = %d, want 13", auto)
	}

	a.resizeGitPanel(16)
	if got := a.gitPanelHeight(); got != 16 {
		t.Fatalf("user height = %d, want 16", got)
	}

	a.resizeGitPanel(100)
	if got, max := a.gitPanelHeight(), a.height-2-gitPanelMinEditorRows; got != max {
		t.Fatalf("over-grow = %d, want clamped to %d", got, max)
	}

	a.resizeGitPanel(1)
	if got := a.gitPanelHeight(); got != gitPanelMinHeight {
		t.Fatalf("over-shrink = %d, want %d", got, gitPanelMinHeight)
	}

	a.resizeGitPanel(30)
	a.height = 20 // terminal shrank under a remembered height of 30
	if got, max := a.gitPanelHeight(), 20-2-gitPanelMinEditorRows; got != max {
		t.Fatalf("after shrink = %d, want re-clamped to %d", got, max)
	}
}

// TestHandleMouse_GitPanelHeaderDrag verifies the full drag pipeline
// through the real mouse router: press on the header rule arms the
// drag, moving with the button held resizes live, release disarms.
func TestHandleMouse_GitPanelHeaderDrag(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	px, py, _, _ := a.gitPanelRect()

	a.handleMouse(tcell.NewEventMouse(px+5, py, tcell.Button1, 0))
	if a.dragMode != "gitpanel" {
		t.Fatalf("dragMode = %q, want gitpanel", a.dragMode)
	}
	a.handleMouse(tcell.NewEventMouse(px+5, py-3, tcell.Button1, 0))
	if got := a.gitPanelHeight(); got != a.height-1-(py-3) {
		t.Fatalf("dragged height = %d, want %d", got, a.height-1-(py-3))
	}
	a.handleMouse(tcell.NewEventMouse(px+5, py-3, tcell.ButtonNone, 0))
	if a.dragMode != "" {
		t.Fatalf("dragMode after release = %q, want cleared", a.dragMode)
	}

	// The ✕ is exempt from the grab handle — pressing it collapses.
	btn := a.gitPanelCloseRect()
	a.handleMouse(tcell.NewEventMouse(btn.x+1, btn.y, tcell.Button1, 0))
	if a.gitPanel.open || a.dragMode != "" {
		t.Fatal("✕ press should collapse, not start a drag")
	}
}

// TestGitPanelListWidth_ClampsAndOverridesAuto pins the file-list column
// rules, the horizontal twin of the height test: a user width beats the
// auto third and both ends clamp to the legal band.
func TestGitPanelListWidth_ClampsAndOverridesAuto(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	_, _, pw, _ := a.gitPanelRect()

	auto := a.gitPanelListW(pw) // pw is 90 here → 90/3, within [24,40]
	if auto != 30 {
		t.Fatalf("auto list width = %d, want 30 (panel width %d)", auto, pw)
	}

	a.resizeGitPanelListWidth(36)
	if got := a.gitPanelListW(pw); got != 36 {
		t.Fatalf("user list width = %d, want 36", got)
	}

	a.resizeGitPanelListWidth(100)
	if got := a.gitPanelListW(pw); got != gitPanelMaxListW {
		t.Fatalf("over-grow = %d, want clamped to %d", got, gitPanelMaxListW)
	}

	a.resizeGitPanelListWidth(1)
	if got := a.gitPanelListW(pw); got != gitPanelMinListW {
		t.Fatalf("over-shrink = %d, want %d", got, gitPanelMinListW)
	}
}

// TestHandleMouse_GitListDividerDrag verifies the list/diff divider is a
// real mouse resize handle: a body-row press on the divider column arms a
// "gitlistdiv" drag, moving with the button held reshapes the columns
// live, and release disarms — while a press on the divider column of the
// header row still starts a height drag, not a width one.
func TestHandleMouse_GitListDividerDrag(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	px, py, pw, _ := a.gitPanelRect()
	divX := a.gitPanelDividerX()
	if divX != px+a.gitPanelListW(pw) {
		t.Fatalf("dividerX = %d, want %d", divX, px+a.gitPanelListW(pw))
	}

	// Press on the divider, one body row down, arms the width drag.
	a.handleMouse(tcell.NewEventMouse(divX, py+2, tcell.Button1, 0))
	if a.dragMode != "gitlistdiv" {
		t.Fatalf("dragMode = %q, want gitlistdiv", a.dragMode)
	}
	// Drag right: the list column should follow the mouse x.
	a.handleMouse(tcell.NewEventMouse(px+36, py+2, tcell.Button1, 0))
	if got := a.gitPanelListW(pw); got != 36 {
		t.Fatalf("dragged list width = %d, want 36", got)
	}
	a.handleMouse(tcell.NewEventMouse(px+36, py+2, tcell.ButtonNone, 0))
	if a.dragMode != "" {
		t.Fatalf("dragMode after release = %q, want cleared", a.dragMode)
	}

	// Same column on the header row is the HEIGHT handle, not the width
	// one — the divider only exists on body rows.
	if got := a.gitPanelPress(divX, py); got != "gitpanel" {
		t.Fatalf("header-row press on divider col = %q, want gitpanel", got)
	}
}

// TestDrawGitPanel_HandlesBrightenWhileDragging pins the grab-handle
// affordance: the header rule and the list/diff divider each sit in
// Subtle when idle and light up in Accent only while their own drag is
// active — so a grabbed handle is unmistakable and the other stays quiet.
func TestDrawGitPanel_HandlesBrightenWhileDragging(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true
	a.gitPanel.files = []gitPanelFile{{Path: "/p/main.go", Rel: "main.go", Code: " M"}}
	px, py, pw, _ := a.gitPanelRect()
	divX := a.gitPanelDividerX()
	ruleX := px + pw - 8 // plain header rule, clear of the title and ✕

	handleFgs := func(mode string) (divFg, ruleFg tcell.Color) {
		a.dragMode = mode
		a.draw()
		a.screen.Show()
		cells, w, _ := a.screen.(tcell.SimulationScreen).GetContents()
		divFg, _, _ = cells[(py+2)*w+divX].Style.Decompose()
		ruleFg, _, _ = cells[py*w+ruleX].Style.Decompose()
		return
	}

	div, rule := handleFgs("")
	if div != a.theme.Subtle || rule != a.theme.Subtle {
		t.Fatalf("idle: divider=%v rule=%v, both want Subtle %v", div, rule, a.theme.Subtle)
	}

	div, rule = handleFgs("gitlistdiv")
	if div != a.theme.Accent {
		t.Fatalf("dragging divider: divider fg=%v, want Accent %v", div, a.theme.Accent)
	}
	if rule != a.theme.Subtle {
		t.Fatalf("dragging divider: header rule fg=%v, want it to stay Subtle", rule)
	}

	div, rule = handleFgs("gitpanel")
	if rule != a.theme.Accent {
		t.Fatalf("dragging header: rule fg=%v, want Accent %v", rule, a.theme.Accent)
	}
	if div != a.theme.Subtle {
		t.Fatalf("dragging header: divider fg=%v, want it to stay Subtle", div)
	}
}

// TestLeaders_ResizeGitPanel pins Esc-= / Esc-- to grow/shrink while
// open and to do nothing while collapsed.
func TestLeaders_ResizeGitPanel(t *testing.T) {
	grow, shrink := leaderActionFor('='), leaderActionFor('-')
	if grow == nil || shrink == nil {
		t.Fatal("Esc-= and Esc-- should be bound")
	}
	a := newTestApp(t, t.TempDir())

	grow(a)
	if a.gitPanel.height != 0 {
		t.Fatal("resize leaders must no-op while the panel is collapsed")
	}

	a.gitPanel.open = true
	base := a.gitPanelHeight()
	grow(a)
	if got := a.gitPanelHeight(); got != base+gitPanelResizeStep {
		t.Fatalf("grow: %d → %d, want %d", base, got, base+gitPanelResizeStep)
	}
	shrink(a)
	if got := a.gitPanelHeight(); got != base {
		t.Fatalf("shrink: got %d, want back to %d", got, base)
	}
}

// TestGitPanelStageState pins the porcelain-XY → checkbox-state map:
// index-only changes read as fully staged, work-tree-only and untracked
// as unstaged, and mixed index+work-tree codes as partial. Short or
// empty codes (defensive) read as unstaged.
func TestGitPanelStageState(t *testing.T) {
	cases := []struct {
		code string
		want gitStageState
	}{
		{"??", stageNone},
		{" M", stageNone},
		{" D", stageNone},
		{"M ", stageFull},
		{"A ", stageFull},
		{"D ", stageFull},
		{"R ", stageFull},
		{"MM", stagePartial},
		{"AM", stagePartial},
		{"", stageNone},
		{"M", stageNone},
	}
	for _, c := range cases {
		if got := gitPanelStageState(c.code); got != c.want {
			t.Errorf("gitPanelStageState(%q) = %d, want %d", c.code, got, c.want)
		}
	}
}

// TestGitPanelCheckbox pins the three glyphs — draw and hit-test both
// assume the box is exactly three cells wide.
func TestGitPanelCheckbox(t *testing.T) {
	for state, want := range map[gitStageState]string{
		stageNone: "[ ]", stagePartial: "[~]", stageFull: "[x]",
	} {
		if got := gitPanelCheckbox(state); got != want {
			t.Errorf("checkbox(%d) = %q, want %q", state, got, want)
		}
	}
}

// TestGitPanelCheckboxClick_StagesAndUnstages is the checkbox e2e:
// ticking an unstaged file's box runs git add through the async
// pipeline and the done-event refresh flips the row to [x]; ticking
// again unstages without touching the work-tree edit. The selection
// must not move — ticking boxes is not browsing.
func TestGitPanelCheckboxClick_StagesAndUnstages(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo, file := panelRepo(t)
	writeFileT(t, file, "one\nCHANGED\n")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuToggleGitPanel()
	if len(a.gitPanel.files) != 1 || gitPanelStageState(a.gitPanel.files[0].Code) != stageNone {
		t.Fatalf("panel files = %+v, want one unstaged entry", a.gitPanel.files)
	}

	px, py, _, _ := a.gitPanelRect()
	a.gitPanelClick(px+1, py+1) // first row's checkbox gutter
	pumpAppEvents(t, a, func() bool {
		return len(a.gitPanel.files) == 1 &&
			gitPanelStageState(a.gitPanel.files[0].Code) == stageFull
	})
	if staged := gitOut(t, repo, "diff", "--cached", "--name-only"); staged != "f.txt" {
		t.Fatalf("staged files = %q, want f.txt", staged)
	}

	a.gitPanelClick(px+1, py+1) // same box, now [x] → unstage
	pumpAppEvents(t, a, func() bool {
		return len(a.gitPanel.files) == 1 &&
			gitPanelStageState(a.gitPanel.files[0].Code) == stageNone
	})
	if staged := gitOut(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("staged files after unstage = %q, want none", staged)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(content) != "one\nCHANGED\n" {
		t.Fatalf("work-tree content = %q — unstage must not revert the edit", content)
	}
	if a.gitPanel.selected != 0 {
		t.Fatalf("selected = %d, checkbox clicks must not move selection", a.gitPanel.selected)
	}
}
