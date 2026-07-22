# Session: ≡-menu — palette-first, collapse-by-default, expand-all button

Session ID: ff7ed658-a146-4a13-ba0f-f3749f09f187
Date: 2026-07-22

Three related tweaks to the action (≡) menu, building on the previous
session's collapsible-sections work (`f22a471`):

1. Move the Command palette to the **first** row of the menu.
2. Start the menu with **all sections contracted** (unless a fold state
   already exists).
3. Add a button to **expand all** sections.

All logic lives in `internal/app/app.go`; tests in
`internal/app/app_test.go`; docs in `CLAUDE.md`.

---

## Result — the menu now opens like this

```
┌────────────────────────────────────┐
│ Menu                           esc │
├────────────────────────────────────┤
│   Command palette            esc a │  ← 1. first row, pinned top zone
│   Expand all sections              │  ← 3. expand/collapse-all toggle
├────────────────────────────────────┤
│ ▸ Tab   ▸ View   ▸ History  …      │  ← 2. all sections start folded
│ ▸ Search ▸ Navigation ▸ Git …      │
│ ▸ Code  ▸ File   ▸ Edit            │
├────────────────────────────────────┤
│   Quit editor                esc q │
└────────────────────────────────────┘
```

The whole menu is a compact index of headers on open; the palette (fuzzy
gateway to every action) and the expand-all button keep everything one
click away.

---

## 1. Command palette pinned to the top

**Before:** the palette was the first item *inside* the "Search" group.

**Change:** removed it from the Search group's `items` and had
`menuLayout` prepend it as a **pinned top-zone row outside every group**,
above the first section header, followed by a divider. This matters
because of change #2 — an item inside a collapsible group would vanish
when that section folds, and the palette must never hide.

- `menuLayout`: builds the top zone (palette row at relY 3, toggle at
  relY 4, divider at relY 5) before the group loop, which now starts at
  relY 6.
- The palette keeps its `esc a` accelerator.
- `paletteActionItems` walks `visibleMenuGroups()`, which no longer
  contains the palette — so it stops listing itself naturally. The old
  `label == paletteMenuLabel` self-skip guard is now a harmless
  belt-and-suspenders (left in place).

## 2. Collapse-by-default at startup

New helper `seedMenuFoldDefault()` folds every collapsible section via
`setAllMenuSections(true)` — **but only when `menuCollapsed == nil`**, so
it never clobbers a fold state the user has already touched this session
("...unless there is a menu expand state already").

- Called from `New()` **after** `loadCustomActions()` so the synthetic
  "Custom" section folds too.
- Fold state is still session-only / not persisted (unchanged), and the
  map semantics ("nil = all expanded") are untouched — we just seed the
  map on first run.
- **Tests build the `App` struct directly (not via `New`)**, so they keep
  the fully-expanded default and existing geometry expectations; a test
  opts into the collapsed default by calling `seedMenuFoldDefault()`.

## 3. Expand-all / collapse-all button

Second top-zone row. New methods:

- `menuToggleAllSections()` — `setAllMenuSections(a.anyMenuSectionExpanded())`:
  if anything is open it collapses all; if all are folded it expands all.
  Like a section header it **leaves the menu open** so the reflow is
  visible in place (it doesn't call `closeMenu`).
- `expandAllToggleLabel()` — dynamic label: "Expand all sections" when
  everything's folded, "Collapse all sections" otherwise.
- Shared plumbing: `menuSectionTitles()` (collapsible groups only, Quit
  excluded, Custom included), `anyMenuSectionExpanded()`,
  `setAllMenuSections(collapsed bool)`.

---

## Tests (`internal/app/app_test.go`)

Updated geometry pins:

- `TestMenuLayout_NoCustomActions`: **57 rows** (2 top-zone + 46 group
  actions + 9 headers), height **63**, dividers **`[2, 5, 60]`**
  (was 56 / 61 / `[2, 58]`).
- `TestMenuLayout_WithCustomActions`: height **66** (was 64).

Rewrote:

- `TestMenuActivate_RunsHovered` — the new toggle row shares the
  `labelFor + empty-label` shape the old test used as a "toggle row"
  marker, and its first loop churned fold state (collapsing the section
  that holds the sidebar toggle). Rewritten to expand sections
  (`setAllMenuSections(false)`) then target the sidebar toggle by its
  dynamic label — the only reliable discriminator.

Added:

- `TestSeedMenuFoldDefault_ContractsEverything`
- `TestSeedMenuFoldDefault_RespectsExistingState`
- `TestMenuToggleAllSections`
- `TestMenuLayout_PaletteIsFirstRow`

All pass under `go test -race ./...`; `go vet` clean.

---

## Known trade-off flagged to owner

With collapse-by-default, the **View** group's terminal toggles now sit
behind a fold. `TestMenuLayout_TerminalRowsAboveTheFold` (the CLAUDE.md
"keep terminal rows above the fold" rule) still passes — it checks scroll
position, not fold state — but reaching "Show terminal" is now one extra
click (expand View, hit Expand-all, or use the palette). Inherent to the
collapse-all request. Offered to exempt the View section from the default
fold if desired; left as-is pending owner call.

---

## Files touched

- `internal/app/app.go` — top-zone in `menuLayout`; palette removed from
  Search group; new helpers (`menuSectionTitles`, `anyMenuSectionExpanded`,
  `setAllMenuSections`, `menuToggleAllSections`, `expandAllToggleLabel`,
  `seedMenuFoldDefault`); `New()` seeds the fold default.
- `internal/app/app_test.go` — pin updates, one rewrite, four new tests.
- `CLAUDE.md` — new "Pinned top zone + collapse-by-default" note under the
  collapsible-sections section; updated geometry-pin numbers.
