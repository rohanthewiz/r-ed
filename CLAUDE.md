<!--
  File: CLAUDE.md
  Author: Spicer Matthews <spicer@cloudmanic.com>
  Created: 2026-04-29
  Copyright: 2026 Cloudmanic, LLC. All rights reserved.
-->

# CLAUDE.md — r-ed

Project-specific guidance for Claude Code. Read this first; it captures
conventions and design decisions that aren't obvious from the code alone.

## What this project is

r-ed is an opinionated, **mouse-first** terminal code editor aimed at
SSH-into-tmux workflows. It looks and behaves like a tiny VS Code: file
tree on the left, tabs across the top, syntax-highlighted editor in the
middle, status bar at the bottom. It ships as a single static Go binary
with no CGO.

Users open the action menu (Save, Quit, Show/Hide Sidebar, …) by clicking
the `≡` icon, right-clicking, or double-tapping `Esc`. There are
intentionally **almost no `Ctrl+` shortcuts** for editor actions — they
conflict with `tmux` and terminal emulators. Don't add more. The one
sanctioned exception is `Ctrl-D` (duplicate line): it collides with
nothing (not flow control, not the tmux/zellij prefixes), and the owner
approved it explicitly. `Alt+Up/Down` (move line) is fine — Alt never
fights tmux.

**tmux folds Esc sequences into Alt events.** tmux buffers a lone ESC
for its escape-time (500ms default), so a fast double-Esc reaches tcell
as one `\x1b\x1b` write → a single `KeyEsc + ModAlt` event, and "Esc,
s" reaches it as `Alt+s`. handleKey therefore treats Alt+Esc as the
double-Esc menu toggle and Alt+<bound rune> as that leader. Keep those
branches — removing them makes the keyboard menu and every leader
unreachable inside tmux.

**Every file action also lives in the main ≡ menu.** macOS Terminal +
tmux often swallows Button3 (right-click), so the editor cannot rely on
right-click as the only path to anything. Tree right-click is a redundant
shortcut, not a primary surface — when adding new file-management
features, make sure they're reachable from the main menu first.

## Module / repo

- Module: `github.com/rohanthewiz/r-ed`
- Binary name: `r-ed` (one word, lowercase — Makefile, goreleaser,
  brew formula all assume this)
- Brew tap: this same repo, `Formula/` directory (no separate tap repo)

## Architecture map

```
main.go                       Entry — parses optional rootDir arg
internal/app/app.go           Event loop, layout, menu modal, splitter, all rendering
internal/editor/buffer.go     Position + Buffer ([]string lines), edit primitives
internal/editor/tab.go        Tab: path, buffer, cursor, anchor, scroll, dirty state
internal/editor/highlight.go  Chroma → []tcell.Style per line
internal/editor/decoration.go Span/GutterMark overlay system merged in Tab.Render
internal/lsp/client.go        Minimal JSON-RPC-over-stdio LSP client (stdlib only)
internal/app/lsp.go           gopls lifecycle, doc sync, diagnostics, definition, hover
internal/app/autosave.go      Idle-debounced auto-save (EditRev signature → autoSaveEvent)
internal/app/zipops.go        Zip file/folder — stdlib archive/zip, async zipDoneEvent
internal/app/format.go        Format-on-save bridge: project config, builtin Go, prompts
internal/app/nav.go           Back/forward file-navigation history (Esc-o/O, Alt+←/→)
internal/app/terminal.go      Embedded grsh terminal panel (REPL strip, not a PTY)
internal/format/              format.json load, trust store, builtin goimports / gopls imports / gofmt
internal/filetree/filetree.go Lazy tree, identity-preserving refresh, hit-test, render
internal/clipboard/clipboard.go OSC 52 to /dev/tty with tmux passthrough wrap
internal/userconfig/userconfig.go ~/.config/r-ed/config.json loader/writer (icons, autosave, termdock)
internal/icons/icons.go       Nerd Font detection + per-file glyph mapping
internal/theme/theme.go       Tokyo Night palette + syntax color mapping
internal/version/version.go   const Version = "x.y.z" — single line, CI bumps it
```

## Conventions

### File headers
Every new source file gets the header block (file name, author, created
date, copyright year). See existing files for the exact format. Keep
copyright year matching the **current year** (2026 right now).

### Comments
- A short doc comment above every function (public **and** private)
  explaining intent. This is a project-wide convention — don't skip it.
- Skip throwaway "what" comments inside functions; favor "why" notes
  for non-obvious decisions.

### Tests — required, not optional
**Every source file gets a corresponding `_test.go` file in the same
package.** New code without tests should not be merged. The bar:

- New exported functions: cover happy path + the obvious failure mode.
- New unexported helpers with non-trivial logic: same.
- Bug fixes: add a test that fails before the fix and passes after.
- Pure data / glue (theme palettes, single-constant files): a smoke
  test that the value is sensible is enough.

Conventions:
- One `_test.go` per source file, in the same package (NOT `_test`),
  so tests can poke unexported helpers directly. Don't split tests
  for one source file across multiple test files.
- Each `Test*` function gets a short doc comment above it explaining
  the behavior it pins down — the same "why over what" rule as
  production code. See `internal/app/fileops_test.go` for the style.
- Use `t.TempDir()` for filesystem state; never write into the repo
  or `/tmp` directly.
- For UI / drawing code that takes a `tcell.Screen`, build one with
  `tcell.NewSimulationScreen("UTF-8")` and assert against
  `scr.GetContents()`.
- Skip a test (`t.Skip`) only when the environment can't satisfy a
  hard requirement (e.g. `/dev/tty` in CI). Don't skip to dodge a
  flaky test — fix it.

Run them locally:
```sh
make test          # go test ./... with race detector
make coverage      # generates coverage.out + an HTML report
```

CI (`.github/workflows/test.yml`) runs `go test ./...` on every push
and every PR; broken tests block merges via the PR's required-checks.

### Commits
- No "Generated with Claude Code" trailers, no Co-Authored-By Claude.
- Don't ask for commit-message approval — commit directly with a good
  message when the user asks you to commit.

## Design patterns to preserve

### `cursorMoved` flag (tab.go)
The cursor only triggers `EnsureVisible` when something actually moved
the cursor. Every cursor mutator sets `t.cursorMoved = true`; `Render`
consumes the flag and clears it. **Do not** call `EnsureVisible`
unconditionally — that re-introduces the "scroll yanks back to cursor
on every tick" bug.

### Scroll clamping with overscroll
`tab.clampScroll(viewH)` allows the last line to scroll roughly to the
middle (`overscroll = max(viewH/2, 3)`). This is intentional — without
it, you can't comfortably read the bottom of a file.

### Custom tcell events for goroutine → main-loop messaging
Background work (auto-scroll during drag, 10s tree refresh) posts custom
events (`autoScrollEvent`, `treeRefreshEvent`) onto the tcell event queue
and the main loop handles them. Don't mutate UI state from goroutines
directly.

### Identity-preserving tree refresh (filetree.go)
`reload` walks the existing children, matches survivors by name, and
keeps their `*Node` pointers (and their `Expanded` state). New entries
get fresh nodes; gone entries are dropped. This is what makes the
10-second auto-refresh feel non-jarring — open folders stay open.

### Decoration layer (editor/decoration.go)
Any "paint something over the code" feature is a `DecorationSource`
producing `Span`s (range + `StyleDelta`) and `GutterMark`s — never a
new branch inside `Tab.Render`'s paint loop. External sources register
via `Tab.DecoSources`; built-ins (selection, find) run last so merge
precedence is: syntax < external annotations < selection < find. The
gutter mark column is the single cell at `x + gutterWidth`, between
the line numbers and the code.

### LSP integration (internal/lsp + app/lsp.go)
The client is a hand-rolled JSON-RPC subset — do NOT add an LSP
framework dependency. House rules it must keep obeying:

- **Silent degradation**: no gopls on PATH / crash / timeout → the
  editor works normally, no nagging. Same contract as formatters.
- **Events only**: the read loop, start handshake, debounce timers,
  and definition/hover requests all run off-loop and post
  `lsp*Event`s; only the main loop touches `App.lsp`.
- **Sync via `Tab.EditRev`**: every content mutation bumps it; the
  post-event check (`lspAfterEvent`) compares against `syncedRev`
  and arms a 300ms debounce. Saves flush pending changes BEFORE
  didSave. New Tab mutation paths must bump `EditRev` or the server
  silently diagnoses stale text.
- Diagnostics are just another `DecorationSource` (registered after
  the git source so the diag gutter dot outranks the git mark).
- Leaders: Esc-d definition, Esc-i hover. Definition jumps record
  into the app-wide navigation history (nav.go) — there is no
  LSP-private jump stack anymore.
- **Absolute paths only**: `New()` absolutizes rootDir and `openFile`
  absolutizes tab paths. A relative root produces a malformed rootUri
  and gopls then publishes diagnostics keyed by absolute paths that
  never match the tabs — the "gopls installed but no squiggles" bug.
- Tests kill the integration (`a.lsp.dead = true` in newTestApp) so
  openFile can't spawn a real gopls; LSP tests inject `fakeLSPConn`.

### Navigation history (app/nav.go)
Browser-style Go back / Go forward across files (≡ menu, Esc-o / Esc-O,
Alt+Left / Alt+Right). Recording happens CENTRALLY: openFile records the
departure point on its success paths, and tabBarClick (which bypasses
openFile) records its own switches — new navigation surfaces get history
for free by calling openFile, so don't add per-surface push calls. The
`nav.suppress` flag is set while navBack/navForward retrace so the
retrace itself never records (removing that corrupts the trail into a
two-entry bounce). Any fresh navigation clears the forward stack, same
rule as a browser. LSP definition jumps record explicitly with the
request's origin position (a same-file jump moves only the cursor, which
path-change-only recording would miss) and open with suppress on.

### Menu shortcut hints
`menuItemDef.shortcut` is a display-only accelerator column rendered
right-aligned and muted in the ≡ menu ("esc s", "alt+←"). Dispatch
still lives in the leader table / handleKey — when adding or rebinding
a key, update both or the menu lies. Rows without a binding leave it
empty; drawMenu skips the hint when a long label would collide.

### Format-on-save precedence + builtin Go pass (app/format.go)
`runFormatOnSave(idx, quiet)` routes: project `format.json` entry
(trust-gated) → builtin Go pass (`format.BuiltinCommandsFor`, NO trust
prompt — the argvs are hardcoded, not repo-supplied) → global-defaults
install offer. The builtin pass is a command PIPELINE: goimports alone
if installed, else `gopls imports -w` chained with `gofmt -w` (a
machine with gopls but no goimports must not lose auto-imports), else
gofmt alone. `quiet=true` (auto-save) never opens a modal and never
flashes; an untrusted config is silently skipped until the next
explicit Save. Tests stub the app-level `builtinCommandsFor` var
(newTestApp sets it nil) so saves never exec the dev machine's Go
tools — keep that in place.

### Auto-save (app/autosave.go)
Debounce mirrors the LSP didChange pattern: `autoSaveAfterEvent` runs
after every dispatch, compares the sum of all tabs' EditRevs, and
(re)arms a 2s `time.AfterFunc` that posts `autoSaveEvent`. Saves are
silent (no flash), run format-on-save in quiet mode, defer while any
modal/menu is open, and skip tabs whose disk file changed after load
(explicit Save remains the overwrite path). The ≡ toggle persists via
`userconfig.SaveAutoSave`, which round-trips unknown JSON keys — don't
replace that with a struct marshal. Default is ON.

### Terminal panel (app/terminal.go)
An embedded grsh session (github.com/rohanthewiz/grsh — the module's
only public package; the embedding contract lives in that repo's
docs/EMBEDDING.md), hosted as a REPL strip. NOT a PTY — do not add
one, or a VT emulator; full-screen child apps (vim, htop) are out of
scope by design. House rules:

- **Two dock modes, one toggle**: the terminal is a bottom strip by
  default, or a full-height vertical strip on the LEFT (≡ → "Dock
  terminal left") — that layout also flips the file tree to the RIGHT
  edge. `App.termDockLeft` drives it; `leftBlockW`/`rightBlockW` are
  the geometry pivots every rect helper goes through. Persisted as
  `"termdock"` in config.json. Bottom mode resizes by header-rule
  drag (rows); left mode by its vertical splitter (columns). The dock
  toggle also OPENS a closed terminal — flipping the layout must never
  leave nothing where the terminal should be (that reads as the layout
  breaking, not a mode change). Keep the Show/Hide terminal and dock
  rows in the View-toggles group near the TOP of the ≡ menu — the menu
  scrolls on short windows and these rows must stay above the fold
  (pinned by `TestMenuLayout_TerminalRowsAboveTheFold`).
- **Single-occupancy bottom strip**: while BOTTOM-docked, the terminal
  and the git panel swap, never stack (opening one collapses the
  other). Two resizable bottom strips would need circular height-clamp
  math on small windows — keep the exclusivity. A LEFT-docked terminal
  doesn't compete for the bottom, so it coexists with the git panel;
  flipping back to bottom evicts the git panel.
- **Focus flag, not a modal**: `term.focused` routes plain editing
  keys to the input line; Esc stays global so leaders and the
  double-Esc menu keep working from inside the terminal. Any click
  outside the panel unfocuses. Esc-` is focus-or-toggle.
- **Coalescing writer**: grsh output lands in `termWriter`'s buffer
  with at most one `termOutputEvent` in flight — never post
  per-chunk events (heavy output would overflow tcell's queue).
- **Stop button, not Ctrl+C**: ⏹ sends Interrupt (SIGINT to the
  child's own process group), a second press escalates to Kill.
  grsh's embedded mode guarantees the signal cannot hit the editor.
- Evals run on goroutines; only main-loop handlers mutate term state.
  Each completed command calls `refreshTreeNow()` — shell commands
  create files.
- grsh's `cd` chdirs the whole editor process (grsh's deliberate
  design) — keep r-ed's own file operations absolute-path based.
- Tests inject `fakeTermEval` via the `newTermEvaluator` stub in
  newTestApp. Only TestTermRealGrshIntegration may execute a real
  command, and it is restricted to `echo`.

### Three-way external-change reconciliation (app.go)
On each tree-refresh tick, `reconcileOpenTabsWithDisk` checks each open
tab's mtime: clean buffer + changed file → silent reload; dirty buffer
+ changed file → warning; file deleted → set `DiskGone` once.

### Single-slot modal interface (modal.go)
Every secondary overlay (prompt, confirm, dirty-close, form, tree
context, finder) is a struct implementing the `modal` interface
(`handleKey` / `handleMouse` / `draw`) held in the single `App.modal`
slot — nil means none. `openModal` enforces mutual exclusivity. When
adding a modal: implement the interface, compute button geometry in ONE
method returning `btnRect`s that both draw and mouse hit-testing
consume, and reuse `textField` for any single-line input. For any
"choose one from a list" UI, reuse the palette as a fuzzy picker via
`a.openPicker(title, items)` (the branch switcher does this) — don't
write a new list modal. Do NOT add
per-modal fields back onto App or new branches to handleKey/handleMouse.
After any workspace mutation call `a.workspaceChanged()` — never the
individual tree/git/finder refreshes.

### Modal layout via `relY` and dynamic `labelFor`
The action menu uses named struct literals with an optional `labelFor`
hook so labels like "Show Sidebar" / "Hide Sidebar" toggle in place.
`menuLayout` recomputes every row's `relY`, the divider offsets, and
the modal height on each call — adding a menu item is just adding it
to its group in `builtinMenuGroups` (then updating the geometry pins
in `TestMenuLayout_NoCustomActions`). When the layout is taller than
the window, the modal clamps to the window and scrolls: frame + title
stay pinned, wheel / keyboard selection move the rows, ▲/▼ mark
clipped content. All scrolled geometry flows through
`menuItemIndexAt` / `menuScrollOffset` — don't hand-compute row
positions anywhere else.

### Sidebar splitter drag
A drag is detected when a press lands at exactly `x == splitterX()`.
Min widths: `minSidebarWidth = 18`, `minEditorAfterDrag = 40`. Don't
let the editor shrink below that.

## Build / run

```sh
make run          # go run . in current dir
make build        # build to ./bin/r-ed
make build-linux  # cross-compile linux/amd64
make install      # go install to $GOPATH/bin
make tidy         # go mod tidy
make clean        # rm -rf bin
```

There's no `dev server` to run for this project — it's a TUI. To test
UI behavior, build and run it against a real directory.

## Releases (don't break this)

Pushes to `main` trigger `.github/workflows/release.yml`:

1. Reads `internal/version/version.go`.
2. **If that file was edited in the pushed commit**, the version is used
   as-is (manual major/minor bump). **Otherwise** the patch is
   auto-bumped, committed back to main with `[skip ci]`, and pushed.
3. Tags `v<x.y.z>`.
4. GoReleaser cross-compiles, attaches archives to a GitHub Release,
   and writes `Formula/r-ed.rb` back into this repo (using the
   default `GITHUB_TOKEN` — no PAT). The formula commit also carries
   `[skip ci]` to break the loop.

If you're touching the workflow or `.goreleaser.yml`, make sure both
auto-commits keep their `[skip ci]` markers — without them the workflow
loops forever.

## What NOT to add

- `Ctrl+` editor shortcuts (they fight tmux/terminals — that's the
  whole reason the action menu exists).
- A config file / dotfile / plugin system. r-ed is opinionated.
- CGO dependencies. The whole point is one static binary.
- Tree-sitter. We use Chroma intentionally — pure Go, no setup.
- A separate `homebrew-tap` repo. The formula lives here under
  `Formula/` and that's deliberate.
