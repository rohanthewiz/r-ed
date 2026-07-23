// =============================================================================
// File: internal/app/copilot_ghost_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the ghost-text inline-completion layer (copilot_ghost.go).
// Same isolation contract as copilot_test.go: no real sidecar, all
// traffic through fakeCopilotConn; async legs asserted with the
// bounded-wait helper. Handlers are driven directly rather than through
// the debounce timer — the timer's only job is delay, and waiting out
// real milliseconds would just make the suite slow and flaky.

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
)

// newGhostTestApp builds a Copilot-connected app with ghost text fully
// armed (signed in + suggestions on) and one real file open, cursor
// parked at the end of its first-line content "func ma".
func newGhostTestApp(t *testing.T) (*App, *fakeCopilotConn, *editor.Tab) {
	t.Helper()
	fake := &fakeCopilotConn{}
	a := newCopilotTestApp(t, fake)
	a.copilot.signedIn = true
	a.copilot.suggest = true
	path := filepath.Join(a.rootDir, "main.go")
	if err := os.WriteFile(path, []byte("func ma\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a.openFile(path)
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != path {
		t.Fatalf("fixture tab not active")
	}
	tab.MoveCursorTo(editor.Position{Line: 0, Col: 7}, false)
	return a, fake, tab
}

// ghostItemJSON is a canonical inlineCompletion item for the fixture
// buffer: range covers the typed "func ma", InsertText completes it.
const ghostItemJSON = `{
	"insertText": "func main() {}",
	"range": {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 7}},
	"command": {"title": "accept", "command": "github.copilot.didAcceptCompletionItem", "arguments": []}
}`

// showFixtureGhost lands ghostItemJSON through the real response
// handler so tests exercise exactly the state a live response builds.
func showFixtureGhost(t *testing.T, a *App, tab *editor.Tab) {
	t.Helper()
	a.copilot.reqSeq = 1
	a.handleCopilotCompletion(&copilotCompletionEvent{
		seq: 1, path: tab.Path, rev: tab.EditRev, pos: tab.Cursor,
		items: []json.RawMessage{json.RawMessage(ghostItemJSON)},
	})
	if tab.Ghost == nil {
		t.Fatal("fixture ghost did not show")
	}
}

// TestCopilotLanguageID spot-checks the extension map: explicit
// mappings, the ext-is-the-id fallthrough, and the no-extension case.
func TestCopilotLanguageID(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go":  "go",
		"web/app.tsx":  "typescriptreact",
		"script.PY":    "python",
		"notes.md":     "markdown",
		"conf.yml":     "yaml",
		"run.sh":       "shellscript",
		"style.css":    "css", // fallthrough: id == extension
		"Makefile":     "plaintext",
		"query.sql":    "sql",
		"lib/util.rs":  "rust",
		"cmd/tool.zig": "zig",
	}
	for path, want := range cases {
		if got := copilotLanguageID(path); got != want {
			t.Errorf("copilotLanguageID(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestCopilotDocSync_OpenFlushClose walks a document's sync lifecycle:
// openFile announces didOpen (with languageId and full text), an edit +
// flush sends didChange at version 2, and closing sends didClose and
// drops the bookkeeping.
func TestCopilotDocSync_OpenFlushClose(t *testing.T) {
	a, fake, tab := newGhostTestApp(t)

	if !fake.notified("textDocument/didOpen") {
		t.Fatal("openFile did not announce didOpen")
	}
	open := fake.paramsFor("textDocument/didOpen")
	if !strings.Contains(string(open[0]), `"languageId":"go"`) ||
		!strings.Contains(string(open[0]), "func ma") {
		t.Errorf("didOpen payload missing languageId/text: %s", open[0])
	}

	// In-sync flush is a no-op — no phantom versions.
	a.copilotFlushDoc(tab)
	if fake.notified("textDocument/didChange") {
		t.Fatal("flush without edits sent a didChange")
	}

	tab.InsertRune('x')
	a.copilotFlushDoc(tab)
	chg := fake.paramsFor("textDocument/didChange")
	if len(chg) != 1 || !strings.Contains(string(chg[0]), `"version":2`) {
		t.Fatalf("didChange missing or wrong version: %v", chg)
	}

	a.closeTab(a.activeTab)
	if !fake.notified("textDocument/didClose") {
		t.Fatal("closeTab did not announce didClose")
	}
	if _, open := a.copilot.docVersions[tab.Path]; open {
		t.Fatal("close left doc bookkeeping behind")
	}
}

// TestCopilotAfterEvent_ArmsOnEditOnly pins the trigger economics: the
// debounce arms on a fresh edit, but never on plain cursor travel — a
// user reading a file must not generate model requests.
func TestCopilotAfterEvent_ArmsOnEditOnly(t *testing.T) {
	a, _, tab := newGhostTestApp(t)

	// Opening seeded armRev to the current rev — no request from open.
	a.copilotAfterEvent()
	if a.copilot.compTimer != nil {
		t.Fatal("afterEvent armed the timer with no edit")
	}

	tab.InsertRune('x')
	a.copilotAfterEvent()
	if a.copilot.compTimer == nil {
		t.Fatal("edit did not arm the completion debounce")
	}

	// Cursor-only movement must not re-arm.
	a.copilotStopCompletionTimer()
	tab.MoveCursor(0, -1, false)
	a.copilotAfterEvent()
	if a.copilot.compTimer != nil {
		t.Fatal("cursor travel armed the timer")
	}
}

// TestCopilotRequestCompletion_WireShape fires a request and checks the
// on-the-wire essentials: the doc was flushed first, and the request
// carries the versioned uri, the UTF-16 position, and the automatic
// trigger kind.
func TestCopilotRequestCompletion_WireShape(t *testing.T) {
	a, fake, tab := newGhostTestApp(t)
	tab.InsertRune('i') // "func mai", cursor col 8
	a.copilotRequestCompletion(tab)

	waitForCopilot(t, "inlineCompletion call", func() bool {
		return fake.called("textDocument/inlineCompletion")
	})
	if !fake.notified("textDocument/didChange") {
		t.Fatal("request fired without flushing the edited doc first")
	}
	p := string(fake.paramsFor("textDocument/inlineCompletion")[0])
	for _, want := range []string{`"version":2`, `"character":8`, `"triggerKind":2`} {
		if !strings.Contains(p, want) {
			t.Errorf("request params missing %s: %s", want, p)
		}
	}
}

// TestHandleCopilotCompletion_ShowsGhost lands a fresh response and
// verifies the visible half (display text minus the typed prefix) and
// the invisible half (accept bookkeeping + shown-telemetry with the raw
// item echoed back).
func TestHandleCopilotCompletion_ShowsGhost(t *testing.T) {
	a, fake, tab := newGhostTestApp(t)
	showFixtureGhost(t, a, tab)

	if tab.Ghost.Text != "in() {}" || tab.Ghost.MoreLines != 0 {
		t.Errorf("ghost = %q (+%d), want %q (+0)", tab.Ghost.Text, tab.Ghost.MoreLines, "in() {}")
	}
	if a.copilot.ghostItem == nil || a.copilot.ghostPath != tab.Path {
		t.Error("accept bookkeeping not recorded")
	}
	if !fake.notified("textDocument/didShowCompletion") {
		t.Error("shown-telemetry not sent")
	}
	shown := string(fake.paramsFor("textDocument/didShowCompletion")[0])
	if !strings.Contains(shown, "didAcceptCompletionItem") {
		t.Errorf("telemetry did not echo the raw item: %s", shown)
	}
}

// TestHandleCopilotCompletion_StaleDropped pins the three staleness
// gates: superseded sequence, buffer edited since the request, and a
// cursor that moved. Each must leave no ghost behind.
func TestHandleCopilotCompletion_StaleDropped(t *testing.T) {
	a, _, tab := newGhostTestApp(t)
	items := []json.RawMessage{json.RawMessage(ghostItemJSON)}

	a.copilot.reqSeq = 2 // response for seq 1 is already superseded
	a.handleCopilotCompletion(&copilotCompletionEvent{
		seq: 1, path: tab.Path, rev: tab.EditRev, pos: tab.Cursor, items: items,
	})
	if tab.Ghost != nil {
		t.Fatal("superseded response painted a ghost")
	}

	a.handleCopilotCompletion(&copilotCompletionEvent{
		seq: 2, path: tab.Path, rev: tab.EditRev - 1, pos: tab.Cursor, items: items,
	})
	if tab.Ghost != nil {
		t.Fatal("response for an older EditRev painted a ghost")
	}

	a.handleCopilotCompletion(&copilotCompletionEvent{
		seq: 2, path: tab.Path, rev: tab.EditRev,
		pos: editor.Position{Line: 0, Col: 3}, items: items,
	})
	if tab.Ghost != nil {
		t.Fatal("response for a moved cursor painted a ghost")
	}
}

// TestCopilotGhostFromItem covers the display-form conversion: the
// typed prefix is trimmed, a buffer/item disagreement is unusable, an
// item adding nothing beyond the prefix is unusable, and a multi-line
// proposal reports its hidden tail.
func TestCopilotGhostFromItem(t *testing.T) {
	_, _, tab := newGhostTestApp(t)
	cursor := tab.Cursor
	rng := `"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":7}}`

	var item copilotInlineItem
	mustItem := func(body string) *copilotInlineItem {
		item = copilotInlineItem{}
		if err := json.Unmarshal([]byte(body), &item); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		return &item
	}

	g, ok := copilotGhostFromItem(tab, mustItem(`{"insertText":"func main() {\n\treturn\n}",`+rng+`}`), cursor)
	if !ok || g.Text != "in() {" || g.MoreLines != 2 {
		t.Errorf("multi-line ghost = %+v ok=%v, want Text %q MoreLines 2", g, ok, "in() {")
	}

	if _, ok := copilotGhostFromItem(tab, mustItem(`{"insertText":"while ma",`+rng+`}`), cursor); ok {
		t.Error("item disagreeing with the typed prefix should be unusable")
	}
	if _, ok := copilotGhostFromItem(tab, mustItem(`{"insertText":"func ma",`+rng+`}`), cursor); ok {
		t.Error("item adding nothing beyond the prefix should be unusable")
	}
}

// TestCopilotAcceptGhost_ViaTabKey drives the accept through the real
// key path: Tab lands the full InsertText over the server's range (one
// undo step), clears the ghost, and reports acceptance by executing the
// item's command.
func TestCopilotAcceptGhost_ViaTabKey(t *testing.T) {
	a, fake, tab := newGhostTestApp(t)
	showFixtureGhost(t, a, tab)

	a.handleKey(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))

	if got := tab.Buffer.Lines[0]; got != "func main() {}" {
		t.Errorf("line after accept = %q, want %q", got, "func main() {}")
	}
	if tab.Ghost != nil || a.copilot.ghostItem != nil {
		t.Error("accept left ghost state behind")
	}
	waitForCopilot(t, "acceptance command", func() bool {
		return fake.called("workspace/executeCommand")
	})
	// One undo step recovers the pre-accept text.
	tab.Undo()
	if got := tab.Buffer.Lines[0]; got != "func ma" {
		t.Errorf("one undo after accept = %q, want %q", got, "func ma")
	}
}

// TestTabKey_NoGhostIndents guards the fallthrough: with nothing to
// accept, Tab must stay plain indentation.
func TestTabKey_NoGhostIndents(t *testing.T) {
	a, _, tab := newGhostTestApp(t)
	tab.MoveCursorTo(editor.Position{Line: 0, Col: 0}, false)

	a.handleKey(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	if got := tab.Buffer.Lines[0]; got != tab.IndentUnit+"func ma" {
		t.Errorf("line after Tab = %q, want indent inserted", got)
	}
}

// TestCopilotAfterEvent_CursorMoveClearsGhost pins invalidation: the
// caret walking away from the anchor drops both the painted ghost and
// the accept bookkeeping on the next dispatch tail.
func TestCopilotAfterEvent_CursorMoveClearsGhost(t *testing.T) {
	a, _, tab := newGhostTestApp(t)
	showFixtureGhost(t, a, tab)

	tab.MoveCursor(0, -1, false)
	a.copilotAfterEvent()
	if tab.Ghost != nil || a.copilot.ghostItem != nil {
		t.Fatal("cursor move did not clear the ghost")
	}
}

// TestMenuToggleSuggestions verifies the ≡ toggle: flipping off clears
// any visible ghost immediately and persists "suggestions": "off";
// flipping back on persists "on".
func TestMenuToggleSuggestions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, _, tab := newGhostTestApp(t)
	showFixtureGhost(t, a, tab)

	a.menuToggleSuggestions()
	if a.copilot.suggest || tab.Ghost != nil {
		t.Fatal("toggle off did not disable suggestions and clear the ghost")
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "r-ed", "config.json"))
	if err != nil || !strings.Contains(string(data), `"suggestions": "off"`) {
		t.Fatalf("preference not persisted: %v %s", err, data)
	}

	a.menuToggleSuggestions()
	if !a.copilot.suggest {
		t.Fatal("toggle back on failed")
	}
}

// TestCopilotDisconnect_ClearsGhostMachinery pins teardown: disconnect
// (disable toggle, exit, crash path) drops the ghost, the debounce, and
// every per-document map — a ghost with no server could never report
// acceptance.
func TestCopilotDisconnect_ClearsGhostMachinery(t *testing.T) {
	a, _, tab := newGhostTestApp(t)
	showFixtureGhost(t, a, tab)
	a.copilotArmCompletionTimer(tab.Path)

	a.copilotDisconnect()
	if tab.Ghost != nil || a.copilot.ghostItem != nil {
		t.Error("disconnect left a ghost behind")
	}
	if a.copilot.compTimer != nil {
		t.Error("disconnect left the debounce timer running")
	}
	if a.copilot.docVersions != nil || a.copilot.armRev != nil {
		t.Error("disconnect left doc bookkeeping behind")
	}
}
