# Session: Terminal `clear` fix + collapsible ≡-menu sections

Session ID: 65f45fc7-4ae5-4c42-95dc-b1d69d5ae98e
Date: 2026-07-21

Two independent pieces of work this session:

1. Make `clear` actually clear the embedded terminal panel (committed
   separately: `ff1da60`).
2. Add expand/collapse-by-section to the action (≡) menu (this commit).

---

## 1. Terminal `clear` did nothing — `ff1da60`

### Symptom

> "typing `clear` in the terminal does not clear the terminal"

### Root cause

The embedded terminal is a grsh REPL strip, **not a PTY**. grsh has no
`clear` builtin, so `clear` runs `/usr/bin/clear`, which just prints the
ANSI erase-screen escapes:

```
$ clear | xxd   →  1b5b 334a 1b5b 481b 5b32 4a   (\033[3J \033[H \033[2J)
```

Those are exactly the CSI sequences `stripTermANSI` throws away before
the scrollback ever sees them, so `clear` / `tput clear` / `reset` were
silent no-ops.

### Fix (`internal/app/terminal.go`)

Handled it at the escape-sequence level so the whole family works, not
just the literal string `clear`:

- **`stripTermANSI`** now *normalizes* (rather than drops) the
  screen-clearing escapes to an ASCII form feed (`\f`): Erase-in-Display
  whole-screen (`[2J`) / scrollback (`[3J`), and the RIS full-reset
  `ESC c`. Partial erases (`[0J`, `[K`) are still stripped as before.
- **`termAppendOutput`** gained a `case '\f'` that wipes `lines`, clears
  the `partial` tail, and resets `scroll` — which also correctly honors
  a literal Ctrl-L in output. It sits alongside the existing `\r`
  progress-bar and `\t` tab-expansion cases in the same rune loop.

Covers `clear`, `tput clear`, `reset`, and `printf '\033c'` uniformly.

### Tests (`internal/app/terminal_test.go`)

- Extended `TestStripTermANSI` with ED/RIS→`\f` cases (updated the
  pre-existing RIS case `a\x1bcb` → `a\fb`) + negative `[0J`/`[K` cases.
- Added `TestTermClearWipesScrollback` — feeds the exact byte stream
  `/usr/bin/clear` emits and asserts lines/partial/scroll all reset, then
  that post-clear output starts a fresh screen.

---

## 2. Collapsible ≡-menu sections (this commit)

### Ask

> "Is there a way to fold up the main menu by section?" → "and allow
> expand / collapse?"

Before: the ≡ menu was a flat list of groups separated by thin,
**unlabeled** dividers — no section headers, nothing to click, and the
only compaction was scrolling when it outgrew the window.

### Design

Each action group is now a titled, foldable section:

```
┌───────────────────────────┐
│  Menu                esc  │
├───────────────────────────┤
│ ▾ Tab                     │  ← chevron/title = fold toggle
│   Save              esc s │
│   Close tab         esc w │
│ ▸ Git                     │  ← folded: header stays, rows gone
│ ▾ View                    │
│   ...                     │
├───────────────────────────┤
│ Quit editor         esc q │  ← headerless (can't fold the exit away)
└───────────────────────────┘
```

### Changes (`internal/app/app.go`)

- **New `menuGroup` type** (`title` + `collapsible` + `items`).
  `builtinMenuGroups()` return type changed `[][]menuItemDef` →
  `[]menuGroup`; the nine builtin groups got titles (Tab, View, History,
  Search, Navigation, Git, Code, File, Edit) and `collapsible: true`.
  Quit is `collapsible: false`.
- **`menuItemDef.header bool`** — marks a synthetic section-header row.
- **`App.menuCollapsed map[string]bool`** — fold state keyed by title.
  Session-only (like `menuScroll` / panel sizes), nil = all expanded,
  survives menu close/reopen (NOT reset in openMenu/closeMenu), not
  persisted to config. Read via nil-safe `sectionCollapsed(title)`;
  flipped by `toggleMenuSection(title)` (the header's action).
- **Split the custom-actions splice out of `menuLayout`** into a new
  `visibleMenuGroups() []menuGroup` (builtins + custom group titled
  "Custom", spliced before Quit). `menuLayout` now iterates that,
  stamping a header row above each collapsible group and **omitting the
  items entirely while collapsed** (so folded rows are neither drawn,
  hit-tested, nor keyboard-reachable). Headerless groups (Quit) get a
  leading divider instead. Headers replaced the old between-group
  dividers, so `dividers` is now just `[2, <before-Quit>]`.
- **`openMenu`** rewritten to preselect the first enabled **non-header**
  row, so a reflex Enter runs an action, not a fold. (Headers remain
  keyboard-selectable via arrows — that's how you fold with the
  keyboard.)
- **`drawMenu`**: header rows draw a fold chevron `▾`/`▸` (AccentSoft) +
  bold-Accent title and no shortcut column; item rows dropped the old
  decorative `▸` and sit in the now-empty gutter so they read as nested.

### Layout math (pins in `TestMenuLayout_NoCustomActions`)

Fully expanded, no custom actions: 47 action rows + 9 headers = **56**
rows; `modalHeight` **61** (was 60 — headers replaced dividers, net +1);
`dividers = [2, 58]`. With 2 custom actions: height **64**.

### Palette bug this would have introduced — fixed (`internal/app/palette.go`)

`paletteActionItems` flattened `menuLayout()`, which now (a) includes
header rows and (b) omits folded sections' items. So the command palette
would have listed section titles as fake commands **and hidden actions
from any folded section**. Fixed by sourcing from `visibleMenuGroups()`
instead — fold-independent and header-free.

### Tests

- `app_test.go`: rewrote `TestMenuLayout_NoCustomActions` (56/61/[2,58]);
  added `TestMenuLayout_CollapseHidesSectionRows`,
  `TestMenuHeaderClickToggles` (mouse dispatch; menu stays open),
  `TestMenuFoldSurvivesReopen`, `TestOpenMenuSkipsHeaderSelection`,
  `TestDrawMenu_HeaderChevronReflectsFold` (`▾`↔`▸` via SimulationScreen).
  Helpers `menuHeaderIndex` / `menuRowIndex`.
- Fixed three tests carrying old assumptions: `TestMenuModalRect_Centered`
  and `TestMenuModalRect_ClampsToWindowHeight` bumped their "tall window"
  from 60→64; `TestMenuMoveSelection_ScrollsIntoView` now starts at row 0
  explicitly (openMenu no longer lands on the first row, since the first
  enabled action sits below the Tab header).
- `palette_test.go`: `TestPalette_FoldedSectionActionsStillListed` — a
  folded section's actions stay in the palette, and no header leaks in.

### Gotcha worth remembering

Folding shrinks the modal, which **re-centers** it vertically — so a row's
screen position shifts under the cursor after a fold. `TestMenuHeaderClickToggles`
recomputes the header's on-screen row before each click for that reason.
The re-centering is expected (same as any modal resize), left as-is.

### Doc

`CLAUDE.md` "Modal layout via `relY`…" section gained a **Collapsible
sections** paragraph (the `menuGroup`/header/`menuCollapsed` model, why
Quit is exempt, session-only fold state, and that the geometry pins count
headers).

---

## Verification

```
go build ./...           # clean
go test -race ./...      # all 13 packages ok
gofmt -w <touched files> # applied
```

## Follow-ups offered (not done)

- Persist fold state to `config.json` so folds survive restarts — wired so
  it's a ~2-line add (load into `menuCollapsed` on New, save in the
  toggle). Left session-only for now, consistent with the "no layout
  config" stance.
