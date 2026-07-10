// =============================================================================
// File: internal/app/zipops.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-09
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Zip-a-file / zip-a-folder. The archive is built with the stdlib's
// archive/zip — no shelling out to a `zip` binary (which macOS and
// minimal Linux boxes disagree about) and no CGO, keeping the
// single-static-binary promise.
//
// Surfaces, per the house rule that every file action must be
// reachable from the ≡ menu first:
//
//   • ≡ → "Zip file"            — the active tab's file
//   • ≡ → "Zip folder (sub/)"   — the active folder (project root allowed)
//   • tree right-click → "Zip"  — redundant shortcut for either kind
//
// The archive lands next to the source (<name>.zip). Zipping the
// project root would put a sibling zip *outside* the tree — invisible
// and confusing — so that one case writes inside the root instead,
// and the walk skips the in-progress archive so it never eats itself.
//
// Compression runs in a goroutine (a big folder can take seconds) and
// posts a zipDoneEvent back to the main loop — the same pattern as
// custom actions and formatters, so UI state stays main-loop-only.

package app

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/filetree"
)

// zipDoneEvent is posted by the zip goroutine when the archive is
// finished (or failed). Carries the destination so the success flash
// can name the file that appeared.
type zipDoneEvent struct {
	when time.Time
	dest string
	err  error
}

// When satisfies the tcell.Event interface.
func (e *zipDoneEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Backend: pure functions, no App state.
// -----------------------------------------------------------------------------

// zipDest returns where the archive for src should be written: a
// sibling <name>.zip in the common case. When src IS the project
// root, the sibling would land outside the tree where the user can't
// see (or maybe even write) it, so the archive goes inside the root
// instead — createZip skips it during the walk.
func zipDest(src, root string) string {
	if filepath.Clean(src) == filepath.Clean(root) {
		return filepath.Join(src, filepath.Base(src)+".zip")
	}
	return src + ".zip"
}

// createZip archives src (file or directory) into a new zip at dest.
// Directory entries are rooted at src's basename (zipping /proj/sub
// yields sub/…), which is what every desktop archiver produces and
// what users expect on extraction — bare top-level entries "explode"
// into the extraction directory.
//
// The destination is opened O_EXCL so an existing archive is never
// clobbered (same refusal contract as createEmptyFile / renameFile),
// and a partial archive is removed on failure so an interrupted zip
// can't leave a corrupt file that a later O_EXCL then refuses to
// replace.
func createZip(src, dest string) (err error) {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			// Best-effort cleanup — the error the user sees is the
			// one that broke the archive, not the unlink.
			_ = os.Remove(dest)
		}
	}()

	zw := zip.NewWriter(out)
	base := filepath.Base(src)
	if !info.IsDir() {
		if err = writeZipEntry(zw, base, src, info); err != nil {
			return err
		}
		return zw.Close()
	}

	destClean := filepath.Clean(dest)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Never archive the archive: when dest lives inside src
		// (project-root zips), the walk would otherwise find the
		// half-written zip and recurse into reading it.
		if filepath.Clean(path) == destClean {
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		name := base
		if rel != "." {
			// Zip entry names are always forward-slashed per spec,
			// regardless of the host OS separator.
			name = base + "/" + filepath.ToSlash(rel)
		}
		fi, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		return writeZipEntry(zw, name, path, fi)
	})
	if err != nil {
		// Close the writer so the file handle is released before the
		// deferred cleanup unlinks the partial archive.
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// writeZipEntry appends one filesystem object to the archive under
// the given entry name. Directories become explicit "name/" entries
// so empty folders survive a round-trip; symlinks are stored as
// links (target path as content, link mode preserved) rather than
// followed — following could loop, or silently inline a file from
// outside the tree being zipped. Sockets/devices are skipped: they
// have no meaningful archive representation.
func writeZipEntry(zw *zip.Writer, name, path string, fi os.FileInfo) error {
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	hdr.Name = name

	switch {
	case fi.IsDir():
		hdr.Name += "/"
		_, err = zw.CreateHeader(hdr)
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return readErr
		}
		hdr.Method = zip.Store
		w, createErr := zw.CreateHeader(hdr)
		if createErr != nil {
			return createErr
		}
		_, err = w.Write([]byte(target))
		return err
	case fi.Mode().IsRegular():
		hdr.Method = zip.Deflate
		w, createErr := zw.CreateHeader(hdr)
		if createErr != nil {
			return createErr
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	default:
		return nil
	}
}

// -----------------------------------------------------------------------------
// App glue: async run + main-loop completion.
// -----------------------------------------------------------------------------

// startZip validates the destination and kicks off the archive
// goroutine. The exists-check happens here on the main loop (not
// inside createZip's O_EXCL, which also guards it) so the user gets
// an immediate, specific refusal instead of an async failure flash.
func (a *App) startZip(src string) {
	root := a.rootDir
	if a.tree != nil && a.tree.Root != nil {
		root = a.tree.Root.Path
	}
	dest := zipDest(src, root)
	if _, err := os.Stat(dest); err == nil {
		a.flash(fmt.Sprintf("Zip failed: %s already exists", filepath.Base(dest)))
		return
	}
	a.flash("Zipping " + filepath.Base(src) + "…")
	scr := a.screen
	go func() {
		err := createZip(src, dest)
		_ = scr.PostEvent(&zipDoneEvent{when: time.Now(), dest: dest, err: err})
	}()
}

// handleZipDone lands the goroutine's result on the main loop: flash
// the outcome and re-sync the workspace so the new .zip appears in
// the tree and finder without waiting for the 10-second tick.
func (a *App) handleZipDone(e *zipDoneEvent) {
	if e == nil {
		return
	}
	if e.err != nil {
		a.flash("Zip failed: " + e.err.Error())
		return
	}
	a.workspaceChanged()
	a.flash("Created " + filepath.Base(e.dest))
}

// menuZipFile archives the active tab's file to a sibling .zip.
func (a *App) menuZipFile() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" {
		return
	}
	a.startZip(tab.Path)
}

// menuZipFolder archives the editor's active folder — the same
// target the New File / Rename / Delete folder rows act on. Unlike
// Delete, the project root is allowed: zipping the whole project is
// a legitimate ask and the operation is read-only on the source.
func (a *App) menuZipFolder() {
	a.closeMenu()
	// Resolve through the tree root (always absolute) rather than
	// a.rootDir, which keeps the user's verbatim argument ("." in the
	// common case) — zipDest's root comparison needs clean absolutes.
	root := a.rootDir
	if a.tree != nil && a.tree.Root != nil {
		root = a.tree.Root.Path
	}
	folder := a.activeFolder
	if folder == "" {
		folder = root
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		// Active folder vanished externally — fall back to the root
		// rather than flashing a confusing async failure.
		folder = root
	}
	a.startZip(folder)
}

// zipFolderLabel is the dynamic label hook for the Zip Folder menu
// row. Same shape as deleteFolderLabel: bare "Zip folder" at the
// project root (where it means "zip the whole project"), a
// "(subdir/)" suffix otherwise so the user sees the target before
// clicking.
func (a *App) zipFolderLabel() string {
	folder := a.activeFolder
	if folder == "" || folder == a.rootDir {
		return "Zip folder"
	}
	rel := a.relativeFolderLabel(folder)
	const maxLen = maxLabelSuffix
	suffix := " (" + rel + ")"
	if runeLen(suffix) > maxLen {
		keep := maxLen - len(" (…)")
		if keep < 4 {
			keep = 4
		}
		if keep < len(rel) {
			rel = "…" + rel[len(rel)-keep:]
		}
		suffix = " (" + rel + ")"
	}
	return "Zip folder" + suffix
}

// ctxZip archives the file or folder the user right-clicked in the
// tree. Redundant shortcut for the ≡ rows, per the house rule.
func ctxZip(a *App, n *filetree.Node) {
	a.startZip(n.Path)
}

// Compile-time check that zipDoneEvent really is a tcell.Event.
var _ tcell.Event = (*zipDoneEvent)(nil)
