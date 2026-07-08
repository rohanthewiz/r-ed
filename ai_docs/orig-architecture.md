# SpiceEdit — Original Architecture (as forked)

Snapshot of the codebase architecture as of v0.0.40 (2026-07-08), before the
mini-IDE evolution begins. This is the "what we inherited" reference — consult
it when deciding where a new feature should live or which debt a refactor is
paying down.

## Scale and shape

- ~11k lines of production Go, ~12k lines of tests, 13 packages.
- Three direct dependencies: `tcell/v2` (terminal), `chroma/v2` (syntax
  highlighting), `go-gitignore`. Everything else stdlib. No CGO.
- Dependency direction is clean and one-way: `main.go` → `internal/app` →
  leaf packages. No leaf package imports `app`.
- The CLAUDE.md architecture map predates roughly half the code: fuzzy finder,
  in-file find, undo/redo, git status, format-on-save + trust, custom actions,
  comment toggle, image preview, and leader keys were all added after it.

## Package map

| Package | Size | Role |
|---|---|---|
| `internal/app/app.go` | 2,518 | Event loop, App god-struct, layout math, menu, key/mouse routing, draw pipeline |
| `internal/app/modals.go` | 1,013 | Prompt / confirm / dirty-close / tree-context modals + shared draw helpers |
| `internal/app/formmodal.go` | 475 | Multi-field form modal (text + select fields), used by custom-action prompts |
| `internal/app/fileops.go` | 638 | New/rename/delete file & folder, path-copy; dual-wired to main menu and tree context |
| `internal/app/finder.go` | 465 | Project fuzzy-finder modal UI (index lives in `internal/finder`) |
| `internal/app/find.go` | 297 | In-file find bar UI (match state lives on `editor.Tab`) |
| `internal/app/format.go` | 411 | Format-on-save bridge: trust prompts, install offers, async exec |
| `internal/app/gitstatus.go` | 204 | Shells out to git for branch + porcelain status |
| `internal/app/leader.go` | 66 | Esc-then-rune hotkey table (s/u/r/w/q/n/t//,f,p) |
| `internal/app/actionvars.go` | 142 | Editor-state vars (`FILE`, `PROJECT_ROOT`, …) exported to custom actions |
| `internal/editor/buffer.go` | 196 | `Buffer{Lines []string}`, rune-indexed `Position`, edit primitives |
| `internal/editor/tab.go` | 736 | Tab = buffer + cursor + selection + scroll + styles + undo + find state; renders itself |
| `internal/editor/undo.go` | 231 | Snapshot undo, 500ms coalescing by edit-kind, 500-entry cap |
| `internal/editor/find.go` | 189 | Case-insensitive substring search, wrap-around, non-overlapping |
| `internal/editor/indent.go` | 168 | Indent-style detection + tab-stop visual-column math (TabStop=4) |
| `internal/editor/comment.go` | 211 | Line-comment toggle, ~60 extensions mapped to markers |
| `internal/editor/image.go` | 206 | PNG/JPEG/GIF preview via Unicode half-blocks (▀), tmux-safe |
| `internal/editor/highlight.go` | 118 | Chroma → per-rune `[][]tcell.Style` grid, whole-file recompute |
| `internal/filetree/filetree.go` | 437 | Lazy tree, identity-preserving reload, row-slice hit-testing |
| `internal/finder/` | 583 | Index (git ls-files fast path / gitignore walk), fzy-style scorer |
| `internal/customactions/` | 332 | `~/.config/spiceedit/actions.json`, `sh -c` actions with prompts + run log |
| `internal/format/` | 579 | Per-project `.spiceedit/format.json` argv registry, global defaults, hash-keyed trust |
| `internal/icons/` | 390 | Nerd Font detection (fc-list, font-dir walk) + per-language glyphs/colors |
| `internal/theme/` | 108 | Single hardcoded Tokyo Night-ish theme; recompile to restyle |
| `internal/clipboard/` | 50 | OSC 52 copy to /dev/tty, tmux passthrough wrap; copy-only |
| `internal/spiceconfig/` | 133 | `config.json`, one key today: `icons: auto\|on\|off` |
| `main.go` | 149 | `resolveArgs` (pure): dir / file / missing-path-as-new-file / version / help |

## Runtime architecture

### Event loop (`app.go:591`)
Classic blocking loop: `PollEvent → handleEvent → draw → Show`, exits on
`a.quit`. **Full `Clear()` + redraw on every event** — no dirty regions or
damage tracking. `handleEvent` (`app.go:609`) type-switches tcell events plus
five custom event types: `autoScrollEvent`, `treeRefreshEvent`,
`customActionDoneEvent`, `formatDoneEvent`, `finderRebuiltEvent`.

### Async pattern (the best structural decision here)
Background goroutines never mutate UI state. They post custom tcell events
onto the queue; the main loop reconciles. Used by: drag auto-scroll, 10s tree
refresh, formatter runs, custom actions, finder index rebuilds. This is
exactly the shape an LSP client, build runner, or test watcher needs.

### Input routing
`handleKey` (`app.go:873`) is a fixed precedence if-chain:
prompt → confirm → dirty-close → form → tree context → find bar → finder →
Esc/leader handling → menu → active tab. First open modal owns the keyboard;
`handleMouse` (`app.go:1013`) mirrors the same chain, then right-click, wheel
(shift→horizontal incl. Zellij sticky-modifier hack), drag continuation
(`dragMode` is stringly-typed: ""/"editor"/"sidebar"), then region dispatch
(`app.go:1126-1140`: splitter / sidebar / tab bar / editor).

**Esc is the only command key**: menu-open→close; double-Esc→menu; single Esc
arms the leader table; Esc+rune fires `leaderActionFor`. No Ctrl shortcuts by
design (tmux conflicts).

### Rendering (`draw` at `app.go:2069`)
Order: too-small guard → sidebar (`tree.Render` + splitter) → tab bar (menu
button + `layoutTabs`, geometry cached in `lastTabRects` for hit-testing) →
editor body (`tab.Render` into its rect) → find bar → status bar (flash/file
info left, git branch right) → modal layer bottom-up (only one modal is ever
open; order is defensive).

### Layout math
`sidebarRect` / `tabBarRect` / `editorRect` / `statusRect` all derive from one
`sidebarW()` and assume **exactly one editor region** between tab bar (row 0)
and status bar (bottom row). The find bar steals a row out of `editorRect` as
a special case (`app.go:819-826`). Splitter drag: press at exactly
`x == splitterX()`; `minSidebarWidth=18`, `minEditorAfterDrag=40`.

## Document model (`internal/editor`)

- **Buffer** (`buffer.go:29`): `Lines []string`, one string per line.
  `Position{Line, Col}` is **rune-indexed** (not bytes, not cells). Every
  structural edit rebuilds the `[]string` — O(lines) per edit; every op does
  `[]rune(line)` — O(line length). No rope/piece table. Fine for its target
  ("review + small edits"), a ceiling for big files.
- **Tab** (`tab.go:30-90`) fuses document state (Buffer, undo stacks, Path,
  Mtime, Dirty, DiskGone) with **view state** (Cursor, Anchor, ScrollX/Y,
  Styles cache, cursorMoved, find state) in one struct. Selection is the
  `(Anchor, Cursor)` pair, normalized on demand via `PosOrdered`.
- **`cursorMoved` flag**: every cursor mutator sets it; `Render` consumes it
  to decide whether to `EnsureVisible`. Do NOT call EnsureVisible
  unconditionally (re-introduces scroll-yank bug).
- **`clampScroll`** allows overscroll of `max(viewH/2, 3)` so the last line
  can sit mid-viewport. Intentional.
- **Undo** (`undo.go`): snapshot-based — each entry is a full deep copy of
  `Buffer.Lines` + Cursor + Anchor. Coalesces same-kind edits (Typing /
  Backspace / Delete; Structural never coalesces) within 500ms. Cap 500
  entries FIFO. `undoOriginal` held separately for Revert (revert is itself
  undoable). New edits clear the redo stack. Dirty = differs-from-original.
- **Highlight** (`highlight.go`): Chroma tokenizes the **entire file** into a
  per-rune `[][]tcell.Style` grid, cached on `Tab.Styles`, invalidated
  wholesale by `StyleStale` on any edit. Selection and find-match highlights
  are ad-hoc bg overrides inside `Tab.Render` (`tab.go:596-608`) — there is
  no general decoration/overlay layer.
- **Render** (`tab.go:504-660`): the Tab paints itself into an arbitrary
  `(x,y,w,h)` rect: gutter (width 6 + 1 separator), line highlight,
  per-rune styles, tab-stop-aware visual columns, ‹/› overflow markers,
  hardware cursor at visual column. `HitTest` inverts screen→Position.
- **Images**: PNG/JPEG/GIF(frame 1) decoded via stdlib, rendered as ▀
  half-blocks (fg=top pixel, bg=bottom pixel) — works in any truecolor
  terminal through tmux, no sixel/kitty/iTerm protocol. Image tabs reuse
  `Tab`; all mutators short-circuit on image mode.

## Modal system (`modals.go`, `formmodal.go`)

Six mutually-exclusive modals, each a parallel field-block on `App`:
prompt (single-line input), confirm (Yes/No, doubles as info/OK via
`confirmInfo`), dirty-close (Save/Discard/Cancel), form (multi-field text +
select, Tab traversal), tree context menu, plus the main ≡ menu. Each has an
`open* / handle*Key / handle*Mouse / draw* / *Rect` quartet. `closeAllModals`
enforces exclusivity; callbacks are captured before close then invoked.

**Known trap:** button geometry is hard-coded twice — draw and mouse hit-test
must match cell-for-cell (e.g. prompt buttons at cells 14-23/30-37 in both
`drawPrompt` and `handlePromptMouse` at `modals.go:189`).

Main menu: `menuItemDef{label, relY, action, enabled, labelFor}`
(`app.go:156`). `builtinMenuGroups` (`app.go:173`) → `menuLayout`
(`app.go:232`) flattens groups, splices custom actions before Quit, computes
dividers + height per call. Enabled-predicates (`hasSavableTab`,
`hasSelection`, `hasUndo`, …) gate dimming and skip-navigation.

## Services

- **filetree**: lazy loading (root's first level at startup, children on
  expand). `reload` (`filetree.go:106`) preserves surviving `*Node` pointers
  by name+IsDir so `Expanded` state survives the 10s refresh. Hardcoded
  `shouldHide` list (.git, node_modules, …); **no gitignore here** (that's the
  finder). `visible []*Node` row slice gives O(1) hit-testing. Git dirty
  files/folders are pushed onto the tree as absolute-path sets.
- **finder**: `Rebuild` gated by `atomic.Int32` CAS (max one goroutine),
  state machine Idle/Building/Ready/Errored. Index: `git ls-files --cached
  --others --exclude-standard -z` fast path; fallback `WalkDir` with hardcoded
  ignore set + project-root `.gitignore` only; 200k-entry cap. Scorer
  (`score.go`): greedy fzy-style subsequence (not full DP) with bonuses
  consecutive(30) > word-boundary(20) > basename(15) > first-char(10),
  gap penalty 1; returns match indexes for highlight. Stable sort by score
  desc then path.
- **format**: per-project `.spiceedit/format.json` maps ext → **argv array**
  (`$FILE` substituted; write-in-place model, buffer reloaded after). Runs
  via `exec.Command` with explicit argv — deliberately no shell. Global
  defaults (`format-defaults.json`) never auto-run; offered for install into
  the project config on first save of a matching ext. **Trust** keyed on
  (symlink-resolved project path, SHA-256 of format.json) — editing the
  config re-prompts; denials persisted; per-ext `DeclinedInstalls` remembered.
  Runs after the disk write so a broken formatter never blocks a save; buffer
  reloaded only if not re-dirtied; missing binary = silent skip.
- **customactions**: `~/.config/spiceedit/actions.json`. `Action{Label,
  Command, Prompts}`; prompts are text/select with `${VAR}`-expandable
  defaults. Executed as `sh -c` (deliberately a shell, unlike formatters)
  with env = editor-state vars + prompt values; combined output; every run
  logged to `$XDG_STATE_HOME/spiceedit/actions.log`. No trust prompt — the
  config lives in the user's own home dir.
- **gitstatus**: best-effort shell-outs on the 10s tick + after every file
  op. `status --porcelain` → absolute dirty-file set → rolled up to ancestor
  folders; branch via `symbolic-ref` falling back to short SHA.
- **clipboard**: OSC 52 written straight to `/dev/tty` (avoids racing tcell's
  stdout), wrapped in tmux passthrough when `$TMUX` set. Copy-only.

## Three-way disk reconciliation (`reconcileOpenTabsWithDisk`, app.go:713)
On each tree-refresh tick, per open tab: clean buffer + changed mtime →
silent reload; dirty buffer + changed mtime → warning; file gone →
`DiskGone` set once.

## Build / release

- Makefile: `run`, `build` (→ bin/spiceedit), `build-linux`
  (CGO_ENABLED=0, -s -w — the static SSH-ship story), `test` (-race),
  `coverage`, `tidy`, plus `site-*` Hugo targets.
- `release.yml` on every push to main: parse `internal/version/version.go`;
  hand-edited → use as-is, else auto-bump patch + commit back `[skip ci]`;
  tag; GoReleaser cross-compiles linux/darwin/windows × amd64/arm64, GitHub
  Release, writes `Formula/spice-edit.rb` back into this repo (default
  GITHUB_TOKEN, no PAT), also `[skip ci]`. Both `[skip ci]` markers are
  load-bearing — removing either loops the workflow.
- `test.yml`: ubuntu+macOS matrix, tidy-check, build, vet, `test -race`.

## Assessment for the mini-IDE direction

### Strengths to build on
1. Async custom-event pattern — ready-made for LSP/build/test runners.
2. `editor` package boundary — Tab is already a self-rendering widget that
   never imports `app`.
3. Finder — a working fuzzy matcher with highlight indexes is 80% of a
   command palette (menu items already have labels + enabled predicates).
4. Test culture (simulation screens, race detector, required checks) —
   the safety net that makes aggressive refactoring viable.
5. Distribution: one static binary, auto-release, brew formula in-repo.

### The four walls (in the order features will hit them)
1. **Modal system doesn't scale.** Six parallel field-blocks; a new modal
   touches App fields, both precedence chains, `closeAllModals`,
   `anyModalOpen`, and `draw`; geometry duplicated draw vs hit-test.
   → Refactor to a `Modal` interface + single stack.
2. **Single-pane layout baked into the math.** All rects derive from one
   `sidebarW()`; find bar already hacks a row out; mouse dispatch is a
   region switch. → Layout tree of rects with per-pane dispatch.
3. **Buffer and view fused in Tab.** Splits (two views of one buffer) need
   Tab separated into shared document (Buffer + undo + path/dirty) and
   per-view state (cursor/anchor/scroll/styles/find).
4. **No decoration layer.** Diagnostics, git gutter, inline hints all want a
   span/overlay system merged at render time; also the escape hatch from
   whole-file re-highlight per keystroke.

### Smaller debts
- `app.go` is a 2,518-line god file; `App` a god struct.
- Every file mutation manually fans out `tree.Refresh + refreshGitStatus +
  invalidateFinder` — forget one and the UI goes stale. Wants a single
  "workspace changed" notification.
- Async completions identify tabs inconsistently (index vs path) — latent
  bug when tabs close mid-operation.
- `saveTabAt` mixes bounds check, guards, disk IO, git refresh, flash, and
  format kick-off.
- Embedded terminal would fight the no-CGO constraint (needs a pure-Go VT
  emulator); a read-only output panel delivers most of the value cheaply.
