# Session: All inner panel edges are draggable with a grab handle

Session ID: 9b8a9289-e6ad-4878-87d9-e79533f1acc9
Date: 2026-07-17

## Goal

> "Ensure all inner panel edges have a drag handle and are draggable with
> the mouse."

Make every boundary between adjacent panels a consistent, mouse-draggable
resize handle with a visible "you can grab me" affordance.

---

## Starting state — the audit

Two of r-ed's inner edges already had a proper grab handle: the **vertical**
splitters — sidebar↔editor (`splitterX`) and left-docked-terminal↔editor
(`termSplitterX`) — drawn as a `│` that idles in `theme.Subtle` and brightens
to `theme.Accent` while grabbed (`drawSplitter`, `drawTermSplitter`).

The rest were inconsistent:

| Inner edge | Draggable before? | Handle before? |
|---|---|---|
| Sidebar ↔ Editor | ✅ | `│` Subtle→Accent |
| Left-terminal ↔ Editor | ✅ | `│` Subtle→Accent |
| Editor ↔ bottom git panel | ✅ (header rule) | `─`, **no** active-drag feedback |
| Editor ↔ bottom terminal | ✅ (header rule) | `─`, **no** active-drag feedback |
| Git panel: file-list ↔ diff | ❌ **not draggable** | static `│` at `width/3` |

So: the two bottom header rules were draggable but gave no visual cue while
dragging, and the git panel's internal list/diff divider wasn't draggable at
all (its width was auto-computed).

## Decision (asked the user)

One scope fork — how far does "all inner panel edges" go? The four main panel
edges were all already draggable; the genuine gap was the git panel's internal
list/diff divider, which is a real behavior change (the panel is an opinionated
"review strip" with an auto-sized list column).

**User chose: "All edges incl. git divider"** — make the git list/diff divider a
draggable splitter *and* add the active-drag highlight to the two bottom header
rules, so every inner edge behaves identically.

## Changes

### 1. Git panel list/diff divider → draggable width handle (`internal/app/gitpanel.go`)

- New session-only `listWidth int` on `gitPanelState` (mirrors `height`;
  `0` = auto third). Session-only on purpose — r-ed deliberately has no layout
  config.
- Refactored the free func `gitPanelListWidth(w int)` → `gitPanelListWidth(w, desired int)`
  (pure clamp honoring `desired`, `<=0` = auto), plus a method
  `(a *App) gitPanelListW(pw)` as the single source every draw/hit-test/resize
  path consults. Updated its 3 call sites.
- Added `gitPanelDividerX()` (screen column of the divider, or -1 when closed —
  mirrors `splitterX`'s contract), `dragGitListDivTo(x)`, and
  `resizeGitPanelListWidth(t)` (clamps to `[gitPanelMinListW, min(gitPanelMaxListW, pw/2)]`;
  leaves scroll offsets alone — width doesn't change the visible row count).
- `gitPanelPress` now returns a `dragMode` **string** (`""` / `"gitpanel"` /
  `"gitlistdiv"`) instead of a bool: header rule → height drag, divider column on
  a body row → width drag, ✕ → collapse, else click.

### 2. Wire the new drag through the mouse router (`internal/app/app.go`)

- Press dispatch simplified to `a.dragMode = a.gitPanelPress(x, y)`.
- New continuation branch: `if leftDown && a.dragMode == "gitlistdiv" { a.dragGitListDivTo(x); return }`,
  beside the existing `gitpanel` branch.

### 3. Active-drag highlight (`gitpanel.go` + `internal/app/terminal.go`)

- `drawGitPanel`: header rule brightens to Accent while `dragMode == "gitpanel"`;
  divider brightens while `dragMode == "gitlistdiv"` (two separate styles so only
  the grabbed handle lights up).
- `drawTermPanel`: header rule brightens to Accent while `dragMode == "termpanel"`
  (bottom-dock only; left-dock keeps its vertical splitter as the handle).

### Resulting state — every inner edge: brightening handle + mouse drag

| Inner edge | Draggable | Idle → grabbed |
|---|---|---|
| Sidebar ↔ Editor | ✅ | `│` Subtle → Accent |
| Left-terminal ↔ Editor | ✅ | `│` Subtle → Accent |
| Editor ↔ bottom git panel | ✅ | `─` Subtle → **Accent (new)** |
| Editor ↔ bottom terminal | ✅ | `─` Subtle → **Accent (new)** |
| Git panel: list ↔ diff | ✅ **(new)** | `│` Subtle → Accent |

## Tests added

- `internal/app/gitpanel_test.go`:
  - `TestGitPanelListWidth_ClampsAndOverridesAuto` — auto third, user override,
    both-end clamps.
  - `TestHandleMouse_GitListDividerDrag` — full press→drag→release pipeline
    through the real mouse router; a header-row press on the divider column still
    starts a **height** drag, not a width one.
  - `TestDrawGitPanel_HandlesBrightenWhileDragging` — divider and header rule each
    light up in Accent only during their own drag (asserted via `SimulationScreen`
    cell styles).
  - Updated the double-click test's `gitPanelListWidth(pw)` → `a.gitPanelListW(pw)`.
- `internal/app/terminal_test.go`:
  - `TestDrawTermPanel_HeaderBrightensWhileDragging` — Subtle when idle, Accent
    while a `termpanel` drag is active.

## Verification

```
go build ./...            # clean
make test                 # go test -race ./... — all packages ok
gofmt -l <touched files>  # no output
go vet ./internal/app/    # clean
```

## Notes / follow-ups

- The git list width is **session-only** (like panel height and terminal sizes) —
  resets on restart. Persisting it would mean a `userconfig` addition; left out on
  purpose, consistent with "no layout config."
- `.claude/settings.local.json` (local permission grants added this session) was
  **not** committed — it's a local-only file.
- `go.mod` / `go.sum` carried a pre-existing tidy change (grsh moved
  indirect → direct); included in this commit.
- A plan file was written at `~/.claude/plans/unified-humming-quilt.md` after the
  fact (plan mode toggled on post-implementation); the work was already complete
  and verified by then.
