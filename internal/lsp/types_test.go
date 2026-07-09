// =============================================================================
// File: internal/lsp/types_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package lsp

import (
	"encoding/json"
	"testing"
)

// TestPathURIRoundTrip pins the URI conversion both ways, including a
// path with a space — the case that breaks naive "file://" + path
// concatenation and that some servers reject outright.
func TestPathURIRoundTrip(t *testing.T) {
	cases := []string{
		"/Users/ro/projs/go/spice-edit/main.go",
		"/tmp/dir with space/file.go",
		"/a/b/c.go",
	}
	for _, path := range cases {
		uri := PathToURI(path)
		if got := URIToPath(uri); got != path {
			t.Errorf("round trip %q → %q → %q", path, uri, got)
		}
	}
	if uri := PathToURI("/tmp/dir with space/file.go"); uri != "file:///tmp/dir%20with%20space/file.go" {
		t.Errorf("space not escaped: %q", uri)
	}
}

// TestURIToPathRejectsNonFile pins the "" return for schemes the editor
// can't open as plain files, so a definition into a zip-scheme stdlib
// archive degrades to a no-op instead of opening a garbage path.
func TestURIToPathRejectsNonFile(t *testing.T) {
	for _, uri := range []string{"https://example.com/x.go", "zipfile:///a.zip", "::bad::"} {
		if got := URIToPath(uri); got != "" {
			t.Errorf("URIToPath(%q) = %q, want empty", uri, got)
		}
	}
}

// TestUTF16ColBMP pins that plain ASCII/BMP text converts 1:1 — the
// overwhelmingly common case must be an identity mapping.
func TestUTF16ColBMP(t *testing.T) {
	line := []rune("hello, wörld")
	for i := 0; i <= len(line); i++ {
		if got := UTF16Col(line, i); got != i {
			t.Errorf("UTF16Col(BMP, %d) = %d, want identity", i, got)
		}
		if got := RuneCol(line, i); got != i {
			t.Errorf("RuneCol(BMP, %d) = %d, want identity", i, got)
		}
	}
}

// TestUTF16ColSurrogates pins the two-code-unit accounting for non-BMP
// runes — the whole reason these helpers exist. "a🙂b": the emoji is
// one rune but two UTF-16 units, so 'b' sits at rune 2 / UTF-16 3.
func TestUTF16ColSurrogates(t *testing.T) {
	line := []rune("a🙂b")
	if got := UTF16Col(line, 2); got != 3 {
		t.Errorf("UTF16Col rune 2 = %d, want 3", got)
	}
	if got := RuneCol(line, 3); got != 2 {
		t.Errorf("RuneCol utf16 3 = %d, want rune 2", got)
	}
	// Column landing mid-surrogate resolves to the emoji's own index.
	if got := RuneCol(line, 2); got != 1 {
		t.Errorf("RuneCol mid-surrogate = %d, want 1", got)
	}
	// Past-the-end clamps on both sides.
	if got := UTF16Col(line, 99); got != 4 {
		t.Errorf("UTF16Col overflow = %d, want 4", got)
	}
	if got := RuneCol(line, 99); got != 3 {
		t.Errorf("RuneCol overflow = %d, want 3", got)
	}
}

// TestHoverText pins the three wire shapes servers actually send for
// hover contents, plus the empty/unrecognised → "" fallback.
func TestHoverText(t *testing.T) {
	cases := []struct {
		name, contents, want string
	}{
		{"markup content", `{"kind":"plaintext","value":"func Foo()"}`, "func Foo()"},
		{"bare string", `"just text"`, "just text"},
		{"array of strings", `["one","two"]`, "one\ntwo"},
		{"array of language pairs", `[{"language":"go","value":"var x int"}]`, "var x int"},
		{"empty object", `{}`, ""},
		{"null-ish", ``, ""},
	}
	for _, tc := range cases {
		h := Hover{Contents: json.RawMessage(tc.contents)}
		if got := h.HoverText(); got != tc.want {
			t.Errorf("%s: HoverText() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
