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

// TestBuiltinCommandsFor_PrefersGoimports pins the preference order:
// when everything is installed, goimports alone must win because it
// formats AND fixes imports in one pass — no chain needed.
func TestBuiltinCommandsFor_PrefersGoimports(t *testing.T) {
	stubLookPath(t, map[string]string{
		"goimports": "/fake/bin/goimports",
		"gopls":     "/fake/bin/gopls",
		"gofmt":     "/fake/bin/gofmt",
	})
	cmds := BuiltinCommandsFor("/proj/main.go")
	if len(cmds) != 1 {
		t.Fatalf("cmds = %v, want single goimports command", cmds)
	}
	argv := cmds[0]
	if len(argv) != 3 || argv[0] != "/fake/bin/goimports" || argv[1] != "-w" {
		t.Fatalf("argv = %v, want [/fake/bin/goimports -w <file>]", argv)
	}
	if !filepath.IsAbs(argv[2]) {
		t.Errorf("file arg %q should be absolute", argv[2])
	}
}

// TestBuiltinCommandsFor_GoplsChain covers the increasingly common
// machine that has gopls (for the LSP) but never installed the
// standalone goimports: import fixing must not silently vanish. The
// pipeline is `gopls imports -w` for the imports, then `gofmt -w` for
// the formatting goimports would otherwise have applied.
func TestBuiltinCommandsFor_GoplsChain(t *testing.T) {
	stubLookPath(t, map[string]string{
		"gopls": "/fake/bin/gopls",
		"gofmt": "/fake/bin/gofmt",
	})
	cmds := BuiltinCommandsFor("/proj/main.go")
	if len(cmds) != 2 {
		t.Fatalf("cmds = %v, want gopls-imports + gofmt chain", cmds)
	}
	if cmds[0][0] != "/fake/bin/gopls" || cmds[0][1] != "imports" || cmds[0][2] != "-w" {
		t.Fatalf("cmds[0] = %v, want [gopls imports -w <file>]", cmds[0])
	}
	if cmds[1][0] != "/fake/bin/gofmt" || cmds[1][1] != "-w" {
		t.Fatalf("cmds[1] = %v, want [gofmt -w <file>]", cmds[1])
	}
}

// TestBuiltinCommandsFor_GoplsOnly pins the gopls-without-gofmt case
// (a machine with a gopls binary but no Go toolchain dir on PATH):
// import fixing still runs alone rather than being dropped.
func TestBuiltinCommandsFor_GoplsOnly(t *testing.T) {
	stubLookPath(t, map[string]string{"gopls": "/fake/bin/gopls"})
	cmds := BuiltinCommandsFor("/proj/main.go")
	if len(cmds) != 1 || cmds[0][0] != "/fake/bin/gopls" || cmds[0][1] != "imports" {
		t.Fatalf("cmds = %v, want lone gopls imports command", cmds)
	}
}

// TestBuiltinCommandsFor_FallsBackToGofmt covers the machine that has
// a Go toolchain but neither goimports nor gopls — formatting should
// still happen, just without import management.
func TestBuiltinCommandsFor_FallsBackToGofmt(t *testing.T) {
	stubLookPath(t, map[string]string{"gofmt": "/fake/bin/gofmt"})
	cmds := BuiltinCommandsFor("/proj/main.go")
	if len(cmds) != 1 || cmds[0][0] != "/fake/bin/gofmt" {
		t.Fatalf("cmds = %v, want gofmt fallback", cmds)
	}
}

// TestBuiltinCommandsFor_NoToolsIsNil pins silent degradation: no Go
// tools on PATH means no builtin formatting and no error — the save
// must behave exactly as it did before this feature existed.
func TestBuiltinCommandsFor_NoToolsIsNil(t *testing.T) {
	stubLookPath(t, nil)
	if cmds := BuiltinCommandsFor("/proj/main.go"); cmds != nil {
		t.Fatalf("cmds = %v, want nil when nothing is installed", cmds)
	}
}

// TestBuiltinCommandsFor_NonGoIsNil ensures the builtin only ever
// fires for Go files — other languages stay on the opt-in
// format.json path, prompts and all.
func TestBuiltinCommandsFor_NonGoIsNil(t *testing.T) {
	stubLookPath(t, map[string]string{
		"goimports": "/fake/bin/goimports",
		"gofmt":     "/fake/bin/gofmt",
	})
	for _, p := range []string{"/proj/notes.txt", "/proj/main.py", "/proj/Makefile", "/proj/go"} {
		if cmds := BuiltinCommandsFor(p); cmds != nil {
			t.Errorf("BuiltinCommandsFor(%q) = %v, want nil", p, cmds)
		}
	}
}
