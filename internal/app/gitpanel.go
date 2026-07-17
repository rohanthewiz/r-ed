// =============================================================================
// File: internal/app/gitpanel.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// gitpanel.go implements the collapsible git panel — an IntelliJ-style
// review surface pinned to the lower portion of the editor area:
// changed files on the left, the selected file's diff against HEAD on
// the right. Its job is "let me eyeball the diffs before I commit",
// so it shows staged + unstaged changes combined (`git diff HEAD`) —
// the sum of what a commit could include — not just the index.
//
// It is NOT a modal: the editor keeps the keyboard, the panel is
// mouse-driven (click a file to see its diff, tick its checkbox to
// stage / unstage it, double-click a diff line to jump the editor
// there, drag the header rule to resize, wheel to scroll, ✕ or Esc-g
// to collapse), following the find bar's "strip
// that owns part of the layout" precedent rather than the single-slot
// overlay system. Esc-= / Esc-- resize from the keyboard.
//
// House patterns in play:
//   - Best-effort git: list/diff failures render an empty panel, never
//     an error. The panel refreshes with git status on the 10s tick.
//   - Custom tcell events: diffs are fetched on a goroutine and posted
//     back as gitPanelDiffEvent; only the main loop mutates state.

package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/theme"
)

const (
	// gitPanelMinHeight is the floor below which the panel stops being
	// useful (header + a handful of diff rows). gitPanelMaxHeight caps
	// the land-grab on tall terminals — the editor stays the main
	// surface; the panel is a review strip, not a second editor.
	gitPanelMinHeight = 6
	gitPanelMaxHeight = 18

	// File-list column bounds. 24 fits "[x] M internal/app/x.go" shapes
	// (checkbox + code + a useful chunk of path) without wasting diff
	// columns; 40 stops pathological growth on ultra-wide terminals.
	gitPanelMinListW = 24
	gitPanelMaxListW = 40

	// gitPanelCheckboxW is the width of the stage-checkbox gutter at the
	// left of each file row — " [x] " — and therefore also the click
	// target: presses at x < px+gitPanelCheckboxW toggle staging, presses
	// beyond it select the row. One constant so draw and hit-test agree.
	gitPanelCheckboxW = 5

	// gitPanelMinEditorRows is how much editor a user-driven resize
	// must leave standing — the panel is a review strip, and dragging
	// the editor down to nothing helps nobody. Same idea as
	// minEditorAfterDrag for the sidebar splitter.
	gitPanelMinEditorRows = 5

	// gitPanelResizeStep is how many rows the Esc-= / Esc-- leaders
	// grow / shrink the panel per press. Two rows per press means a
	// noticeable change without needing to mash the key.
	gitPanelResizeStep = 2
)

// gitPanelUntrackedHeader labels the synthesized all-added view for
// untracked files. diffTargetLine keys off it too, so the two sides of
// the synthetic format can't drift apart.
const gitPanelUntrackedHeader = "new file (untracked)"

// gitPanelFile is one changed entry in the panel's file list: the
// absolute path (diff commands and selection identity), the
// project-relative label the list renders, and the two-char porcelain
// XY code that picks the row's color, checkbox state, and the
// untracked fallback.
type gitPanelFile struct {
	Path string
	Rel  string
	Code string
}

// gitStageState classifies a porcelain XY code for the panel's stage
// checkbox: nothing staged, everything staged, or a mix (index and work
// tree both carry changes — e.g. "MM" after editing a staged file).
type gitStageState int

const (
	stageNone gitStageState = iota
	stagePartial
	stageFull
)

// gitPanelStageState reads a porcelain XY code into a gitStageState.
// The X column says whether the index has changes (anything but ' ',
// '?', '!'); the Y column says whether the work tree still differs from
// the index. Staged X + dirty Y = partial.
func gitPanelStageState(code string) gitStageState {
	if len(code) < 2 {
		return stageNone
	}
	switch code[0] {
	case ' ', '?', '!':
		return stageNone
	}
	if code[1] != ' ' {
		return stagePartial
	}
	return stageFull
}

// gitPanelCheckbox is the three-cell glyph drawn for each stage state.
// [~] for partial follows the tri-state checkbox convention — it reads
// as "some of this file is staged", and clicking it stages the rest.
func gitPanelCheckbox(s gitStageState) string {
	switch s {
	case stageFull:
		return "[x]"
	case stagePartial:
		return "[~]"
	default:
		return "[ ]"
	}
}

// gitPanelState is the panel's whole state, mutated only on the main
// loop. Selection and scroll survive a collapse — reopening the panel
// puts the user back where they were, same as the sidebar toggle.
type gitPanelState struct {
	open bool
	// height is the user-chosen row count from a header drag or the
	// resize leaders; 0 means "auto" (a third of the screen). Session-
	// only, like sidebarWidth — r-ed deliberately has no layout config.
	height int
	// listWidth is the user-chosen file-list column count from a
	// list/diff divider drag; 0 means "auto" (a third of the panel).
	// The horizontal twin of height, same session-only lifetime.
	listWidth  int
	files      []gitPanelFile
	selected   int
	listScroll int
	// diffPath is the path diffLines belong to. It lags selection while
	// a fetch is in flight and is what makes stale async results
	// detectable (see handleGitPanelDiff).
	diffPath   string
	diffLines  []string
	diffScroll int
}

// gitPanelDiffEvent carries one file's freshly-fetched diff text from
// the background goroutine to the main loop.
type gitPanelDiffEvent struct {
	when  time.Time
	path  string
	lines []string
}

// When satisfies the tcell.Event interface.
func (e *gitPanelDiffEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Toggle + refresh
// -----------------------------------------------------------------------------

// menuToggleGitPanel is the ≡ menu / Esc-g entry point. Opening
// refreshes immediately so the list reflects this instant, not the
// last 10s tick; collapsing keeps the state for a cheap reopen.
func (a *App) menuToggleGitPanel() {
	a.closeMenu()
	// Outside a repo there's nothing to review — the menu row is
	// already dimmed via hasGitRepo; this guard gives the Esc-g leader
	// the same silent no-op the other leaders promise. Collapsing an
	// already-open panel is always allowed.
	if !a.gitPanel.open && !a.gitIsRepo {
		return
	}
	a.gitPanel.open = !a.gitPanel.open
	if a.gitPanel.open {
		// Single-occupancy bottom strip: a bottom-docked terminal
		// yields (its session and scrollback survive — Esc-` brings
		// it right back). A left-docked strip isn't competing for
		// the bottom, so it stays.
		if !a.termDockLeft {
			a.term.open = false
			a.term.focused = false
		}
		a.refreshGitPanelFiles()
	}
}

// gitPanelToggleLabel is the dynamic menu label — reads as the action
// it will perform, mirroring the sidebar toggle's convention.
func (a *App) gitPanelToggleLabel() string {
	if a.gitPanel.open {
		return "Hide git panel"
	}
	return "Show git panel"
}

// refreshGitPanelFiles reloads the changed-file list, preserving the
// selection by path (the identity-preserving-refresh idea from the
// file tree, minus the tree). No-op while the panel is collapsed so
// the 10s tick doesn't pay two extra git forks for a hidden surface.
func (a *App) refreshGitPanelFiles() {
	if !a.gitPanel.open {
		return
	}
	prevPath := ""
	if f, ok := a.gitPanelSelectedFile(); ok {
		prevPath = f.Path
	}
	a.gitPanel.files = loadGitStatusFiles(a.rootDir)
	a.gitPanel.selected = 0
	for i, f := range a.gitPanel.files {
		if f.Path == prevPath {
			a.gitPanel.selected = i
			break
		}
	}
	a.gitPanelClampScrolls()
	a.gitPanelEnsureSelectedVisible()
	if f, ok := a.gitPanelSelectedFile(); ok {
		a.requestGitPanelDiff(f)
	} else {
		// Nothing changed anymore (e.g. the user just committed) —
		// clear the stale diff instead of showing text for a file
		// that's no longer listed.
		a.gitPanel.diffPath = ""
		a.gitPanel.diffLines = nil
		a.gitPanel.diffScroll = 0
	}
}

// gitPanelSelectedFile returns the currently selected entry, ok=false
// when the list is empty.
func (a *App) gitPanelSelectedFile() (gitPanelFile, bool) {
	s := a.gitPanel.selected
	if s < 0 || s >= len(a.gitPanel.files) {
		return gitPanelFile{}, false
	}
	return a.gitPanel.files[s], true
}

// loadGitStatusFiles returns the changed entries under rootDir in
// porcelain order, with their XY codes. Same best-effort contract as
// loadGitStatus; entries outside rootDir are filtered because the
// panel reviews the project the editor has open, not the whole repo.
func loadGitStatusFiles(rootDir string) []gitPanelFile {
	if rootDir == "" {
		return nil
	}
	topBytes, err := exec.Command("git", "-C", rootDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	toplevel := strings.TrimRight(string(topBytes), "\n\r")
	out, err := exec.Command("git", "-C", rootDir, "status", "--porcelain").Output()
	if err != nil || toplevel == "" {
		return nil
	}

	var files []gitPanelFile
	for _, raw := range bytes.Split(out, []byte{'\n'}) {
		line := string(raw)
		if len(line) < 4 {
			continue
		}
		code := line[:2]
		body := line[3:]
		// Renames report "old -> new"; the new path is what exists on
		// disk and what a diff can be shown for.
		if idx := strings.Index(body, " -> "); idx >= 0 {
			body = body[idx+len(" -> "):]
		}
		rel := unquotePath(body)
		if rel == "" {
			continue
		}
		abs := filepath.Join(toplevel, rel)
		if !pathInside(abs, rootDir) {
			continue
		}
		label, err := filepath.Rel(rootDir, abs)
		if err != nil {
			label = rel
		}
		files = append(files, gitPanelFile{Path: abs, Rel: label, Code: code})
	}
	return files
}

// -----------------------------------------------------------------------------
// Diff fetching — goroutine side
// -----------------------------------------------------------------------------

// requestGitPanelDiff fetches f's diff against HEAD on a goroutine and
// posts the result. Untracked files (and anything HEAD can't diff,
// like a staged add on an unborn branch) fall back to `diff --cached`,
// then to the file's own content rendered as pure additions — IntelliJ
// does the same so a brand-new file is still reviewable.
func (a *App) requestGitPanelDiff(f gitPanelFile) {
	if a.screen == nil || a.rootDir == "" {
		return
	}
	scr := a.screen
	root := a.rootDir
	go func() {
		lines := loadGitPanelDiff(root, f)
		_ = scr.PostEvent(&gitPanelDiffEvent{when: time.Now(), path: f.Path, lines: lines})
	}()
}

// loadGitPanelDiff produces the text lines shown in the diff pane.
// Split from requestGitPanelDiff so tests can call it synchronously.
func loadGitPanelDiff(rootDir string, f gitPanelFile) []string {
	out, err := exec.Command("git", "-C", rootDir, "diff", "HEAD", "--no-color", "--", f.Path).Output()
	if err != nil {
		// HEAD may not exist yet (unborn branch) — the index diff is
		// the next best answer for staged files.
		out, err = exec.Command("git", "-C", rootDir, "diff", "--cached", "--no-color", "--", f.Path).Output()
		if err != nil {
			out = nil
		}
	}
	if lines := splitDiffLines(out); len(lines) > 0 {
		return lines
	}
	// Empty diff + untracked file: synthesize an all-added view from
	// disk, capped implicitly by the panel's scroll (no need to limit
	// here; even large files are just a []string).
	if strings.HasPrefix(f.Code, "?") {
		content, err := os.ReadFile(f.Path)
		if err != nil {
			return nil
		}
		lines := []string{gitPanelUntrackedHeader}
		for _, ln := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
			lines = append(lines, "+"+ln)
		}
		return lines
	}
	return nil
}

// splitDiffLines converts raw git output into display lines, dropping
// the trailing newline's empty tail so the pane doesn't end on a
// phantom blank row.
func splitDiffLines(out []byte) []string {
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// handleGitPanelDiff stores a completed diff fetch. Runs on the main
// loop only. Results for a file the user has already clicked away
// from are dropped — same staleness rule as LSP hover.
func (a *App) handleGitPanelDiff(e *gitPanelDiffEvent) {
	if !a.gitPanel.open {
		return
	}
	f, ok := a.gitPanelSelectedFile()
	if !ok || f.Path != e.path {
		return
	}
	if a.gitPanel.diffPath != e.path {
		a.gitPanel.diffScroll = 0
	}
	a.gitPanel.diffPath = e.path
	a.gitPanel.diffLines = e.lines
	a.gitPanelClampScrolls()
}

// -----------------------------------------------------------------------------
// Geometry — one source for draw AND mouse routing
// -----------------------------------------------------------------------------

// gitPanelHeight returns the panel's row count for the current window.
// A user-set height (drag / resize leaders) wins; auto mode takes a
// third of the screen capped at gitPanelMaxHeight. Both are re-clamped
// against the live window every call, so a terminal resize can never
// leave a remembered height that squeezes the editor out.
func (a *App) gitPanelHeight() int {
	h := a.gitPanel.height
	if h == 0 {
		h = a.height / 3
		if h > gitPanelMaxHeight {
			h = gitPanelMaxHeight
		}
	}
	if h < gitPanelMinHeight {
		h = gitPanelMinHeight
	}
	if max := a.maxGitPanelHeight(); h > max {
		h = max
	}
	return h
}

// maxGitPanelHeight is the tallest the panel may grow while leaving
// the editor its minimum working rows. A user drag may exceed the
// auto-mode cap (gitPanelMaxHeight) — an explicit choice outranks the
// default's restraint — but never this hard limit.
func (a *App) maxGitPanelHeight() int {
	max := a.height - 2 - gitPanelMinEditorRows
	if a.findOpen {
		max -= findBarHeight
	}
	if max < gitPanelMinHeight {
		max = gitPanelMinHeight
	}
	return max
}

// resizeGitPanel records a user-chosen height, clamped to the legal
// band, and re-clamps the scroll offsets against the new viewport.
func (a *App) resizeGitPanel(target int) {
	if target < gitPanelMinHeight {
		target = gitPanelMinHeight
	}
	if max := a.maxGitPanelHeight(); target > max {
		target = max
	}
	a.gitPanel.height = target
	a.gitPanelClampScrolls()
}

// dragGitPanelTo resizes the panel so its header rule tracks the mouse
// row during a drag — same "glued to the cursor" feel as the sidebar
// splitter.
func (a *App) dragGitPanelTo(y int) {
	bottom := a.height - 1
	if a.findOpen {
		bottom -= findBarHeight
	}
	a.resizeGitPanel(bottom - y)
}

// growGitPanel / shrinkGitPanel are the Esc-= / Esc-- leader targets.
// Silent no-ops while the panel is collapsed, per the leader contract.
// There are deliberately no menu rows for these: resize has a primary
// mouse path (dragging the header), the same reasoning that leaves the
// sidebar splitter without menu entries.
func (a *App) growGitPanel() {
	if !a.gitPanel.open {
		return
	}
	a.resizeGitPanel(a.gitPanelHeight() + gitPanelResizeStep)
}

// shrinkGitPanel steps the panel shorter; see growGitPanel.
func (a *App) shrinkGitPanel() {
	if !a.gitPanel.open {
		return
	}
	a.resizeGitPanel(a.gitPanelHeight() - gitPanelResizeStep)
}

// gitPanelRect returns the panel's on-screen rectangle: editor-width
// (the column band between the docked side blocks), sitting directly
// above the find bar (when open) and the status bar — the editor
// shrinks to make room (see editorRect).
func (a *App) gitPanelRect() (x, y, w, h int) {
	lw := a.leftBlockW()
	h = a.gitPanelHeight()
	y = a.height - 1 - h
	if a.findOpen {
		y -= findBarHeight
	}
	return lw, y, a.width - lw - a.rightBlockW(), h
}

// gitPanelListWidth clamps a desired file-list column width for a panel
// of total width w into the legal band: at least gitPanelMinListW, at
// most gitPanelMaxListW, and never past half the panel so the diff pane
// keeps its readable share. A desired <= 0 means "auto" — a third of the
// panel. The half-panel cap is applied last so it wins on narrow strips.
func gitPanelListWidth(w, desired int) int {
	lw := desired
	if lw <= 0 {
		lw = w / 3
	}
	if lw < gitPanelMinListW {
		lw = gitPanelMinListW
	}
	if lw > gitPanelMaxListW {
		lw = gitPanelMaxListW
	}
	if max := w / 2; lw > max {
		lw = max
	}
	return lw
}

// gitPanelListW returns the effective file-list column width for a panel
// of total width pw: the user's chosen width from a divider drag, or the
// auto third when listWidth is 0 — clamped either way. The single source
// every draw / hit-test / resize path consults so they can't drift.
func (a *App) gitPanelListW(pw int) int {
	return gitPanelListWidth(pw, a.gitPanel.listWidth)
}

// gitPanelDividerX returns the screen column of the file-list/diff
// divider — the draggable resize handle between the two panes — or -1
// when the panel is closed. Mirrors splitterX's contract for the sidebar.
func (a *App) gitPanelDividerX() int {
	if !a.gitPanel.open {
		return -1
	}
	px, _, pw, _ := a.gitPanelRect()
	return px + a.gitPanelListW(pw)
}

// dragGitListDivTo resizes the file-list column so the divider tracks the
// mouse x during a drag — the internal-seam twin of dragGitPanelTo, glued
// to the cursor the same way the sidebar splitter is.
func (a *App) dragGitListDivTo(x int) {
	px, _, _, _ := a.gitPanelRect()
	a.resizeGitPanelListWidth(x - px)
}

// resizeGitPanelListWidth records a user-chosen file-list column width,
// clamped to the legal band for the current panel width. Width doesn't
// change the visible row count, so unlike resizeGitPanel it leaves the
// scroll offsets alone.
func (a *App) resizeGitPanelListWidth(target int) {
	_, _, pw, _ := a.gitPanelRect()
	a.gitPanel.listWidth = gitPanelListWidth(pw, target)
}

// gitPanelCloseRect returns the ✕ collapse button's rectangle in the
// header row — computed here once so draw and click hit-testing can't
// drift apart (the btnRect house rule).
func (a *App) gitPanelCloseRect() btnRect {
	px, py, pw, _ := a.gitPanelRect()
	return btnRect{x: px + pw - 4, y: py, w: 3}
}

// gitPanelContains reports whether (x, y) falls inside the open panel.
// Callers check gitPanel.open first; kept separate so the mouse router
// reads as a plain geometry question.
func (a *App) gitPanelContains(x, y int) bool {
	px, py, pw, ph := a.gitPanelRect()
	return x >= px && x < px+pw && y >= py && y < py+ph
}

// gitPanelClampScrolls pins both scroll offsets into range after any
// list/diff mutation. Unlike the editor's overscroll, the panel clamps
// hard — it's a viewer, and half a screen of void below a diff reads
// as "the diff is longer" when it isn't.
func (a *App) gitPanelClampScrolls() {
	_, _, _, ph := a.gitPanelRect()
	visible := ph - 1 // header row
	clamp := func(scroll, total int) int {
		max := total - visible
		if max < 0 {
			max = 0
		}
		if scroll > max {
			scroll = max
		}
		if scroll < 0 {
			scroll = 0
		}
		return scroll
	}
	a.gitPanel.listScroll = clamp(a.gitPanel.listScroll, len(a.gitPanel.files))
	a.gitPanel.diffScroll = clamp(a.gitPanel.diffScroll, len(a.gitPanel.diffLines))
}

// gitPanelEnsureSelectedVisible snaps the list scroll so the selected
// row is on-screen. Called only when the selection itself moves (click,
// refresh reshuffling indexes) — NOT from generic clamping, or wheel
// scrolling the list away from the selection would snap right back
// (the same bug the editor's cursorMoved flag guards against).
func (a *App) gitPanelEnsureSelectedVisible() {
	_, _, _, ph := a.gitPanelRect()
	visible := ph - 1
	if a.gitPanel.selected < a.gitPanel.listScroll {
		a.gitPanel.listScroll = a.gitPanel.selected
	}
	if visible > 0 && a.gitPanel.selected >= a.gitPanel.listScroll+visible {
		a.gitPanel.listScroll = a.gitPanel.selected - visible + 1
	}
}

// -----------------------------------------------------------------------------
// Mouse
// -----------------------------------------------------------------------------

// gitPanelPress routes an initial left press inside the panel and
// reports the drag it started, if any, as a dragMode string the caller
// hands straight to a.dragMode ("" for none). The header rule outside
// the ✕ is the height handle ("gitpanel"); the list/diff divider column
// on any body row is the width handle ("gitlistdiv"); the ✕ collapses;
// everything else is a plain click.
func (a *App) gitPanelPress(x, y int) (dragMode string) {
	_, py, _, _ := a.gitPanelRect()
	if y == py && !a.gitPanelCloseRect().contains(x, y) {
		return "gitpanel"
	}
	if y > py && x == a.gitPanelDividerX() {
		return "gitlistdiv"
	}
	a.gitPanelClick(x, y)
	return ""
}

// gitPanelClick routes a left press inside the panel: the header's ✕
// collapses, a file-list row selects that file and fetches its diff,
// and a double-click on a diff line jumps the editor to that line —
// the panel's whole reason to exist is "spot something, go fix it".
func (a *App) gitPanelClick(x, y int) {
	if a.gitPanelCloseRect().contains(x, y) {
		a.gitPanel.open = false
		return
	}
	px, py, pw, _ := a.gitPanelRect()
	if y == py {
		return // header row outside the ✕ (drag is handled in gitPanelPress)
	}
	if x >= px+a.gitPanelListW(pw) {
		// Diff pane: single clicks are inert, a double-click jumps.
		// Reuses the editor's lastClick record + window so the two
		// double-click gestures feel identical.
		now := time.Now()
		if a.lastClick.x == x && a.lastClick.y == y && now.Sub(a.lastClick.when) < doubleClickMs {
			a.lastClick = clickRecord{}
			a.gitPanelJumpToDiffRow(a.gitPanel.diffScroll + (y - py - 1))
			return
		}
		a.lastClick = clickRecord{x: x, y: y, when: now}
		return
	}
	idx := a.gitPanel.listScroll + (y - py - 1)
	if idx < 0 || idx >= len(a.gitPanel.files) {
		return
	}
	if x < px+gitPanelCheckboxW {
		// Checkbox gutter: toggle staging without moving the selection —
		// ticking boxes down the list shouldn't churn the diff pane.
		a.gitPanelToggleStage(a.gitPanel.files[idx])
		return
	}
	if idx == a.gitPanel.selected {
		return // re-click on the same row: nothing to refetch
	}
	a.gitPanel.selected = idx
	a.requestGitPanelDiff(a.gitPanel.files[idx])
}

// gitPanelToggleStage flips one file's staged-ness: fully staged files
// are unstaged, everything else (unstaged or partial) is staged — so a
// partially-staged file first completes its staging, and the next click
// clears it. The commands run through the shared async helpers; the
// checkbox redraws when the done-event's refresh lands, not
// optimistically — the porcelain snapshot stays the single source of
// truth.
func (a *App) gitPanelToggleStage(f gitPanelFile) {
	if gitPanelStageState(f.Code) == stageFull {
		a.unstageFilePath(f.Path)
		return
	}
	a.stageFilePath(f.Path)
}

// gitPanelJumpToDiffRow opens the selected file and moves the cursor
// to the buffer line behind diff row idx. Best-effort like everything
// else here: rows with no line mapping (file headers, an empty pane)
// do nothing, and a stale line past EOF clamps to the last line.
func (a *App) gitPanelJumpToDiffRow(idx int) {
	f, ok := a.gitPanelSelectedFile()
	if !ok {
		return
	}
	line, ok := diffTargetLine(a.gitPanel.diffLines, idx)
	if !ok {
		return
	}
	a.openFile(f.Path)
	t := a.activeTabPtr()
	if t == nil || t.Path != f.Path {
		return // open failed (e.g. a deleted file) — openFile flashed why
	}
	if lc := t.Buffer.LineCount(); line >= lc {
		line = lc - 1
	}
	t.MoveCursorTo(editor.Position{Line: line, Col: 0}, false)
}

// diffTargetLine maps a display row of the diff pane to a 0-based line
// in the file as it exists NOW (the "+" side). It walks the unified
// diff, tracking the new-side line counter that hunk headers reset:
//
//	@@ -a,b +c,d @@   → jump target = c (the hunk's first line)
//	'+' and context   → the tracked line; counter advances
//	'-' (old side)    → the boundary line the deletion sits at;
//	                    counter stays (the text no longer exists)
//	file headers      → no target (before any @@, nothing to map)
//
// The synthesized untracked view maps 1:1 — row N is file line N-1.
func diffTargetLine(lines []string, idx int) (int, bool) {
	if idx < 0 || idx >= len(lines) {
		return 0, false
	}
	if lines[0] == gitPanelUntrackedHeader {
		if idx == 0 {
			return 0, false
		}
		return idx - 1, true
	}
	newLine := 0 // 1-based new-side counter; 0 = still in the file header
	for i := 0; i <= idx; i++ {
		ln := lines[i]
		if strings.HasPrefix(ln, "@@") {
			if _, _, c, _, ok := parseHunkHeader(ln); ok {
				newLine = c
				if newLine < 1 {
					newLine = 1 // "+0,0": deletion at the very top
				}
			}
			if i == idx {
				return newLine - 1, newLine > 0
			}
			continue
		}
		if newLine == 0 {
			if i == idx {
				return 0, false // diff --git / index / --- / +++ region
			}
			continue
		}
		if i == idx {
			return newLine - 1, true
		}
		// Deleted lines live on the old side only — every other row
		// occupies a real line in the current file, so advance.
		if !strings.HasPrefix(ln, "-") {
			newLine++
		}
	}
	return 0, false
}

// gitPanelScroll wheels whichever half of the panel the cursor is
// over: file list on the left, diff text on the right.
func (a *App) gitPanelScroll(x, _, delta int) {
	px, _, pw, _ := a.gitPanelRect()
	if x < px+a.gitPanelListW(pw) {
		a.gitPanel.listScroll += delta
	} else {
		a.gitPanel.diffScroll += delta
	}
	a.gitPanelClampScrolls()
}

// -----------------------------------------------------------------------------
// Drawing
// -----------------------------------------------------------------------------

// drawGitPanel paints the whole panel: header rule with title, count
// and ✕; the file list column; a vertical divider; the diff text with
// unified-diff coloring.
func (a *App) drawGitPanel() {
	px, py, pw, ph := a.gitPanelRect()
	listW := a.gitPanelListW(pw)
	th := a.theme

	headerSt := tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Subtle)
	titleSt := tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Accent).Bold(true)
	listBG := tcell.StyleDefault.Background(th.SidebarBG)
	diffBG := tcell.StyleDefault.Background(th.BG)

	// The two resize handles idle in Subtle and brighten to Accent while
	// grabbed — the same grab-handle language as the sidebar splitter, so
	// the header rule (height) and the list/diff divider (width) both read
	// as "you can drag me" the moment they're seized.
	ruleSt := headerSt
	if a.dragMode == "gitpanel" {
		ruleSt = tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Accent)
	}
	divSt := headerSt
	if a.dragMode == "gitlistdiv" {
		divSt = tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Accent)
	}

	// Header: a horizontal rule carrying the title, the change count,
	// and the ✕ — it doubles as the visual border against the editor.
	for cx := px; cx < px+pw; cx++ {
		a.screen.SetContent(cx, py, '─', nil, ruleSt)
	}
	title := " Git changes · " + itoa(len(a.gitPanel.files)) + " "
	if len(a.gitPanel.files) == 1 {
		title += "file "
	} else {
		title += "files "
	}
	drawAt(a.screen, px+1, py, title, titleSt)
	closeBtn := a.gitPanelCloseRect()
	drawAt(a.screen, closeBtn.x, closeBtn.y, " ✕ ", titleSt)

	// Body rows.
	for row := 0; row < ph-1; row++ {
		ry := py + 1 + row
		// File list cell fill, then entry text.
		for cx := px; cx < px+listW; cx++ {
			a.screen.SetContent(cx, ry, ' ', nil, listBG)
		}
		a.drawGitPanelListRow(row, px, ry, listW)
		// Divider — the width resize handle, brightened while dragged.
		a.screen.SetContent(px+listW, ry, '│', nil, divSt)
		// Diff cell fill, then diff text.
		for cx := px + listW + 1; cx < px+pw; cx++ {
			a.screen.SetContent(cx, ry, ' ', nil, diffBG)
		}
		a.drawGitPanelDiffRow(row, px+listW+2, ry, pw-listW-3)
	}
}

// drawGitPanelListRow paints one file-list row: status code + relative
// path in the status color, with the selected row on the selection
// background. An empty list shows a single muted "(clean)" row.
func (a *App) drawGitPanelListRow(row, px, ry, listW int) {
	th := a.theme
	idx := a.gitPanel.listScroll + row
	if len(a.gitPanel.files) == 0 {
		if row == 0 {
			drawAt(a.screen, px+1, ry, "(clean)",
				tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Muted))
		}
		return
	}
	if idx < 0 || idx >= len(a.gitPanel.files) {
		return
	}
	f := a.gitPanel.files[idx]
	bg := th.SidebarBG
	if idx == a.gitPanel.selected {
		bg = th.Selection
	}
	st := tcell.StyleDefault.Background(bg).Foreground(gitPanelStatusColor(f.Code, th))
	if idx == a.gitPanel.selected {
		// Repaint the row fill so the selection reads as a block.
		for cx := px; cx < px+listW; cx++ {
			a.screen.SetContent(cx, ry, ' ', nil, st)
		}
	}
	code := strings.TrimSpace(f.Code)
	if len(code) > 2 {
		code = code[:2]
	}
	// Row shape: " [x] M  path" — the checkbox gutter (gitPanelCheckboxW
	// cells) first so every box lines up in a tickable column, then the
	// porcelain code, then the path.
	text := " " + gitPanelCheckbox(gitPanelStageState(f.Code)) + " " + code
	for len(text) < gitPanelCheckboxW+3 {
		text += " "
	}
	text += f.Rel
	if runeLen(text) > listW-1 {
		text = string([]rune(text)[:listW-2]) + "…"
	}
	drawAt(a.screen, px, ry, text, st)
}

// drawGitPanelDiffRow paints one diff line with unified-diff coloring,
// truncating to the pane width — horizontal scroll isn't worth its
// complexity for a review strip (long lines still show their prefix
// and most of their content).
func (a *App) drawGitPanelDiffRow(row, x, ry, w int) {
	if w <= 0 {
		return
	}
	idx := a.gitPanel.diffScroll + row
	if idx < 0 || idx >= len(a.gitPanel.diffLines) {
		return
	}
	line := a.gitPanel.diffLines[idx]
	st := gitPanelDiffStyle(line, a.theme)
	if runeLen(line) > w {
		line = string([]rune(line)[:w-1]) + "…"
	}
	drawAt(a.screen, x, ry, line, st)
}

// gitPanelStatusColor maps a porcelain XY code to the row color, using
// the same green/blue/red convention as the diff gutter. Deletions
// win over everything (a deleted file can't be anything else), adds
// and untracked read as green, the rest as modified blue.
func gitPanelStatusColor(code string, th theme.Theme) tcell.Color {
	switch {
	case strings.Contains(code, "D"):
		return th.GitDeleted
	case strings.Contains(code, "A") || strings.HasPrefix(code, "?"):
		return th.GitAdded
	default:
		return th.GitModified
	}
}

// gitPanelDiffStyle colors one unified-diff line: additions green,
// deletions red, hunk headers accented, file headers muted, context
// lines in normal text.
func gitPanelDiffStyle(line string, th theme.Theme) tcell.Style {
	base := tcell.StyleDefault.Background(th.BG)
	switch {
	case strings.HasPrefix(line, "@@"):
		return base.Foreground(th.AccentSoft)
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"),
		strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"):
		return base.Foreground(th.Muted)
	case strings.HasPrefix(line, "+"):
		return base.Foreground(th.GitAdded)
	case strings.HasPrefix(line, "-"):
		return base.Foreground(th.GitDeleted)
	default:
		return base.Foreground(th.Text)
	}
}
