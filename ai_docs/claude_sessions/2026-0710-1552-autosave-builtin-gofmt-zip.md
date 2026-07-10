# Session: auto-save, builtin goimports/gofmt on save, zip file/folder

- **Session ID**: `0d35b81d-f723-49dc-89c8-7e1d66d926a6`
- **Date**: 2026-07-09 → 2026-07-10 (saved ~15:52)
- **Repo**: `~/projs/go/spice-edit` (module `github.com/rohanthewiz/r-ed`)
- **Branch**: `main`
- **Commits this session**: `03fd4e0` (auto-save + builtin Go
  format-on-save), `862cebc` (zip file/folder), plus this doc — all
  pushed
- **Previous session**: `2026-0709-2039-stage-unstage-stash-and-scrollable-menu.md`

## What this session was

Two user asks. First: "Add auto-import, auto-save, and go fmt on
save" — delivered as two features, since goimports covers both
auto-import AND gofmt formatting. Second (arrived mid-task): a
separate zip-a-file-or-folder feature, committed separately.

## Feature 1 — builtin Go formatting on save (`03fd4e0`)

Extended the existing format-on-save pipeline (`internal/app/format.go`
+ `internal/format/`) instead of adding a new system.

- New `internal/format/builtin.go`: `BuiltinCommandFor(path)` returns
  `[goimports -w <abs>]` for `.go` files, falling back to
  `[gofmt -w <abs>]`, nil when neither is on PATH (silent degradation
  — same contract as gopls). `lookPath` is a package var so tests
  stub PATH resolution.
- **No trust prompt** for the builtin: the argv is hardcoded in the
  binary, not repo-supplied config. Precedence:
  project `format.json` "go" entry (trust-gated) → builtin →
  global-defaults install offer (now skipped for Go — the builtin
  makes it redundant).
- `runFormatOnSave(idx)` grew a `quiet bool` param, threaded through
  `runWithTrust` / `execFormatter` / `formatDoneEvent.quiet` /
  `handleFormatDone`. Quiet mode (used by auto-save): no trust
  prompt (unknown trust silently skipped until next explicit Save),
  no install offer, no flashes for run/success/failure — but a clean
  buffer still reloads, and a reload *failure* still flashes.
- Flash label is now `filepath.Base(argv[0])` so the builtin's
  resolved path shows as "goimports", not `/opt/…/goimports`.

## Feature 2 — auto-save (`03fd4e0`)

New `internal/app/autosave.go`, mirroring the LSP didChange debounce:

- `autoSaveAfterEvent` runs after every dispatch (next to
  `lspAfterEvent`), compares the **sum of all tabs' EditRevs**
  (`autoSaveSig`) and (re)arms a 2s `time.AfterFunc` that posts
  `autoSaveEvent`. Handler saves every eligible dirty tab.
- Eligibility (`autoSavable` + `diskChangedSinceLoad`): dirty, has a
  path, not an image tab, not `DiskGone`, and the on-disk mtime is
  not newer than `Tab.Mtime` — auto-save must never clobber an
  external edit or resurrect a deleted file; explicit Save remains
  the overwrite path.
- Saves are **silent** (no flash — the tab's dirty dot clearing is
  the feedback) and run format-on-save in quiet mode. While any
  modal/menu is open the handler re-arms instead of saving (a save
  landing mid "save or discard?" dialog would invalidate it).
- `autoSaveTab` deliberately does NOT route through `saveTabAt`
  (which flashes and prompts); it shares the same follow-ups:
  `requestFileDiff`, `lspDidSave`, one `refreshGitStatus` per batch.
- **Default ON.** ≡ menu toggle ("Disable/Enable auto-save", in the
  Tab actions group) persists via new `userconfig.SaveAutoSave`,
  which does a raw-map read-modify-write so unknown/hand-set JSON
  keys survive; malformed config refuses the write rather than
  eating the user's file. `{"autosave": "on"|"off"}`, absent = on
  (string not bool, so absent ≠ false).

## Feature 3 — zip file/folder (`862cebc`)

New `internal/app/zipops.go`, pure stdlib `archive/zip` (no zip
binary, no CGO):

- ≡ menu first (house rule): "Zip file" (active tab, `hasFileTab`)
  and "Zip folder (subdir/)" (active folder, dynamic label like
  rename/delete-folder; **project root allowed** — zipping the whole
  project is legitimate and read-only on the source). Tree
  right-click gets a redundant "Zip" row for files, folders, root.
- Placement: sibling `<name>.zip`; zipping the root writes *inside*
  the root (a sibling would land outside the tree) and the walk
  skips the in-progress archive by path so it can't eat itself.
- Refuses to clobber an existing archive (O_EXCL + an immediate
  main-loop stat check for a specific flash); removes the partial
  archive on failure so a corrupt zip can't block retries.
- Folder entries are rooted at the folder's basename (`sub/…`),
  empty dirs get explicit `name/` entries, symlinks are stored as
  links (target as content, Store method), sockets/devices skipped.
- Runs in a goroutine posting `zipDoneEvent` → `handleZipDone`
  flashes and calls `workspaceChanged()`.

## Test-infrastructure changes (important for future sessions)

- `newTestApp` now stubs the app-level `builtinCommandFor` var to
  nil (with cleanup restore) — same spirit as `a.lsp.dead = true` —
  so no test save ever execs the dev machine's goimports/gofmt.
  Tests that want the builtin call `stubBuiltinFormatter(t, argv)`
  (in `format_test.go`).
- Menu geometry pins in `TestMenuLayout_NoCustomActions` moved twice:
  now height 49, 37 items, dividers `[2,7,11,15,25,29,39,44,46]`
  (auto-save row +1 in group 1; zip rows +2 in the File group).
  `WithCustomActions` height is 52.
- Tree-context label pins in `modals_test.go` now include "Zip".
- `TestRunFormatOnSave_NoConfigIsNoop` was re-pointed at a `.txt`
  file — the "no config → no exec" promise now explicitly excludes
  Go, which is the deliberate builtin exception.

## Gotchas / decisions worth remembering

- **VS Code-style tension resolved differently**: VS Code skips
  formatting on auto-save; here auto-save DOES format (the user
  asked for the features together) but quietly. The known trade-off:
  after a 2s pause the buffer may reload with shifted lines
  (goimports adding an import). `handleFormatDone` only reloads
  clean buffers, so typing during the format run keeps your edits.
- With auto-save on, the dirty-close dialog rarely appears and
  explicit Save is rare — inherent to auto-save semantics, toggle
  restores old behavior.
- CLAUDE.md gained sections for both patterns (format precedence +
  auto-save) and architecture-map lines for `autosave.go`,
  `zipops.go`, `format.go`, `internal/format/`, and the userconfig
  writer.
- Two pushes to main → release workflow auto-bumped patch twice.

## Possible next work

- Auto-save could skip the format pass on files with active LSP
  syntax errors (avoid pointless goimports failures on broken code).
- Unzip / extract an archive from the tree context menu.
- A status-bar indicator for auto-save state (e.g. "AS" glyph)
  instead of relying on the menu label.
