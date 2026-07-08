# Session: Phase 0 + Phase 1 — Rebrand to r-ed and Modal-Stack Refactor

- **Session ID**: `6d3c64c3-bda1-4a53-b9f8-c62ab34e8ffa`
- **Date**: 2026-07-08 (ended 14:55)
- **Repo**: `~/projs/go/spice-edit` (module now `github.com/rohanthewiz/r-ed`)
- **Branch**: `main`
- **Commits this session**: `3bf2dc4`, `e084852`, `cefca7a`

## What this session was

The kickoff session for turning SpiceEdit into **r-ed**, a mouse-first
terminal mini-IDE. Three parts: a deep architecture study of the
inherited codebase, the rebrand (phase 0), and the modal-stack refactor
(phase 1).

## Key documents produced (read these first in future sessions)

- `ai_docs/orig-architecture.md` — full architecture of the codebase as
  forked (v0.0.40): package map, event loop, document model, modal
  system (pre-refactor), services, build/release, strengths, and the
  "four walls" debt analysis.
- `ai_docs/mini-ide-plan.md` — the phased roadmap with live status
  checkboxes. Phases 0 and 1 are done; next is **phase 2: command
  palette**.

## Decisions locked in

- New identity: **r-ed**, repo `github.com/rohanthewiz/r-ed`, binary
  `r-ed` (avoids colliding with Unix `red`, the restricted ed).
- Target capabilities: **LSP smarts (gopls first), command palette,
  git deep integration**. Explicitly deferred: split panes / bottom
  output panel (removes the layout-tree and buffer/view-separation
  refactors from the critical path), embedded terminal, marketing site.
- LICENSE and per-file `Author:`/`Copyright:` Cloudmanic headers stay
  untouched (MIT notice retention on a fork). New files get the user's
  own header.
- Release workflow **paused** (workflow_dispatch only) until the rename
  settles; re-enable by restoring the `push: branches: [main]` trigger
  in `.github/workflows/release.yml`. Version hand-bumped to `0.1.0`.

## Phase 0 — rebrand (commit `3bf2dc4`)

- Module path `github.com/cloudmanic/spice-edit` → `github.com/rohanthewiz/r-ed`;
  ordered sed sweep over 49 files (domain `spice-edit.com` protected;
  `website/` deferred).
- Config surfaces: `~/.config/spiceedit/` → `~/.config/r-ed/`,
  project dir `.spiceedit/` → `.r-ed/`, env `SPICEEDIT_*` → `RED_*`,
  state `~/.local/state/r-ed/`. No migration shim.
- `internal/spiceconfig` package renamed to `internal/userconfig`.
- `Formula/spice-edit.rb` deleted — goreleaser regenerates as
  `Formula/r-ed.rb` on the first release; brew owner → `rohanthewiz`.
- Local git remote set to `https://github.com/rohanthewiz/r-ed.git`.
  **User action**: rename the GitHub repo `spice-edit` → `r-ed` in
  Settings (rename, not new repo — keeps releases + redirects).

## Phase 1a — single-slot modal interface (commit `e084852`)

Replaced the six parallel modal field-blocks on `App` with one
`App.modal` slot and a `modal` interface (`handleKey` / `handleMouse` /
`draw`). Each modal is a struct owning its own state:

- `internal/app/modal.go` (new): interface, `openModal`/`closeModal`,
  `btnRect` (single geometry source for draw + hit-test), `textField`
  (shared single-line input — was triplicated across prompt/form/finder).
- `modals.go`: `promptModal`, `confirmModal` (info flavour folded in;
  `cancelHook` lives on the instance now — `openConfirm` returns the
  modal so format-trust flows arm it directly), `dirtyModal`,
  `contextModal`, shared `modalChrome.drawFrame`.
- `formmodal.go`: `formModal` + per-row `formRow` (textField or selIdx).
- `finder.go`: `finderModal` transient UI state; index stays on
  `App.finder`.
- Routing: `handleKey`/`handleMouse`/`draw` each collapsed from a
  seven-branch precedence chain to one dispatch. Net −95 lines.
- Tests ported to typed accessors (`promptOf(a)` etc. in
  `modal_test.go`); obsolete "state cleared after close" / "noop when
  closed" tests replaced with routing-level equivalents. Kept the
  historical (wider-than-label) click zones.

## Phase 1b — workspaceChanged() (commit `cefca7a`)

`tree.Refresh + refreshGitStatus + invalidateFinder` trio collapsed
into `App.workspaceChanged()` (fileops ×4, formatter install, 10s tick
via `refreshTreeNow` which adds the open-tab disk reconcile). **Fixed a
real bug**: the formatter-install path missed `invalidateFinder`, so a
fresh `.r-ed/format.json` stayed out of the finder index until the next
tick. The planned async tab-identity cleanup was a non-issue — format
and custom-action completions already resolve tabs by path.

## Conventions added to CLAUDE.md (do not regress)

- New modal = implement the `modal` interface; button geometry in ONE
  method returning `btnRect`s; reuse `textField`; never add per-modal
  fields to `App` or new branches to the routers.
- After any workspace mutation call `a.workspaceChanged()`, never the
  individual refreshes.

## State at session end

- `make test` (race detector) fully green across all 13 packages;
  `go vet` clean; gofmt clean.
- Binary smoke-tested: `r-ed 0.1.0`, help text correct.
- Manual TUI smoke test still recommended (`make run`: rename prompt,
  delete confirm, custom action with prompts, Esc-p finder).

## Next session: phase 2 — command palette

One new struct implementing `modal`; reuse `textField` + the fzy scorer
(`internal/finder/score.go`) over the action inventory that already
exists (`menuLayout` items with labels + enabled predicates, plus
custom actions). Design with pluggable sources (actions now; files,
symbols, git commands later). Then phase 3 (decoration layer) →
phase 4 (git gutter) → phase 5 (LSP).
