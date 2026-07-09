// =============================================================================
// File: internal/app/gitdiff.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// gitdiff.go implements the diff gutter: per-open-tab `git diff -U0`
// runs parsed into added / modified / deleted hunks, rendered as
// colored marks in the editor's gutter mark column via the decoration
// layer, plus next/previous-hunk navigation.
//
// The moving parts, following the two house patterns they build on:
//
//   - Best-effort git (gitstatus.go's rule): any git failure — not a
//     repo, no git on PATH, file untracked — yields no hunks and the
//     editor renders normally. Never block, never error at the user.
//   - Custom tcell events for goroutine → main-loop messaging:
//     requestFileDiff shells out on a goroutine and posts a
//     gitDiffEvent; only the main loop touches App.fileDiffs.
//
// Diffs refresh on file open, after every save, and on the existing
// 10-second tick (refreshTreeNow), so external changes (a git commit
// in another pane clearing the gutter) appear within a tick.

package app

import (
	"bytes"
	"os/exec"
	"time"

	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/theme"
)

// diffKind classifies one hunk of working-tree change.
type diffKind uint8

const (
	diffAdded    diffKind = iota // lines that don't exist upstream
	diffModified                 // lines changed in place
	diffDeleted                  // lines removed — marked on the boundary line
)

// diffHunk is one contiguous changed region in the working file.
// Start/End are 0-based line indexes, inclusive, in the file as it is
// NOW (the "+" side of the diff) — which is the coordinate space the
// editor renders, so no translation happens at draw time. A deleted
// hunk has Start == End pointing at the line the content was removed
// after; there's no cell for text that no longer exists, so the
// boundary line carries the mark.
type diffHunk struct {
	Start, End int
	Kind       diffKind
}

// parseUnifiedDiff extracts hunks from `git diff -U0` output. With
// zero context lines every @@ header describes exactly one changed
// region, so we only read the headers and ignore the +/- body lines:
//
//	@@ -oldStart[,oldCount] +newStart[,newCount] @@
//
//	oldCount == 0 → pure insertion  → added,   newStart..+newCount-1
//	newCount == 0 → pure deletion   → deleted, boundary line newStart-1
//	both > 0      → replacement     → modified, newStart..+newCount-1
//
// Counts default to 1 when the ",count" suffix is omitted (git's
// shorthand for single-line hunks). Malformed lines are skipped —
// best-effort all the way down.
func parseUnifiedDiff(out []byte) []diffHunk {
	var hunks []diffHunk
	for _, raw := range bytes.Split(out, []byte{'\n'}) {
		line := string(raw)
		if len(line) < 4 || line[0] != '@' || line[1] != '@' {
			continue
		}
		_, oldCount, newStart, newCount, ok := parseHunkHeader(line)
		if !ok {
			continue
		}
		switch {
		case newCount == 0:
			// git reports the 1-based line BEFORE the gap on the new
			// side; 0 means the deletion is at the very top of the file.
			boundary := newStart - 1
			if boundary < 0 {
				boundary = 0
			}
			hunks = append(hunks, diffHunk{Start: boundary, End: boundary, Kind: diffDeleted})
		case oldCount == 0:
			hunks = append(hunks, diffHunk{Start: newStart - 1, End: newStart - 1 + newCount - 1, Kind: diffAdded})
		default:
			hunks = append(hunks, diffHunk{Start: newStart - 1, End: newStart - 1 + newCount - 1, Kind: diffModified})
		}
	}
	return hunks
}

// parseHunkHeader pulls the four numbers out of one "@@ -a,b +c,d @@"
// line. Hand-rolled rather than regexp because this runs for every
// hunk of every open tab on every tick — and the format is rigid
// enough that a scanner is both faster and no harder to read.
func parseHunkHeader(line string) (oldStart, oldCount, newStart, newCount int, ok bool) {
	i := 2 // skip "@@"
	skipSpaces := func() {
		for i < len(line) && line[i] == ' ' {
			i++
		}
	}
	readInt := func() (int, bool) {
		start := i
		n := 0
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			n = n*10 + int(line[i]-'0')
			i++
		}
		return n, i > start
	}
	readPair := func(sigil byte) (int, int, bool) {
		skipSpaces()
		if i >= len(line) || line[i] != sigil {
			return 0, 0, false
		}
		i++
		start, okStart := readInt()
		if !okStart {
			return 0, 0, false
		}
		count := 1 // git omits ",1"
		if i < len(line) && line[i] == ',' {
			i++
			var okCount bool
			count, okCount = readInt()
			if !okCount {
				return 0, 0, false
			}
		}
		return start, count, true
	}
	oldStart, oldCount, okOld := readPair('-')
	newStart, newCount, okNew := readPair('+')
	if !okOld || !okNew {
		return 0, 0, 0, 0, false
	}
	return oldStart, oldCount, newStart, newCount, true
}

// loadFileDiff runs `git diff -U0` for one file and returns its hunks.
// nil means "no changes or no answer" — untracked files, non-repos,
// and git errors all land there, and the gutter simply stays blank.
func loadFileDiff(rootDir, path string) []diffHunk {
	if rootDir == "" || path == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", rootDir, "diff", "-U0", "--no-color", "--", path).Output()
	if err != nil {
		return nil
	}
	return parseUnifiedDiff(out)
}

// gitDiffEvent carries one file's freshly-computed hunks from the
// background goroutine to the main loop — the same messaging pattern
// every other background job here uses (never mutate App off-loop).
type gitDiffEvent struct {
	when  time.Time
	path  string
	hunks []diffHunk
}

// When satisfies the tcell.Event interface.
func (e *gitDiffEvent) When() time.Time { return e.when }

// requestFileDiff computes path's diff on a goroutine and posts the
// result. Fire-and-forget: a dropped event (screen shutting down)
// just means the gutter waits for the next tick.
func (a *App) requestFileDiff(path string) {
	if path == "" || a.screen == nil {
		return
	}
	scr := a.screen
	root := a.rootDir
	go func() {
		hunks := loadFileDiff(root, path)
		_ = scr.PostEvent(&gitDiffEvent{when: time.Now(), path: path, hunks: hunks})
	}()
}

// requestOpenTabDiffs refreshes the diff of every open file tab.
// Called from the 10-second tick so gutters track external git
// activity (commits, checkouts in another pane) without a save.
func (a *App) requestOpenTabDiffs() {
	for _, t := range a.tabs {
		if t.Path != "" && !t.IsImage() {
			a.requestFileDiff(t.Path)
		}
	}
}

// handleGitDiff stores a completed diff, keyed by path. Runs on the
// main loop only. The map is lazily created so tests that assemble an
// App by hand don't need extra setup.
func (a *App) handleGitDiff(e *gitDiffEvent) {
	if a.fileDiffs == nil {
		a.fileDiffs = map[string][]diffHunk{}
	}
	if len(e.hunks) == 0 {
		delete(a.fileDiffs, e.path)
		return
	}
	a.fileDiffs[e.path] = e.hunks
}

// -----------------------------------------------------------------------------
// Decoration source — the render-side half
// -----------------------------------------------------------------------------

// Gutter mark glyphs. A vertical bar for lines that exist (added /
// modified) and a low underscore-bar for the deletion boundary — "the
// missing lines were below here".
const (
	glyphChanged = '▎'
	glyphDeleted = '▁'
)

// gitDiffSource adapts App.fileDiffs into decoration gutter marks.
// One instance per tab, registered at open; it holds the App pointer
// because diffs are app state (they're keyed by path and outlive tab
// switches), while the decoration layer only knows tabs.
type gitDiffSource struct {
	app *App
}

// Decorations emits one gutter mark per changed line in the visible
// window. No spans — the diff gutter deliberately never recolors code.
func (s gitDiffSource) Decorations(t *editor.Tab, th theme.Theme, firstLine, lastLine int) ([]editor.Span, []editor.GutterMark) {
	hunks := s.app.fileDiffs[t.Path]
	if len(hunks) == 0 {
		return nil, nil
	}
	var marks []editor.GutterMark
	for _, h := range hunks {
		glyph, fg := glyphChanged, th.GitModified
		switch h.Kind {
		case diffAdded:
			fg = th.GitAdded
		case diffDeleted:
			glyph, fg = glyphDeleted, th.GitDeleted
		}
		for line := max(h.Start, firstLine); line <= h.End && line <= lastLine; line++ {
			marks = append(marks, editor.GutterMark{Line: line, Glyph: glyph, FG: fg})
		}
	}
	return nil, marks
}

// -----------------------------------------------------------------------------
// Hunk navigation — menu actions + Esc-h / Esc-H leaders
// -----------------------------------------------------------------------------

// hasDiffHunks is the menu predicate for the hunk-navigation rows:
// enabled only when the active tab has known changes to jump between.
func (a *App) hasDiffHunks() bool {
	t := a.activeTabPtr()
	return t != nil && len(a.fileDiffs[t.Path]) > 0
}

// menuNextHunk jumps the cursor to the start of the next changed
// region after the cursor line, wrapping to the first hunk past the
// end of the file — same wrap contract as find's next-match.
func (a *App) menuNextHunk() {
	a.closeMenu()
	a.jumpHunk(+1)
}

// menuPrevHunk jumps to the previous changed region, wrapping to the
// last hunk when the cursor sits above every change.
func (a *App) menuPrevHunk() {
	a.closeMenu()
	a.jumpHunk(-1)
}

// jumpHunk moves the cursor to the nearest hunk start in direction
// dir (+1 next / -1 previous), wrapping around the file. Hunks are
// already in document order because git emits them that way and the
// parser preserves it. Cursor lands at column 0 of the hunk's first
// line — the change is what the eye should catch, not a column.
func (a *App) jumpHunk(dir int) {
	t := a.activeTabPtr()
	if t == nil {
		return
	}
	hunks := a.fileDiffs[t.Path]
	if len(hunks) == 0 {
		return
	}
	cur := t.Cursor.Line
	target := -1
	if dir > 0 {
		for _, h := range hunks {
			if h.Start > cur {
				target = h.Start
				break
			}
		}
		if target < 0 {
			target = hunks[0].Start // wrap to top
		}
	} else {
		for i := len(hunks) - 1; i >= 0; i-- {
			if hunks[i].Start < cur {
				target = hunks[i].Start
				break
			}
		}
		if target < 0 {
			target = hunks[len(hunks)-1].Start // wrap to bottom
		}
	}
	t.MoveCursorTo(editor.Position{Line: target, Col: 0}, false)
}
