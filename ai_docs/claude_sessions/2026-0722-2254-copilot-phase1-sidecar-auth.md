# Session: Copilot integration — Phase 1 (sidecar + device-flow auth)

Session ID: b1c30de7-cf3d-495a-a4b4-60d88c6b4311
Date: 2026-07-22

### Ask

> "What would it take to build an AI panel supporting Github Copilot
> into r-ed similar to that used by the Zed editor?"

Then: **"Start on phase 1."** Two mid-flight directives landed and are
now standing decisions:

1. **Layout**: keep the file tree on the RIGHT, dock the future AI/chat
   panel on the LEFT ("I know it is unconventional, but it's my
   preferred setup right now").
2. **Chat transport**: ACP — "And yes. ACP sounds great!"

### Research → the phased plan

Zed's "AI panel" is two features. Research findings that shaped the plan:

- GitHub ships `copilot-language-server` as **prebuilt native binaries**
  (github/copilot-language-server-release) — no Node runtime. JSON-RPC
  2.0 over stdio with Content-Length framing, i.e. exactly what
  `internal/lsp/client.go` already speaks; the transport turned out to
  be fully protocol-generic.
- Auth is a device flow via custom methods: `signIn` →
  `{userCode, verificationUri, command}`; the client shows the code and
  confirms via `workspace/executeCommand`, which **blocks until browser
  auth completes** (minutes). `signOut`, `checkStatus`,
  `didChangeStatus` round out phase 1's surface. The handshake must
  send `initializationOptions.editorInfo/editorPluginInfo` or the
  server refuses service. Credentials are stored server-side, shared
  with other editors.
- Chat: the server's `--acp` mode (Agent Client Protocol) is the
  officially supported editor-chat path (what Zed/JetBrains use) vs.
  the internal REST API. Owner chose ACP.

**Phases**: (1) sidecar lifecycle + auth — this session; (2) ghost-text
inline completions (`textDocument/inlineCompletion` + a new paint hook
in `Tab.Render` — decorations can only restyle existing cells, so ghost
text is a new mechanism; Tab-accepts-only-while-visible); (3) chat panel
via ACP, docked LEFT (needs word-wrap/markdown rendering and a
multi-line composer — neither exists yet; must also resolve left-edge
sharing with a left-docked terminal).

### What was built (Phase 1, complete)

- **`internal/lsp/client.go`**: `CallWithTimeout` — `Call` delegates to
  it with the 5s default. Exists solely because the device-flow
  confirmation legitimately blocks for minutes.
- **`internal/userconfig`**: `"copilot"` key (`on`/`off`, default on) +
  `SaveCopilot`, same absent-vs-false string pattern and unknown-key
  round-trip as every other key.
- **`internal/app/copilot.go`** (new): `copilotState` on App,
  `copilotConn` interface (the *generic* Call/Notify surface, not named
  wrappers — every method is custom), six `copilot*Event`s, async
  spawn + handshake (`copilotInitialize`), sign-in/sign-out flows, menu
  labels, status-bar segment. Eager start from `New()` (async, no-op
  when disabled/missing) so the ≡ labels are honest from first click.
- **Wiring** (`app.go`): `App.copilot` field, config load, six
  handleEvent cases, `copilotShutdown()` in Close, new collapsible
  "Copilot" menu group (after "Code"), status-bar right side now
  composes `[Copilot fragment] · [git branch]`.
- **Sign-in UX**: confirm modal shows "Enter code XXXX-XXXX at
  github.com/login/device"; Yes copies the code (OSC 52), opens the
  browser, and **parks the code in the status bar** for the whole
  blocking wait — the modal is gone by then. Three distinct endings
  flashed: signed in (with login), `NotAuthorized` (no subscription),
  did-not-complete.
- **Deliberate UX deviation**: Copilot menu rows stay clickable when
  unavailable and flash WHY, unlike the dimming LSP rows — Sign in is a
  new user's first touch; a dimmed row is a dead end. Also
  `signInWanted` queues a sign-in clicked mid-handshake.
- **Policy**: no auto-restart after crash (`dead` verdict); the ≡
  enable/disable toggle is the deliberate retry path (enable clears
  `dead` and re-runs LookPath).

### Tests

Full suite green with `-race`. New: `TestCallWithTimeout` (in-memory
pipes; note io.Pipe writes block until read, so even the timed-out call
needs the fake server to *consume* the request); 4 userconfig copilot
tests; 17 tests in `copilot_test.go` around a mutex-guarded, scripted
`fakeCopilotConn` with a bounded-wait helper for goroutine legs.
`newTestApp` now sets `a.copilot.dead = true` and neuters
`copilotCopyCode`/`copilotOpenBrowser` (stubbable vars, the
`builtinCommandsFor` pattern) so no test touches the dev clipboard or a
browser. Menu geometry pins updated: 61 rows (2 top-zone + 49 actions +
10 headers), height 67, dividers `[2, 5, 64]` — CLAUDE.md's pin numbers
were one version stale and got corrected too.

### Docs / memory

- CLAUDE.md: architecture-map entry + "Copilot sidecar" design section
  (contracts, phases, the left-dock decision).
- Claude memory: `ai-panel-docks-left`, `copilot-integration-phases`.

### Trying it live

`copilot-language-server` is NOT installed on this machine yet.
`npm i -g @github/copilot-language-server` (or a native binary from the
release repo), then `make run` → ≡ → Copilot → Sign in. Existing
Copilot users likely land on "already signed in" (shared credential
store).

### Next

Phase 2: inline ghost-text completions — doc sync piggybacking on
`EditRev` (mirror the 300ms LSP debounce), ghost-text paint on the
cursor line inside `Tab.Render`, Tab-to-accept,
`didShowCompletion`/`didPartiallyAcceptCompletion` telemetry.
