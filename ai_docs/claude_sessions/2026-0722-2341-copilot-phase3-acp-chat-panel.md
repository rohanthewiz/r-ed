# Session: Copilot integration — Phase 3 (ACP chat panel)

Session ID: f35684d7-ccc9-457e-a97c-66c9a228a297
Date: 2026-07-22

### Ask

> "Start on phase 3."

Loaded from the phase-2 session doc (2026-0722-2316). Phase 3 = chat
panel via ACP, docked LEFT with the file tree flipping RIGHT (owner
preference, recorded in memory).

### What was built (Phase 3, complete)

**ACP transport — `internal/lsp/acp.go` + small `client.go` extensions**

- ACP (Agent Client Protocol) is the same JSON-RPC 2.0 envelope the
  client already speaks; the ONLY wire difference is framing — one
  JSON object per line (ndjson) instead of Content-Length headers.
  So it rides the existing `lsp.Client` via a `ndjson` flag: `send`
  emits body+`\n`, `readLineMessage` parses lines (skips blanks,
  handles an unterminated final record at EOF, hard-errors on garbage
  since framing can't resync).
- Second addition: an `onRequest` hook. ACP agents send the client
  REAL requests (`session/request_permission`, `fs/*`) that need
  domain answers; when the hook is set, `respondToServer` delegates to
  it entirely (result → response, error → JSON-RPC -32601). Nil keeps
  the LSP auto-responder (gopls' workspace/configuration echo). Hook
  runs on the read-loop goroutine — post events, never touch App.
- Constructors: `NewClientACP(r, w, onNotify, onRequest, onExit)` and
  `StartACP(dir, bin, args, ...)`. No SDK, no second framing package.

**Chat panel — `internal/app/copilot_chat.go` (new, ~1100 lines)**

- Runs the SAME `copilot-language-server` binary as phases 1–2 but as
  a SECOND process in `--acp` mode (chat and completions are separate
  protocols by GitHub's design). Auth is phase 1's device flow — the
  agent reads the same credential store, no second sign-in.
- Handshake: `initialize` (protocolVersion 1, fs capabilities declared
  FALSE — see scope guard) → `session/new` {cwd: absolute rootDir,
  mcpServers: []} → sessionId. Lazy start on first panel open.
- Turns: Enter → `session/prompt` with one ACP text block; the call
  BLOCKS server-side for the whole turn so it uses `CallWithTimeout`
  (15 min), never the 5s default. `session/update` notifications
  stream in-flight: `agent_message_chunk`s merge into ONE trailing
  agent message; `tool_call` becomes a muted `⚙ <title>` one-liner;
  thoughts/plans deliberately dropped. Stale-session updates ignored.
  ⏹ sends `session/cancel` (once per turn); stopReason "cancelled"
  gets a "— stopped" marker, clean end adds nothing.
- **Scope guard — chat only**: no fs capabilities declared, and
  `session/request_permission` is auto-declined with the agent's own
  reject option (`chatAutoRejectPermission`, pure, runs on the read
  loop; reject_once preferred, cancelled outcome as fallback), with a
  `⊘ declined: <title>` transcript note. Permission UI is a later
  phase by decision, not omission.
- **Layout**: full-height LEFT strip, file tree flips RIGHT. All
  geometry now pivots on `treeOnRight()` (`termDockLeft || chat.open`)
  and `leftBlockW()` — sidebarRect, splitterX, inSidebarBlock,
  resizeSidebar, rightBlockW all generalized off the raw termDockLeft
  checks. Left edge is SINGLE-OCCUPANCY: opening chat closes a
  left-docked terminal and vice versa (menuToggleTerminal /
  menuToggleTermDock evict chat; menuToggleChat evicts the terminal),
  mirroring the bottom strip's terminal/git exclusivity and for the
  same clamp-math reason. A bottom-docked terminal coexists. Vertical
  splitter on the strip's right edge (dragMode "chatsplit").
- **Transcript is the model, rows are derived**: `chatRows(width)`
  re-wraps `[]chatMsg` on demand — greedy word wrap for prose
  (`wrapChatText`, long words hard-broken), hard wrap for fenced code
  (indentation is meaning), fences styled on the sidebar background,
  `❯` gutter on user prompts, blank separator rows. Resize re-flows
  free. Scroll follows the termAtBottom rule (streaming only follows
  the tail if the user was already there); transcript capped at 500
  messages.
- **Composer**: single-line `textField` — Enter sends, Up/Down history
  with draft stash (readline behavior, cloned from the terminal),
  Cmd+V pastes with newlines flattened to spaces. A prompt typed
  mid-handshake is QUEUED (`queuedPrompt`, the signInWanted pattern)
  and flushed by handleChatReady — the first Enter must never vanish.
- **Failure transparency**: unlike the silent sidecar, a failed
  handshake writes WHY into the transcript (plus a "Sign in first: ≡ →
  Copilot → Sign in to GitHub" hint when signed out) — an open panel
  failing silently reads as breakage. Death follows the no-auto-restart
  rule; ≡ Copilot off/on clears BOTH dead verdicts.
- **No new config key**: `"copilot"` gates chat too. Disabling Copilot
  tears down the chat agent and closes the panel; open state is
  session-only (like the git panel). Menu row "Show/Hide Copilot chat"
  lives in the View group above the fold (pinned by the
  TerminalRowsAboveTheFold test). No leader binding yet — menu +
  palette reach it; 'c' stays unbound per the leader table's note.
- Focus model: `chat.focused` routes plain keys (checked before
  term.focused in handleKey — click handlers keep the two flags
  mutually exclusive); Esc stays global; any click outside unfocuses.

### Tests

Full suite green with `-race`; vet + gofmt clean. New:

- `lsp/acp_test.go` (6): ndjson parser (blanks, unterminated tail,
  garbage), Call round trip over line framing (fails loudly if LSP
  framing leaks), Notify is one line, onRequest answers + error →
  JSON-RPC error response, notification dispatch. Gotcha caught: on
  io.Pipe a Notify from the test goroutine deadlocks against the
  fake agent's read — run it on a goroutine.
- `app/copilot_chat_test.go` (16): toggle labels, open-path flashes
  (disabled / dead — never a silent dead end), left-edge eviction both
  directions + bottom coexistence + dock-flip reclaim, layout flip
  (leftBlockW/rightBlockW/splitterX/sidebarRect), resize clamps, word
  wrap, row derivation (gutter/separator/fence rows), send wire shape
  via the real Tab-key path, queue-while-starting flush, chunk
  streaming/merge + stale-session drop, turn endings, interrupt
  once-per-turn wire shape, exit transparency (error + sign-in hint),
  permission auto-reject choices, history move, panel press
  (✕ / focus-steal), Copilot-toggle teardown, sim-screen paint smoke.
- `wireChat` helper injects `fakeCopilotConn` (chat shares the
  sidecar's conn interface on purpose); newTestApp now also sets
  `a.chat.dead = true` so no test can spawn the real binary.
- Menu geometry pins updated for the new View row: 63 rows, height 69,
  dividers `[2, 5, 66]`; WithCustomActions 72; ModalRect tests 69;
  CLAUDE.md numbers match.

### Docs / memory

- CLAUDE.md: architecture-map entries (copilot_chat.go, lsp/acp.go),
  new "Copilot chat panel (ACP)" design section, phase-3 "planned"
  note removed, pin numbers corrected.
- Claude memory: `copilot-integration-phases` — all 3 phases done
  2026-07-22; follow-ups recorded (multi-line composer via
  editor.Buffer, richer markdown, permission UI).

### Trying it live

`copilot-language-server` on PATH + signed in (≡ → Copilot → Sign in),
then ≡ → View → "Show Copilot chat". Type, Enter; ⏹ stops a turn; drag
the splitter to resize; ≡ Copilot off/on is the retry path after a
crash.

### Next / follow-ups (not scheduled)

- Multi-line composer (consider reusing editor.Buffer).
- Richer markdown rendering (only fences + wrap today).
- Permission UI so the agent could eventually edit files — would mean
  declaring fs capabilities and replacing the auto-decline.
