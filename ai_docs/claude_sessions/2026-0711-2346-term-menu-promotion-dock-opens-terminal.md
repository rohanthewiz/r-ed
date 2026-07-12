# Session: Terminal Menu Promotion + Dock Toggle Opens Terminal

Session ID: 3a939a78-40dd-4316-8397-301d481a1f11
Date: 2026-07-11

## Goal

Two user reports about the left-dock terminal layout shipped in the
previous session:

1. "Show terminal" needs to be a top-level menu item.
2. Flipping to "filetree on the right" then showing the terminal
   appeared to revert the layout (tree back on the left) with no
   terminal visible at all.

## Diagnosis — a UX trap, not a layout bug

Reproduced the exact flows end-to-end with simulation-screen tests
(menu clicks through the real `handleMouse` → `menuItemIndexAt`
scroll-aware hit-testing, then `draw()` + `GetContents()` assertions):

- "Show terminal" while `termDockLeft` was verified CORRECT at HEAD —
  strip renders on the left (header on row 0), tree stays docked right,
  in both the toggled-in-session and persisted-config-restart paths.
- The real culprit: **"Dock terminal left (tree right)" only flipped
  the layout flag without opening the terminal.** With the terminal
  closed, `termStripW()` = 0, so the tree teleported to the right edge
  and NOTHING appeared on the left — reads as breakage. Clicking the
  same row again (label now "Dock terminal at bottom") flipped the tree
  back, still with no terminal — exactly the reported symptom.
- Contributing factor: the View-toggles group sat at relY ~50 of a
  57-row menu layout; on typical windows the menu clamps and scrolls,
  so "Show terminal" was below the fold and hard to find at all.

Draw and hit-test share one geometry source (`relY` ± scroll), so no
off-by-one existed there. Also worth remembering: simulation-screen
tests must call `scr.Show()` before `GetContents()` or the screen reads
back empty.

## Changes

### 1. View toggles promoted to the top of the ≡ menu (app.go)

The group (Show/Hide sidebar, Show/Hide terminal, Dock terminal
left/bottom) moved from second-to-last to the SECOND group, right under
Tab actions — visible with zero scrolling even on a 24-row window
(visible band starts at relY 3; the rows land at relY 8–10). Comment in
`builtinMenuGroups` explains why the position is deliberate.

### 2. Dock toggle opens a closed terminal (terminal.go)

`menuToggleTermDock` now treats picking a dock as a "put the terminal
THERE" gesture: a closed terminal opens (+focused, `ensureTermSession`)
as part of the flip, in both directions. The layout can never flip to
show an empty edge. Bottom-flip still evicts the git panel (the
single-occupancy strip rule); the eviction condition simplified since
`term.open` is always true at that point.

## Tests (all green, `make test` with -race)

- `TestMenuToggleTermDockOpensClosedTerminal` — the bug-fix pin: both
  flip directions open a closed terminal; bottom flip evicts git panel.
- `TestTermDockLeftMenuFlow` — the reported journey end-to-end: real
  mouse events on the drawn screen, then asserts the strip header is on
  row 0 and `sidebarRect` is in the right half. Helper
  `clickMenuRowByLabel` scrolls the row into the band and clicks where
  `drawMenu` paints it, exercising the scroll-aware dispatch.
- `TestMenuLayout_TerminalRowsAboveTheFold` — guards the promotion:
  terminal rows must keep relY ≤ 22.
- Updated: `TestMenuLayout_NoCustomActions` divider pins →
  `{2, 7, 11, 15, 19, 29, 33, 46, 54}` (count 45 / height 57
  unchanged); `TestMenuToggleTermDock` asserts the flip now opens the
  terminal.

## CLAUDE.md updates (keep these honored)

- Terminal panel, "Two dock modes" bullet: the dock toggle also OPENS a
  closed terminal — never flip the layout to show nothing; keep the
  terminal rows in the View-toggles group near the TOP of the menu
  (pinned by `TestMenuLayout_TerminalRowsAboveTheFold`).

## Loose ends / follow-ups

- If the user still sees the old behavior, they're likely running a
  stale installed binary (brew / `$GOPATH/bin`) — `make install` or the
  next release picks up the fix. Fresh build verified at `bin/r-ed`.
- Pre-existing gopls style hints (modernize min/max, rangeint) surfaced
  during edits — cosmetic, untouched.
