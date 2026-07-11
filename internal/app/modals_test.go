// =============================================================================
// File: internal/app/modals_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Tests for the secondary-modal logic in modals.go: prompt, confirm, and
// the right-click context menu. We don't render anything; we drive the
// modals directly and assert on the App fields they touch. Mutual
// exclusivity (closeAllModals) is the linchpin so most tests double-check
// no other modal flag is left on.

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/filetree"
)

// TestCloseAllModals_ClearsEverything proves the helper dismisses every
// overlay (menu, modal slot, find bar) and clears the side-state (drag,
// auto-scroll dir, menu hover). Per-modal state needs no clearing any
// more — it lives on the struct and dies with the slot.
func TestCloseAllModals_ClearsEverything(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.menuOpen = true
	a.modal = &contextModal{node: a.tree.Root, items: []contextItem{{label: "x"}}}
	a.hoveredMenuRow = 3
	a.findOpen = true
	a.findValue = []rune("q")
	a.dragMode = "editor"
	a.autoScrollDir = 1

	a.closeAllModals()

	if a.menuOpen || a.modal != nil {
		t.Fatal("expected menu and modal slot cleared")
	}
	if a.findOpen || a.findValue != nil {
		t.Fatal("find bar state not cleared")
	}
	if a.hoveredMenuRow != -1 {
		t.Fatalf("hoveredMenuRow not cleared: %d", a.hoveredMenuRow)
	}
	if a.dragMode != "" {
		t.Fatalf("dragMode not cleared: %q", a.dragMode)
	}
	if a.autoScrollDir != 0 {
		t.Fatalf("autoScrollDir not reset: %d", a.autoScrollDir)
	}
}

// TestAnyModalOpen returns true for any one flag and false for none.
func TestAnyModalOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if a.anyModalOpen() {
		t.Fatal("none open")
	}
	a.menuOpen = true
	if !a.anyModalOpen() {
		t.Fatal("expected true with menu open")
	}
	a.menuOpen = false
	a.modal = &promptModal{}
	if !a.anyModalOpen() {
		t.Fatal("expected true with prompt open")
	}
	a.modal = nil
	a.modal = &confirmModal{}
	if !a.anyModalOpen() {
		t.Fatal("expected true with confirm open")
	}
	a.modal = nil
	a.modal = &contextModal{}
	if !a.anyModalOpen() {
		t.Fatal("expected true with context open")
	}
}

// TestOpenPrompt_Submit runs the callback with a trimmed value and closes
// the modal.
func TestOpenPrompt_Submit(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	got := ""
	a.openPrompt("Title", "Hint", "  hello  ", func(_ *App, v string) { got = v })
	if promptOf(a) == nil {
		t.Fatal("openPrompt should set promptOpen")
	}
	if promptOf(a).field.cursor != len([]rune("  hello  ")) {
		t.Fatalf("cursor at end of initial: got %d", promptOf(a).field.cursor)
	}
	promptOf(a).submit(a)
	if got != "hello" {
		t.Fatalf("callback got %q, want trimmed 'hello'", got)
	}
	if promptOf(a) != nil {
		t.Fatal("promptSubmit should close the modal")
	}
}

// TestPromptSubmit_EmptyIsNoop keeps the modal open and skips the callback.
func TestPromptSubmit_EmptyIsNoop(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openPrompt("T", "H", "   ", func(*App, string) { called = true })
	promptOf(a).submit(a)
	if called {
		t.Fatal("empty submit should not run callback")
	}
	if promptOf(a) == nil {
		t.Fatal("empty submit should keep modal open")
	}
}

// TestPromptCancel skips the callback.
func TestPromptCancel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openPrompt("T", "H", "x", func(*App, string) { called = true })
	a.closeModal()
	if called {
		t.Fatal("cancel should not run callback")
	}
	if promptOf(a) != nil {
		t.Fatal("cancel should close")
	}
}

// TestKeysAfterCloseDontReachModal guards against double-submits from
// stale events: once the modal is closed, further keystrokes route to
// the editor, never to the departed modal — the structural replacement
// for the old "noop when closed" guards inside each handler.
func TestKeysAfterCloseDontReachModal(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := 0
	a.openPrompt("T", "H", "x", func(*App, string) { called++ })
	promptOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if called != 1 {
		t.Fatalf("submit should fire once, got %d", called)
	}
	// The modal is gone; another Enter must not re-fire the callback.
	a.handleKey(keyEv(tcell.KeyEnter, 0))
	if called != 1 {
		t.Fatalf("stale Enter re-fired the callback: %d", called)
	}
}

// keyEv builds a synthetic tcell key event for tests.
func keyEv(k tcell.Key, r rune) *tcell.EventKey {
	return tcell.NewEventKey(k, r, tcell.ModNone)
}

// TestHandlePromptKey_Editing exercises every editing branch of the prompt
// keyboard handler.
func TestHandlePromptKey_Editing(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPrompt("T", "H", "", nil)

	// Insert "abc".
	for _, r := range "abc" {
		promptOf(a).handleKey(a, keyEv(tcell.KeyRune, r))
	}
	if string(promptOf(a).field.value) != "abc" || promptOf(a).field.cursor != 3 {
		t.Fatalf("after insert: %q cur=%d", string(promptOf(a).field.value), promptOf(a).field.cursor)
	}

	// Control-rune is ignored.
	promptOf(a).handleKey(a, keyEv(tcell.KeyRune, 0x01))
	if string(promptOf(a).field.value) != "abc" {
		t.Fatalf("control rune should be ignored: %q", string(promptOf(a).field.value))
	}

	// Home / End.
	promptOf(a).handleKey(a, keyEv(tcell.KeyHome, 0))
	if promptOf(a).field.cursor != 0 {
		t.Fatalf("Home: got %d", promptOf(a).field.cursor)
	}
	promptOf(a).handleKey(a, keyEv(tcell.KeyEnd, 0))
	if promptOf(a).field.cursor != 3 {
		t.Fatalf("End: got %d", promptOf(a).field.cursor)
	}

	// Left / Right.
	promptOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	if promptOf(a).field.cursor != 2 {
		t.Fatalf("Left: got %d", promptOf(a).field.cursor)
	}
	promptOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if promptOf(a).field.cursor != 3 {
		t.Fatalf("Right: got %d", promptOf(a).field.cursor)
	}
	// Right past end is clamped.
	promptOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if promptOf(a).field.cursor != 3 {
		t.Fatalf("Right past end: got %d", promptOf(a).field.cursor)
	}
	// Left past start is clamped.
	promptOf(a).field.cursor = 0
	promptOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	if promptOf(a).field.cursor != 0 {
		t.Fatalf("Left past start: got %d", promptOf(a).field.cursor)
	}

	// Backspace: cursor between 'a' and 'b' → removes 'a'.
	promptOf(a).field.cursor = 1
	promptOf(a).handleKey(a, keyEv(tcell.KeyBackspace, 0))
	if string(promptOf(a).field.value) != "bc" || promptOf(a).field.cursor != 0 {
		t.Fatalf("Backspace: %q cur=%d", string(promptOf(a).field.value), promptOf(a).field.cursor)
	}
	// Backspace at column 0 is a no-op.
	promptOf(a).handleKey(a, keyEv(tcell.KeyBackspace2, 0))
	if string(promptOf(a).field.value) != "bc" {
		t.Fatalf("Backspace at 0 should be no-op: %q", string(promptOf(a).field.value))
	}

	// Delete forward: removes 'b' at cursor=0.
	promptOf(a).handleKey(a, keyEv(tcell.KeyDelete, 0))
	if string(promptOf(a).field.value) != "c" {
		t.Fatalf("Delete: %q", string(promptOf(a).field.value))
	}
	// Delete at end is no-op.
	promptOf(a).field.cursor = 1
	promptOf(a).handleKey(a, keyEv(tcell.KeyDelete, 0))
	if string(promptOf(a).field.value) != "c" {
		t.Fatalf("Delete at end should be no-op: %q", string(promptOf(a).field.value))
	}
}

// TestHandlePromptKey_Esc cancels the modal.
func TestHandlePromptKey_Esc(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openPrompt("T", "H", "x", func(*App, string) { called = true })
	promptOf(a).handleKey(a, keyEv(tcell.KeyEsc, 0))
	if promptOf(a) != nil {
		t.Fatal("Esc should close")
	}
	if called {
		t.Fatal("Esc should not run callback")
	}
}

// TestHandlePromptKey_Enter submits.
func TestHandlePromptKey_Enter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	got := ""
	a.openPrompt("T", "H", "ok", func(_ *App, v string) { got = v })
	promptOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if got != "ok" {
		t.Fatalf("Enter callback: got %q", got)
	}
}

// TestOpenConfirm_DefaultsToNo lands on the safe button so accidental
// Enter never does anything destructive.
func TestOpenConfirm_DefaultsToNo(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openConfirm("Delete", "Sure?", func(*App) {})
	if confirmOf(a) == nil {
		t.Fatal("confirm should open")
	}
	if confirmOf(a).hover != 0 {
		t.Fatalf("default focus should be No (0); got %d", confirmOf(a).hover)
	}
}

// TestConfirmYes runs the callback.
func TestConfirmYes(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openConfirm("T", "M", func(*App) { called = true })
	confirmOf(a).yes(a)
	if !called {
		t.Fatal("confirmYes should run callback")
	}
	if confirmOf(a) != nil {
		t.Fatal("confirmYes should close")
	}
}

// TestConfirmCancel skips the callback.
func TestConfirmCancel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openConfirm("T", "M", func(*App) { called = true })
	confirmOf(a).cancel(a)
	if called {
		t.Fatal("cancel should skip callback")
	}
}

// TestHandleConfirmKey_AllBranches walks Tab, Left, Right, Enter, Esc.
func TestHandleConfirmKey_AllBranches(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openConfirm("T", "M", func(*App) { called = true })

	// Tab cycles 0 ↔ 1.
	confirmOf(a).handleKey(a, keyEv(tcell.KeyTab, 0))
	if confirmOf(a).hover != 1 {
		t.Fatalf("Tab: got %d, want 1", confirmOf(a).hover)
	}
	confirmOf(a).handleKey(a, keyEv(tcell.KeyTab, 0))
	if confirmOf(a).hover != 0 {
		t.Fatalf("Tab back: got %d", confirmOf(a).hover)
	}

	// Left snaps to No (0).
	confirmOf(a).hover = 1
	confirmOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	if confirmOf(a).hover != 0 {
		t.Fatalf("Left: got %d", confirmOf(a).hover)
	}

	// Right snaps to Yes (1).
	confirmOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if confirmOf(a).hover != 1 {
		t.Fatalf("Right: got %d", confirmOf(a).hover)
	}

	// Enter on Yes runs the callback.
	confirmOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if !called {
		t.Fatal("Enter on Yes should fire callback")
	}

	// Re-open and verify Enter on No cancels.
	called = false
	a.openConfirm("T", "M", func(*App) { called = true })
	confirmOf(a).hover = 0
	confirmOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if called {
		t.Fatal("Enter on No should cancel without firing callback")
	}

	// Esc cancels.
	a.openConfirm("T", "M", func(*App) {})
	confirmOf(a).handleKey(a, keyEv(tcell.KeyEsc, 0))
	if confirmOf(a) != nil {
		t.Fatal("Esc should close confirm")
	}
}

// TestOpenTreeContext_Folder offers New File + Rename + Delete plus the
// two clipboard rows.
func TestOpenTreeContext_Folder(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "child")
	if err := mkdir(sub); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	// Find the child node in the tree.
	var node *filetree.Node
	for _, c := range a.tree.Root.Children {
		if c.Name == "child" {
			node = c
			break
		}
	}
	if node == nil {
		t.Fatal("child node not in tree")
	}
	a.openTreeContext(node, 5, 5)
	if contextOf(a) == nil {
		t.Fatal("context should open")
	}
	wantLabels := []string{"New File", "Rename", "Delete", "Copy", "Zip", "Copy rel path", "Copy abs path"}
	if len(contextOf(a).items) != len(wantLabels) {
		t.Fatalf("folder context should have %d items, got %d", len(wantLabels), len(contextOf(a).items))
	}
	for i, w := range wantLabels {
		if contextOf(a).items[i].label != w {
			t.Fatalf("item %d label: got %q, want %q", i, contextOf(a).items[i].label, w)
		}
	}
}

// TestOpenTreeContext_File offers Rename + Delete plus the two clipboard
// rows. New File is folder-only.
func TestOpenTreeContext_File(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := writeFile(target, "x"); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	var node *filetree.Node
	for _, c := range a.tree.Root.Children {
		if c.Name == "f.txt" {
			node = c
			break
		}
	}
	if node == nil {
		t.Fatal("file node not in tree")
	}
	a.openTreeContext(node, 5, 5)
	wantLabels := []string{"Rename", "Delete", "Copy", "Zip", "Copy rel path", "Copy abs path"}
	if len(contextOf(a).items) != len(wantLabels) {
		t.Fatalf("file context should have %d items, got %d", len(wantLabels), len(contextOf(a).items))
	}
	for i, w := range wantLabels {
		if contextOf(a).items[i].label != w {
			t.Fatalf("item %d label: got %q, want %q", i, contextOf(a).items[i].label, w)
		}
	}
}

// TestOpenTreeContext_Root offers New File and the two clipboard rows —
// Rename / Delete on the project root would be a footgun.
func TestOpenTreeContext_Root(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openTreeContext(a.tree.Root, 5, 5)
	wantLabels := []string{"New File", "Zip", "Copy rel path", "Copy abs path"}
	if len(contextOf(a).items) != len(wantLabels) {
		t.Fatalf("root context should have %d items, got %d", len(wantLabels), len(contextOf(a).items))
	}
	for i, w := range wantLabels {
		if contextOf(a).items[i].label != w {
			t.Fatalf("item %d label: got %q, want %q", i, contextOf(a).items[i].label, w)
		}
	}
}

// TestPlaceContext_FlipsLeftAndUp tests that the popup flips when it would
// otherwise overflow the window edges, and clamps at 0.
func TestPlaceContext_FlipsLeftAndUp(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.width, a.height = 30, 20

	// Click far to the right → expect cx to flip left so the popup ends
	// near the click x.
	cx, cy := a.placeContext(28, 5, 3)
	if cx >= 28 {
		t.Fatalf("expected flip left when near right edge: cx=%d", cx)
	}
	if cy != 5 {
		t.Fatalf("y unchanged at top half: cy=%d", cy)
	}

	// Click near the bottom → expect cy to flip up.
	cx, cy = a.placeContext(5, 19, 5)
	if cy >= 19 {
		t.Fatalf("expected flip up when near bottom: cy=%d", cy)
	}
	_ = cx

	// Click out at -10,-10 → expect clamp to (0,0).
	cx, cy = a.placeContext(-10, -10, 3)
	if cx != 0 || cy != 0 {
		t.Fatalf("expected clamp (0,0); got (%d,%d)", cx, cy)
	}
}

// TestContextActivate runs the highlighted item's action against the node
// the menu was opened for, and closes all modals first.
func TestContextActivate(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := 0
	var seenNode *filetree.Node
	a.modal = &contextModal{}
	contextOf(a).node = a.tree.Root
	contextOf(a).items = []contextItem{
		{label: "x", action: func(_ *App, n *filetree.Node) {
			called++
			seenNode = n
		}},
	}
	contextOf(a).hover = 0
	contextOf(a).activate(a)
	if called != 1 {
		t.Fatalf("expected action to fire once, got %d", called)
	}
	if seenNode != a.tree.Root {
		t.Fatal("action did not receive the contextNode")
	}
	if contextOf(a) != nil {
		t.Fatal("contextActivate should close modals")
	}
}

// TestContextActivate_OutOfRangeIsNoop guards against stale hover
// indexes: activating with hover outside the item list must not fire
// any action or panic.
func TestContextActivate_OutOfRangeIsNoop(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := 0
	m := &contextModal{
		node:  a.tree.Root,
		items: []contextItem{{label: "x", action: func(*App, *filetree.Node) { called++ }}},
	}
	a.modal = m
	m.hover = 5
	m.activate(a) // no panic, no state change
	m.hover = -1
	m.activate(a)
	if called != 0 {
		t.Fatalf("out-of-range activate fired the action %d times", called)
	}
}

// TestRuneLen counts visible cells one-per-rune.
func TestRuneLen(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"abc":   3,
		"héllo": 5, // five runes, one cell each by this helper's contract
		"日本":    2,
	}
	for s, want := range cases {
		if got := runeLen(s); got != want {
			t.Errorf("runeLen(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestTrimSpace strips ASCII whitespace from both ends.
func TestTrimSpace(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"   ":         "",
		"abc":         "abc",
		"  abc  ":     "abc",
		"\t\nabc\r\n": "abc",
		"a b c":       "a b c", // interior whitespace untouched
	}
	for in, want := range cases {
		if got := trimSpace(in); got != want {
			t.Errorf("trimSpace(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPromptModalRect centers the prompt and clamps a tiny window.
func TestPromptModalRect(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	m := &promptModal{}
	x, y, w, h := m.rect(a)
	if w != promptModalWidth || h != promptModalHeight {
		t.Fatalf("size: (%d,%d)", w, h)
	}
	if x != (a.width-w)/2 || y != (a.height-h)/2 {
		t.Fatalf("origin: (%d,%d)", x, y)
	}
	a.width, a.height = 4, 4
	x, y, _, _ = m.rect(a)
	if x != 0 || y != 0 {
		t.Fatalf("clamp: (%d,%d)", x, y)
	}
}

// TestConfirmModalRect centers the confirm modal and clamps a tiny window.
func TestConfirmModalRect(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	m := &confirmModal{}
	x, y, w, h := m.rect(a)
	if w != confirmModalWidth || h != confirmModalHeight {
		t.Fatalf("size: (%d,%d)", w, h)
	}
	_ = x
	_ = y
	a.width, a.height = 4, 4
	x, y, _, _ = m.rect(a)
	if x != 0 || y != 0 {
		t.Fatalf("clamp: (%d,%d)", x, y)
	}
}

// TestContextRect returns origin + width + count-derived height.
func TestContextRect(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	m := &contextModal{x: 10, y: 5, items: []contextItem{{label: "a"}, {label: "b"}}}
	x, y, w, h := m.rect(a)
	if x != 10 || y != 5 || w != contextMenuWidth || h != 4 {
		t.Fatalf("contextRect: (%d,%d,%d,%d)", x, y, w, h)
	}
}

// TestHandlePromptMouse_OutsideCancels closes the prompt when clicked
// outside its rect.
func TestHandlePromptMouse_OutsideCancels(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPrompt("T", "H", "x", nil)
	promptOf(a).handleMouse(a, 0, 0, tcell.Button1)
	if promptOf(a) != nil {
		t.Fatal("outside click should cancel")
	}
}

// TestHandlePromptMouse_OK clicks the OK button area and submits.
func TestHandlePromptMouse_OK(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	got := ""
	a.openPrompt("T", "H", "ok", func(_ *App, v string) { got = v })
	mx, my, _, _ := promptOf(a).rect(a)
	promptOf(a).handleMouse(a, mx+32, my+6, tcell.Button1)
	if got != "ok" {
		t.Fatalf("OK button: callback got %q", got)
	}
}

// TestHandlePromptMouse_Cancel clicks the Cancel button area.
func TestHandlePromptMouse_Cancel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openPrompt("T", "H", "ok", func(*App, string) { called = true })
	mx, my, _, _ := promptOf(a).rect(a)
	promptOf(a).handleMouse(a, mx+18, my+6, tcell.Button1)
	if called {
		t.Fatal("cancel button should skip callback")
	}
}

// TestHandlePromptMouse_FieldClickMovesCursor places the cursor at the
// clicked rune.
func TestHandlePromptMouse_FieldClickMovesCursor(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPrompt("T", "H", "abcdef", nil)
	mx, my, _, _ := promptOf(a).rect(a)
	// Click 2 cells into the field.
	promptOf(a).handleMouse(a, mx+3+2, my+4, tcell.Button1)
	if promptOf(a).field.cursor != 2 {
		t.Fatalf("field click: got %d, want 2", promptOf(a).field.cursor)
	}
}

// TestHandlePromptMouse_NoButton is a no-op for motion events.
func TestHandlePromptMouse_NoButton(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openPrompt("T", "H", "x", nil)
	promptOf(a).handleMouse(a, 0, 0, 0)
	if promptOf(a) == nil {
		t.Fatal("motion event should not close prompt")
	}
}

// TestHandleConfirmMouse_OutsideCancels closes when clicked outside.
func TestHandleConfirmMouse_OutsideCancels(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openConfirm("T", "M", nil)
	confirmOf(a).handleMouse(a, 0, 0, tcell.Button1)
	if confirmOf(a) != nil {
		t.Fatal("outside click should cancel")
	}
}

// TestHandleConfirmMouse_HoverAndClick covers hover (no button) and click
// branches over both buttons.
func TestHandleConfirmMouse_HoverAndClick(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openConfirm("T", "M", func(*App) { called = true })
	mx, my, _, _ := confirmOf(a).rect(a)

	// Hover over Yes (28..38) — sets confirmHover to 1.
	confirmOf(a).handleMouse(a, mx+30, my+5, 0)
	if confirmOf(a).hover != 1 {
		t.Fatalf("hover Yes: got %d", confirmOf(a).hover)
	}
	// Hover back over No (14..22).
	confirmOf(a).handleMouse(a, mx+16, my+5, 0)
	if confirmOf(a).hover != 0 {
		t.Fatalf("hover No: got %d", confirmOf(a).hover)
	}

	// Click Yes — fires callback.
	confirmOf(a).handleMouse(a, mx+30, my+5, tcell.Button1)
	if !called {
		t.Fatal("click Yes should fire callback")
	}

	// Re-open and click No.
	called = false
	a.openConfirm("T", "M", func(*App) { called = true })
	confirmOf(a).handleMouse(a, mx+16, my+5, tcell.Button1)
	if called {
		t.Fatal("click No should not fire callback")
	}
}

// TestHandleContextKey covers Up/Down clamps, Esc cancels, Enter activates.
func TestHandleContextKey(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := 0
	a.modal = &contextModal{}
	contextOf(a).node = a.tree.Root
	contextOf(a).items = []contextItem{
		{label: "one", action: func(*App, *filetree.Node) { called++ }},
		{label: "two", action: func(*App, *filetree.Node) { called += 10 }},
	}
	contextOf(a).hover = 0

	// Up at top — clamp.
	contextOf(a).handleKey(a, keyEv(tcell.KeyUp, 0))
	if contextOf(a).hover != 0 {
		t.Fatalf("Up clamp: got %d", contextOf(a).hover)
	}
	// Down advances.
	contextOf(a).handleKey(a, keyEv(tcell.KeyDown, 0))
	if contextOf(a).hover != 1 {
		t.Fatalf("Down: got %d", contextOf(a).hover)
	}
	// Down at bottom — clamp.
	contextOf(a).handleKey(a, keyEv(tcell.KeyDown, 0))
	if contextOf(a).hover != 1 {
		t.Fatalf("Down clamp: got %d", contextOf(a).hover)
	}
	// Enter activates the second item.
	contextOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if called != 10 {
		t.Fatalf("Enter: called=%d", called)
	}
	// Re-open and Esc.
	a.modal = &contextModal{}
	contextOf(a).handleKey(a, keyEv(tcell.KeyEsc, 0))
	if contextOf(a) != nil {
		t.Fatal("Esc should close")
	}
}

// TestHandleContextMouse_HoverAndClick verifies hover updates and click
// activates; outside click dismisses.
func TestHandleContextMouse_HoverAndClick(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := 0
	a.modal = &contextModal{}
	contextOf(a).x, contextOf(a).y = 5, 5
	contextOf(a).node = a.tree.Root
	contextOf(a).items = []contextItem{
		{label: "one", action: func(*App, *filetree.Node) { called++ }},
		{label: "two", action: func(*App, *filetree.Node) { called += 10 }},
	}
	contextOf(a).hover = 0

	// Hover row index 1 (relY=2 inside box at y=5 → screen y=7).
	contextOf(a).handleMouse(a, 7, 7, 0)
	if contextOf(a).hover != 1 {
		t.Fatalf("hover: got %d", contextOf(a).hover)
	}
	// Click row 0. A fresh modal instance needs its own node — state
	// no longer lingers on the App between opens.
	a.modal = &contextModal{x: 5, y: 5, node: a.tree.Root, items: []contextItem{
		{label: "one", action: func(*App, *filetree.Node) { called++ }},
	}}
	contextOf(a).handleMouse(a, 7, 6, tcell.Button1)
	if called == 0 {
		t.Fatal("click on row should activate")
	}

	// Outside click closes. Anchor away from (0,0) so the click point
	// really is outside the popup's rect.
	a.modal = &contextModal{x: 5, y: 5, items: []contextItem{{label: "x"}}}
	contextOf(a).handleMouse(a, 0, 0, tcell.Button1)
	if contextOf(a) != nil {
		t.Fatal("outside click should close")
	}
}

// -- small filesystem helpers used by the context-menu tests above ----------

// mkdir is a thin wrapper around os.Mkdir so the test bodies stay readable.
func mkdir(path string) error { return os.Mkdir(path, 0755) }

// writeFile writes payload to path. Wraps os.WriteFile to keep call sites
// terse.
func writeFile(path, payload string) error {
	return os.WriteFile(path, []byte(payload), 0644)
}

// -----------------------------------------------------------------------------
// Save / Discard / Cancel modal (unsaved-changes prompt)
// -----------------------------------------------------------------------------

// seedDirtyApp opens a tab and immediately marks it dirty so each test
// can drive the close / quit flow without re-typing the same fixture.
func seedDirtyApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(target, []byte("seed"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	a.activeTabPtr().InsertString(" edit") // dirty
	return a
}

// TestOpenDirtyClose_DefaultsToCancel pins down the "an accidental
// Enter never loses work" property: focus lands on Cancel (idx 0) when
// the modal opens, so a stray Enter just dismisses.
func TestOpenDirtyClose_DefaultsToCancel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openDirtyClose("T", "M", func(*App) {}, func(*App) {})
	if dirtyOf(a) == nil {
		t.Fatal("modal should open")
	}
	if dirtyOf(a).hover != 0 {
		t.Fatalf("default focus should be Cancel (0), got %d", dirtyOf(a).hover)
	}
}

// TestRequestCloseTab_DirtySaveClosesTab walks the Save path: opening
// the modal, picking Save should both write the file (dirty flag clears)
// and close the tab.
func TestRequestCloseTab_DirtySaveClosesTab(t *testing.T) {
	a := seedDirtyApp(t)
	target := a.tabs[0].Path

	a.requestCloseTab(0)
	if dirtyOf(a) == nil {
		t.Fatal("dirty close should open the modal")
	}
	dirtyOf(a).hover = 2 // Save
	dirtyOf(a).activate(a)

	if len(a.tabs) != 0 {
		t.Fatalf("Save should also close the tab; %d tabs left", len(a.tabs))
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after save: %v", err)
	}
	if string(got) != " editseed" {
		t.Fatalf("unexpected file contents after save: %q", got)
	}
}

// TestRequestCloseTab_DirtyDiscardClosesWithoutSaving is the
// counterpart: Discard should drop the tab without touching disk.
func TestRequestCloseTab_DirtyDiscardClosesWithoutSaving(t *testing.T) {
	a := seedDirtyApp(t)
	target := a.tabs[0].Path

	a.requestCloseTab(0)
	dirtyOf(a).hover = 1 // Discard
	dirtyOf(a).activate(a)

	if len(a.tabs) != 0 {
		t.Fatal("Discard should close the tab")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "seed" {
		t.Fatalf("Discard should not touch disk; got %q", got)
	}
}

// TestRequestCloseTab_DirtyCancelKeepsTab proves Cancel leaves
// everything alone — the tab stays open, the buffer stays dirty.
func TestRequestCloseTab_DirtyCancelKeepsTab(t *testing.T) {
	a := seedDirtyApp(t)

	a.requestCloseTab(0)
	dirtyOf(a).hover = 0 // Cancel
	dirtyOf(a).activate(a)

	if len(a.tabs) != 1 {
		t.Fatalf("Cancel should keep the tab; got %d", len(a.tabs))
	}
	if !a.activeTabPtr().Dirty {
		t.Fatal("Cancel should not flip the dirty flag")
	}
	if dirtyOf(a) != nil {
		t.Fatal("Cancel should dismiss the modal")
	}
}

// TestMenuQuit_NoDirtyTabsExitsImmediately keeps the fast path: when
// nothing is dirty, the modal must NOT open and a.quit flips on the
// spot.
func TestMenuQuit_NoDirtyTabsExitsImmediately(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clean.txt")
	if err := os.WriteFile(target, []byte("seed"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	a.menuQuit()
	if dirtyOf(a) != nil {
		t.Fatal("clean state should skip the modal")
	}
	if !a.quit {
		t.Fatal("clean menuQuit should set quit")
	}
}

// TestMenuQuit_DirtyOpensModal proves a dirty tab blocks the immediate
// exit and routes through the Save / Discard / Cancel modal.
func TestMenuQuit_DirtyOpensModal(t *testing.T) {
	a := seedDirtyApp(t)
	a.menuQuit()
	if a.quit {
		t.Fatal("dirty quit should not exit until the user picks an action")
	}
	if dirtyOf(a) == nil {
		t.Fatal("dirty quit should open the modal")
	}
}

// TestMenuQuit_DirtySaveSavesAllAndQuits drives the Save path on quit:
// every dirty tab is written and a.quit flips. This is the test that
// would catch a regression where Save quits without writing — losing
// the user's edits.
func TestMenuQuit_DirtySaveSavesAllAndQuits(t *testing.T) {
	a := seedDirtyApp(t)
	target := a.tabs[0].Path

	a.menuQuit()
	dirtyOf(a).hover = 2 // Save
	dirtyOf(a).activate(a)

	if !a.quit {
		t.Fatal("Save in quit modal should set quit")
	}
	got, _ := os.ReadFile(target)
	if string(got) != " editseed" {
		t.Fatalf("Save in quit modal should have written; got %q", got)
	}
}

// TestMenuQuit_DirtyDiscardQuitsWithoutSaving proves Discard skips the
// save and exits anyway.
func TestMenuQuit_DirtyDiscardQuitsWithoutSaving(t *testing.T) {
	a := seedDirtyApp(t)
	target := a.tabs[0].Path

	a.menuQuit()
	dirtyOf(a).hover = 1 // Discard
	dirtyOf(a).activate(a)

	if !a.quit {
		t.Fatal("Discard should still quit")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "seed" {
		t.Fatalf("Discard must not touch disk; got %q", got)
	}
}

// TestHandleDirtyKey_AllBranches walks Left / Right / Tab / Enter / Esc.
// Each branch is small; pinning all of them keeps the navigation
// behaviour stable across refactors.
func TestHandleDirtyKey_AllBranches(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	saveCalled, discardCalled := false, false
	a.openDirtyClose("T", "M",
		func(*App) { saveCalled = true },
		func(*App) { discardCalled = true })

	// Right walks Cancel -> Discard -> Save and stops at Save.
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if dirtyOf(a).hover != 1 {
		t.Fatalf("Right(0->1) got %d", dirtyOf(a).hover)
	}
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if dirtyOf(a).hover != 2 {
		t.Fatalf("Right(1->2) got %d", dirtyOf(a).hover)
	}
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyRight, 0))
	if dirtyOf(a).hover != 2 {
		t.Fatalf("Right(2->2) should clamp, got %d", dirtyOf(a).hover)
	}

	// Left walks back, also clamps at 0.
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyLeft, 0))
	if dirtyOf(a).hover != 0 {
		t.Fatalf("Left should clamp at 0, got %d", dirtyOf(a).hover)
	}

	// Tab cycles all the way around.
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyTab, 0))
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyTab, 0))
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyTab, 0))
	if dirtyOf(a).hover != 0 {
		t.Fatalf("Tab cycle should land back on 0, got %d", dirtyOf(a).hover)
	}

	// Enter on Cancel (default) — runs neither callback.
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if saveCalled || discardCalled {
		t.Fatalf("Enter on Cancel should run neither cb (save=%v discard=%v)",
			saveCalled, discardCalled)
	}

	// Re-open and Enter on Discard.
	a.openDirtyClose("T", "M",
		func(*App) { saveCalled = true },
		func(*App) { discardCalled = true })
	dirtyOf(a).hover = 1
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if !discardCalled {
		t.Fatal("Enter on Discard should fire discard callback")
	}

	// Re-open and Enter on Save.
	saveCalled = false
	a.openDirtyClose("T", "M",
		func(*App) { saveCalled = true },
		func(*App) {})
	dirtyOf(a).hover = 2
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyEnter, 0))
	if !saveCalled {
		t.Fatal("Enter on Save should fire save callback")
	}

	// Esc dismisses without firing anything.
	a.openDirtyClose("T", "M", func(*App) { t.Fatal("save fired on Esc") },
		func(*App) { t.Fatal("discard fired on Esc") })
	dirtyOf(a).handleKey(a, keyEv(tcell.KeyEsc, 0))
	if dirtyOf(a) != nil {
		t.Fatal("Esc should close the modal")
	}
}

// TestSaveAllDirty_SavesEveryTab covers the multi-tab quit path: every
// dirty tab is written; a clean tab is left alone.
func TestSaveAllDirty_SavesEveryTab(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte("seed"), 0644); err != nil {
			t.Fatal(err)
		}
		a.openFile(full)
	}
	// Dirty tabs 0 and 2; leave 1 clean.
	a.tabs[0].Buffer.InsertString(a.tabs[0].Cursor, "x")
	a.tabs[0].Dirty = true
	a.tabs[2].Buffer.InsertString(a.tabs[2].Cursor, "y")
	a.tabs[2].Dirty = true

	if !a.saveAllDirty() {
		t.Fatal("expected saveAllDirty to succeed")
	}
	if a.tabs[0].Dirty || a.tabs[2].Dirty {
		t.Fatal("dirty flags should clear after save")
	}
}

// TestDirtyTabCount counts only tabs whose Dirty flag is set.
func TestDirtyTabCount(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	for _, name := range []string{"a.txt", "b.txt"} {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte("seed"), 0644); err != nil {
			t.Fatal(err)
		}
		a.openFile(full)
	}
	if got := a.dirtyTabCount(); got != 0 {
		t.Fatalf("expected 0 dirty, got %d", got)
	}
	a.tabs[0].Dirty = true
	if got := a.dirtyTabCount(); got != 1 {
		t.Fatalf("expected 1 dirty, got %d", got)
	}
}

// TestDirtyButtonAt_HitsAndMisses pins the geometry helper so the
// click rect math stays in sync with the draw layout: a hit one cell
// into each button resolves to its index, and cells between / past the
// buttons miss.
func TestDirtyButtonAt_HitsAndMisses(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openDirtyClose("Unsaved", "save?", nil, nil)
	m := dirtyOf(a)
	b := m.buttons(a)
	cases := []struct {
		x, y int
		want int
	}{
		{b[0].x + 1, b[0].y, 0},
		{b[1].x + 1, b[1].y, 1},
		{b[2].x + 1, b[2].y, 2},
		{b[0].x - 1, b[0].y, -1},          // left of Cancel
		{b[2].x + b[2].w + 5, b[2].y, -1}, // past Save
		{b[1].x + 1, b[1].y + 1, -1},      // right column, wrong row
	}
	for _, c := range cases {
		if got := m.buttonAt(a, c.x, c.y); got != c.want {
			t.Errorf("(%d,%d): got %d, want %d", c.x, c.y, got, c.want)
		}
	}
}
