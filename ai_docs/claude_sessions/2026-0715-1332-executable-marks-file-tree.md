# Session: Executable-file marks in the file tree (`ls -F` style `*`) + userconfig toggle

Session ID: 2e5bda0d-3506-4888-a68c-fcb8032e35d0
Date: 2026-07-15

## Goal

A design question that turned into a feature:

1. "In the file tree, does it make sense to show executable files in
   green? Would that conflict with the git features?"
2. → "Sketch the `*`-suffix version."
3. → "Add a userconfig toggle."

## Design conclusion (why not green)

The tree's row foreground is a **single winner-takes-all cascade** in
`drawNodeRow`: `base → dotfile(Muted) → active(Accent) → dirty(Modified)`.
Only one colour wins per row.

- **Hard conflict**: "executable → green" has to slot into that cascade.
  An executable that is also git-dirty could show green *or* amber
  (Modified), not both — and scripts are exactly the edit-and-run files,
  so you'd routinely lose the dirty signal on them.
- **Soft conflict**: green is already the app's git-added colour
  (`GitAdded`, used in the editor gutter + git panel). Introducing green
  in the tree reads as "new/untracked file", not "executable".
- The **glyph** already carries per-language colour (`icons.ColorFor`;
  `.sh` is green-ish), so recolouring the name too would double down and
  fight the git meaning.

Decision: don't recolour. Use an `ls -F` trailing `*` — a marker, not a
colour — so it rides the row's own style and never competes with the git
palette. (Mirrors the directory's trailing `/`.)

## What shipped

### 1. Executable detection + marker (`internal/filetree`)

- `Node.IsExec bool` — set in `reload()` from
  `DirEntry.Info().Mode()`, gated on `IsRegular() && mode&0o111 != 0`
  (symlinks report their own non-regular mode; dirs excluded). Set on
  **both** the new-node and survivor-pointer paths, so a `chmod +x`
  surfaces on the next 10s refresh without an editor restart.
- `drawNodeRow` appends `*` to an executable file's name, drawn in the
  row's own `rowStyle` (inherits dirty/muted/normal fg — no new colour).

### 2. userconfig toggle (`internal/userconfig`)

New `execmarks` preference, following the exact `autosave`/`termdock`
pattern:

- `Config.ExecMarks bool`, **default on** (`Defaults()`); `"execmarks"`
  JSON key with `on`/`off` parsing (case/whitespace-tolerant; typo →
  error, same contract as icons/autosave/termdock).
- `SaveExecMarks(path, on)` — read-modify-write through the shared
  `saveKey`, so unknown keys round-trip (forward-compat).

### 3. Render gate + menu toggle (`internal/app`)

- `Tree.ExecMarks bool` — the render-time gate. **Default on** in
  `filetree.New()` (so the `*` shows out of the box); `drawNodeRow`
  gained an `execMarks` param. `IsExec` is still computed on every
  reload regardless, so toggling re-renders **instantly, no reload**.
- `loadUserConfig` stamps `cfg.ExecMarks` onto the tree (next to the
  existing `IconsEnabled` stamp).
- New View-toggles menu row, placed right after "Show file explorer"
  (keeps the terminal pair contiguous + above the fold). Dynamic label
  "Hide/Show executable marks" (Show/Hide verb, matching the group).
- `menuToggleExecMarks` + `execMarksToggleLabel` live in the new
  `internal/app/execmarks.go` (one-file-per-feature, like autosave.go).
  Handler flips `tree.ExecMarks`, flashes, persists via `SaveExecMarks`.

Default is **on**, matching the always-on behaviour of the sketch — no
existing user sees a change; opting out is the new capability. No leader
key added (menu-only, empty shortcut column like the termdock row) —
CLAUDE.md leans against adding keys.

## Files touched

- `internal/filetree/filetree.go` + `_test.go` — `Node.IsExec`,
  `Tree.ExecMarks` (default on in `New`), reload detection,
  `drawNodeRow` param + gate.
- `internal/userconfig/userconfig.go` + `_test.go` — `ExecMarks`
  field/default, `execmarks` parse arm, `SaveExecMarks`, doc block.
- `internal/app/execmarks.go` + `_test.go` — new; toggle handler +
  label.
- `internal/app/app.go` — `loadUserConfig` stamp; menu row.
- `internal/app/app_test.go` — menu geometry pins updated for the added
  row: **47 items, height 60** (63 with custom actions), dividers
  `[2 7 12 16 20 23 33 36 49 57]`.
- `CLAUDE.md` — userconfig architecture-map row now lists `execmarks`.

## Test notes

- filetree: reload marks exec vs plain vs dir; `chmod +x` + Refresh
  flips `IsExec` on the reused pointer; render shows `run.sh*` / not
  `plain.txt*`; `ExecMarks=false` drops the `*` while `IsExec` stays
  set; dirty exec's `*` cell renders in `Modified` (marker-not-colour
  contract); `New()` defaults on. Exec-bit tests `t.Skip` on Windows
  (no Unix exec bit — the sanctioned skip case).
- userconfig: default-on, value table, invalid→error, save round-trip
  preserving `icons` + unknown keys.
- app: toggle flips flag + persists + preserves `icons`; round-trips
  back on; label naming; `loadUserConfig` applies `execmarks:off`. All
  redirect `XDG_CONFIG_HOME` to a temp dir — no test writes real config.
- Full suite green: `go test ./...` (13 pkgs) + `-race` on the three
  touched packages, `go vet`, `go build ./...`.
- Pre-existing gopls "modernize" hints (`rangeint`, `minmax`,
  `unusedparams` in modals.go/format.go) are unrelated noise — new test
  loops deliberately match the file's existing `for x:=0; x<w; x++`
  idiom.

## Follow-up ideas (not done)

- Symlink marker (`@`) and other `ls -F` classifiers — deliberately out
  of scope; only `*` for executables was added.
- A leader key for the toggle if it proves frequently used (currently
  menu-only by choice).
