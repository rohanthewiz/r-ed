# r-ed Mini-IDE Plan

Roadmap for evolving SpiceEdit into **r-ed**, a mouse-first terminal mini-IDE.
Companion doc: [orig-architecture.md](orig-architecture.md) — the inherited
architecture, its strengths, and the debts each phase below pays down.

Decisions locked in (2026-07-08):
- New identity: **r-ed**, repo `github.com/rohanthewiz/r-ed`.
- Target capabilities: **LSP smarts, command palette, git deep integration.**
- Explicitly deferred: split panes / bottom output panel (removes the layout-tree
  and buffer/view-separation refactors from the critical path), embedded
  terminal (fights the no-CGO constraint), marketing site.
- Release workflow paused until the rename settles.

## Phase 0 — Rebrand to r-ed (one commit, before any divergence)

- Module path `github.com/cloudmanic/spice-edit` → `github.com/rohanthewiz/r-ed`;
  rewrite all internal imports.
- Binary `spiceedit` → `r-ed` in Makefile, .goreleaser.yml, main.go help/version
  text, install.sh. (Plain `red` would collide with Unix "restricted ed".)
- Config surfaces: `~/.config/spiceedit/` → `~/.config/r-ed/` (config.json,
  actions.json, format-defaults.json, format-trust.json), `SPICEEDIT_*` env
  overrides → `RED_*`, `$XDG_STATE_HOME/spiceedit` → `.../r-ed`. No migration
  shim — fresh start.
- Release pipeline: pause release.yml (workflow_dispatch only) until rename
  settles; formula name becomes `Formula/r-ed.rb`; keep both `[skip ci]`
  auto-commit markers when re-enabling (load-bearing — without them the
  workflow loops). Reset version.go to 0.1.0 as a hand-edited bump.
- Defer the Hugo site + CNAME.

## Phase 1 — Modal stack refactor (enabler, no visible change)

- `Modal` interface (`HandleKey`, `HandleMouse`, `Draw`, `Rect`) + a single
  modal slot on App (one-deep stack is enough today).
- Each modal computes button/field rects in ONE place consumed by both draw
  and mouse hit-test (kills the duplicated-geometry trap, e.g. prompt buttons
  hard-coded in both drawPrompt and handlePromptMouse).
- Convert smallest-first: prompt → confirm → dirty → form → context → finder.
  The ~3,400 lines of modal tests are the safety net.
- Opportunistic cleanups while in there:
  - Collapse the `tree.Refresh + refreshGitStatus + invalidateFinder` trio
    into one `workspaceChanged()`.
  - Async completion events identify tabs by path consistently (never index).

## Phase 2 — Command palette (first visible feature)

- New modal reusing the fzy scorer (`internal/finder/score.go`) over ACTIONS
  instead of paths. Action inventory already exists: menuLayout items (labels +
  enabled predicates) + custom actions.
- Match-index highlighting like the finder; leader-key binding.
- Pluggable sources from day one: actions now; files can merge in; symbols
  (LSP) and git commands arrive in later phases — this is the "fuzzy
  everything" seam.

## Phase 3 — Decoration layer (enabler for 4 and 5)

- Span/overlay system in `internal/editor`: sources produce
  `{range, style-delta, gutter-mark}` spans; `Tab.Render` merges them over the
  base syntax grid.
- Migrate the two existing ad-hoc overlays (selection, find matches) onto it
  to prove the design; add a gutter mark column.
- Single shared prerequisite for git gutter and LSP diagnostics.

## Phase 4 — Git deep integration

- Diff gutter: async `git diff -U0 <file>` per open tab (on save + existing
  10s tick), parsed into added/modified/deleted marks via decorations.
- Hunk navigation: next/prev hunk as palette actions + leader keys.
- Later, behind the palette: stage file, commit (form modal for message),
  branch switch — shell-outs following the gitstatus.go best-effort pattern.

## Phase 5 — LSP (gopls first)

- Hand-rolled minimal JSON-RPC-over-stdio client (stdlib only; the protocol
  subset needed is small). Server lifecycle per workspace;
  didOpen/didChange/didSave with debounced sync.
- Diagnostics → decoration layer (underline + gutter mark) + status-bar count.
- Go-to-definition → "jump to file:line:col" helper on top of openFile +
  back-navigation stack (leader `o` = jump back).
- Hover → info modal near the cursor.
- Missing server binary = silent degradation (same philosophy as formatters).

## Sequencing logic

0 → 1 → 2 yields a rebranded editor with a command palette quickly (2 is small
once 1 lands). 3 is the shared prerequisite for 4 and 5; 4 goes before 5
because it is a tenth of the size and exercises the decoration layer before
LSP complexity arrives. Every phase keeps `make test` green; every source file
keeps its `_test.go` sibling.

## Status

- [x] Phase 0 — rebrand (done 2026-07-08)
- [x] Phase 1 — modal stack (done 2026-07-08; async tab-identity check
      turned out to be a non-issue — format/custom-action completions
      already resolve tabs by path)
- [x] Phase 2 — command palette (done 2026-07-08; `internal/app/palette.go`,
      Esc-a leader + "Command palette" menu row in the Search group;
      pluggable `paletteSources()` seam ready for files/symbols/git)
- [ ] Phase 3 — decoration layer
- [ ] Phase 4 — git integration
- [ ] Phase 5 — LSP
