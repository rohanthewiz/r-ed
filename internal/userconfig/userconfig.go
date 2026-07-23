// =============================================================================
// File: internal/userconfig/userconfig.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Package userconfig loads the editor's small user-level config from
// ~/.config/r-ed/config.json. It's separate from customactions on
// purpose: actions.json is a list of shell-out menu entries, config.json
// is editor preferences. Keeping them apart means a malformed actions
// file can't break editor settings and vice-versa.
//
// Schema today is intentionally tiny, but the loader is wrapped in a
// struct so we can grow new top-level fields without breaking older
// configs:
//
//	{"icons": "auto"}       // default; auto-detect Nerd Fonts on startup
//	{"icons": "on"}         // force-on, even if detection would say no
//	{"icons": "off"}        // force-off, even if a Nerd Font is installed
//	{"autosave": "on"}      // default; save dirty buffers after an idle pause
//	{"autosave": "off"}     // only explicit ≡ → Save writes to disk
//	{"termdock": "bottom"}  // default; terminal panel is a bottom strip
//	{"termdock": "left"}    // terminal docks as a vertical strip on the
//	                        // left; the file tree flips to the right
//	{"execmarks": "on"}     // default; append an ls -F '*' to executables
//	{"execmarks": "off"}    // hide the executable marker in the file tree
//	{"copilot": "on"}       // default; run copilot-language-server when
//	                        // it's installed (silent no-op when absent)
//	{"copilot": "off"}      // never spawn the Copilot sidecar
//
// The loader is best-effort the same way customactions is: missing
// file → defaults, malformed file → error returned for the app to
// flash, but the editor still starts cleanly.
package userconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IconsMode is the user's preference for Nerd Font icons in the file
// tree. "auto" means "use them iff a Nerd Font is installed"; the
// other two values bypass detection entirely.
type IconsMode string

const (
	IconsAuto IconsMode = "auto"
	IconsOn   IconsMode = "on"
	IconsOff  IconsMode = "off"
)

// TermDock is where the terminal panel docks: the classic bottom
// strip, or a vertical strip on the left (which flips the file tree
// to the right edge).
type TermDock string

const (
	TermDockBottom TermDock = "bottom"
	TermDockLeft   TermDock = "left"
)

// Config is the resolved, validated form of config.json. Callers get a
// fully-populated Config back from Load — defaults are filled in for
// any field the file omitted, so consumers never need to nil-check.
type Config struct {
	Icons IconsMode

	// AutoSave controls whether dirty buffers are written to disk
	// automatically after an idle pause. Defaults to on — the editor
	// is opinionated toward "your work is always on disk", and the
	// ≡ menu toggle (which persists here) is the escape hatch.
	AutoSave bool

	// TermDock is the terminal panel's home edge. Defaults to the
	// bottom strip; "left" selects the alternate layout (terminal
	// vertical on the left, file tree on the right). Persisted by
	// the ≡ layout toggle, same as AutoSave.
	TermDock TermDock

	// ExecMarks controls whether the file tree appends an ls -F style
	// '*' to executable regular files. Defaults to on. Persisted by the
	// ≡ view toggle, same as AutoSave.
	ExecMarks bool

	// Copilot controls whether the editor runs the GitHub Copilot
	// sidecar (copilot-language-server). Defaults to on because the
	// binary is only ever spawned when the user has installed it —
	// presence on PATH is itself the opt-in; this key is the opt-out
	// for people who have the binary for other editors but don't want
	// r-ed touching it. Persisted by the ≡ toggle, same as AutoSave.
	Copilot bool
}

// Defaults returns a Config populated with the values used when no
// config file is present (or every field in it is blank). Centralised
// so tests and the loader can't drift from each other.
func Defaults() Config {
	return Config{Icons: IconsAuto, AutoSave: true, TermDock: TermDockBottom, ExecMarks: true, Copilot: true}
}

// fileFormat mirrors the on-disk JSON shape. We decode into this and
// then promote into Config so the public type doesn't have to carry
// JSON tags or pointer fields just for "field was absent" detection.
// AutoSave is a string ("on"/"off"), not a bool, for the same absent-
// field reason: a missing key must mean "keep the default", and JSON
// false is indistinguishable from absent on a plain bool.
type fileFormat struct {
	Icons     string `json:"icons,omitempty"`
	AutoSave  string `json:"autosave,omitempty"`
	TermDock  string `json:"termdock,omitempty"`
	ExecMarks string `json:"execmarks,omitempty"`
	Copilot   string `json:"copilot,omitempty"`
}

// configFilePath resolves the r-ed config directory
// ($XDG_CONFIG_HOME/r-ed, else ~/.config/r-ed) and joins name onto it.
// Returns "" when neither XDG_CONFIG_HOME nor a home directory resolves,
// which callers treat as "no config location — use defaults / skip".
// DefaultPath and RcPath both go through here so the two files can never
// drift into different directories.
func configFilePath(name string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "r-ed", name)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "r-ed", name)
}

// DefaultPath returns the canonical config-file location:
// $XDG_CONFIG_HOME/r-ed/config.json, falling back to
// ~/.config/r-ed/config.json. Returns "" when neither resolves
// — callers should treat that as "use defaults".
func DefaultPath() string { return configFilePath("config.json") }

// RcPath returns the canonical grsh rc-file location:
// $XDG_CONFIG_HOME/r-ed/rc.grsh, falling back to ~/.config/r-ed/rc.grsh
// (or "" when no config location resolves).
//
// This is the grsh analog of ~/.zshrc: the embedded terminal panel
// sources it once, when the grsh session is created, so a user's aliases
// and functions are available in every r-ed shell. It MUST be grsh
// syntax, not zsh — the terminal embeds grsh (its own shell language),
// so it never reads ~/.zshrc or any zsh startup file, and this file is
// exactly the gap that fills.
func RcPath() string { return configFilePath("rc.grsh") }

// Load reads and parses the config file at path, returning a Config
// with defaults filled in for any missing or blank fields.
//
// Contract:
//   - path == ""              → (Defaults(), nil). Treated as "no
//     config configured".
//   - file doesn't exist      → (Defaults(), nil). Same as above.
//   - file unreadable         → (Defaults(), err). Caller can flash a
//     message; editor keeps running on defaults.
//   - file empty / all-blank  → (Defaults(), nil).
//   - unknown icons value     → (Defaults(), err). We'd rather tell
//     the user their config has a typo than silently fall back to
//     defaults and hide the bug.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return cfg, nil
	}

	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	switch IconsMode(strings.ToLower(strings.TrimSpace(ff.Icons))) {
	case "":
		// field omitted — keep default
	case IconsAuto:
		cfg.Icons = IconsAuto
	case IconsOn:
		cfg.Icons = IconsOn
	case IconsOff:
		cfg.Icons = IconsOff
	default:
		return Defaults(), fmt.Errorf(
			"%s: icons must be %q, %q, or %q (got %q)",
			path, IconsAuto, IconsOn, IconsOff, ff.Icons,
		)
	}

	switch strings.ToLower(strings.TrimSpace(ff.AutoSave)) {
	case "":
		// field omitted — keep default
	case "on":
		cfg.AutoSave = true
	case "off":
		cfg.AutoSave = false
	default:
		return Defaults(), fmt.Errorf(
			"%s: autosave must be \"on\" or \"off\" (got %q)",
			path, ff.AutoSave,
		)
	}

	switch TermDock(strings.ToLower(strings.TrimSpace(ff.TermDock))) {
	case "":
		// field omitted — keep default
	case TermDockBottom:
		cfg.TermDock = TermDockBottom
	case TermDockLeft:
		cfg.TermDock = TermDockLeft
	default:
		return Defaults(), fmt.Errorf(
			"%s: termdock must be %q or %q (got %q)",
			path, TermDockBottom, TermDockLeft, ff.TermDock,
		)
	}

	switch strings.ToLower(strings.TrimSpace(ff.ExecMarks)) {
	case "":
		// field omitted — keep default
	case "on":
		cfg.ExecMarks = true
	case "off":
		cfg.ExecMarks = false
	default:
		return Defaults(), fmt.Errorf(
			"%s: execmarks must be \"on\" or \"off\" (got %q)",
			path, ff.ExecMarks,
		)
	}

	switch strings.ToLower(strings.TrimSpace(ff.Copilot)) {
	case "":
		// field omitted — keep default
	case "on":
		cfg.Copilot = true
	case "off":
		cfg.Copilot = false
	default:
		return Defaults(), fmt.Errorf(
			"%s: copilot must be \"on\" or \"off\" (got %q)",
			path, ff.Copilot,
		)
	}
	return cfg, nil
}

// SaveAutoSave persists the auto-save preference into the config file
// at path. See saveKey for the round-trip guarantees.
func SaveAutoSave(path string, on bool) error {
	val := "on"
	if !on {
		val = "off"
	}
	return saveKey(path, "autosave", val)
}

// SaveTermDock persists the terminal-dock preference into the config
// file at path. See saveKey for the round-trip guarantees.
func SaveTermDock(path string, dock TermDock) error {
	return saveKey(path, "termdock", string(dock))
}

// SaveExecMarks persists the executable-marker preference into the
// config file at path. See saveKey for the round-trip guarantees.
func SaveExecMarks(path string, on bool) error {
	val := "on"
	if !on {
		val = "off"
	}
	return saveKey(path, "execmarks", val)
}

// SaveCopilot persists the Copilot-sidecar preference into the config
// file at path. See saveKey for the round-trip guarantees.
func SaveCopilot(path string, on bool) error {
	val := "on"
	if !on {
		val = "off"
	}
	return saveKey(path, "copilot", val)
}

// saveKey writes one preference into the config file at path,
// preserving every other key the user may have set by hand (icons
// today, anything we add tomorrow). The read-modify-write goes
// through a raw map — not fileFormat — so keys this binary doesn't
// know about survive a round-trip with a newer or older r-ed.
// Writes atomically (temp + rename), same as the format-config
// installer, so a crash mid-write can't corrupt the config.
func saveKey(path, key, val string) error {
	if path == "" {
		return errors.New("no config directory resolved — cannot persist preference")
	}
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(data) > 0:
		if err := json.Unmarshal(data, &raw); err != nil {
			// A malformed config is the user's hand-edit; overwriting
			// it with a single fresh key would eat their file.
			return fmt.Errorf("parse %s: %w", path, err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}

	raw[key] = val

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
