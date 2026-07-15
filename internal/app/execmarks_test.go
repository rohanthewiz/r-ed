// =============================================================================
// File: internal/app/execmarks_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-15
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/r-ed/internal/userconfig"
)

// TestMenuToggleExecMarks_PersistsAndPreservesConfig drives the menu
// toggle end to end: the tree's render gate flips, the choice lands in
// config.json, and hand-set keys the toggle doesn't own (icons) survive
// the rewrite.
func TestMenuToggleExecMarks_PersistsAndPreservesConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	cfgPath := filepath.Join(cfgDir, "r-ed", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"icons":"off"}`+"\n"), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	a := newTestApp(t, t.TempDir())
	if !a.tree.ExecMarks {
		t.Fatal("fixture tree should start with ExecMarks on")
	}

	a.menuToggleExecMarks()

	if a.tree.ExecMarks {
		t.Fatal("toggle should flip the marker off")
	}
	cfg, err := userconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.ExecMarks {
		t.Fatal("persisted config should say execmarks off")
	}
	if cfg.Icons != userconfig.IconsOff {
		t.Fatalf("icons setting lost in rewrite: got %q", cfg.Icons)
	}
}

// TestMenuToggleExecMarks_RoundTrips confirms a second toggle restores
// the marker — the flag isn't a one-way latch.
func TestMenuToggleExecMarks_RoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := newTestApp(t, t.TempDir())

	a.menuToggleExecMarks() // on → off
	if a.tree.ExecMarks {
		t.Fatal("first toggle should turn marker off")
	}
	a.menuToggleExecMarks() // off → on
	if !a.tree.ExecMarks {
		t.Fatal("second toggle should turn marker back on")
	}
}

// TestExecMarksToggleLabel pins the menu row's action-naming rule: the
// label describes what clicking will do, not the current state.
func TestExecMarksToggleLabel(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	a.tree.ExecMarks = true
	if got := a.execMarksToggleLabel(); got != "Hide executable marks" {
		t.Errorf("label with marks on = %q, want %q", got, "Hide executable marks")
	}
	a.tree.ExecMarks = false
	if got := a.execMarksToggleLabel(); got != "Show executable marks" {
		t.Errorf("label with marks off = %q, want %q", got, "Show executable marks")
	}
}

// TestLoadUserConfig_AppliesExecMarks pins the app-level plumbing:
// loadUserConfig must stamp config.json's "execmarks" preference onto
// the tree so the render gate reflects the user's choice at startup.
func TestLoadUserConfig_AppliesExecMarks(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	cfgPath := filepath.Join(cfgDir, "r-ed", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// icons:"off" keeps Resolve from shelling out to font detection.
	if err := os.WriteFile(cfgPath, []byte(`{"icons":"off","execmarks":"off"}`+"\n"), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	a := newTestApp(t, t.TempDir())
	if !a.tree.ExecMarks {
		t.Fatal("fixture tree should start with ExecMarks on")
	}

	a.loadUserConfig()

	if a.tree.ExecMarks {
		t.Fatal("loadUserConfig should apply execmarks:off from config.json")
	}
}
