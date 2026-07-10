// =============================================================================
// File: internal/app/gitcmd.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// gitcmd.go implements the write-side git commands — stage / unstage a
// file, commit what's staged, stash / pop, switch branches — reachable
// from the ≡ menu's Git group and therefore from the command palette
// (which lists enabled menu rows automatically). The git panel's
// checkboxes reuse the stage/unstage helpers here.
//
// The moving parts follow the two house patterns they build on:
//
//   - Best-effort git (gitstatus.go's rule) for the read side: branch
//     listing degrades to an empty slice on any failure. Write commands
//     are different — the user explicitly asked for them, so a failure
//     is surfaced in the info modal instead of being swallowed.
//   - Custom tcell events for goroutine → main-loop messaging:
//     runGitCmd shells out on a goroutine and posts a gitCmdDoneEvent;
//     only the main loop touches App state. Commits can run hooks that
//     take seconds — running them inline would freeze the UI.

package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitCmdDoneEvent carries one finished git command from the background
// goroutine to the main loop: the human-readable label for the status
// flash, the exit error (nil on success), and the combined output for
// the failure modal.
type gitCmdDoneEvent struct {
	when   time.Time
	label  string
	err    error
	output []byte
}

// When satisfies the tcell.Event interface.
func (e *gitCmdDoneEvent) When() time.Time { return e.when }

// runGitCmd runs one git subcommand against the project root on a
// goroutine and posts the outcome. Fire-and-forget like every other
// background job here; a dropped event (screen shutting down) just
// means the result is never reported.
func (a *App) runGitCmd(label string, args ...string) {
	if a.screen == nil || a.rootDir == "" {
		return
	}
	scr := a.screen
	root := a.rootDir
	go func() {
		cmdArgs := append([]string{"-C", root}, args...)
		out, err := exec.Command("git", cmdArgs...).CombinedOutput()
		_ = scr.PostEvent(&gitCmdDoneEvent{when: time.Now(), label: label, err: err, output: out})
	}()
}

// handleGitCmdDone surfaces the result of an async git command. Runs
// on the main loop only. Success flashes a confirmation and re-syncs
// everything git touches — the tree's dirty colors, the branch label,
// open-tab reconciliation after a branch switch rewrites files, and
// the diff gutters. Failure opens the info modal with git's own
// output; git error messages ("nothing to commit", merge conflicts on
// switch) are exactly what the user needs to read.
func (a *App) handleGitCmdDone(e *gitCmdDoneEvent) {
	if e.err != nil {
		a.openInfo("Git failed: "+e.label, errorBodyLines(e.err, e.output, "… (truncated)"))
		return
	}
	a.flash(e.label + " — done")
	a.refreshTreeNow()
}

// -----------------------------------------------------------------------------
// Menu predicates
// -----------------------------------------------------------------------------

// hasGitRepo is the menu predicate for git commands that only need a
// repository (Switch branch). Reads the flag refreshGitStatus stamps
// rather than forking git — enabled() runs on every menu draw.
func (a *App) hasGitRepo() bool {
	return a.gitIsRepo
}

// hasStageableFile gates "Stage file": the active tab must be a real
// file with uncommitted changes per the last git-status snapshot.
// Staging an untouched file is a silent no-op in git, so offering the
// row would just teach the user that Enter sometimes does nothing.
func (a *App) hasStageableFile() bool {
	if !a.gitIsRepo || a.tree == nil {
		return false
	}
	t := a.activeTabPtr()
	return t != nil && t.Path != "" && a.tree.DirtyFiles[t.Path]
}

// hasUnstageableFile gates "Unstage file": the active tab must carry a
// staged change per the last git-status snapshot — mirror image of
// hasStageableFile, reading the per-file staged set instead of the
// dirty set.
func (a *App) hasUnstageableFile() bool {
	if !a.gitIsRepo {
		return false
	}
	t := a.activeTabPtr()
	return t != nil && t.Path != "" && a.gitStagedFiles[t.Path]
}

// hasGitStaged gates "Commit staged": something must actually sit in
// the index. The flag refreshes on the 10s tick and after every git
// command, so a stage performed here enables the commit row as soon
// as its done-event lands.
func (a *App) hasGitStaged() bool {
	return a.gitIsRepo && a.gitHasStaged
}

// hasGitChanges gates "Stash changes": stashing with a clean tree only
// yields git's "No local changes to save" — dim the row instead of
// offering the error. DirtyFiles covers staged, unstaged, and untracked
// entries alike, exactly the set `stash push -u` would take.
func (a *App) hasGitChanges() bool {
	return a.gitIsRepo && a.tree != nil && len(a.tree.DirtyFiles) > 0
}

// hasGitStash gates "Pop stash": refs/stash must exist. Field read for
// the same fork-free reason as the other predicates.
func (a *App) hasGitStash() bool {
	return a.gitIsRepo && a.gitHasStash
}

// -----------------------------------------------------------------------------
// Menu actions
// -----------------------------------------------------------------------------

// stageFilePath stages one file (`git add`), shared by the menu row and
// the git panel's checkboxes. The flash label carries the basename so
// "Stage main.go — done" confirms which file landed in the index. Also
// the right verb for a deleted work-tree file: modern git add stages
// the removal.
func (a *App) stageFilePath(path string) {
	a.runGitCmd("Stage "+filepath.Base(path), "add", "--", path)
}

// unstageFilePath removes one file's staged changes from the index.
// `git reset -- <path>` rather than `restore --staged` because reset
// also works on an unborn branch (it resets against the empty tree),
// where restore fails with "could not resolve HEAD" — and a first-ever
// commit is exactly when someone plays with staging.
func (a *App) unstageFilePath(path string) {
	a.runGitCmd("Unstage "+filepath.Base(path), "reset", "-q", "--", path)
}

// menuGitStageFile stages the active tab's file.
func (a *App) menuGitStageFile() {
	a.closeMenu()
	t := a.activeTabPtr()
	if t == nil || t.Path == "" {
		return
	}
	a.stageFilePath(t.Path)
}

// menuGitUnstageFile pulls the active tab's file back out of the index —
// the undo for a mistaken Stage file, without touching the work tree.
func (a *App) menuGitUnstageFile() {
	a.closeMenu()
	t := a.activeTabPtr()
	if t == nil || t.Path == "" {
		return
	}
	a.unstageFilePath(t.Path)
}

// menuGitStash shelves every local change (`stash push -u`). Untracked
// files are included because the user's intent is "give me a clean
// tree" — leaving new files behind makes the tree look only half
// stashed. No message prompt: git's auto label (branch + subject)
// identifies the entry, and the prompt modal rejects empty submits, so
// prompting would force a message git itself treats as optional.
func (a *App) menuGitStash() {
	a.closeMenu()
	a.runGitCmd("Stash changes", "stash", "push", "-u")
}

// menuGitStashPop re-applies the most recent stash entry and drops it on
// success (`stash pop`). Conflicts surface through the failure modal
// with git's own explanation, and git keeps the entry in that case —
// nothing is lost.
func (a *App) menuGitStashPop() {
	a.closeMenu()
	a.runGitCmd("Pop stash", "stash", "pop")
}

// menuGitCommit prompts for a commit message, then commits whatever is
// staged. Deliberately not `commit -a` — the stage-then-commit split
// keeps the editor's model identical to git's, and the "Commit staged"
// label promises exactly what happens. The prompt rejects an empty
// submit (promptModal's contract), so no empty-message guard is needed
// in the callback.
func (a *App) menuGitCommit() {
	a.closeMenu()
	a.openPrompt("Commit staged changes", "message", "", func(app *App, msg string) {
		app.runGitCmd("Commit", "commit", "-m", msg)
	})
}

// menuGitSwitchBranch lists local branches in a fuzzy picker and
// switches to the chosen one. The current branch is excluded — offering
// a no-op row only clutters the list. The branch listing runs inline
// (one fork, same budget as refreshGitStatus) because the picker needs
// the list before it can open.
func (a *App) menuGitSwitchBranch() {
	a.closeMenu()
	items := make([]paletteItem, 0, 8)
	for _, br := range loadGitBranches(a.rootDir) {
		if br == a.gitBranch {
			continue
		}
		br := br // capture per-iteration for the closure
		items = append(items, paletteItem{
			label: br,
			run:   func(app *App) { app.runGitCmd("Switch to "+br, "switch", br) },
		})
	}
	if len(items) == 0 {
		a.flash("No other branches")
		return
	}
	a.openPicker("Switch branch", items)
}

// loadGitBranches returns the repo's local branch names, best-effort:
// non-repos, missing git, and any other failure yield nil and the
// caller shows "No other branches". Detached-HEAD placeholder lines
// ("(HEAD detached at …)") are filtered — they name a state, not a
// branch you can switch to.
func loadGitBranches(rootDir string) []string {
	if rootDir == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", rootDir, "branch", "--format=%(refname:short)").Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "(") {
			continue
		}
		branches = append(branches, ln)
	}
	return branches
}
