// =============================================================================
// File: internal/app/copilot_chat_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the Copilot chat panel (copilot_chat.go). A real
// copilot-language-server --acp is never spawned — newTestApp marks the
// integration dead, and these tests inject fakeCopilotConn (the chat
// layer deliberately shares the sidecar's conn interface). Prompt turns
// that hop through a goroutine are asserted with the same bounded wait
// the sidecar tests use.

package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// wireChat installs a live fake chat connection, bypassing the async
// start — the injection twin of the sidecar tests' handleCopilotReady
// path.
func wireChat(a *App) *fakeCopilotConn {
	fake := &fakeCopilotConn{}
	a.copilot.enabled = true
	a.chat.dead = false
	a.chat.client = fake
	a.chat.sessionID = "sess-1"
	return fake
}

// TestChatToggleLabel pins the flip-in-place menu label.
func TestChatToggleLabel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if got := a.chatToggleLabel(); got != "Show Copilot chat" {
		t.Errorf("closed label = %q", got)
	}
	a.chat.open = true
	if got := a.chatToggleLabel(); got != "Hide Copilot chat" {
		t.Errorf("open label = %q", got)
	}
}

// TestMenuToggleChat_OpensAndExplains drives the three open outcomes:
// Copilot disabled flashes the dependency, a dead agent flashes the
// install hint, and a healthy state opens focused — never a silently
// dimmed dead end (the menuCopilotAuth rule).
func TestMenuToggleChat_OpensAndExplains(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	a.copilot.enabled = false
	a.menuToggleChat()
	if a.chat.open || !strings.Contains(a.statusMsg, "Copilot is disabled") {
		t.Fatalf("disabled: open=%v flash=%q", a.chat.open, a.statusMsg)
	}

	a.copilot.enabled = true
	a.chat.dead = true // newTestApp default, restated for clarity
	a.menuToggleChat()
	if a.chat.open || !strings.Contains(a.statusMsg, "unavailable") {
		t.Fatalf("dead: open=%v flash=%q", a.chat.open, a.statusMsg)
	}

	wireChat(a)
	a.menuToggleChat()
	if !a.chat.open || !a.chat.focused {
		t.Fatalf("healthy: open=%v focused=%v", a.chat.open, a.chat.focused)
	}
	// Toggling again hides without tearing the session down.
	a.menuToggleChat()
	if a.chat.open || a.chat.client == nil {
		t.Fatalf("hide: open=%v client=%v", a.chat.open, a.chat.client)
	}
}

// TestChatLeftEdgeSingleOccupancy pins the eviction in both directions:
// opening chat closes a LEFT-docked terminal (bottom-docked coexists),
// and re-opening the left-docked terminal evicts the chat.
func TestChatLeftEdgeSingleOccupancy(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)

	// Chat evicts the left-docked terminal…
	a.termDockLeft = true
	a.term.open = true
	a.menuToggleChat()
	if !a.chat.open || a.term.open {
		t.Fatalf("chat open should evict left terminal: chat=%v term=%v", a.chat.open, a.term.open)
	}

	// …and the terminal takes the edge back.
	a.menuToggleTerminal()
	if !a.term.open || a.chat.open {
		t.Fatalf("left terminal should evict chat: chat=%v term=%v", a.chat.open, a.term.open)
	}

	// A bottom-docked terminal coexists with the chat strip.
	a.term.open = false
	a.termDockLeft = false
	a.menuToggleChat()
	a.menuToggleTerminal()
	if !a.chat.open || !a.term.open {
		t.Fatalf("bottom terminal should coexist: chat=%v term=%v", a.chat.open, a.term.open)
	}
}

// TestChatDockFlipEvictsChat pins the other reclaim path: flipping the
// terminal dock to the left (which force-opens the terminal there)
// takes the edge from an open chat panel.
func TestChatDockFlipEvictsChat(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	a.menuToggleChat()
	if !a.chat.open {
		t.Fatal("setup: chat should be open")
	}
	a.menuToggleTermDock() // bottom → left, opens the terminal there
	if !a.termDockLeft || !a.term.open || a.chat.open {
		t.Fatalf("dock flip: dockLeft=%v term=%v chat=%v", a.termDockLeft, a.term.open, a.chat.open)
	}
}

// TestChatLayoutFlipsTreeRight pins the geometry contract: an open chat
// strip owns the left block, the sidebar flips to the right edge, and
// the strip's splitter sits on its rightmost column.
func TestChatLayoutFlipsTreeRight(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	if a.treeOnRight() {
		t.Fatal("classic layout should keep the tree left")
	}
	a.menuToggleChat()

	if !a.treeOnRight() {
		t.Fatal("open chat should flip the tree right")
	}
	if got := a.leftBlockW(); got != a.chatStripW() {
		t.Errorf("leftBlockW = %d, want chat strip %d", got, a.chatStripW())
	}
	if got := a.rightBlockW(); got != a.sidebarW() {
		t.Errorf("rightBlockW = %d, want sidebar %d", got, a.sidebarW())
	}
	if got := a.chatSplitterX(); got != a.chatStripW()-1 {
		t.Errorf("chatSplitterX = %d, want %d", got, a.chatStripW()-1)
	}
	sx, _, _, _ := a.sidebarRect()
	if sx != a.width-a.sidebarW()+1 {
		t.Errorf("sidebar x = %d, want right-docked %d", sx, a.width-a.sidebarW()+1)
	}
}

// TestResizeChatPanelWidth_Clamps pins the resize band: the strip can't
// shrink below the minimum or starve the editor next to the sidebar.
func TestResizeChatPanelWidth_Clamps(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	a.chat.open = true

	a.resizeChatPanelWidth(1)
	if a.chat.width != chatPanelMinWidth {
		t.Errorf("tiny target: width = %d, want %d", a.chat.width, chatPanelMinWidth)
	}
	a.resizeChatPanelWidth(9999)
	if a.chat.width != a.maxChatPanelWidth() {
		t.Errorf("huge target: width = %d, want %d", a.chat.width, a.maxChatPanelWidth())
	}
}

// TestWrapChatText pins the word-wrapper: greedy wrapping at width,
// paragraph blanks preserved, and over-long words hard-broken instead
// of overflowing the strip.
func TestWrapChatText(t *testing.T) {
	got := wrapChatText("alpha beta gamma", 11)
	want := []string{"alpha beta", "gamma"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	if got := wrapChatText("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("empty line = %q, want one blank row", got)
	}
	got = wrapChatText("supercalifragilistic", 6)
	if len(got) != 4 || got[0] != "superc" {
		t.Errorf("long word = %q, want 6-cell chunks", got)
	}
}

// TestChatRows_RolesAndFences pins the transcript row derivation: the
// user prompt gets its ❯ gutter, messages are separated by a blank
// row, and fenced blocks in agent prose come back flagged as code with
// their interior hard-wrapped, not word-wrapped.
func TestChatRows_RolesAndFences(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.chat.msgs = []chatMsg{
		{role: chatRoleUser, text: "hi"},
		{role: chatRoleAgent, text: "Sure:\n```go\nfmt.Println(1)\n```"},
	}
	rows := a.chatRows(40)

	if rows[0].text != "❯ hi" || rows[0].role != chatRoleUser {
		t.Errorf("row 0 = %+v, want the gutter-prefixed prompt", rows[0])
	}
	if rows[1].text != "" {
		t.Errorf("row 1 = %+v, want the blank separator", rows[1])
	}
	var codeRows []string
	for _, r := range rows {
		if r.code {
			codeRows = append(codeRows, r.text)
		}
	}
	want := []string{"```go", "fmt.Println(1)", "```"}
	if len(codeRows) != len(want) {
		t.Fatalf("code rows = %q, want %q", codeRows, want)
	}
	for i := range want {
		if codeRows[i] != want[i] {
			t.Errorf("code row %d = %q, want %q", i, codeRows[i], want[i])
		}
	}
}

// TestChatSend_WireShape drives Enter through the real key path and
// asserts the session/prompt payload: the session id and the typed
// text as one ACP text block.
func TestChatSend_WireShape(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	fake := wireChat(a)
	a.chat.open = true
	a.chat.focused = true
	typeChatText(a, "hello agent")
	a.handleChatKey(enterKey())

	waitForCopilot(t, "session/prompt call", func() bool { return fake.called("session/prompt") })
	if !a.chat.turnActive {
		t.Error("turnActive should be set while the prompt is in flight")
	}
	params := fake.paramsFor("session/prompt")
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params[len(params)-1], &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.SessionID != "sess-1" || len(p.Prompt) != 1 || p.Prompt[0].Type != "text" || p.Prompt[0].Text != "hello agent" {
		t.Errorf("prompt params = %+v", p)
	}
	// The prompt is echoed into the transcript and the input cleared.
	if len(a.chat.msgs) == 0 || a.chat.msgs[0].text != "hello agent" || a.chat.msgs[0].role != chatRoleUser {
		t.Errorf("transcript echo missing: %+v", a.chat.msgs)
	}
	if a.chat.input.String() != "" {
		t.Errorf("input not cleared: %q", a.chat.input.String())
	}
}

// TestChatSend_QueuesWhileStarting pins the first-Enter race: a prompt
// submitted mid-handshake is queued, and handleChatReady sends it the
// moment the session exists — never silently dropped.
func TestChatSend_QueuesWhileStarting(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.chat.dead = false
	a.chat.starting = true
	a.chat.open = true
	typeChatText(a, "early bird")
	a.handleChatKey(enterKey())

	if a.chat.queuedPrompt != "early bird" {
		t.Fatalf("queuedPrompt = %q", a.chat.queuedPrompt)
	}
	fake := &fakeCopilotConn{}
	a.handleChatReady(&chatReadyEvent{when: time.Now(), client: fake, sessionID: "sess-2"})
	waitForCopilot(t, "queued prompt sent", func() bool { return fake.called("session/prompt") })
	if a.chat.queuedPrompt != "" {
		t.Errorf("queue not drained: %q", a.chat.queuedPrompt)
	}
}

// TestChatUpdate_StreamsChunks pins the streaming merge: consecutive
// agent_message_chunks extend ONE transcript message, a tool_call adds
// its muted one-liner, and updates for a stale session are dropped.
func TestChatUpdate_StreamsChunks(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)

	update := func(sid, body string) *chatUpdateEvent {
		return &chatUpdateEvent{when: time.Now(), sessionID: sid, update: json.RawMessage(body)}
	}
	a.handleChatUpdate(update("sess-1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hel"}}`))
	a.handleChatUpdate(update("sess-1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}`))
	if len(a.chat.msgs) != 1 || a.chat.msgs[0].text != "Hello" || a.chat.msgs[0].role != chatRoleAgent {
		t.Fatalf("chunk merge: %+v", a.chat.msgs)
	}

	a.handleChatUpdate(update("sess-1", `{"sessionUpdate":"tool_call","title":"Search the web"}`))
	if len(a.chat.msgs) != 2 || a.chat.msgs[1].text != "⚙ Search the web" || a.chat.msgs[1].role != chatRoleTool {
		t.Fatalf("tool line: %+v", a.chat.msgs)
	}

	a.handleChatUpdate(update("old-session", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ghost"}}`))
	if len(a.chat.msgs) != 2 {
		t.Fatalf("stale-session update should be dropped: %+v", a.chat.msgs)
	}
}

// TestChatTurnDone pins the turn endings: an error surfaces as an info
// line, a cancel gets its marker, and a clean end adds nothing — the
// streamed answer is the feedback.
func TestChatTurnDone(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)

	a.chat.turnActive = true
	a.handleChatTurnDone(&chatTurnDoneEvent{when: time.Now(), stopReason: "end_turn"})
	if a.chat.turnActive || len(a.chat.msgs) != 0 {
		t.Fatalf("clean end: active=%v msgs=%+v", a.chat.turnActive, a.chat.msgs)
	}

	a.chat.turnActive = true
	a.handleChatTurnDone(&chatTurnDoneEvent{when: time.Now(), stopReason: "cancelled"})
	if len(a.chat.msgs) != 1 || a.chat.msgs[0].text != "— stopped" {
		t.Fatalf("cancel marker: %+v", a.chat.msgs)
	}

	a.chat.turnActive = true
	a.handleChatTurnDone(&chatTurnDoneEvent{when: time.Now(), err: errors.New("boom")})
	if len(a.chat.msgs) != 2 || !strings.Contains(a.chat.msgs[1].text, "boom") {
		t.Fatalf("error line: %+v", a.chat.msgs)
	}
}

// TestChatInterrupt pins the ⏹ contract: one session/cancel per turn,
// carrying the session id, and a no-op while idle.
func TestChatInterrupt(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	fake := wireChat(a)

	a.chatInterrupt() // idle — nothing to cancel
	if fake.notified("session/cancel") {
		t.Fatal("idle interrupt should not notify")
	}

	a.chat.turnActive = true
	a.chatInterrupt()
	a.chatInterrupt() // second press within the same turn is dropped
	if got := len(fake.paramsFor("session/cancel")); got != 1 {
		t.Fatalf("cancel notifications = %d, want 1", got)
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(fake.paramsFor("session/cancel")[0], &p)
	if p.SessionID != "sess-1" {
		t.Errorf("cancel sessionId = %q", p.SessionID)
	}
}

// TestChatExit_SurfacesReason pins the failure transparency rule: a
// handshake error lands in the transcript with the sign-in hint when
// the user isn't signed in, and the integration is dead afterwards.
func TestChatExit_SurfacesReason(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	a.copilot.signedIn = false

	a.handleChatExit(&chatExitEvent{when: time.Now(), err: errors.New("auth required")})
	if !a.chat.dead || a.chat.client != nil {
		t.Fatalf("exit should kill the integration: dead=%v client=%v", a.chat.dead, a.chat.client)
	}
	if len(a.chat.msgs) != 2 ||
		!strings.Contains(a.chat.msgs[0].text, "auth required") ||
		!strings.Contains(a.chat.msgs[1].text, "Sign in") {
		t.Fatalf("transcript = %+v, want error + sign-in hint", a.chat.msgs)
	}
}

// TestChatAutoRejectPermission pins the phase-3 scope guard: the
// agent's own reject_once option is chosen when present, and the
// cancelled outcome is the fallback when it isn't.
func TestChatAutoRejectPermission(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {"title": "Edit main.go"},
		"options": [
			{"optionId": "ok", "kind": "allow_once"},
			{"optionId": "no", "kind": "reject_once"}
		]}`)
	res, title := chatAutoRejectPermission(params)
	if title != "Edit main.go" {
		t.Errorf("title = %q", title)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"optionId":"no"`) {
		t.Errorf("outcome = %s, want the reject_once option", b)
	}

	res, _ = chatAutoRejectPermission(json.RawMessage(`{"options":[{"optionId":"ok","kind":"allow_once"}]}`))
	b, _ = json.Marshal(res)
	if !strings.Contains(string(b), `"cancelled"`) {
		t.Errorf("no-reject fallback = %s, want cancelled", b)
	}
}

// TestChatHistoryMove pins the readline behavior on the composer: Up
// recalls the previous prompt, Down walks back to the stashed draft.
func TestChatHistoryMove(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.chat.history = []string{"first", "second"}
	a.chat.histIdx = 2
	typeChatText(a, "draft")

	a.chatHistoryMove(-1)
	if a.chat.input.String() != "second" {
		t.Errorf("up = %q, want second", a.chat.input.String())
	}
	a.chatHistoryMove(-1)
	if a.chat.input.String() != "first" {
		t.Errorf("up up = %q, want first", a.chat.input.String())
	}
	a.chatHistoryMove(1)
	a.chatHistoryMove(1)
	if a.chat.input.String() != "draft" {
		t.Errorf("back down = %q, want the stashed draft", a.chat.input.String())
	}
}

// TestChatPanelPress pins the mouse contract: ✕ closes, a body click
// focuses the composer and steals focus from the terminal.
func TestChatPanelPress(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	a.chat.open = true
	a.term.focused = true

	px, py, pw, ph := a.chatPanelRect()
	a.chatPanelPress(px+1, py+ph/2)
	if !a.chat.focused || a.term.focused {
		t.Fatalf("body click: chat=%v term=%v", a.chat.focused, a.term.focused)
	}

	c := a.chatCloseRect()
	a.chatPanelPress(c.x+1, c.y)
	if a.chat.open {
		t.Fatal("✕ should close the panel")
	}
	_ = pw
}

// TestMenuToggleCopilot_TearsDownChat pins the shared kill switch:
// disabling Copilot closes the chat connection AND the panel, and
// re-enabling clears the chat's dead verdict for a fresh start.
func TestMenuToggleCopilot_TearsDownChat(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	fake := wireChat(a)
	a.chat.open = true
	a.copilot.dead = true // keep copilotEnsureStarted from spawning anything

	a.menuToggleCopilot() // on → off
	if a.chat.open || a.chat.client != nil || !fake.closed {
		t.Fatalf("disable: open=%v client=%v closed=%v", a.chat.open, a.chat.client, fake.closed)
	}

	a.chat.dead = true
	a.menuToggleCopilot() // off → on
	if a.chat.dead {
		t.Error("re-enable should clear the chat dead verdict")
	}
	// The eager start marked copilot dead again (no binary lookup in
	// tests matters here); the point is chat.dead cleared before it.
	a.copilot.enabled = false
}

// TestDrawChatPanel_Smoke renders the open panel on the simulation
// screen and checks the header title landed on row 0 — the whole
// paint path (header, transcript wrap, input row, splitter) at once.
func TestDrawChatPanel_Smoke(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	wireChat(a)
	a.menuToggleChat()
	a.chat.msgs = []chatMsg{{role: chatRoleAgent, text: "Hello from Copilot"}}
	a.draw()
	scr := a.screen.(tcell.SimulationScreen)
	scr.Show()

	cells, w, _ := scr.GetContents()
	var header strings.Builder
	for x := 0; x < a.chatStripW(); x++ {
		c := cells[0*w+x]
		if len(c.Runes) > 0 {
			header.WriteRune(c.Runes[0])
		}
	}
	if !strings.Contains(header.String(), "Copilot chat") {
		t.Errorf("header row = %q, want the panel title", header.String())
	}
}

// typeChatText feeds runes through the composer's real key handler.
func typeChatText(a *App, s string) {
	for _, r := range s {
		a.chat.input.handleKey(tcell.NewEventKey(tcell.KeyRune, r, 0))
	}
}

// enterKey builds the Enter keystroke used by the send tests.
func enterKey() *tcell.EventKey {
	return tcell.NewEventKey(tcell.KeyEnter, 0, 0)
}
