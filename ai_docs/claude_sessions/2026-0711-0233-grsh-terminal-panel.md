# Session: Embedded grsh Terminal Panel

Session ID: 673cfd81-a8dd-4987-baab-1ec2fd25d72f
Date: 2026-07-11

## Goal

Add terminal integration to r-ed using github.com/rohanthewiz/grsh (the
user's hybrid bash+Go shell language). Two decisions were made up front
via discussion:

1. **Embedded grsh REPL panel**, not a PTY terminal. A real terminal
   would require adopting/writing a VT100 emulator (the maintained
   pure-Go options are thin); r-ed users are already inside a terminal
   and can split a tmux pane for vim-grade needs. The panel's job is
   `go test ./...`, git commands, quick greps, and grsh's Go
   interpolation.
2. **grsh's public API first** — everything in grsh lived under
   `internal/`, so r-ed literally could not import it.

## Part 1 — grsh public embedding API (repo: ~/projs/go/grsh)

Commit `78d4ea9` "Public embedding API: root grsh package + embedded
execution mode" (committed + pushed this session).

- New root `package grsh` (session.go), the module's ONLY public
  package: `NewSession(Options)` → `Session` with serialized,
  panic-recovering `Eval`, plus `NeedsMore`, `Idents`, `Notifications`,
  `LastStatus`, `Cwd`, `Interrupt`/`Kill`, and `ExitCode`/`UserMessage`
  helpers.
- New **embedded mode** in internal/shellexec (embedded.go): foreground
  pipelines run in their own process group so a host can SIGINT/SIGKILL
  them without signaling itself. Crucially it waits with `exec.Cmd.Wait()`
  (not `syscall.Wait4`) because hosts pass non-file writers — os/exec
  pumps those through copier goroutines that only `Wait` flushes.
  No tcsetpgrp, ever.
- Host-terminal protection: embedded stdin (including inside `$()`
  substitutions) defaults to EOF so children can't read the host's
  raw-mode tty; `$()` stderr routes to the session's stderr via new
  `State.CaptureErr` instead of the process stderr.
- Contract doc: grsh/docs/EMBEDDING.md; README got an Embedding section.
- Full grsh suite passes with `-race`.

Key contract points for r-ed: `cd`/`export` are process-global (grsh's
deliberate design — r-ed must stay absolute-path based); background
`&` job output to in-process writers is discarded (redirect to keep it);
pure-Go evaluation (interpreted `for {}`) is not interruptible.

## Part 2 — r-ed terminal panel

New file `internal/app/terminal.go` + `terminal_test.go`; integration
edits in app.go, gitpanel.go, leader.go, app_test.go; CLAUDE.md updated
(architecture map + new "Terminal panel" design-patterns section).

Design (all documented in CLAUDE.md):

- **Single-occupancy bottom strip**: terminal and git panel swap, never
  stack — avoids circular height-clamp math between two resizable
  strips. Opening one collapses the other; both keep their state.
- **Focus flag, not a modal**: `term.focused` routes plain editing keys
  to the input line. Esc stays global, so leaders and double-Esc menu
  work from inside the terminal. Any click outside the panel unfocuses.
- **Esc-`** leader = focus-or-toggle (VS Code's terminal key, minus the
  Ctrl). `=`/`-` leaders now resize whichever bottom panel is open via
  growBottomPanel/shrinkBottomPanel. ≡ menu row "Show/Hide terminal"
  in the View group (labelFor toggle).
- **Coalescing writer**: grsh output appends to a mutex-guarded buffer
  with at most one `termOutputEvent` in flight — a command printing
  thousands of chunks can't overflow tcell's bounded event queue.
- **Stop button ⏹** (no Ctrl+C in r-ed): first press Interrupt
  (SIGINT to the child's own pgroup), second press Kill. Ladder resets
  on command completion.
- Scrollback: ANSI escapes stripped (theme owns color), bare `\r`
  rewinds the partial line (progress bars), tabs expand to 8-col stops,
  5000-line cap, follow-tail only when already at bottom (the
  cursorMoved yank rule applied to a shell).
- Continuation: `NeedsMore` accumulates lines with a `…` prompt so
  blocks span prompts exactly like the standalone grsh REPL.
- `exit` closes the panel and discards the session; next open is fresh.
- After each command completes: `refreshTreeNow()` — shell commands
  create/modify files.
- Cmd+V pastes the text clipboard into the command line while focused.
- History: Up/Down with readline-style draft stash.

### Testing

- `fakeTermEval` injected via `newTermEvaluator` package-var stub in
  newTestApp (same pattern as builtinCommandFor / fakeLSPConn) — tests
  can never execute real commands.
- One deliberate exception: `TestTermRealGrshIntegration` swaps the
  real grsh session in and runs `echo` end-to-end so a grsh upgrade
  that breaks the embedding contract fails in CI.
- ~25 new tests; menu-layout geometry pins updated (41 items, height
  53, last divider 50; custom-actions variant 56).
- `make test` (race) fully green; `go vet` + `gofmt` clean.

### Dependency note

r-ed go.mod: added `github.com/rohanthewiz/grsh` at commit 78d4ea9
(pseudo-version), which bumped the `go` directive 1.24.0 → 1.26.1
(grsh requires it — uses `errors.AsType`). CI workflows use
`go-version: stable`, so no workflow changes were needed. New indirect
deps (logrus via logger, x/sys, x/term upgrades) are pure Go — the
no-CGO static-binary contract holds.

## Follow-up ideas (discussed, not built)

- Tab completion from `Session.Idents()`.
- Scrollback text selection / copy.
- SGR color rendering (currently stripped).
- Virtual per-session cwd in grsh (today `cd` chdirs the whole editor
  process by grsh's design).

## Manual verification checklist

Run `./bin/r-ed`, then: ≡ → Show terminal (or Esc-`), run `ls`,
`go version`, a `for i := 0; i < 3; i++ { fmt.Println(i) }` Go loop,
`touch zz.txt` (tree should show it within a beat), `sleep 30` + ⏹,
drag the header rule, wheel-scroll up during output, `exit`.
