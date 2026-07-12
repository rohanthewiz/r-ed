// =============================================================================
// File: internal/app/nav.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// nav.go is the editor's file-navigation history: every jump between
// files — tree click, tab click, finder pick, go-to-definition — records
// where the cursor was, so Go back / Go forward can retrace the trail in
// both directions, browser-style. This generalizes what used to be an
// LSP-only "jump back" stack (see lsp.go's history in git) into a single
// app-wide history that all navigation surfaces share.
//
// Recording happens centrally in openFile (and tabBarClick, which
// bypasses openFile), so new navigation surfaces get history for free.
// The two stacks work like a browser's:
//
//	back stack        fwd stack
//	[a, b, c]  ──back──►  push current, pop c  ──►  [a, b] / [current]
//	any NEW navigation clears the fwd stack (you can't go forward to a
//	branch you abandoned — same rule browsers use).
//
// All mutation happens on the main loop; there is no locking here.

package app

import (
	"os"
	"path/filepath"

	"github.com/rohanthewiz/r-ed/internal/editor"
)

// navStackMax caps each navigation stack. Fifty jumps of history is
// more than anyone retraces; the cap just stops a long session from
// growing the slices forever.
const navStackMax = 50

// navLoc is one entry of the navigation history: a file plus the cursor
// position the user was at when they navigated away.
type navLoc struct {
	path string
	pos  editor.Position
}

// navState holds the back/forward history. The suppress flag is set
// while navBack/navForward themselves drive openFile, so retracing the
// history doesn't also record into it (which would corrupt the trail —
// going back twice would bounce between two entries forever).
type navState struct {
	back     []navLoc
	fwd      []navLoc
	suppress bool
}

// hasNavBack is the menu predicate for Go back.
func (a *App) hasNavBack() bool { return len(a.nav.back) > 0 }

// hasNavForward is the menu predicate for Go forward.
func (a *App) hasNavForward() bool { return len(a.nav.fwd) > 0 }

// currentNavLoc snapshots the active tab as a history entry. Untitled
// tabs (no path yet) report false — history retraces via openFile, and
// there is no path to reopen an unsaved buffer by.
func (a *App) currentNavLoc() (navLoc, bool) {
	t := a.activeTabPtr()
	if t == nil || t.Path == "" {
		return navLoc{}, false
	}
	return navLoc{path: t.Path, pos: t.Cursor}, true
}

// recordNav pushes a departure point onto the back stack and clears the
// forward stack — a fresh navigation abandons any forward trail, same
// as a browser. No-op while suppress is set (the retrace itself must
// not record) and when the entry duplicates the current stack top
// (clicking the same tree row twice shouldn't cost a Go back press).
func (a *App) recordNav(from navLoc) {
	if a.nav.suppress {
		return
	}
	if n := len(a.nav.back); n > 0 && a.nav.back[n-1] == from {
		return
	}
	a.nav.back = appendNavCapped(a.nav.back, from)
	a.nav.fwd = nil
}

// appendNavCapped appends onto a history stack, dropping the oldest
// entries when the stack outgrows navStackMax.
func appendNavCapped(stack []navLoc, loc navLoc) []navLoc {
	stack = append(stack, loc)
	if len(stack) > navStackMax {
		stack = stack[len(stack)-navStackMax:]
	}
	return stack
}

// navBack retraces one step: the current location moves to the forward
// stack and the top of the back stack becomes the active file + cursor.
func (a *App) navBack() {
	n := len(a.nav.back)
	if n == 0 {
		a.flash("Nowhere to go back to")
		return
	}
	loc := a.nav.back[n-1]
	a.nav.back = a.nav.back[:n-1]
	cur, hasCur := a.currentNavLoc()
	if !a.gotoNavLoc(loc) {
		return // entry pointed at a now-unopenable file; drop it
	}
	if hasCur {
		a.nav.fwd = appendNavCapped(a.nav.fwd, cur)
	}
}

// navForward re-advances one step after a Go back: the mirror image of
// navBack, moving the current location onto the back stack. It pushes
// directly rather than via recordNav — recordNav clears the forward
// stack, which is exactly what a retrace must not do.
func (a *App) navForward() {
	n := len(a.nav.fwd)
	if n == 0 {
		a.flash("Nowhere to go forward to")
		return
	}
	loc := a.nav.fwd[n-1]
	a.nav.fwd = a.nav.fwd[:n-1]
	cur, hasCur := a.currentNavLoc()
	if !a.gotoNavLoc(loc) {
		return
	}
	if hasCur {
		a.nav.back = appendNavCapped(a.nav.back, cur)
	}
}

// gotoNavLoc lands on a history entry: open (or switch to) the file and
// restore the cursor. The open runs suppressed so retracing history
// never re-records into it. Returns false when the entry can't be
// landed on, so the caller drops it. The stat guard matters because
// openFile treats a missing path as "create new file" (an empty
// buffer) — without it, Go back would silently resurrect files deleted
// since the entry was recorded. A still-open tab bypasses the guard:
// its buffer exists even when the disk file is gone.
func (a *App) gotoNavLoc(loc navLoc) bool {
	if !a.hasTabForPath(loc.path) {
		if _, err := os.Stat(loc.path); err != nil {
			a.flash("File no longer exists: " + filepath.Base(loc.path))
			return false
		}
	}
	a.nav.suppress = true
	a.openFile(loc.path)
	a.nav.suppress = false
	t := a.activeTabPtr()
	if t == nil || t.Path != loc.path {
		return false
	}
	t.MoveCursorTo(loc.pos, false) // Clamp inside guards a since-shrunk file
	return true
}

// hasTabForPath reports whether some open tab already holds path.
func (a *App) hasTabForPath(path string) bool {
	for _, t := range a.tabs {
		if t.Path == path {
			return true
		}
	}
	return false
}

// menuNavBack is the ≡ menu / Esc-o / Alt+Left entry point for Go back.
func (a *App) menuNavBack() {
	a.closeMenu()
	a.navBack()
}

// menuNavForward is the ≡ menu / Esc-O / Alt+Right entry point for Go
// forward.
func (a *App) menuNavForward() {
	a.closeMenu()
	a.navForward()
}
