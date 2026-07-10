// =============================================================================
// File: internal/format/builtin_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-09
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package format

import (
	"errors"
	"path/filepath"
	"testing"
)

// stubLookPath swaps the package's PATH resolver for one that only
// knows the given tools, restoring the real resolver when the test
// ends. Keys are tool names, values the fake resolved binary paths.
func stubLookPath(t *testing.T, tools map[string]string) {
	t.Helper()
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if p, ok := tools[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = orig })
}

// TestBuiltinCommandFor_PrefersGoimports pins the preference order:
// when both tools are installed, goimports must win because it's the
// one that also fixes imports — picking gofmt would silently drop the
// auto-import half of the feature.
func TestBuiltinCommandFor_PrefersGoimports(t *testing.T) {
	stubLookPath(t, map[string]string{
		"goimports": "/fake/bin/goimports",
		"gofmt":     "/fake/bin/gofmt",
	})
	argv := BuiltinCommandFor("/proj/main.go")
	if len(argv) != 3 || argv[0] != "/fake/bin/goimports" || argv[1] != "-w" {
		t.Fatalf("argv = %v, want [/fake/bin/goimports -w <file>]", argv)
	}
	if !filepath.IsAbs(argv[2]) {
		t.Errorf("file arg %q should be absolute", argv[2])
	}
}

// TestBuiltinCommandFor_FallsBackToGofmt covers the machine that has
// a Go toolchain but never installed goimports — formatting should
// still happen, just without import management.
func TestBuiltinCommandFor_FallsBackToGofmt(t *testing.T) {
	stubLookPath(t, map[string]string{"gofmt": "/fake/bin/gofmt"})
	argv := BuiltinCommandFor("/proj/main.go")
	if len(argv) != 3 || argv[0] != "/fake/bin/gofmt" {
		t.Fatalf("argv = %v, want gofmt fallback", argv)
	}
}

// TestBuiltinCommandFor_NoToolsIsNil pins silent degradation: no Go
// tools on PATH means no builtin formatting and no error — the save
// must behave exactly as it did before this feature existed.
func TestBuiltinCommandFor_NoToolsIsNil(t *testing.T) {
	stubLookPath(t, nil)
	if argv := BuiltinCommandFor("/proj/main.go"); argv != nil {
		t.Fatalf("argv = %v, want nil when nothing is installed", argv)
	}
}

// TestBuiltinCommandFor_NonGoIsNil ensures the builtin only ever
// fires for Go files — other languages stay on the opt-in
// format.json path, prompts and all.
func TestBuiltinCommandFor_NonGoIsNil(t *testing.T) {
	stubLookPath(t, map[string]string{
		"goimports": "/fake/bin/goimports",
		"gofmt":     "/fake/bin/gofmt",
	})
	for _, p := range []string{"/proj/notes.txt", "/proj/main.py", "/proj/Makefile", "/proj/go"} {
		if argv := BuiltinCommandFor(p); argv != nil {
			t.Errorf("BuiltinCommandFor(%q) = %v, want nil", p, argv)
		}
	}
}
