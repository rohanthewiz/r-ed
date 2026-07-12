# Session: Esc Menu Fix, Line Ops, Left-Dock Terminal Layout, gopls Fixes

Session ID: 6b13733f-a1f8-4c65-9072-9f46735da7da
Date: 2026-07-11

## Goal

Five user-reported items in one pass:

1. Esc not opening the action menu (only the ≡ click worked).
2. Shift+Arrows should grow the selection.
3. Ctrl-D duplicate line; Alt-Up/Down move line.
4. Moveable splitter between editor and terminal, plus an optional
   layout with the terminal vertical on the LEFT and the file tree on
   the RIGHT.
5. gopls is on PATH but no syntax errors show and auto-imports don't
   happen on save.

## Root causes found

### 1. tmux folds Esc sequences into Alt events (the Esc-menu bug)

tmux buffers a lone ESC for its `escape-time` (500ms default). A fast
double-Esc therefore reaches the editor as ONE buffered `\x1b\x1b`
write, which tcell 2.13's ECMA-48 parser reports as a single
`KeyEsc + ModAlt` event — the doubleEscMs detector never saw two
presses, so the menu was keyboard-unreachable inside tmux. Same
mechanism breaks every Esc-leader: "Esc, s" inside the window arrives
as `Alt+s` with no separate Esc event to arm `lastEscape`.

**Fix (app.go handleKey):**
- `Alt+Esc` → toggle the menu directly (open or close).
- `Alt+<rune>` bound in the leader table → fire that leader; unbound
  Alt runes still fall through to typing (Option-as-Meta safety).

### 2. Shift+Arrows — editor logic was already correct

`handleKey` already passed `extend` from ModShift into
`Tab.MoveCursor`. Pinned end-to-end with
`TestHandleKey_ShiftArrowsGrowSelection` (arrows, Shift+End, cross-line
growth, collapse on plain arrow, backwards Shift+Up) — all pass. tcell
2.13 parses `CSI 1;2A`-style modified arrows in ANY terminal (verified
in tcell's input.go: the `ss3Keys` + `calcModifier` path), so a failure
in the wild means the terminal isn't SENDING the modified sequence.
Diagnosis for the user: `cat -v` + press Shift+Down → expect
`^[[1;2B`; if plain `^[[B`, fix the terminal/tmux config
(`set -g default-terminal "tmux-256color"`, or macOS Terminal keyboard
profile). Nothing more to fix code-side.

### 5a. No gopls diagnostics — relative rootDir

`main.go` roots the editor at `"."` and `App.rootDir` was never
absolutized (filetree.New DID absolutize its own root, which is why
tab paths were absolute and didOpen looked fine). The LSP handshake
sent `rootUri: file://.` — malformed — so gopls never resolved the
module/workspace and never published diagnostics.

**Fix:** `New()` absolutizes rootDir; `openFile` absolutizes tab paths
(covers the `r-ed foo.go` relative CLI case). Verified end-to-end with
a throwaway integration test spawning REAL gopls against a scratch
module: `declared and not used` diagnostic arrived keyed by tab.Path in
~120ms. (Scratch test deleted afterwards — real-gopls tests are too
slow/flaky for CI; the repo convention kills LSP in newTestApp.)

### 5b. No auto-imports — goimports missing, gopls unused

The user's machine has `gopls` and `gofmt` on PATH but NOT `goimports`,
so the builtin format-on-save pass silently degraded to plain gofmt (no
import fixing). `gopls imports -w <file>` does the same import fixing
(verified by running it on a scratch file — it added `fmt`/`os`
imports).

**Fix:** `format.BuiltinCommandFor` ([]string) became
`format.BuiltinCommandsFor` ([][]string — a PIPELINE):
- goimports alone, if installed;
- else `gopls imports -w` chained with `gofmt -w`;
- else gofmt alone; else nil (silent degradation unchanged).
App side: `execFormatterChain` runs the argvs sequentially in one
goroutine, posts ONE formatDoneEvent, stops on first real failure,
treats binary-not-found as skip-and-continue. `execFormatter` is now a
thin single-command wrapper over it. The app-level stub var renamed to
`builtinCommandsFor` (newTestApp still nils it).

## Features built

### 3. Line ops (editor/lineops.go + wiring)

- `Tab.DuplicateLines()` — duplicates the selected line block (or
  cursor line) below itself; cursor/anchor/selection ride down onto the
  copy. Structural undo step.
- `Tab.MoveLines(±1)` — shifts the block one row, cursor travels with
  it, refuses at buffer edges without mutating. New coalescing group
  `undoGroupLineMove` so a nudge burst undoes in ONE step.
- Keys: `Ctrl-D` duplicate (the ONE sanctioned exception to the
  no-Ctrl rule — no tmux/flow-control collision; documented in
  CLAUDE.md); `Alt+Up` / `Alt+Down` move (works in tmux — both the
  `ESC [ 1;3 A` and ESC-prefixed forms land as ModAlt).
- Menu rows (menu-first house rule): Duplicate line / Move line up /
  Move line down in the Clipboard group, gated by new `hasEditableTab`.

### 4. Left-dock terminal layout (+ splitter clarifications)

The editor↔terminal splitter in the bottom layout ALREADY existed —
drag the panel's header rule (`─` line). Left as-is.

New alternate layout, `App.termDockLeft` (≡ → "Dock terminal left
(tree right)" / "Dock terminal at bottom"):

- Terminal becomes a full-height vertical strip on the LEFT with a
  draggable `│` splitter on its right edge (dragMode "termsplit");
  the file tree flips to the RIGHT edge, its splitter now on the
  block's LEFT (`splitterX = width - sidebarWidth`, drag computes
  `width - x`).
- Geometry pivots: new `leftBlockW()` / `rightBlockW()` — every rect
  helper (tabBarRect, editorRect, menuButtonRect, findBarRect,
  gitPanelRect, termPanelRect, layoutTabs) goes through them. New
  `inSidebarBlock(x)` keeps click/scroll routing layout-agnostic;
  `tryTreeContextClick` became rect-based.
- Width state: `term.width` (0 = auto → width/3), clamped
  [`termPanelMinWidth`=24, `maxTermPanelWidth()` = width − sidebar −
  minEditorAfterDrag]. Esc-=/Esc-- resize columns in this mode.
  Header rule is NOT a grab handle in left dock (gated in
  termPanelPress).
- Single-occupancy amendment: the rule is about the BOTTOM strip. A
  left-docked terminal coexists with the git panel; flipping back to
  bottom evicts the git panel. Both toggle paths gated on
  `termDockLeft`.
- Persistence: `"termdock": "bottom"|"left"` in config.json.
  `userconfig.SaveAutoSave` refactored onto a shared `saveKey`
  helper; new `SaveTermDock`, `TermDock` type, Load validation.
- Edge fix found while here: `termTitleCwd` could return an
  untruncated cwd when maxW ≤ 1 (reachable with a min-width strip)
  and paint past the panel edge — now returns "" when maxW < 4.
- Mouse dispatch reorder: term-panel hit-test moved BEFORE the y==0
  tab-bar case (a left strip spans y 0); harmless for bottom dock.

## Tests added (all green, `make test` with -race)

- app_test.go: AltEscTogglesMenu, AltRuneFiresLeader,
  ShiftArrowsGrowSelection, CtrlDDuplicatesLine, AltArrowsMoveLine,
  OpenFile_AbsolutizesRelativePath. Menu geometry pins updated
  (45 items, height 57; +3 line-op rows, +1 dock toggle row).
- editor/lineops_test.go: duplicate single/block, move up/down, edge
  no-ops, coalesced-undo burst, image-tab no-ops, selectedLineRange.
- terminal_test.go: TermLeftDockGeometry, SplitterDrag (full
  handleMouse gesture), SidebarDragFromRight, WidthResizeClamp,
  HeaderIsNotAGrabHandle, CoexistsWithGitPanel, MenuToggleTermDock
  (persistence via XDG_CONFIG_HOME), TermLeftDockDraw (simulation
  screen).
- userconfig_test.go: termdock defaults/values/invalid,
  SaveTermDock round-trip preserving unknown keys.
- format/builtin_test.go: rewritten for the pipeline (goimports wins,
  gopls chain, gopls-only, gofmt fallback, none→nil, non-Go→nil).

## CLAUDE.md updates (keep these honored)

- Ctrl-D exception + "tmux folds Esc into Alt" section (don't remove
  the Alt branches in handleKey).
- Terminal panel: two dock modes; single-occupancy scoped to
  bottom-dock only.
- LSP: "Absolute paths only" bullet (New/openFile absolutize).
- Format-on-save: builtin is now a pipeline via BuiltinCommandsFor.
- Architecture map: userconfig (termdock), format (gopls imports).

## Loose ends / follow-ups

- Shift+Arrows: if still broken for the user it's terminal config —
  ask for `cat -v` output of Shift+Down.
- gopls `Initialize` uses the generic 5s Call timeout; a very large
  cold workspace could conceivably exceed it (silent degradation).
  Not observed; revisit only if diagnostics vanish on huge repos.
- The stray `r-ed` binary at repo root is untracked leftover from a
  manual build; fresh builds go to `bin/r-ed`.
