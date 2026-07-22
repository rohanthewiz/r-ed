# Session: Auto-source a grsh rc file into the terminal panel

Session ID: ccd34cdb-9aa5-4670-afb9-8cb32b572053
Date: 2026-07-22

### Ask

> "In r-ed's terminal (grsh), my zsh aliases and shell funcs etc, are not
> being picked up."

Then, after diagnosis: **"yes, implement the auto-sourced rc file."**

### Diagnosis

The terminal panel embeds a **grsh** session
(`github.com/rohanthewiz/grsh`) — grsh's own shell language (bash-style
shell + Go expressions), **not zsh** and **not a PTY**. `grsh.NewSession`
starts a blank session and r-ed never sourced anything into it
(`ensureTermSession`), so zsh startup files were never read. Two finer
points surfaced:

- Aliases/functions are **zsh-local** — zsh never exports them, so *no*
  embedded shell could inherit them regardless.
- **Exported env vars DO carry over** (grsh uses the real process
  environment) — it was specifically aliases/functions that were missing.

grsh has its own `alias`, functions, and a `source f.grsh` builtin, but
no rc-file convention was wired into the embedding path.

### Design decision

Give the terminal the **grsh analog of `~/.zshrc`**: auto-source
`~/.config/r-ed/rc.grsh` when the session is created. Fits r-ed's config
conventions (`~/.config/r-ed/`, XDG-aware) and the silent-degradation
contract used by the LSP/formatter integrations.

Key choices:

- **Uses grsh's `source` builtin** (`source '<path>'`) rather than
  read-and-eval, so grsh gives file:line diagnostics for a broken rc.
  Single-quoted path so spaces can't split the arg.
- **Go stats the file first** — an absent rc (the common case) must stay
  silent, so we don't let grsh's `source` error on a missing file. Any
  stat error (incl. permissions) → skip; the terminal must still open.
- **Synchronous eval on the main loop** — a real shell blocks on its rc,
  and doing it before `submitTermCommand` can run kills the race where a
  typed command outruns an async source. Documented as "handful of
  aliases, not long-running work" (same as `~/.zshrc`).
- **Broken rc → one `termErr` scrollback line**, never a modal. An rc
  that calls `exit` is ignored (no teardown).
- Re-sourced on every fresh session (after `exit`, next open) — matches
  opening a new shell.

### Changes

**`internal/userconfig/userconfig.go`**
- New **`RcPath()`** → `$XDG_CONFIG_HOME/r-ed/rc.grsh` else
  `~/.config/r-ed/rc.grsh` (or `""`).
- Factored shared dir logic into unexported **`configFilePath(name)`**;
  `DefaultPath` now delegates to it too, so `config.json` and `rc.grsh`
  can't drift into different directories.

**`internal/app/terminal.go`**
- New package var **`termRcPath = userconfig.RcPath`** — a test seam like
  `newTermEvaluator`/`termExitCode`.
- New method **`sourceTermRc()`**, called at the end of
  `ensureTermSession` after the session is built. Guards nil-sess /
  empty-path / stat-fail, then `Eval("source '<path>'")`; surfaces a
  non-exit error as a `termErr` line via `grsh.UserMessage`.

**`internal/app/app_test.go`**
- `newTestApp` now stubs `termRcPath = func() string { return "" }` (with
  cleanup) so `ensureTermSession` never sources the dev machine's real
  `rc.grsh` — its `source` eval would otherwise land in
  `fakeTermEval.evals` and skew command assertions.

**`CLAUDE.md`** — new bullet in the Terminal-panel house rules describing
the rc file, the grsh-not-zsh reason, the silent-degradation contract,
the synchronous-source rationale, and the `termRcPath` test seam.

### Tests

**`internal/userconfig/userconfig_test.go`** (2 new)
- `TestRcPathHonoursXDG` — XDG wins; also asserts `RcPath` dir ==
  `DefaultPath` dir (the shared-helper guarantee).
- `TestRcPathFallsBackToHome` — `~/.config/r-ed/rc.grsh` fallback.

**`internal/app/terminal_test.go`** (3 new)
- `TestTermRc_SourcedWhenPresent` — rc present → the session's first (and
  only) eval is `source '<path>'`, recorded synchronously (no waitEvals).
- `TestTermRc_SkippedWhenAbsent` — non-existent path → zero evals.
- `TestTermRc_ErrorSurfacedInScrollback` — injects a `fakeTermEval` whose
  `Eval` fails → a `termErr` line is appended.

### Verification

```
go build ./...                          # clean
go vet ./...                            # clean
go test -race ./...                     # all 13 packages ok
```

Plus a **throwaway real-grsh smoke check** (created under a temp dir in
the repo module, run, then deleted): a real `grsh.NewSession` sourced an
rc containing `alias hi='echo aliased-hello'` and then invoking `hi`
expanded to `aliased-hello` — proving end-to-end behavior beyond the
fakeTermEval unit tests. Working tree left clean (no throwaway artifacts,
`go.mod`/`go.sum` untouched).

### User action taken

User created a starter rc during the session:

```
~/.config/r-ed/rc.grsh   →   alias g='git status'
```

### Committed

`14809db` — "Auto-source ~/.config/r-ed/rc.grsh into the terminal panel"
(6 files: the feature, tests, CLAUDE.md). No AI trailers, per project
convention. Local only — not yet pushed at commit time; a release still
needs the deliberate push-to-`release` flow.

### Notes / caveats for the user

- The rc file must be **grsh syntax, not zsh**. grsh v1 aliases split
  values on whitespace (no nested quoting); zsh-specific constructs won't
  work.
- Rebuild to pick it up: `make install` (or `make build` → `./bin/r-ed`).
- Offered but not done: seed a fuller starter `rc.grsh` by translating
  the user's `~/.zshrc` aliases to grsh (awaiting the user to share them).
