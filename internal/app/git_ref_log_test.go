// =============================================================================
// File: internal/app/git_ref_log_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseReflogBranches_OrdersDedupesFilters pins the parser's whole
// contract on one crafted reflog: only "checkout: moving from" lines
// count, the destination branch is taken from the FINAL " to ", results
// come out most-recent-first, the same branch appears once (keeping its
// most-recent timestamp), and the current branch is dropped as a no-op.
func TestParseReflogBranches_OrdersDedupesFilters(t *testing.T) {
	lines := strings.Join([]string{
		"checkout: moving from feature to main\tHEAD@{5 minutes ago}",
		"commit: some work\tHEAD@{10 minutes ago}", // not a checkout — ignored
		"checkout: moving from main to feature\tHEAD@{1 hour ago}",
		"checkout: moving from feature to gone\tHEAD@{2 hours ago}", // gone: filtered by exists
		"checkout: moving from gone to a1b2c3d\tHEAD@{3 hours ago}", // detached sha: filtered by exists
		"checkout: moving from main to feature\tHEAD@{4 hours ago}", // feature again — deduped
		"reset: moving to HEAD~1\tHEAD@{5 hours ago}",               // not a checkout — ignored
		"malformed line without a tab or marker",                    // ignored
	}, "\n")
	exists := map[string]bool{"main": true, "feature": true}

	got := parseReflogBranches([]byte(lines), "", exists)
	want := []recentBranch{
		{name: "main", when: "5 minutes ago"},
		{name: "feature", when: "1 hour ago"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Excluding the current branch drops main, leaving just feature.
	if cur := parseReflogBranches([]byte(lines), "main", exists); len(cur) != 1 || cur[0].name != "feature" {
		t.Errorf("with current=main got %+v, want just feature", cur)
	}

	// A nil exists map disables the existence filter, so the deleted
	// branch and the detached-HEAD sha both survive.
	all := parseReflogBranches([]byte(lines), "", nil)
	var names []string
	for _, rb := range all {
		names = append(names, rb.name)
	}
	if strings.Join(names, ",") != "main,feature,gone,a1b2c3d" {
		t.Errorf("with nil exists got %v, want main,feature,gone,a1b2c3d", names)
	}
}

// TestParseReflogBranches_LastToWins pins that the destination is split
// on the LAST " to " — a branch literally named "to" must still be read
// as the checked-out branch, not mistaken for the separator.
func TestParseReflogBranches_LastToWins(t *testing.T) {
	got := parseReflogBranches([]byte("checkout: moving from main to to\tHEAD@{1 second ago}\n"), "", nil)
	if len(got) != 1 || got[0].name != "to" {
		t.Fatalf("got %+v, want a single branch named \"to\"", got)
	}
}

// TestReflogWhen pins the relative-time extraction from a reflog
// selector, including the degenerate forms that must yield "" so the
// caller simply omits the timestamp instead of showing braces.
func TestReflogWhen(t *testing.T) {
	cases := map[string]string{
		"HEAD@{5 minutes ago}": "5 minutes ago",
		"HEAD@{0}":             "0",
		"stash@{2}":            "2",
		"HEAD":                 "", // no braces
		"HEAD@{}":              "", // empty braces
		"":                     "",
	}
	for in, want := range cases {
		if got := reflogWhen(in); got != want {
			t.Errorf("reflogWhen(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadRecentBranches_Integration drives the real read side against a
// repo with a genuine checkout history: the traversed branches come back
// most-recent-first, the current branch is excluded, and a branch
// deleted after being visited drops out because it no longer exists.
func TestLoadRecentBranches_Integration(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	// Traverse main → feature → main → topic → main, so the reflog holds
	// visits to both feature and topic, topic most recently.
	gitRun(t, repo, "checkout", "-q", "-b", "feature")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "checkout", "-q", "-b", "topic")
	gitRun(t, repo, "checkout", "-q", "main")

	got := loadRecentBranches(repo, "main")
	if len(got) != 2 || got[0].name != "topic" || got[1].name != "feature" {
		t.Fatalf("recent branches = %+v, want [topic feature]", got)
	}
	for _, rb := range got {
		if rb.when == "" {
			t.Errorf("branch %q has no relative time", rb.name)
		}
	}

	// Deleting feature removes it from the switch targets even though it
	// still sits in the reflog.
	gitRun(t, repo, "branch", "-D", "feature")
	if after := loadRecentBranches(repo, "main"); len(after) != 1 || after[0].name != "topic" {
		t.Fatalf("after deleting feature = %+v, want just topic", after)
	}

	// Best-effort contract: non-repos and an empty root yield nil.
	if b := loadRecentBranches(t.TempDir(), ""); b != nil {
		t.Errorf("non-repo recent branches = %+v, want nil", b)
	}
	if b := loadRecentBranches("", ""); b != nil {
		t.Errorf("empty-root recent branches = %+v, want nil", b)
	}
}

// TestMenuGitRecentBranches_PickerAndSwitch drives the menu action end to
// end: the traversed branches open in the picker under their own title,
// most-recent-first, and choosing the top row actually moves HEAD via the
// async switch path.
func TestMenuGitRecentBranches_PickerAndSwitch(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "checkout", "-q", "-b", "feature")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "checkout", "-q", "-b", "topic")
	gitRun(t, repo, "checkout", "-q", "main")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuGitRecentBranches()

	m := paletteOf(a)
	if m == nil {
		t.Fatal("recent branches should open the picker")
	}
	if m.title != "Recent branches" {
		t.Fatalf("picker title = %q", m.title)
	}
	if m.sourced {
		t.Fatal("picker items must not be source-backed")
	}
	if len(m.items) != 2 ||
		!strings.HasPrefix(m.items[0].label, "topic") ||
		!strings.HasPrefix(m.items[1].label, "feature") {
		t.Fatalf("picker items = %+v, want [topic feature] most-recent-first", m.items)
	}

	// Empty query keeps recency order, so the top row is topic.
	m.runSelected(a)
	pumpAppEvents(t, a, func() bool {
		return strings.Contains(a.statusMsg, "Switch to topic — done")
	})
	if br := loadGitBranch(repo); br != "topic" {
		t.Fatalf("branch after switch = %q, want topic", br)
	}
	if a.gitBranch != "topic" {
		t.Fatalf("status-bar branch = %q, want topic (refresh missing?)", a.gitBranch)
	}
}

// TestMenuGitRecentBranches_NoneFlashes pins the empty guard: a repo with
// no branch traversal in the reflog flashes instead of opening a picker
// with zero rows.
func TestMenuGitRecentBranches_NoneFlashes(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	a := newTestApp(t, repo)
	a.refreshGitStatus()
	a.menuGitRecentBranches()

	if a.modal != nil {
		t.Fatalf("modal = %T, want none", a.modal)
	}
	if !strings.Contains(a.statusMsg, "No recent branches") {
		t.Fatalf("statusMsg = %q, want the no-branches flash", a.statusMsg)
	}
}
