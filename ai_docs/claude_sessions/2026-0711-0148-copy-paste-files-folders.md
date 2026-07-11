# Session: copy/paste files & folders (Cmd+C / Cmd+V + menu paths)

- **Session ID**: `a917a767-564e-4857-a13d-073b55e57a56`
- **Date**: 2026-07-11 (saved ~01:48)
- **Repo**: `~/projs/go/spice-edit` (module `github.com/rohanthewiz/r-ed`)
- **Branch**: `main`
- **Commits this session**: the copy/paste feature + this doc
- **Previous session**: `2026-0710-1552-autosave-builtin-gofmt-zip.md`

## What this session was

One user ask: "Give me the ability to copy and paste folders and files
with standard Cmd+C and Cmd+V". Delivered as an internal **file
clipboard** (Copy arms a path; Paste duplicates it into a folder under
a collision-free name), surfaced through the ≡ menu, the tree
right-click menu, and Cmd+C/Cmd+V where the terminal delivers them.

## The Cmd-key reality (why this isn't just a keybinding)

- Terminals that speak the **kitty keyboard protocol** (kitty, Ghostty,
  WezTerm, iTerm2 with CSI-u) pass Cmd/Super through; tcell v2.13.9
  reports it as `ModMeta` (`input.go:607` maps kitty's Super →
  `ModMeta`). Classic macOS Terminal + tmux swallows Cmd entirely.
- So Cmd+C/Cmd+V is a **convenience layer**; the ≡ menu and tree
  context menu are the guaranteed paths (house rule: every file action
  reachable from ≡ first).
- This does NOT violate the no-Ctrl-shortcuts rule: Cmd never collides
  with tmux prefixes or terminal flow control, which is what that rule
  actually protects.

## Design

### New `internal/app/copypaste.go`

**State (on App, in app.go):**
- `fileClipPath string` — absolute path armed by Copy; empty = nothing
  copied. Copy is **by-reference** (nothing read until paste, like
  Finder), so edits between Copy and Paste are included.
- `clipKind clipboardKind` (`clipNone`/`clipText`/`clipFile`) — which
  clipboard (text selection vs file) was written **most recently**, so
  Cmd+V routes last-write-wins like a system clipboard.
  `copySelection` now sets `clipText`.

**Backend (pure functions):**
- `copyFileContents(src, dst, perm)` — O_EXCL create (never-clobber
  contract, same as `createEmptyFile`/`renameFile`), preserves perms.
- `copyTree(src, dst)` — recursive; symlinks recreated as links (not
  followed — could loop or pull in outside content), sockets/devices
  skipped. Same rules as the zip walker.
- `uniquePastePath(dir, base, isDir)` — Finder-style collision names:
  `main.go` → `main copy.go` → `main copy 2.go`. Dotfiles
  (`.gitignore` is all "extension" — empty stem) and directories get
  the suffix appended whole.
- `pasteIntoOwnSubtree(src, destDir)` — recursion guard; trailing-
  separator prefix match so `/proj/foo` doesn't catch `/proj/foobar`
  (same trick as `tabPathRemoved`).

**App glue:**
- `copyToFileClip(path)` — validates source exists, arms clip, flashes
  "Copied X — Paste to duplicate".
- `startPaste(destDir)` — all refusals synchronous on the main loop
  (specific flashes): clip empty; source gone (→ **disarms** the
  clip); dest folder gone; folder-into-own-subtree. Then computes the
  unique dest and copies in a **goroutine** posting `pasteDoneEvent`
  (same async pattern as zip/format/custom actions). On copy failure
  the goroutine `os.RemoveAll`s the partial dest (safe — dest was
  guaranteed fresh).
- `handlePasteDone` — flash + `workspaceChanged()`.
- `pasteTargetDir()` — active folder if it still exists, else project
  root (resolved via `tree.Root.Path` since `a.rootDir` may be ".").

**Surfaces:**
- ≡ File actions group: `Copy file` (hasFileTab), `Copy folder (sub/)`
  (hasActiveSubfolder + dynamic label), `Paste <name>` (hasFileClip +
  dynamic label showing the armed basename — distinguishes it from the
  text-clipboard "Paste" row). Palette picks these up automatically.
- Tree right-click: `Copy` on non-root nodes (root gated out with
  Rename/Delete — every paste destination is inside it, so a root copy
  is unusable); `Paste` appended **only when armed** (no permanently
  dead row in the small popup), root allowed. `ctxPaste` on a folder
  pastes into it (auto-expands, like `ctxNewFile`); on a file, into
  its parent.
- Keys (in `handleKey`, after the Esc-leader block): `KeyRune` +
  `ModMeta` → 'c' `cmdCopy`, 'v' `cmdPaste`.
  - `cmdCopy`: text selection wins (standard) → active tab's file →
    active subfolder → "Nothing to copy" flash.
  - `cmdPaste`: `clipKind == clipFile` → `startPaste(pasteTargetDir())`,
    else `pasteClipboard()` (text).

## Files touched

- `internal/app/copypaste.go` — NEW, everything above.
- `internal/app/copypaste_test.go` — NEW, 14 tests: unique-name
  policy (incl. dotfile/dir cases), perms + O_EXCL, recursive copy
  with symlink, recursion-guard boundaries, arm/validate, async happy
  path, collision suffix, self-paste refusal, source-gone disarm,
  Cmd+C selection-beats-file, Cmd+V routing, handleKey ModMeta
  dispatch (+ plain 'c' still types), context Paste-only-when-armed,
  ctxPaste-on-file-targets-parent. `waitForPasteEvent` mirrors
  `waitForZipEvent`.
- `internal/app/app.go` — struct fields, `clipKind = clipText` in
  `copySelection`, ModMeta dispatch, 3 menu rows, `pasteDoneEvent`
  routing in `handleEvent`.
- `internal/app/modals.go` — `openTreeContext` Copy/Paste rows.
- Test pins updated for the 3 new menu rows (layout 49→52 tall,
  37→40 items, dividers `{2,7,11,15,25,29,42,47,49}`; custom-actions
  height 52→55; two tests needed taller sim screens, 50→60).

## Verification

- `go test -race ./...` — all green.
- `make build` — clean.

## Gotchas for next time

- Any new menu row moves the geometry pins in
  `TestMenuLayout_NoCustomActions`, `TestMenuLayout_WithCustomActions`,
  `TestMenuModalRect_Centered` (needs window taller than the layout),
  and `TestMenuModalRect_ClampsToWindowHeight` (its "tall window"
  branch must exceed the layout height).
- The tree context row sets are pinned exactly in
  `TestOpenTreeContext_{Folder,File,Root}` — adding a row means
  updating `wantLabels` there.
- `contextMenuWidth` is 19 — context labels must stay short.
- If a "Cut" (move) is ever added: reuse the clip + `renameFile`
  instead of copy+delete, and remember open-tab path rewriting
  (`doRenameFolder` shows the pattern).
