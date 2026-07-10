// =============================================================================
// File: internal/app/zipops_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-09
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readZipEntries opens a finished archive and returns its entries as
// a name → content map (directories map to ""). Shared by every test
// that asserts on archive shape.
func readZipEntries(t *testing.T, dest string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			out[f.Name] = ""
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		out[f.Name] = string(data)
	}
	return out
}

// TestCreateZip_SingleFile pins the file case: one entry, named by
// the file's basename (no directory prefix), contents intact.
func TestCreateZip_SingleFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(src, []byte("hello zip\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "notes.txt.zip")

	if err := createZip(src, dest); err != nil {
		t.Fatalf("createZip: %v", err)
	}

	entries := readZipEntries(t, dest)
	if len(entries) != 1 || entries["notes.txt"] != "hello zip\n" {
		t.Fatalf("entries = %v, want single notes.txt", entries)
	}
}

// TestCreateZip_FolderRootedAtBasename pins the extraction contract:
// entries are prefixed with the folder's name so unzipping produces
// one folder, not a spray of top-level files. Also covers nesting
// and the explicit empty-directory entry.
func TestCreateZip_FolderRootedAtBasename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "proj")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.go"), []byte("package b\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "proj.zip")

	if err := createZip(src, dest); err != nil {
		t.Fatalf("createZip: %v", err)
	}

	entries := readZipEntries(t, dest)
	if entries["proj/a.go"] != "package a\n" {
		t.Errorf("proj/a.go missing or wrong: %v", entries)
	}
	if entries["proj/sub/b.go"] != "package b\n" {
		t.Errorf("proj/sub/b.go missing or wrong: %v", entries)
	}
	if _, ok := entries["proj/empty/"]; !ok {
		t.Errorf("empty directory entry missing: %v", entries)
	}
}

// TestCreateZip_RefusesClobber pins the O_EXCL contract shared with
// createEmptyFile / renameFile: an existing archive is never
// overwritten, and its contents survive the refused attempt.
func TestCreateZip_RefusesClobber(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(dir, "notes.txt.zip")
	if err := os.WriteFile(dest, []byte("precious"), 0644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	if err := createZip(src, dest); err == nil {
		t.Fatal("createZip should refuse an existing destination")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "precious" {
		t.Fatalf("existing archive was modified: %q", got)
	}
}

// TestCreateZip_RemovesPartialOnFailure pins the cleanup contract: a
// walk error mid-archive must not leave a corrupt .zip behind, or
// the O_EXCL refusal would then block every retry with garbage.
func TestCreateZip_RemovesPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "proj")
	locked := filepath.Join(src, "locked")
	if err := os.MkdirAll(locked, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Make the subdirectory unreadable so the walk fails partway.
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0755) })
	if os.Getuid() == 0 {
		t.Skip("running as root — permission walls don't apply")
	}
	dest := filepath.Join(dir, "proj.zip")

	if err := createZip(src, dest); err == nil {
		t.Fatal("createZip should fail on an unreadable subdirectory")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("partial archive should be removed on failure")
	}
}

// TestZipDest pins the placement rule: sibling <name>.zip normally,
// but INSIDE the root when zipping the root itself — a sibling of
// the project root would land outside the tree where the user can't
// see it.
func TestZipDest(t *testing.T) {
	if got := zipDest("/proj/sub", "/proj"); got != "/proj/sub.zip" {
		t.Errorf("subfolder dest = %q, want /proj/sub.zip", got)
	}
	if got := zipDest("/proj/a.go", "/proj"); got != "/proj/a.go.zip" {
		t.Errorf("file dest = %q, want /proj/a.go.zip", got)
	}
	if got := zipDest("/proj", "/proj"); got != filepath.Join("/proj", "proj.zip") {
		t.Errorf("root dest = %q, want /proj/proj.zip", got)
	}
}

// TestCreateZip_RootSkipsOwnArchive covers the zip-inside-target
// case that root zips create: the walk must not try to archive the
// half-written archive itself.
func TestCreateZip_RootSkipsOwnArchive(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := zipDest(src, src) // inside src, like a project-root zip

	if err := createZip(src, dest); err != nil {
		t.Fatalf("createZip: %v", err)
	}

	base := filepath.Base(src)
	entries := readZipEntries(t, dest)
	if _, ok := entries[base+"/"+filepath.Base(dest)]; ok {
		t.Fatal("archive contains itself")
	}
	if entries[base+"/a.txt"] != "a" {
		t.Fatalf("expected a.txt in archive, got %v", entries)
	}
}

// waitForZipEvent drains the simulation screen's queue until the
// async zip goroutine reports back — same shape as waitForFormatEvent.
func waitForZipEvent(t *testing.T, a *App) *zipDoneEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ev := a.screen.PollEvent()
		if ev == nil {
			t.Fatal("screen returned nil event")
		}
		if ze, ok := ev.(*zipDoneEvent); ok {
			return ze
		}
	}
	t.Fatal("timed out waiting for zipDoneEvent")
	return nil
}

// TestStartZip_HappyPath drives the app glue end to end: startZip
// spawns the goroutine, the done event lands, and handleZipDone
// flashes the new archive's name.
func TestStartZip_HappyPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, root)

	a.startZip(target)
	ev := waitForZipEvent(t, a)
	if ev.err != nil {
		t.Fatalf("zip err: %v", ev.err)
	}
	a.handleZipDone(ev)

	if _, err := os.Stat(filepath.Join(root, "main.go.zip")); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if !strings.Contains(a.statusMsg, "main.go.zip") {
		t.Fatalf("flash %q should name the archive", a.statusMsg)
	}
}

// TestStartZip_RefusesExistingDest pins the immediate main-loop
// refusal: when the archive already exists the user gets a specific
// flash and no goroutine is spawned to fail asynchronously.
func TestStartZip_RefusesExistingDest(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(target+".zip", []byte("old"), 0644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	a := newTestApp(t, root)

	a.startZip(target)

	if !strings.Contains(a.statusMsg, "already exists") {
		t.Fatalf("flash %q should explain the refusal", a.statusMsg)
	}
	got, _ := os.ReadFile(target + ".zip")
	if string(got) != "old" {
		t.Fatalf("existing archive was touched: %q", got)
	}
}

// TestMenuZipFolder_UsesActiveFolder confirms the ≡ row zips the
// same directory the other folder rows (New file / Rename / Delete)
// act on, producing a sibling archive.
func TestMenuZipFolder_UsesActiveFolder(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, root)
	a.setActiveFolder(sub)

	a.menuZipFolder()
	ev := waitForZipEvent(t, a)
	if ev.err != nil {
		t.Fatalf("zip err: %v", ev.err)
	}

	if _, err := os.Stat(filepath.Join(root, "sub.zip")); err != nil {
		t.Fatalf("sibling archive missing: %v", err)
	}
}

// TestZipFolderLabel mirrors the delete/rename label tests: bare at
// the project root (where it means "zip the whole project"), target
// suffix otherwise.
func TestZipFolderLabel(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a := newTestApp(t, root)

	if got := a.zipFolderLabel(); got != "Zip folder" {
		t.Errorf("root label = %q, want bare Zip folder", got)
	}
	a.setActiveFolder(sub)
	if got := a.zipFolderLabel(); got != "Zip folder (pkg/)" {
		t.Errorf("subfolder label = %q, want Zip folder (pkg/)", got)
	}
}
