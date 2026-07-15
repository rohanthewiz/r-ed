// =============================================================================
// File: internal/app/execmarks.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-15
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Executable marks: the file tree appends an ls -F style '*' to
// executable regular files. This file holds the ≡ menu toggle that
// shows/hides that marker and persists the choice. The rendering and
// the IsExec detection live in internal/filetree — here we only flip
// tree.ExecMarks (the render-time gate) and mirror it into config.json,
// the same pattern the auto-save and terminal-dock toggles follow.
package app

import "github.com/rohanthewiz/r-ed/internal/userconfig"

// menuToggleExecMarks flips the executable '*' marker on/off from the ≡
// menu and persists the choice to the user config so it sticks across
// sessions. The IsExec bit is recomputed on every tree reload
// regardless, so the flip re-renders instantly — no refresh needed.
func (a *App) menuToggleExecMarks() {
	a.closeMenu()
	if a.tree == nil {
		return
	}
	a.tree.ExecMarks = !a.tree.ExecMarks
	if a.tree.ExecMarks {
		a.flash("Executable marks on")
	} else {
		a.flash("Executable marks off")
	}
	if err := userconfig.SaveExecMarks(userconfig.DefaultPath(), a.tree.ExecMarks); err != nil {
		a.flash("config: " + err.Error())
	}
}

// execMarksToggleLabel is the dynamic menu label for the marker row —
// the same toggle-in-place pattern as the sidebar row, so the menu
// always names the action it will perform, not the current state. A nil
// tree (defensive; shouldn't happen in normal operation) reads as
// "marks off" so the row still offers to show them.
func (a *App) execMarksToggleLabel() string {
	if a.tree != nil && a.tree.ExecMarks {
		return "Hide executable marks"
	}
	return "Show executable marks"
}
