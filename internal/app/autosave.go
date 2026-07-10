// =============================================================================
// File: internal/app/autosave.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-09
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Auto-save: dirty buffers are written to disk after the user pauses
// typing for autoSaveDelay. The mechanism mirrors the LSP didChange
// debounce — the only piece that leaves the main loop is a
// time.AfterFunc that posts an autoSaveEvent back onto the tcell
// queue, so all tab state is still mutated from event dispatch only:
//
//	edit → EditRev bump → autoSaveAfterEvent re-arms timer
//	                       └─ idle autoSaveDelay ─► autoSaveEvent → save dirty tabs
//
// Design decisions worth knowing before touching this:
//
//   - Auto-saves are SILENT. No "Saved main.go" flash — with a 2s
//     idle trigger the status bar would flicker constantly. The
//     dirty dot disappearing from the tab is the feedback.
//   - Auto-saves still run format-on-save, but in quiet mode: the
//     builtin goimports/gofmt pass keeps working (that's the point
//     of having both features), while trust prompts, install offers,
//     and error flashes are suppressed — a modal popping open or an
//     error flashing because the code is mid-thought would make the
//     feature feel hostile.
//   - A tab whose file changed on disk after we loaded it is skipped:
//     blindly writing would clobber the external edit before the
//     reconcile tick had a chance to warn. Explicit Save remains the
//     "I know, overwrite it" path. Same for DiskGone tabs — auto-save
//     silently resurrecting a file someone just deleted is surprising.
//   - The toggle lives in the ≡ menu (house rule: every action is
//     reachable there) and persists to ~/.config/r-ed/config.json so
//     it survives restarts.
package app

import (
	"os"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/userconfig"
)

// autoSaveDelay is how long the user must be idle (no buffer
// mutations anywhere) before dirty tabs are written out. Two seconds
// matches the "I finished a statement and paused" rhythm — short
// enough that work is always on disk, long enough that we're not
// saving on every keystroke boundary.
const autoSaveDelay = 2 * time.Second

// autoSaveEvent is posted by the debounce timer when the idle window
// elapses. Carries no payload — the handler re-derives which tabs
// need saving from live state, so a stale event (timer fired just as
// the user resumed typing) degrades to a cheap no-op or a re-arm.
type autoSaveEvent struct {
	when time.Time
}

// When satisfies the tcell.Event interface.
func (e *autoSaveEvent) When() time.Time { return e.when }

// autoSaveAfterEvent runs after every event dispatch (same slot as
// lspAfterEvent) and re-arms the debounce timer whenever any buffer
// mutated since the last check. The EditRev sum is the cheapest
// possible "did anything change?" signature: every mutation path
// already bumps a tab's EditRev, so summing them detects edits from
// keys, paste, modals, and reloads without hooking each path.
func (a *App) autoSaveAfterEvent() {
	if !a.autoSaveEnabled {
		return
	}
	sig := 0
	dirty := false
	for _, t := range a.tabs {
		sig += t.EditRev
		if autoSavable(t) {
			dirty = true
		}
	}
	if sig == a.autoSaveSig {
		return
	}
	a.autoSaveSig = sig
	if dirty {
		a.armAutoSave()
	}
}

// armAutoSave (re)starts the idle countdown. Restarting on every
// further edit is what makes it a debounce — the save only fires
// after a genuine pause, never mid-burst.
func (a *App) armAutoSave() {
	if a.autoSaveTimer != nil {
		a.autoSaveTimer.Stop()
	}
	scr := a.screen
	a.autoSaveTimer = time.AfterFunc(autoSaveDelay, func() {
		_ = scr.PostEvent(&autoSaveEvent{when: time.Now()})
	})
}

// stopAutoSave cancels any pending idle countdown. Safe to call with
// no timer armed; used by the menu toggle and Close.
func (a *App) stopAutoSave() {
	if a.autoSaveTimer != nil {
		a.autoSaveTimer.Stop()
		a.autoSaveTimer = nil
	}
}

// handleAutoSave is the debounce firing on the main loop: write every
// tab that is still dirty and safe to write. While any modal or the
// action menu is open we defer instead — a save could invalidate the
// very question a modal is asking (the dirty-close dialog's "save or
// discard?" becomes nonsense if auto-save lands mid-decision).
func (a *App) handleAutoSave() {
	if !a.autoSaveEnabled {
		return
	}
	if a.modal != nil || a.menuOpen {
		a.armAutoSave()
		return
	}
	saved := false
	for i, t := range a.tabs {
		if !autoSavable(t) || diskChangedSinceLoad(t) {
			continue
		}
		if a.autoSaveTab(i) {
			saved = true
		}
	}
	// One status refresh for the whole batch — refreshGitStatus forks
	// git, so per-tab calls would multiply that cost for no benefit.
	if saved {
		a.refreshGitStatus()
	}
}

// autoSaveTab writes a single tab to disk and runs the quiet
// follow-ups (diff refresh, LSP didSave, quiet format-on-save).
// Deliberately not routed through saveTabAt: that path flashes
// "Saved …" and may open formatter prompts, both wrong for a
// background save. Write errors DO flash — silently failing to save
// is the one thing an auto-save feature can never do, because the
// user has stopped thinking about saving at all.
func (a *App) autoSaveTab(idx int) bool {
	tab := a.tabs[idx]
	if err := tab.Save(); err != nil {
		a.flash("Auto-save failed: " + err.Error())
		return false
	}
	a.requestFileDiff(tab.Path)
	a.lspDidSave(tab)
	a.runFormatOnSave(idx, true)
	return true
}

// autoSavable reports whether a tab is eligible for background
// saving: it has unsaved edits, a real path to write to, and isn't a
// read-only image view or a buffer whose backing file was deleted
// externally (resurrection should be an explicit choice).
func autoSavable(t *editor.Tab) bool {
	return t.Dirty && t.Path != "" && !t.DiskGone && !t.IsImage()
}

// diskChangedSinceLoad reports whether the file on disk is newer than
// the content this tab was loaded from — i.e. someone else wrote it
// while we hold unsaved edits. Auto-save must not win that race
// silently; the reconcile tick will surface the conflict and the
// user can explicitly Save to overwrite. Stat errors report true
// (skip the save) — when we can't see the file's state, guessing
// "it's fine, overwrite" is the wrong default.
func diskChangedSinceLoad(t *editor.Tab) bool {
	info, err := os.Stat(t.Path)
	if err != nil {
		return true
	}
	return !t.Mtime.IsZero() && info.ModTime().After(t.Mtime)
}

// menuToggleAutoSave flips auto-save on/off from the ≡ menu and
// persists the choice to the user config so it sticks across
// sessions. Turning it ON arms the timer immediately when something
// is already dirty — the user's intent is "start keeping my work
// saved", not "start after my next keystroke".
func (a *App) menuToggleAutoSave() {
	a.closeMenu()
	a.autoSaveEnabled = !a.autoSaveEnabled
	if a.autoSaveEnabled {
		a.flash("Auto-save on")
		for _, t := range a.tabs {
			if autoSavable(t) {
				a.armAutoSave()
				break
			}
		}
	} else {
		a.stopAutoSave()
		a.flash("Auto-save off")
	}
	if err := userconfig.SaveAutoSave(userconfig.DefaultPath(), a.autoSaveEnabled); err != nil {
		a.flash("config: " + err.Error())
	}
}

// autoSaveToggleLabel is the dynamic menu label for the auto-save
// row — same toggle-in-place pattern as the sidebar row, so the menu
// always names the action it will perform, not the current state.
func (a *App) autoSaveToggleLabel() string {
	if a.autoSaveEnabled {
		return "Disable auto-save"
	}
	return "Enable auto-save"
}

// Compile-time check that autoSaveEvent really is a tcell.Event.
var _ tcell.Event = (*autoSaveEvent)(nil)
