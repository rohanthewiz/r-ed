// =============================================================================
// File: internal/app/gitcmd_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// pumpAppEvents drains the simulation screen's event queue through
// handleEvent until cond holds — the same pump pattern the gitdiff and
// format async tests use, pulled into a helper because every e2e test
// in this file needs it. The poll runs on a goroutine so a lost event
// fails the test at the deadline instead of hanging it.
func pumpAppEvents(t *testing.T, a *App, cond func() bool) {
	t.Helper()
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
	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case ev := <-events:
			a.handleEvent(ev)
		case <-deadline:
			t.Fatal("condition not met within 3s")
		}
	}
}

// gitOut runs a git command in cwd and returns its trimmed stdout —
// the read-side twin of gitRun for assertions that need the output.
func gitOut(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, cwd, err)
	}
	return strings.TrimSpace(string(out))
}

// TestLoadGitBranches_ListsLocalBranches verifies the happy path — a
// repo with two branches reports both — and the best-effort contract:
// non-repos and empty roots yield nil rather than an error.
func TestLoadGitBranches_ListsLocalBranches(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "branch", "feature")

	got := loadGitBranches(repo)
	want := map[string]bool{"main": true, "feature": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("branches = %v, want main + feature", got)
	}

	if b := loadGitBranches(t.TempDir()); b != nil {
		t.Fatalf("non-repo branches = %v, want nil", b)
	}
	if b := loadGitBranches(""); b != nil {
		t.Fatalf("empty-root branches = %v, want nil", b)
	}
}

// TestHandleGitCmdDone_Success pins the success path: a done-event
// with no error flashes "<label> — done" and never opens a modal.
func TestHandleGitCmdDone_Success(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.handleGitCmdDone(&gitCmdDoneEvent{when: time.Now(), label: "Stage f.txt"})
	if !strings.Contains(a.statusMsg, "Stage f.txt — done") {
		t.Fatalf("statusMsg = %q, want stage confirmation", a.statusMsg)
	}
	if a.modal != nil {
		t.Fatal("success must not open a modal")
	}
}

// TestHandleGitCmdDone_FailureOpensInfoModal pins the failure path:
// git's own output lands in the info modal, because messages like
// "nothing to commit" are the actual answer the user needs.
func TestHandleGitCmdDone_FailureOpensInfoModal(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.handleGitCmdDone(&gitCmdDoneEvent{
		when:   time.Now(),
		label:  "Commit",
		err:    errors.New("exit status 1"),
		output: []byte("nothing to commit, working tree clean"),
	})
	m, ok := a.modal.(*confirmModal)
	if !ok || !m.info {
		t.Fatalf("modal = %T, want info confirmModal", a.modal)
	}
	if m.title != "Git failed: Commit" {
		t.Fatalf("title = %q", m.title)
	}
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "nothing to commit") {
		t.Fatalf("modal body missing git output: %q", joined)
	}
}

// TestGitPredicates_NonRepo pins that all three git-command rows stay
// disabled outside a repository, even with a file tab open.
func TestGitPredicates_NonRepo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.refreshGitStatus()
	a.openFile(target)

	if a.hasGitRepo() || a.hasStageableFile() || a.hasGitStaged() {
		t.Fatal("git command predicates must all be false outside a repo")
	}
}

// TestMenuGitStageFile_AsyncRoundTrip drives the full production
// pipeline: a modified tracked file enables the Stage row, staging
// runs `git add` on a goroutine, the done-event flashes and re-syncs
// git status — which flips hasGitStaged on so Commit becomes offered.
func TestMenuGitStageFile_AsyncRoundTrip(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	file := filepath.Join(repo, "f.txt")
	if err := os.WriteFile(file, []byte("one\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	if err := os.WriteFile(file, []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.openFile(file)
	if !a.hasStageableFile() {
		t.Fatal("modified tracked file should be stageable")
	}
	if a.hasGitStaged() {
		t.Fatal("nothing staged yet — commit row must be disabled")
	}

	a.menuGitStageFile()
	pumpAppEvents(t, a, func() bool {
		return strings.Contains(a.statusMsg, "Stage f.txt — done")
	})

	if staged := gitOut(t, repo, "diff", "--cached", "--name-only"); staged != "f.txt" {
		t.Fatalf("staged files = %q, want f.txt", staged)
	}
	if !a.hasGitStaged() {
		t.Fatal("done-event refresh should have flipped hasGitStaged on")
	}
}

// TestMenuGitCommit_PromptThenCommit walks the whole commit flow: the
// menu action opens the message prompt, Enter fires the async commit,
// and the repo's HEAD ends up carrying the typed message.
func TestMenuGitCommit_PromptThenCommit(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	if !a.hasGitStaged() {
		t.Fatal("staged file should enable the commit row")
	}

	a.menuGitCommit()
	m, ok := a.modal.(*promptModal)
	if !ok {
		t.Fatalf("modal = %T, want the commit-message prompt", a.modal)
	}
	for _, r := range "add f" {
		m.handleKey(a, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	m.handleKey(a, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	pumpAppEvents(t, a, func() bool {
		return strings.Contains(a.statusMsg, "Commit — done")
	})
	if msg := gitOut(t, repo, "log", "-1", "--format=%s"); msg != "add f" {
		t.Fatalf("HEAD message = %q, want %q", msg, "add f")
	}
	if a.hasGitStaged() {
		t.Fatal("index should be empty again after the commit")
	}
}

// TestMenuGitSwitchBranch_PickerExcludesCurrent verifies the picker
// contents — every local branch except the one we're on, under its own
// title, immune to source re-collection — and that choosing an entry
// actually moves HEAD.
func TestMenuGitSwitchBranch_PickerExcludesCurrent(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "branch", "feature")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuGitSwitchBranch()

	m := paletteOf(a)
	if m == nil {
		t.Fatal("switch branch should open the picker")
	}
	if m.title != "Switch branch" {
		t.Fatalf("picker title = %q", m.title)
	}
	if m.sourced {
		t.Fatal("picker items must not be source-backed")
	}
	if len(m.items) != 1 || m.items[0].label != "feature" {
		t.Fatalf("picker items = %+v, want just 'feature'", m.items)
	}

	m.runSelected(a)
	pumpAppEvents(t, a, func() bool {
		return strings.Contains(a.statusMsg, "Switch to feature — done")
	})
	if br := loadGitBranch(repo); br != "feature" {
		t.Fatalf("branch after switch = %q, want feature", br)
	}
	if a.gitBranch != "feature" {
		t.Fatalf("status-bar branch = %q, want feature (refresh missing?)", a.gitBranch)
	}
}

// TestMenuGitSwitchBranch_NoOtherBranches pins the empty-picker guard:
// with nowhere to switch to, the action flashes instead of opening a
// modal with zero rows.
func TestMenuGitSwitchBranch_NoOtherBranches(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuGitSwitchBranch()

	if a.modal != nil {
		t.Fatalf("modal = %T, want none", a.modal)
	}
	if !strings.Contains(a.statusMsg, "No other branches") {
		t.Fatalf("statusMsg = %q, want the no-branches flash", a.statusMsg)
	}
}
