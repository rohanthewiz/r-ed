// =============================================================================
// File: internal/app/copypaste_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-11
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/filetree"
)

// waitForPasteEvent drains the simulation screen's queue until the
// async paste goroutine reports back — same shape as waitForZipEvent.
func waitForPasteEvent(t *testing.T, a *App) *pasteDoneEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ev := a.screen.PollEvent()
		if ev == nil {
			t.Fatal("screen returned nil event")
		}
		if pe, ok := ev.(*pasteDoneEvent); ok {
			return pe
		}
	}
	t.Fatal("timed out waiting for pasteDoneEvent")
	return nil
}

// TestUniquePastePath_Suffixing pins the collision policy: the bare
// name when free, then Finder-style " copy" / " copy N" inserted
// before the extension so repeated pastes never clobber anything.
func TestUniquePastePath_Suffixing(t *testing.T) {
	dir := t.TempDir()

	if got := uniquePastePath(dir, "main.go", false); got != filepath.Join(dir, "main.go") {
		t.Fatalf("free name should be used as-is, got %q", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := uniquePastePath(dir, "main.go", false); got != filepath.Join(dir, "main copy.go") {
		t.Fatalf("first collision = %q, want main copy.go", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "main copy.go"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := uniquePastePath(dir, "main.go", false); got != filepath.Join(dir, "main copy 2.go") {
		t.Fatalf("second collision = %q, want main copy 2.go", got)
	}
}

// TestUniquePastePath_DotfilesAndDirs pins the two cases where the
// suffix is appended whole instead of spliced before the extension:
// dotfiles (".gitignore" is all extension — splitting would leave an
// empty stem) and directories (whose dots aren't extensions at all).
func TestUniquePastePath_DotfilesAndDirs(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := uniquePastePath(dir, ".gitignore", false); got != filepath.Join(dir, ".gitignore copy") {
		t.Fatalf("dotfile collision = %q, want .gitignore copy", got)
	}

	if err := os.Mkdir(filepath.Join(dir, "my.dir"), 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := uniquePastePath(dir, "my.dir", true); got != filepath.Join(dir, "my.dir copy") {
		t.Fatalf("dir collision = %q, want my.dir copy", got)
	}
}

// TestCopyFileContents_PermsAndNoClobber verifies the copy preserves
// the source's permission bits and that O_EXCL holds the never-clobber
// contract even when the caller races another writer to the name.
func TestCopyFileContents_PermsAndNoClobber(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dst := filepath.Join(dir, "run copy.sh")
	if err := copyFileContents(src, dst, 0755); err != nil {
		t.Fatalf("copy: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("perm = %v, want 0755", info.Mode().Perm())
	}

	if err := copyFileContents(src, dst, 0755); err == nil {
		t.Fatal("copy onto existing dst should refuse")
	}
}

// TestCopyTree_Directory drives the recursive copy over a nested
// folder including a symlink, and checks the link is recreated as a
// link (not followed) — the same rule the zip walker follows.
func TestCopyTree_Directory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(src, "inner"), 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "inner", "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	dst := filepath.Join(dir, "sub copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "inner", "b.txt"))
	if err != nil || string(got) != "b" {
		t.Fatalf("nested file: %q, %v", got, err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || target != "a.txt" {
		t.Fatalf("symlink not preserved: %q, %v", target, err)
	}
}

// TestPasteIntoOwnSubtree pins the recursion guard's boundaries,
// including the /proj/foo vs /proj/foobar near-miss that a plain
// prefix match would get wrong.
func TestPasteIntoOwnSubtree(t *testing.T) {
	cases := []struct {
		src, dest string
		want      bool
	}{
		{"/proj/foo", "/proj/foo", true},
		{"/proj/foo", "/proj/foo/inner", true},
		{"/proj/foo", "/proj/foobar", false},
		{"/proj/foo", "/proj", false},
		{"/proj/foo", "/proj/other", false},
	}
	for _, c := range cases {
		if got := pasteIntoOwnSubtree(c.src, c.dest); got != c.want {
			t.Errorf("pasteIntoOwnSubtree(%q, %q) = %v, want %v", c.src, c.dest, got, c.want)
		}
	}
}

// TestCopyToFileClip_ArmsAndValidates verifies Copy arms the path and
// flips clipKind, and that a vanished source refuses to arm — an armed
// clip pointing at nothing would just defer the error to paste time.
func TestCopyToFileClip_ArmsAndValidates(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.copyToFileClip(target)
	if a.fileClipPath != target || a.clipKind != clipFile {
		t.Fatalf("clip not armed: path=%q kind=%v", a.fileClipPath, a.clipKind)
	}
	if !strings.Contains(a.statusMsg, "Copied f.txt") {
		t.Fatalf("flash = %q, want copied confirmation", a.statusMsg)
	}

	a.fileClipPath, a.clipKind = "", clipNone
	a.copyToFileClip(filepath.Join(dir, "gone.txt"))
	if a.fileClipPath != "" {
		t.Fatal("missing source should not arm the clip")
	}
	if !strings.Contains(a.statusMsg, "Copy failed") {
		t.Fatalf("flash = %q, want failure message", a.statusMsg)
	}
}

// TestStartPaste_FileHappyPath drives the app glue end to end: arm the
// clip, paste into a folder, wait for the done event, and check the
// duplicate exists with the source's content.
func TestStartPaste_FileHappyPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.copyToFileClip(src)
	a.startPaste(sub)
	ev := waitForPasteEvent(t, a)
	if ev.err != nil {
		t.Fatalf("paste err: %v", ev.err)
	}
	a.handlePasteDone(ev)

	got, err := os.ReadFile(filepath.Join(sub, "f.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("pasted file: %q, %v", got, err)
	}
	if !strings.Contains(a.statusMsg, "Pasted f.txt") {
		t.Fatalf("flash = %q, want pasted confirmation", a.statusMsg)
	}
}

// TestStartPaste_CollisionGetsCopySuffix pastes a file into its own
// folder — the everyday "duplicate this" gesture — and expects the
// " copy" name rather than an overwrite refusal or clobber.
func TestStartPaste_CollisionGetsCopySuffix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.copyToFileClip(src)
	a.startPaste(dir)
	ev := waitForPasteEvent(t, a)
	if ev.err != nil {
		t.Fatalf("paste err: %v", ev.err)
	}
	if filepath.Base(ev.dest) != "f copy.txt" {
		t.Fatalf("dest = %q, want f copy.txt", ev.dest)
	}
	if got, err := os.ReadFile(ev.dest); err != nil || string(got) != "hello" {
		t.Fatalf("duplicate content: %q, %v", got, err)
	}
}

// TestStartPaste_FolderIntoItselfRefused pins the recursion guard at
// the app layer: pasting a folder into itself (or a descendant) is
// refused synchronously with a flash, and no goroutine is spawned.
func TestStartPaste_FolderIntoItselfRefused(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.copyToFileClip(sub)
	a.startPaste(filepath.Join(sub, "inner"))
	if !strings.Contains(a.statusMsg, "into itself") {
		t.Fatalf("flash = %q, want self-paste refusal", a.statusMsg)
	}
	if _, err := os.Lstat(filepath.Join(sub, "inner", "sub")); err == nil {
		t.Fatal("refused paste must not create anything")
	}
}

// TestStartPaste_SourceGoneDisarms verifies that pasting after the
// copied source was deleted flashes a specific error and disarms the
// clip so the Paste row stops offering a paste that can never work.
func TestStartPaste_SourceGoneDisarms(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.copyToFileClip(src)
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove: %v", err)
	}
	a.startPaste(dir)
	if !strings.Contains(a.statusMsg, "no longer exists") {
		t.Fatalf("flash = %q, want source-gone message", a.statusMsg)
	}
	if a.fileClipPath != "" || a.clipKind != clipNone {
		t.Fatal("stale clip should be disarmed")
	}
}

// TestCmdCopy_SelectionBeatsFile pins Cmd+C's routing: with a text
// selection it does a text copy (clipKind stays clipText), without one
// it arms the file clipboard with the active tab's file.
func TestCmdCopy_SelectionBeatsFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)

	tab := a.activeTabPtr()
	tab.MoveLineEnd(true) // extend selection from BOL to EOL
	a.cmdCopy()
	if a.clipKind != clipText || a.clipBuf != "hello" {
		t.Fatalf("selection copy: kind=%v buf=%q", a.clipKind, a.clipBuf)
	}
	if a.fileClipPath != "" {
		t.Fatal("selection copy must not arm the file clip")
	}

	tab.MoveLineHome(false) // collapse the selection
	a.cmdCopy()
	if a.clipKind != clipFile || a.fileClipPath != target {
		t.Fatalf("file copy: kind=%v path=%q", a.clipKind, a.fileClipPath)
	}
}

// TestCmdPaste_RoutesByClipKind pins Cmd+V's last-write-wins routing:
// clipFile pastes the file into the active folder, clipText inserts
// into the buffer.
func TestCmdPaste_RoutesByClipKind(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(src)

	// File route: armed clip + Cmd+V duplicates into the active folder.
	a.copyToFileClip(src)
	a.cmdPaste()
	ev := waitForPasteEvent(t, a)
	if ev.err != nil {
		t.Fatalf("paste err: %v", ev.err)
	}
	if filepath.Base(ev.dest) != "f copy.txt" {
		t.Fatalf("dest = %q, want f copy.txt", ev.dest)
	}

	// Text route: a later text copy retakes the key.
	a.clipBuf, a.clipKind = "XYZ", clipText
	before := a.activeTabPtr().Buffer.Lines[0]
	a.cmdPaste()
	after := a.activeTabPtr().Buffer.Lines[0]
	if !strings.Contains(after, "XYZ") || after == before {
		t.Fatalf("text paste: line %q → %q, want XYZ inserted", before, after)
	}
}

// TestHandleKey_CmdCVDispatch verifies the ModMeta (Cmd/Super) rune
// events land on cmdCopy / cmdPaste — the terminal-side contract for
// kitty-protocol terminals that pass Cmd through.
func TestHandleKey_CmdCVDispatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)

	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'c', tcell.ModMeta))
	if a.fileClipPath != target {
		t.Fatalf("Cmd+C should arm the file clip, got %q", a.fileClipPath)
	}

	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'v', tcell.ModMeta))
	ev := waitForPasteEvent(t, a)
	if ev.err != nil {
		t.Fatalf("Cmd+V paste err: %v", ev.err)
	}

	// Without ModMeta the same rune must still type normally.
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'c', tcell.ModNone))
	if !strings.Contains(a.activeTabPtr().Buffer.Lines[0], "c") {
		t.Fatal("plain 'c' should insert into the buffer")
	}
}

// TestOpenTreeContext_PasteOnlyWhenArmed pins the context-menu rule:
// the Paste row appears (root included) only after something has been
// copied, so the tiny popup never carries a permanently dead row.
func TestOpenTreeContext_PasteOnlyWhenArmed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	hasPaste := func() bool {
		for _, it := range contextOf(a).items {
			if it.label == "Paste" {
				return true
			}
		}
		return false
	}

	a.openTreeContext(a.tree.Root, 5, 5)
	if hasPaste() {
		t.Fatal("Paste row should be absent while nothing is copied")
	}
	a.closeAllModals()

	a.copyToFileClip(src)
	a.openTreeContext(a.tree.Root, 5, 5)
	if !hasPaste() {
		t.Fatal("Paste row should appear on the root once armed")
	}
	a.closeAllModals()
}

// TestCtxPaste_FileNodeTargetsParent verifies right-click-pasting on a
// file node lands the duplicate in the file's folder — a file can't
// contain anything, so its parent is the only sensible destination.
func TestCtxPaste_FileNodeTargetsParent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inner := filepath.Join(sub, "b.txt")
	if err := os.WriteFile(inner, []byte("b"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("a"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	// Find sub's node and expand it so the file node exists in the tree.
	var subNode *filetree.Node
	for _, c := range a.tree.Root.Children {
		if c.Name == "sub" {
			subNode = c
			break
		}
	}
	if subNode == nil {
		t.Fatal("sub node not in tree")
	}
	a.tree.Toggle(subNode)
	var fileNode *filetree.Node
	for _, c := range subNode.Children {
		if c.Name == "b.txt" {
			fileNode = c
			break
		}
	}
	if fileNode == nil {
		t.Fatal("file node not in tree")
	}

	a.copyToFileClip(src)
	ctxPaste(a, fileNode)
	ev := waitForPasteEvent(t, a)
	if ev.err != nil {
		t.Fatalf("paste err: %v", ev.err)
	}
	if got := filepath.Dir(ev.dest); got != sub {
		t.Fatalf("dest dir = %q, want %q", got, sub)
	}
}
