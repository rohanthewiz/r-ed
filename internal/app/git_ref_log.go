// =============================================================================
// File: internal/app/git_ref_log.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// git_ref_log.go adds a reflog-backed "Recent branches" picker to the ≡
// menu's Git group. Where "Switch branch" lists every local branch
// alphabetically, this one shows the branches you've actually traversed
// — parsed from HEAD's reflog "checkout: moving from X to Y" entries —
// in most-recently-visited order with a relative timestamp, so jumping
// back to the branch you were just on is one click.
//
// It follows the same two house patterns the rest of git does:
//
//   - Best-effort read side (gitstatus.go's rule): loadRecentBranches
//     degrades to nil on non-repos, missing git, or any parse trouble,
//     and the caller shows "No recent branches" rather than an error.
//   - Picker via openPicker (menuGitSwitchBranch's template): each row
//     is a paletteItem whose run switches to that branch through the
//     async runGitCmd path, so a switch that touches open files
//     reconciles exactly like a menu-driven one.

package app

import (
	"os/exec"
	"strings"
)

// recentBranch is one entry in the traversal history: the branch name
// (a valid switch target) and a human-readable relative time of the most
// recent visit ("5 minutes ago"), empty when git couldn't supply one.
type recentBranch struct {
	name string
	when string
}

// reflogCheckoutMarker is the reflog-subject prefix git writes for a
// branch switch; the text after the final " to " names the branch that
// was checked out. Branch names can't contain spaces, so the separator
// is unambiguous even when the source ref is a bare commit (detached
// HEAD checkouts read "moving from <sha> to <branch>").
const reflogCheckoutMarker = "checkout: moving from "

// menuGitRecentBranches lists the branches traversed per HEAD's reflog
// in a fuzzy picker and switches to the chosen one — the recency-ordered
// companion to menuGitSwitchBranch's alphabetical list. The current
// branch is excluded (a no-op switch), and the listing runs inline (one
// fork, same budget as refreshGitStatus) because the picker needs the
// list before it can open.
func (a *App) menuGitRecentBranches() {
	a.closeMenu()
	recents := loadRecentBranches(a.rootDir, a.gitBranch)
	if len(recents) == 0 {
		a.flash("No recent branches")
		return
	}
	items := make([]paletteItem, 0, len(recents))
	for _, rb := range recents {
		rb := rb // capture per-iteration for the closure
		label := rb.name
		if rb.when != "" {
			label = rb.name + "  ·  " + rb.when
		}
		items = append(items, paletteItem{
			label: label,
			run:   func(app *App) { app.runGitCmd("Switch to "+rb.name, "switch", rb.name) },
		})
	}
	a.openPicker("Recent branches", items)
}

// loadRecentBranches returns the local branches recently checked out,
// most-recent-first, parsed from HEAD's reflog. Best-effort: non-repos,
// missing git, and any other failure yield nil. Branches that no longer
// exist locally (deleted since, or a detached-HEAD commit hash) and the
// current branch are filtered out so every row is a valid switch target.
func loadRecentBranches(rootDir, current string) []recentBranch {
	if rootDir == "" {
		return nil
	}
	// %gs = reflog subject, %gd = shortened selector ("HEAD@{5 minutes
	// ago}" under --date=relative); a tab (%x09) joins them so the parser
	// can split without tripping on spaces inside either field.
	out, err := exec.Command("git", "-C", rootDir, "reflog",
		"--date=relative", "--format=%gs%x09%gd").Output()
	if err != nil {
		return nil
	}
	exists := make(map[string]bool)
	for _, b := range loadGitBranches(rootDir) {
		exists[b] = true
	}
	return parseReflogBranches(out, current, exists)
}

// parseReflogBranches extracts the ordered, de-duplicated list of
// checked-out branches from reflog output formatted as
// "<subject>\t<selector>" per line. Split out from loadRecentBranches so
// the ordering, de-duplication, and filtering can be tested without a
// repo. exists (when non-nil) keeps only branches still present locally;
// current and already-seen branches are always dropped.
func parseReflogBranches(out []byte, current string, exists map[string]bool) []recentBranch {
	var result []recentBranch
	seen := make(map[string]bool)
	for _, ln := range strings.Split(string(out), "\n") {
		subj, sel, ok := strings.Cut(ln, "\t")
		if !ok || !strings.HasPrefix(subj, reflogCheckoutMarker) {
			continue
		}
		rest := subj[len(reflogCheckoutMarker):]
		// "<from> to <to>" — the branch landed on is after the final
		// " to ". LastIndex, not Index, so a source named with a " to "
		// look-alike can't mis-split the destination.
		idx := strings.LastIndex(rest, " to ")
		if idx < 0 {
			continue
		}
		to := strings.TrimSpace(rest[idx+len(" to "):])
		if to == "" || to == current || seen[to] {
			continue
		}
		if exists != nil && !exists[to] {
			continue
		}
		seen[to] = true
		result = append(result, recentBranch{name: to, when: reflogWhen(sel)})
	}
	return result
}

// reflogWhen pulls the relative time out of a reflog selector like
// "HEAD@{5 minutes ago}", returning "5 minutes ago". A selector without
// the {...} braces (or an empty one) yields "" so callers just omit the
// timestamp rather than showing garbage.
func reflogWhen(selector string) string {
	open := strings.IndexByte(selector, '{')
	closeIdx := strings.LastIndexByte(selector, '}')
	if open < 0 || closeIdx <= open+1 {
		return ""
	}
	return selector[open+1 : closeIdx]
}
