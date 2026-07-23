// =============================================================================
// File: internal/app/copilot_ghost.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// copilot_ghost.go is phase 2 of the GitHub Copilot integration:
// ghost-text inline completions. It rides the phase-1 sidecar (see
// copilot.go for lifecycle/auth) and follows the same house rules —
// silent degradation, events-only state mutation, menu-first controls.
//
// The pipeline, keystroke to ghost:
//
//	edit → EditRev bump → copilotAfterEvent (dispatch tail)
//	     → 300ms debounce timer → copilotCompletionTickEvent
//	     → flush didChange, Call textDocument/inlineCompletion (async)
//	     → copilotCompletionEvent → still same rev+cursor? → Tab.Ghost
//	Tab key accepts (only while visible); any other move/edit clears.
//
// Design choices worth spelling out:
//
//   - Doc sync is LAZY: didOpen/didClose track tab lifecycle, but
//     didChange is flushed only when a completion request is about to
//     fire. Unlike gopls (which needs steady sync to publish
//     diagnostics unprompted), the Copilot server only ever answers
//     questions we ask — syncing between requests would be traffic for
//     nobody. One debounce timer therefore serves both concerns.
//   - Only EditRev movement arms the debounce — pure cursor travel
//     never spends a request. Wandering through a file shouldn't
//     generate model calls; typing is the signal of intent.
//   - Responses are validated against the request's (path, EditRev,
//     cursor) triple AND a sequence number before painting. A stale
//     ghost is worse than none: it suggests text for a buffer state
//     that no longer exists.
//   - The ghost's display form lives on the Tab (editor package paints
//     it); the authoritative accept data (range + full insertText +
//     the raw item for telemetry) stays here. Accepting replaces the
//     server-given range, so multi-line and mid-line completions land
//     exactly as the model intended, not as the display approximated.

package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/lsp"
	"github.com/rohanthewiz/r-ed/internal/userconfig"
)

// copilotCompletionDebounce is how long after the last edit the inline
// completion request fires. Deliberately the same rhythm as the LSP
// didChange debounce: long enough that a typing burst costs one request,
// short enough that the ghost appears the moment the user hesitates.
const copilotCompletionDebounce = 300 * time.Millisecond

// -----------------------------------------------------------------------------
// Wire shapes — textDocument/inlineCompletion
// -----------------------------------------------------------------------------

// copilotInlineItem is the slice of one inline-completion item this
// integration consumes: the replacement text, the buffer range it
// replaces (typically line-start → cursor, so InsertText re-includes
// what's already typed), and the command executed to report acceptance.
// Items carry extra correlation fields the server needs echoed back in
// telemetry, which is why handlers keep the raw JSON alongside this.
type copilotInlineItem struct {
	InsertText string          `json:"insertText"`
	Range      *lsp.Range      `json:"range"`
	Command    *copilotCommand `json:"command"`
}

// copilotInlineResult is the inlineCompletion response envelope. Items
// stay raw here — see copilotInlineItem for why.
type copilotInlineResult struct {
	Items []json.RawMessage `json:"items"`
}

// -----------------------------------------------------------------------------
// Custom tcell events — the goroutine → main-loop bridge
// -----------------------------------------------------------------------------

// copilotCompletionTickEvent is posted by the debounce timer: "the
// document has been quiet — ask for a completion now".
type copilotCompletionTickEvent struct {
	when time.Time
	path string
}

// When satisfies the tcell.Event interface.
func (e *copilotCompletionTickEvent) When() time.Time { return e.when }

// copilotCompletionEvent lands an inlineCompletion response, tagged
// with everything needed to detect staleness on arrival.
type copilotCompletionEvent struct {
	when  time.Time
	seq   int             // request sequence — only the latest may paint
	path  string          // document the request was made against
	rev   int             // Tab.EditRev at request time
	pos   editor.Position // cursor at request time
	items []json.RawMessage
	err   error
}

// When satisfies the tcell.Event interface.
func (e *copilotCompletionEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Document sync
// -----------------------------------------------------------------------------

// copilotSyncs reports whether a tab is a document the Copilot server
// should know about. Broader than lspHandles on purpose — Copilot
// completes every language, so any real text file qualifies.
func copilotSyncs(t *editor.Tab) bool {
	return t != nil && t.Path != "" && !t.IsImage()
}

// copilotLanguageID maps a file path to the LSP languageId Copilot uses
// to pick its prompting context. Unknown extensions fall through to the
// bare extension — the server treats unrecognised ids as plaintext, so
// a miss degrades gracefully instead of gating the feature on a table.
func copilotLanguageID(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	switch ext {
	case "":
		return "plaintext"
	case "js", "mjs", "cjs":
		return "javascript"
	case "jsx":
		return "javascriptreact"
	case "ts":
		return "typescript"
	case "tsx":
		return "typescriptreact"
	case "py":
		return "python"
	case "rb":
		return "ruby"
	case "rs":
		return "rust"
	case "kt", "kts":
		return "kotlin"
	case "cs":
		return "csharp"
	case "c", "h":
		return "c"
	case "cc", "cpp", "cxx", "hpp":
		return "cpp"
	case "md", "markdown":
		return "markdown"
	case "yml", "yaml":
		return "yaml"
	case "sh", "bash", "zsh":
		return "shellscript"
	case "htm", "html":
		return "html"
	case "ex", "exs":
		return "elixir"
	case "hs":
		return "haskell"
	case "pl":
		return "perl"
	case "tf":
		return "terraform"
	case "txt":
		return "plaintext"
	default:
		// go, java, php, swift, json, toml, css, scss, sql, lua, zig,
		// dart, r, scala, vue, svelte … — the id IS the extension.
		return ext
	}
}

// copilotOpenDoc announces one tab's document to the Copilot server.
// Also seeds the completion arm bookkeeping to the tab's current rev so
// merely opening a file never fires a request — typing is the trigger.
// Safe to call for any tab; non-documents are skipped.
func (a *App) copilotOpenDoc(t *editor.Tab) {
	if !a.copilotReady() || !copilotSyncs(t) {
		return
	}
	if a.copilot.docVersions == nil {
		a.copilot.docVersions = map[string]int{}
		a.copilot.docSyncedRev = map[string]int{}
		a.copilot.armRev = map[string]int{}
	}
	a.copilot.docVersions[t.Path] = 1
	a.copilot.docSyncedRev[t.Path] = t.EditRev
	a.copilot.armRev[t.Path] = t.EditRev
	_ = a.copilot.client.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        lsp.PathToURI(t.Path),
			"languageId": copilotLanguageID(t.Path),
			"version":    1,
			"text":       t.Buffer.String(),
		},
	})
}

// copilotCloseDoc announces a closed tab and drops its bookkeeping,
// including any ghost the closing document owned.
func (a *App) copilotCloseDoc(path string) {
	if path == "" {
		return
	}
	if a.copilot.ghostPath == path {
		a.copilotClearGhost()
	}
	if _, open := a.copilot.docVersions[path]; !open {
		return
	}
	delete(a.copilot.docVersions, path)
	delete(a.copilot.docSyncedRev, path)
	delete(a.copilot.armRev, path)
	if a.copilotReady() {
		_ = a.copilot.client.Notify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": lsp.PathToURI(path)},
		})
	}
}

// copilotFlushDoc sends one full-text didChange if the buffer is ahead
// of what the server has seen. No-op when in sync — which is the usual
// case, since this runs once per completion request, not per keystroke.
func (a *App) copilotFlushDoc(t *editor.Tab) {
	if t == nil || !a.copilotReady() {
		return
	}
	if _, open := a.copilot.docVersions[t.Path]; !open {
		return
	}
	if t.EditRev == a.copilot.docSyncedRev[t.Path] {
		return
	}
	a.copilot.docVersions[t.Path]++
	a.copilot.docSyncedRev[t.Path] = t.EditRev
	_ = a.copilot.client.Notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{
			"uri":     lsp.PathToURI(t.Path),
			"version": a.copilot.docVersions[t.Path],
		},
		"contentChanges": []map[string]any{{"text": t.Buffer.String()}},
	})
}

// -----------------------------------------------------------------------------
// Trigger — the dispatch-tail hook
// -----------------------------------------------------------------------------

// copilotGhostActive reports whether inline completions should run at
// all right now: sidecar up, signed in, and the suggestions toggle on.
func (a *App) copilotGhostActive() bool {
	return a.copilotReady() && a.copilot.signedIn && a.copilot.suggest
}

// copilotAfterEvent runs after every event dispatch, mirroring
// lspAfterEvent: first drop a ghost the event invalidated, then arm the
// completion debounce if the active document's text moved. Cheap when
// idle — a nil check and a couple of integer compares.
func (a *App) copilotAfterEvent() {
	a.copilotInvalidateGhost()
	if !a.copilotGhostActive() || a.screen == nil {
		return
	}
	t := a.activeTabPtr()
	if !copilotSyncs(t) {
		return
	}
	if _, open := a.copilot.docVersions[t.Path]; !open {
		return
	}
	if t.EditRev == a.copilot.armRev[t.Path] {
		return
	}
	a.copilot.armRev[t.Path] = t.EditRev
	a.copilotArmCompletionTimer(t.Path)
}

// copilotInvalidateGhost clears the visible ghost when the world moved
// under it: different tab, further edits, or a cursor that walked away.
// The paint layer double-checks Pos == Cursor too, but clearing here
// also releases the accept bookkeeping so Tab can't land a stale item.
func (a *App) copilotInvalidateGhost() {
	if a.copilot.ghostItem == nil {
		return
	}
	t := a.activeTabPtr()
	if t == nil || t.Path != a.copilot.ghostPath ||
		t.EditRev != a.copilot.ghostRev || t.Cursor != a.copilot.ghostPos {
		a.copilotClearGhost()
	}
}

// copilotClearGhost removes the ghost from whichever tab carries it and
// resets the accept bookkeeping. Idempotent.
func (a *App) copilotClearGhost() {
	if a.copilot.ghostPath != "" {
		if t := a.tabByPath(a.copilot.ghostPath); t != nil {
			t.Ghost = nil
		}
	}
	a.copilot.ghostPath = ""
	a.copilot.ghostRev = 0
	a.copilot.ghostPos = editor.Position{}
	a.copilot.ghostItem = nil
	a.copilot.ghostRaw = nil
}

// copilotArmCompletionTimer (re)starts the single completion debounce.
// One timer, not per-path like the LSP's: completions only ever concern
// the active document, and a tab switch mid-countdown is handled by the
// tick's own staleness check.
func (a *App) copilotArmCompletionTimer(path string) {
	if a.copilot.compTimer != nil {
		a.copilot.compTimer.Stop()
	}
	scr := a.screen
	a.copilot.compTimer = time.AfterFunc(copilotCompletionDebounce, func() {
		_ = scr.PostEvent(&copilotCompletionTickEvent{when: time.Now(), path: path})
	})
}

// copilotStopCompletionTimer cancels a pending debounce, if any.
func (a *App) copilotStopCompletionTimer() {
	if a.copilot.compTimer != nil {
		a.copilot.compTimer.Stop()
		a.copilot.compTimer = nil
	}
}

// -----------------------------------------------------------------------------
// Request / response
// -----------------------------------------------------------------------------

// handleCopilotCompletionTick is the debounce firing on the main loop.
// Bail-outs guard everything that can have changed during the wait —
// tab switched, sidecar died, toggle flipped, a modal took the
// keyboard (a ghost popping in behind a modal would just be noise).
func (a *App) handleCopilotCompletionTick(e *copilotCompletionTickEvent) {
	if !a.copilotGhostActive() || a.modal != nil || a.menuOpen {
		return
	}
	t := a.activeTabPtr()
	if t == nil || t.Path != e.path {
		return
	}
	a.copilotRequestCompletion(t)
}

// copilotRequestCompletion syncs the document and fires the async
// inlineCompletion request, stamped with the state it was asked for.
func (a *App) copilotRequestCompletion(t *editor.Tab) {
	if _, open := a.copilot.docVersions[t.Path]; !open {
		return
	}
	a.copilotFlushDoc(t)
	a.copilot.reqSeq++
	seq := a.copilot.reqSeq
	client := a.copilot.client
	scr := a.screen
	path, rev, pos := t.Path, t.EditRev, t.Cursor
	// The server wants the editor's indentation so its insertText
	// matches what Tab would type. Width falls back to 4 for a tab
	// IndentUnit — the conventional display width.
	tabSize := len(t.IndentUnit)
	insertSpaces := !strings.Contains(t.IndentUnit, "\t")
	if !insertSpaces || tabSize == 0 {
		tabSize = 4
	}
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":     lsp.PathToURI(path),
			"version": a.copilot.docVersions[path],
		},
		"position": lspPosFor(t, pos),
		// triggerKind 2 = automatic (as-you-type) in the server's
		// dialect of the inline-completion protocol; 1 is an explicit
		// user invocation, which r-ed doesn't have a gesture for yet.
		"context":           map[string]any{"triggerKind": 2},
		"formattingOptions": map[string]any{"tabSize": tabSize, "insertSpaces": insertSpaces},
	}
	go func() {
		var res copilotInlineResult
		err := client.Call("textDocument/inlineCompletion", params, &res)
		_ = scr.PostEvent(&copilotCompletionEvent{
			when: time.Now(), seq: seq, path: path, rev: rev, pos: pos,
			items: res.Items, err: err,
		})
	}()
}

// handleCopilotCompletion lands a response: if it's still the latest
// request and the buffer hasn't moved, paint the first usable item as
// ghost text and tell the server it was shown. Errors and empty
// results end silently — no completion is a normal outcome, not a
// problem the user needs to hear about.
func (a *App) handleCopilotCompletion(e *copilotCompletionEvent) {
	if e.err != nil || e.seq != a.copilot.reqSeq || !a.copilotGhostActive() {
		return
	}
	t := a.activeTabPtr()
	if t == nil || t.Path != e.path || t.EditRev != e.rev || t.Cursor != e.pos {
		return
	}
	for _, raw := range e.items {
		var item copilotInlineItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		g, ok := copilotGhostFromItem(t, &item, e.pos)
		if !ok {
			continue
		}
		a.copilot.ghostPath = t.Path
		a.copilot.ghostRev = t.EditRev
		a.copilot.ghostPos = e.pos
		a.copilot.ghostItem = &item
		a.copilot.ghostRaw = raw
		t.Ghost = g
		// Shown-telemetry keeps the server's acceptance stats honest;
		// the raw item goes back verbatim so its correlation ids
		// survive fields this client doesn't model.
		_ = a.copilot.client.Notify("textDocument/didShowCompletion",
			map[string]any{"item": raw})
		return
	}
}

// copilotGhostFromItem converts a completion item into its display
// form at the cursor. The server's range usually starts at the line's
// beginning with InsertText repeating what's already typed, so the
// visible ghost is InsertText minus that prefix; an item whose text
// disagrees with the buffer (or adds nothing) is unusable.
func copilotGhostFromItem(t *editor.Tab, item *copilotInlineItem, cursor editor.Position) (*editor.GhostText, bool) {
	text := item.InsertText
	if text == "" {
		return nil, false
	}
	if item.Range != nil {
		start := editorPosFor(t, item.Range.Start)
		prefix := t.Buffer.Substring(start, cursor)
		if !strings.HasPrefix(text, prefix) {
			return nil, false
		}
		text = strings.TrimPrefix(text, prefix)
	}
	if text == "" {
		return nil, false
	}
	lines := strings.Split(text, "\n")
	return &editor.GhostText{Pos: cursor, Text: lines[0], MoreLines: len(lines) - 1}, true
}

// -----------------------------------------------------------------------------
// Accept
// -----------------------------------------------------------------------------

// copilotAcceptGhost lands the visible suggestion into the buffer.
// Returns true when it consumed the keystroke — the Tab key falls
// through to its normal indent when there's nothing to accept.
// Acceptance replaces the server-given range with the full InsertText
// (via the selection path, so it's one undo step), then reports the
// accept by executing the item's command — the protocol's acceptance
// signal, which also feeds the user's Copilot usage stats.
func (a *App) copilotAcceptGhost() bool {
	item := a.copilot.ghostItem
	if item == nil {
		return false
	}
	t := a.activeTabPtr()
	if t == nil || t.Ghost == nil || t.Path != a.copilot.ghostPath ||
		t.EditRev != a.copilot.ghostRev || t.Cursor != a.copilot.ghostPos {
		// Bookkeeping says ghost but the world disagrees — clean up and
		// let the key do its normal job.
		a.copilotClearGhost()
		return false
	}
	start, end := t.Cursor, t.Cursor
	if item.Range != nil {
		start = editorPosFor(t, item.Range.Start)
		end = editorPosFor(t, item.Range.End)
	}
	// Select the replacement range and insert over it: DeleteSelection +
	// InsertString coalesce into a single structural undo step, so one
	// Esc-u removes the whole accepted completion.
	t.MoveCursorTo(start, false)
	t.MoveCursorTo(end, true)
	t.InsertString(item.InsertText)
	if item.Command != nil && a.copilotReady() {
		client := a.copilot.client
		cmd := *item.Command
		go func() {
			_ = client.Call("workspace/executeCommand",
				map[string]any{"command": cmd.Command, "arguments": cmd.Arguments}, nil)
		}()
	}
	a.copilotClearGhost()
	return true
}

// -----------------------------------------------------------------------------
// Menu row
// -----------------------------------------------------------------------------

// suggestionsToggleLabel names the inline-suggestions row for its
// current direction, mirroring the other flip-in-place toggles.
func (a *App) suggestionsToggleLabel() string {
	if a.copilot.suggest {
		return "Disable inline suggestions"
	}
	return "Enable inline suggestions"
}

// menuToggleSuggestions flips ghost-text completions on or off and
// persists the choice. Turning off clears any visible ghost
// immediately — a suggestion lingering after "disable" reads as the
// toggle not working.
func (a *App) menuToggleSuggestions() {
	a.closeMenu()
	a.copilot.suggest = !a.copilot.suggest
	if a.copilot.suggest {
		if a.copilot.enabled {
			a.flash("Inline suggestions enabled")
		} else {
			// The toggle still took — but nothing will paint until the
			// sidecar itself is back on. Say so, or this looks broken.
			a.flash("Inline suggestions enabled — enable Copilot to see them")
		}
	} else {
		a.copilotStopCompletionTimer()
		a.copilotClearGhost()
		a.flash("Inline suggestions disabled")
	}
	if err := userconfig.SaveSuggestions(userconfig.DefaultPath(), a.copilot.suggest); err != nil {
		a.flash("suggestions: " + err.Error())
	}
}
