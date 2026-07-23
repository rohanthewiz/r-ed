# Session: Copilot integration — Phase 2 (ghost-text inline completions)

Session ID: 40e90e2f-39b3-40bb-882d-6225bc5c8475
Date: 2026-07-22

### Ask

> "Start on phase 2."

Loaded from the phase-1 session doc (2026-0722-2254). One mid-flight
directive landed and is now a standing decision:

1. **Inline completions must be OPTIONAL** — a separate persisted
   toggle, not bundled into the main Copilot on/off.

### What was built (Phase 2, complete)

**Rendering — `internal/editor/ghost.go` (new) + `tab.go`**

- `GhostText{Pos, Text, MoreLines}` display form on the Tab; the app
  layer owns setting/clearing it.
- Ghost text is deliberately NOT a `DecorationSource` — decorations can
  only restyle cells the buffer owns; a suggestion ADDS cells. Instead
  `Tab.Render` splices the proposal into the cursor row's runes/styles
  (`ghostOverlay`) AFTER decoration merge, so the existing paint walk
  (tab stops, ScrollX, overflow arrows) needed zero changes.
- First line paints inline (dim italic, `th.Muted`); multi-line
  proposals get a `⋯+N` marker — no virtual rows ever (they'd ripple
  through scrolling, hit-testing, every rect helper).
- The overlay refuses to paint when its anchor drifted from the live
  cursor — belt-and-braces under the app-level clearing.

**Doc sync + requests — `internal/app/copilot_ghost.go` (new)**

- `didOpen`/`didClose` follow tab lifecycle for ALL text files (Copilot
  completes every language, unlike gopls); `copilotLanguageID` maps
  ext → languageId with the-ext-is-the-id fallthrough.
- **Lazy didChange**: flushed only right before a completion request.
  The Copilot server only answers questions we ask — steady sync would
  be traffic for nobody. One debounce serves both concerns.
- 300ms debounce (mirrors the LSP rhythm) armed ONLY by EditRev
  movement — cursor travel never spends a request; `copilotOpenDoc`
  seeds `armRev` so opening a file doesn't fire one either.
- Request: `textDocument/inlineCompletion` with versioned uri, UTF-16
  position, `context.triggerKind: 2` (automatic, the server's dialect),
  formattingOptions from `IndentUnit`. Client capability
  `textDocument.inlineCompletion` added to the initialize handshake.
- Response staleness gates: reqSeq must be latest AND (path, EditRev,
  cursor) must still match. Stale/error/empty all end silently.
- Items decoded from raw JSON but the raw is KEPT — `didShowCompletion`
  telemetry echoes it verbatim so the server's correlation fields
  survive fields this client doesn't model.

**Accept / dismiss**

- Tab accepts only while the ghost is painted; falls through to plain
  indent otherwise. Accept replaces the server-given RANGE with the
  full InsertText via MoveCursorTo + extend + InsertString — the
  selection path coalesces into ONE structural undo step (pinned by
  test: one undo restores pre-accept text).
- Acceptance telemetry = executing the item's command (async,
  fire-and-forget). Partial-accept telemetry not applicable — no
  partial-accept UI in this phase.
- Esc clears the ghost as a pure side effect (never swallowed — leader
  and double-Esc menu behavior unchanged). Any invalidating event
  clears via `copilotAfterEvent` in the dispatch tail (runs right after
  `lspAfterEvent`).

**The optional toggle (mid-flight directive)**

- New `"suggestions"` config key (default on), `SaveSuggestions`, same
  absent-vs-off string pattern and unknown-key round-trip as every key.
- New ≡ Copilot-group row "Disable/Enable inline suggestions" between
  the auth and Copilot toggles. Independent of `"copilot"` so a user
  keeps the sidecar (sign-in today, chat later) while opting out of
  just the ghost text. Toggling off clears any visible ghost + timer
  immediately; toggling on while Copilot itself is disabled flashes the
  dependency instead of looking broken.

**State/teardown**

- All phase-2 state lives on `copilotState` (docVersions/docSyncedRev/
  armRev maps, single compTimer, reqSeq, ghostPath/Rev/Pos/Item/Raw).
- `copilotDisconnect` now also stops the timer, clears the ghost, and
  nils the doc maps; `handleCopilotReady` announces already-open tabs
  (same catch-up as handleLSPReady).

### Tests

Full suite green with `-race`; vet + gofmt clean. New:

- `editor/ghost_test.go` (4): splice math + style lockstep, staleness
  refusals, `⋯+N` marker, full Render paint on a SimulationScreen
  (buffer stays untouched).
- `app/copilot_ghost_test.go` (12): languageId map, doc-sync lifecycle
  (didOpen payload, in-sync flush no-op, version 2 didChange, didClose
  + bookkeeping drop), arm-on-edit-only, request wire shape, ghost
  show + telemetry echo, three staleness gates, accept via the REAL
  Tab-key path (incl. one-undo restore + executeCommand), no-ghost
  indent fallthrough, cursor-move invalidation, toggle + persistence
  (XDG_CONFIG_HOME redirect), disconnect teardown.
- 4 userconfig `"suggestions"` tests mirroring the copilot-key set.
- `fakeCopilotConn` grew param recording (`paramsFor`/`notified`) so
  tests assert wire shape, not just method names.
- Menu geometry pins updated for the new row: 62 rows, height 68,
  dividers `[2, 5, 65]` — in `TestMenuLayout_NoCustomActions`,
  `TestMenuLayout_WithCustomActions` (71), `TestMenuModalRect_Centered`
  (68), `TestMenuModalRect_ClampsToWindowHeight` (68), and CLAUDE.md.

### Docs / memory

- CLAUDE.md: architecture-map entries (copilot_ghost.go, ghost.go) +
  new "Copilot ghost text" design section; phase list now points at
  phase 3 only; pin numbers corrected.
- Claude memory: `copilot-integration-phases` updated — phases 1–2
  done, phase 3 (ACP chat) next; the optionality directive recorded.

### Trying it live

`copilot-language-server` on PATH + ≡ → Copilot → Sign in, then type
in any file and pause ~300ms; Tab accepts, Esc (or any move/edit)
dismisses. ≡ → Copilot → "Disable inline suggestions" is the opt-out.

### Next

Phase 3: chat panel via ACP (`copilot-language-server --acp`, same
binary/transport), docked LEFT (file tree stays RIGHT — owner
preference). Open problems: left-edge sharing with a left-docked
terminal, word-wrap/markdown rendering (none exists), multi-line
composer (consider reusing editor.Buffer).
