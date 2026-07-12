# Session: File Navigation History (Go Back / Go Forward) + Menu Shortcut Hints

Session ID: f31ba87d-20a0-4519-b65c-793a86063c2f
Date: 2026-07-12

## Goal

Two user requests:

1. Add the concept of navigation — go back to the previous file, or
   forward to the more current file (browser / VS Code style).
2. Show shortcut keys beside menu items in the ≡ menu.

## What shipped

### 1. App-wide navigation history (`internal/app/nav.go`, new)

Generalized the LSP-only "Jump back" stack (formerly
`lspState.navStack`) into a browser-style two-stack history owned by
`App.nav` (`navState{back, fwd []navLoc, suppress bool}`):

- `navLoc` = `{path, cursor pos}` — moved out of lsp.go.
- **Central recording**: `openFile` records the departure point on its
  two success paths (switch-to-open-tab, new tab); `tabBarClick`
  (which bypasses openFile) records its own switches. Tree clicks,
  finder, palette, and definition jumps all feed one trail for free.
- **Browser rules**: back pushes current onto fwd; any fresh
  navigation clears fwd; both stacks cap at `navStackMax = 50`;
  same-path opens and duplicate stack tops don't record.
- **`suppress` flag**: set while navBack/navForward drive openFile so
  the retrace never records into itself (else two backs bounce
  between two entries forever). LSP definition jumps also open
  suppressed and record explicitly with the request's origin position
  (same-file jumps move only the cursor — path-change recording would
  miss them).
- **Deleted-file guard** in `gotoNavLoc`: `openFile` treats a missing
  path as "create new file", so without an os.Stat guard Go back
  would silently resurrect deleted files as empty buffers. Entry is
  dropped with a "File no longer exists" flash. A still-open tab
  bypasses the stat (buffer exists even if disk file is gone).

Surfaces (per the every-action-in-the-≡-menu house rule):

- ≡ menu: new "Navigation" group (Go back / Go forward) after Search.
- Leaders: `Esc-o` back (kept from the LSP era), `Esc-O` forward
  (shifted = opposite direction, mirroring h/H hunks).
- `Alt+←` / `Alt+→`: handled in handleKey ABOVE the focused-terminal
  and image-tab branches so navigation works from anywhere. tmux's
  Esc-prefixed arrows fold into ModAlt, same as Alt+↑/↓.
- `menuJumpBack` / `pushNav` / `hasNavBack`-on-lsp are gone;
  `menuNavBack` / `menuNavForward` / `recordNav` replace them.

### 2. Menu shortcut hints

- `menuItemDef` gained a `shortcut string` field — display-only
  accelerator column, right-aligned + muted in drawMenu ("esc s",
  "alt+←", "ctrl+d", "cmd+c", "esc o / alt+←" …).
- Dispatch still lives in the leader table / handleKey — rebinding a
  key means updating both or the menu lies (noted in CLAUDE.md).
- Hover rows draw the hint on the hover bg (no hole in the highlight
  bar); a long label suppresses the hint rather than colliding.

## Files touched

- `internal/app/nav.go` + `nav_test.go` — new.
- `internal/app/lsp.go` — navStack/pushNav/menuJumpBack removed;
  handleLSPDefinition records via recordNav with suppress.
- `internal/app/app.go` — `App.nav` field; recording in openFile +
  tabBarClick; Navigation menu group; Alt+←/→ branch; `shortcut`
  field + drawMenu accelerator column; shortcut strings on ~20 rows.
- `internal/app/leader.go` — 'o' → menuNavBack, 'O' → menuNavForward.
- `internal/app/lsp_test.go` — jump tests moved to a.nav.back, fwd
  assertions added; TestPushNavCap moved to nav_test; LSP leader test
  narrowed to d/i; "Jump back" menu row assertions removed.
- `internal/app/app_test.go` — menu geometry pins: 46 items, 10
  groups, height 59 (61→62 with custom actions), dividers
  [2 7 11 15 19 22 32 35 48 56].
- `CLAUDE.md` — architecture map row for nav.go; new "Navigation
  history" and "Menu shortcut hints" design-pattern sections; LSP
  leader note updated.

## Test notes

- `nav_test.go` pins: central recording (+ same-path no-op), full
  back/forward round trip with cursor restore, retrace-doesn't-record,
  fwd cleared on new navigation, 50-cap, empty-stack flashes,
  deleted-file drop, tab-bar recording (via drawTabBar +
  lastTabRects hit test), Alt-arrow handleKey path, o/O leader
  bindings, menu rows + predicates, and the drawn hint column
  (SimulationScreen needs `scr.Show()` before `GetContents()` — it
  serves the front buffer).
- Full suite green: `make test` (race detector), `go vet`, gofmt,
  `make build`.

## Follow-up ideas (not done)

- Same-file cursor history (VS Code records big intra-file jumps —
  find, hunk nav, PgUp bursts). recordNav already supports it; the
  call sites just don't record those today.
- Show shortcut hints in the palette rows too (palette shares
  menuLayout labels but doesn't render the shortcut column).
- Mouse buttons 4/5 (back/forward) — tcell reports them as
  Button4/Button5 wheel on some terminals only; needs investigation.
