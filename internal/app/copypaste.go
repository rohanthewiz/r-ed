// =============================================================================
// File: internal/app/copypaste.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-11
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Copy / paste for files and folders. Copy arms an internal "file
// clipboard" (just the source path — nothing is read until paste);
// Paste duplicates the source into a destination folder under a
// collision-free name ("name copy.ext", "name copy 2.ext", …), so the
// operation is never destructive.
//
// Surfaces, per the house rule that every file action must be
// reachable from the ≡ menu first:
//
//   • ≡ → "Copy file" / "Copy folder (sub/)"  — active tab / active folder
//   • ≡ → "Paste <name>"                      — into the active folder
//   • tree right-click → "Copy" / "Paste"     — redundant shortcut
//   • Cmd+C / Cmd+V                           — where the terminal delivers
//     them (kitty keyboard protocol → tcell ModMeta); terminals that
//     swallow Cmd still have the menu paths above.
//
// Cmd+C/Cmd+V share the key with text copy/paste, so App.clipKind
// tracks which clipboard (text selection vs file) was armed most
// recently and Cmd+V pastes that one — the same last-write-wins
// behavior a system clipboard has.
//
// Big folders can take seconds to copy, so the paste itself runs in a
// goroutine and posts a pasteDoneEvent back to the main loop — the
// same pattern as zips, formatters, and custom actions.

package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/filetree"
)

// clipboardKind identifies which internal clipboard was armed most
// recently, so a Cmd+V can route to the right paste action.
type clipboardKind int

const (
	clipNone clipboardKind = iota
	clipText               // copySelection / cutSelection wrote clipBuf
	clipFile               // copyToFileClip armed fileClipPath
)

// pasteDoneEvent is posted by the paste goroutine when the copy is
// finished (or failed). Carries the destination so the success flash
// can name the file or folder that appeared.
type pasteDoneEvent struct {
	when time.Time
	dest string
	err  error
}

// When satisfies the tcell.Event interface.
func (e *pasteDoneEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Backend: pure functions, no App state.
// -----------------------------------------------------------------------------

// copyFileContents copies the regular file src to a new file dst with
// the given permissions. dst is opened O_EXCL — the caller picks a
// collision-free name via uniquePastePath, and this keeps the same
// never-clobber contract as createEmptyFile / renameFile even if
// something races us to the name.
func copyFileContents(src, dst string, perm os.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// copyTree recursively copies the file or directory at src to dst,
// which must not exist yet. Symlinks are recreated as links (not
// followed) — following could loop, or silently pull in content from
// outside the tree being copied — and sockets/devices are skipped,
// mirroring writeZipEntry's rules for the same reasons. On failure the
// caller is expected to remove the partial dst (startPaste does).
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		return copyFileContents(src, dst, info.Mode().Perm())
	default:
		return nil
	}
}

// uniquePastePath picks a collision-free destination for pasting base
// into dir. The base name itself is used when free; otherwise a
// Finder-style " copy" / " copy N" is inserted before the extension
// ("main.go" → "main copy.go"). Directories and dotfiles get the
// suffix appended whole — splitting ".gitignore" on its "extension"
// would leave an empty stem, and "my.dir copy" reads better than
// "my copy.dir" for a folder.
func uniquePastePath(dir, base string, isDir bool) string {
	cand := filepath.Join(dir, base)
	if _, err := os.Lstat(cand); err != nil {
		return cand
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if isDir || stem == "" {
		stem, ext = base, ""
	}
	for n := 1; ; n++ {
		name := stem + " copy" + ext
		if n > 1 {
			name = fmt.Sprintf("%s copy %d%s", stem, n, ext)
		}
		cand = filepath.Join(dir, name)
		if _, err := os.Lstat(cand); err != nil {
			return cand
		}
	}
}

// pasteIntoOwnSubtree reports whether destDir is src itself or lives
// inside it. Pasting a folder into its own subtree would make the copy
// walk chase the destination it is writing — the classic infinite
// duplication bug — so startPaste refuses it. The trailing separator
// keeps /proj/foo from matching /proj/foobar, same as tabPathRemoved.
func pasteIntoOwnSubtree(src, destDir string) bool {
	src = filepath.Clean(src)
	destDir = filepath.Clean(destDir)
	if destDir == src {
		return true
	}
	return strings.HasPrefix(destDir, src+string(filepath.Separator))
}

// -----------------------------------------------------------------------------
// App glue: clipboard state, async paste, main-loop completion.
// -----------------------------------------------------------------------------

// copyToFileClip arms the file clipboard with path. Nothing is copied
// yet — paste reads the source at paste time, so edits made between
// Copy and Paste are included, matching how Finder's Cmd+C behaves.
func (a *App) copyToFileClip(path string) {
	if _, err := os.Lstat(path); err != nil {
		a.flash(fmt.Sprintf("Copy failed: %s no longer exists", filepath.Base(path)))
		return
	}
	a.fileClipPath = path
	a.clipKind = clipFile
	a.flash(fmt.Sprintf("Copied %s — Paste to duplicate", filepath.Base(path)))
}

// hasFileClip is the menu predicate for the Paste row: true when a
// file or folder has been copied this session. Staleness (source
// deleted after Copy) is caught at paste time with a specific flash
// rather than silently disabling the row.
func (a *App) hasFileClip() bool { return a.fileClipPath != "" }

// pasteTargetDir resolves where a menu / Cmd+V paste should land: the
// active folder when it still exists, else the project root. Resolved
// through the tree root (always absolute) rather than a.rootDir, which
// keeps the user's verbatim argument ("." in the common case) — the
// self-subtree guard compares clean absolute paths.
func (a *App) pasteTargetDir() string {
	root := a.rootDir
	if a.tree != nil && a.tree.Root != nil {
		root = a.tree.Root.Path
	}
	folder := a.activeFolder
	if folder == "" {
		return root
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return root
	}
	return folder
}

// startPaste validates the pending file-clipboard paste into destDir
// and kicks off the copy goroutine. All refusals happen here on the
// main loop so the user gets an immediate, specific flash instead of
// an async failure.
func (a *App) startPaste(destDir string) {
	src := a.fileClipPath
	if src == "" {
		a.flash("Nothing to paste — Copy a file or folder first")
		return
	}
	info, err := os.Lstat(src)
	if err != nil {
		// The copied source vanished; disarm so the Paste row doesn't
		// keep offering a paste that can never succeed.
		a.fileClipPath = ""
		if a.clipKind == clipFile {
			a.clipKind = clipNone
		}
		a.flash(fmt.Sprintf("Paste failed: %s no longer exists", filepath.Base(src)))
		return
	}
	if dinfo, derr := os.Stat(destDir); derr != nil || !dinfo.IsDir() {
		a.flash("Paste failed: destination folder no longer exists")
		return
	}
	if info.IsDir() && pasteIntoOwnSubtree(src, destDir) {
		a.flash("Can't paste a folder into itself")
		return
	}
	dest := uniquePastePath(destDir, filepath.Base(src), info.IsDir())
	a.flash("Pasting " + filepath.Base(src) + "…")
	scr := a.screen
	go func() {
		err := copyTree(src, dest)
		if err != nil {
			// Remove the partial copy so a failed paste can't leave a
			// half-populated folder wearing the destination's name.
			// Safe: uniquePastePath guaranteed dest didn't exist before.
			_ = os.RemoveAll(dest)
		}
		_ = scr.PostEvent(&pasteDoneEvent{when: time.Now(), dest: dest, err: err})
	}()
}

// handlePasteDone lands the goroutine's result on the main loop: flash
// the outcome and re-sync the workspace so the new file or folder
// appears in the tree and finder without waiting for the 10-second tick.
func (a *App) handlePasteDone(e *pasteDoneEvent) {
	if e == nil {
		return
	}
	if e.err != nil {
		a.flash("Paste failed: " + e.err.Error())
		return
	}
	a.workspaceChanged()
	a.flash("Pasted " + filepath.Base(e.dest))
}

// -----------------------------------------------------------------------------
// Main menu actions.
// -----------------------------------------------------------------------------

// menuCopyFile arms the file clipboard with the active tab's file.
func (a *App) menuCopyFile() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" {
		return
	}
	a.copyToFileClip(tab.Path)
}

// menuCopyFolder arms the file clipboard with the editor's active
// folder — the same target the Rename / Delete / Zip folder rows act
// on. The project root is gated out by hasActiveSubfolder: a root copy
// could never be pasted anywhere (every destination is inside it).
func (a *App) menuCopyFolder() {
	a.closeMenu()
	if !a.hasActiveSubfolder() {
		return
	}
	a.copyToFileClip(a.activeFolder)
}

// copyFolderLabel is the dynamic label hook for the Copy Folder menu
// row. Same shape as zipFolderLabel: bare label when no subfolder is
// active, "(subdir/)" suffix otherwise so the user sees the target
// before clicking.
func (a *App) copyFolderLabel() string {
	folder := a.activeFolder
	if folder == "" || folder == a.rootDir {
		return "Copy folder"
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
	return "Copy folder" + suffix
}

// menuPasteItem pastes the file clipboard into the active folder (or
// the project root when none is active).
func (a *App) menuPasteItem() {
	a.closeMenu()
	a.startPaste(a.pasteTargetDir())
}

// pasteItemLabel is the dynamic label hook for the Paste row. Shows the
// armed source's basename ("Paste main.go") so the row is unambiguous
// next to the text-clipboard "Paste" row in the Clipboard group; falls
// back to the generic label while nothing is armed (the row is disabled
// then anyway, but it still renders dimmed).
func (a *App) pasteItemLabel() string {
	if a.fileClipPath == "" {
		return "Paste file/folder"
	}
	base := filepath.Base(a.fileClipPath)
	const maxLen = maxLabelSuffix
	if runeLen(base) > maxLen {
		// Keep the tail — the name and extension are the informative part.
		r := []rune(base)
		base = "…" + string(r[len(r)-maxLen+1:])
	}
	return "Paste " + base
}

// -----------------------------------------------------------------------------
// Tree context-menu actions.
// -----------------------------------------------------------------------------

// ctxCopy arms the file clipboard with the node the user right-clicked.
// The project root is gated out at openTreeContext, same as Rename /
// Delete — a root copy has nowhere legal to paste.
func ctxCopy(a *App, n *filetree.Node) {
	a.copyToFileClip(n.Path)
}

// ctxPaste pastes the file clipboard relative to the node the user
// right-clicked: into the node itself for folders (auto-expanding so
// the result is visible, same as ctxNewFile), or into the parent
// folder for files.
func ctxPaste(a *App, n *filetree.Node) {
	dest := n.Path
	if n.IsDir {
		if !n.Expanded {
			a.tree.Toggle(n)
		}
	} else {
		dest = filepath.Dir(n.Path)
	}
	a.startPaste(dest)
}

// -----------------------------------------------------------------------------
// Cmd+C / Cmd+V dispatch.
// -----------------------------------------------------------------------------

// cmdCopy is what Cmd+C means when the terminal delivers it: copy the
// text selection when there is one (the standard reading), otherwise
// arm the file clipboard with the active tab's file, falling back to
// the active folder for tab-less sessions. Copying is harmless either
// way — nothing happens until a paste — so the fallback chain can be
// generous without risking surprise mutations.
func (a *App) cmdCopy() {
	if a.hasSelection() {
		a.copySelection()
		return
	}
	if tab := a.activeTabPtr(); tab != nil && tab.Path != "" {
		a.copyToFileClip(tab.Path)
		return
	}
	if a.hasActiveSubfolder() {
		a.copyToFileClip(a.activeFolder)
		return
	}
	a.flash("Nothing to copy — select text or open a file")
}

// cmdPaste routes Cmd+V by whichever clipboard was armed most
// recently: a copied file/folder pastes into the active folder, text
// pastes at the cursor. pasteClipboard already handles the
// empty-clipboard case with its own hint flash.
func (a *App) cmdPaste() {
	if a.clipKind == clipFile {
		a.startPaste(a.pasteTargetDir())
		return
	}
	a.pasteClipboard()
}

// Compile-time check that pasteDoneEvent really is a tcell.Event.
var _ tcell.Event = (*pasteDoneEvent)(nil)
