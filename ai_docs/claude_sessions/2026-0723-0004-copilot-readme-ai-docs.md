# Session: Copilot AI features — README documentation (+ phase-3 wrap)

Session ID: f35684d7-ccc9-457e-a97c-66c9a228a297
Date: 2026-07-23 (continues the 2026-0722-2341 phase-3 session doc)

### Ask

> "Update the README.md with the instructions for enabling the AI
> features."

Follow-on to phase 3 (ACP chat panel), built earlier in this same
session — full design notes live in
`2026-0722-2341-copilot-phase3-acp-chat-panel.md`.

### What changed

**README.md** — first user-facing documentation of the Copilot
integration (the README previously never mentioned it):

- Features list: new "GitHub Copilot, optional" bullet linking to the
  section; leads with "installing the binary is the whole opt-in".
- New section **"AI features (GitHub Copilot)"** (after Format on
  save), written as a numbered walkthrough:
  1. *Install* — `npm install -g @github/copilot-language-server` or a
     native binary from github/copilot-language-server-release; verify
     with `--version`; the ≡ Copilot off/on toggle is the documented
     retry gesture after a mid-session install.
  2. *Sign in* — device flow as implemented: code + URL dialog, Yes
     copies the code and opens a browser, SSH note (finish auth on the
     laptop; the code stays in the status bar), `Copilot ✓` when done.
     Credentials stored by the server, shared with other editors —
     once per machine.
  3. *Inline suggestions* — Tab accepts / falls through to indent,
     Esc dismisses, `⋯+N` multi-line marker, one-undo-step accept, any
     text file; the separate "Disable inline suggestions" toggle.
  4. *Chat panel* — ≡ → View → Show Copilot chat, left dock with the
     tree flipping right, Enter/↑↓ history/⏹/✕/splitter, left-edge
     sharing rule with a left-docked terminal, and the honest scope
     note: chat is read-only, file-edit permission requests are
     auto-declined with a `⊘ declined` transcript marker.
- "Turning it off": the `"copilot"` / `"suggestions"` keys in
  `~/.config/r-ed/config.json` (first README mention of config.json
  keys for Copilot), both default on.
- Project layout tree: added the `internal/lsp/` line (JSON-RPC client
  — gopls, Copilot sidecar + ACP chat) so the tree matches the new
  section's references.

### Wrap

Committed with the phase-3 implementation and pushed to main:

- "Add GitHub Copilot phase 3: ACP chat panel" — lsp/acp.go (+client
  ndjson/onRequest), app/copilot_chat.go, layout pivots
  (`treeOnRight()`), tests, CLAUDE.md design section.
- "Document Copilot AI features in README" — this doc + README.

### Next / follow-ups (unchanged from phase 3)

Multi-line composer, richer markdown rendering, permission UI —
recorded in the copilot-integration-phases memory.
