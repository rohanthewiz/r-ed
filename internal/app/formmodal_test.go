// =============================================================================
// File: internal/app/formmodal_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/customactions"
)

// scpPrompts is the worked example from the docs / README — keeps
// the form-modal tests grounded in the actual user-facing schema
// rather than a synthetic shape that drifts.
func scpPrompts() []customactions.Prompt {
	return []customactions.Prompt{
		{Key: "HOST", Label: "Host", Type: customactions.PromptSelect,
			Options: []string{"cascade", "rager"}},
		{Key: "DEST_DIR", Label: "Local destination", Type: customactions.PromptText,
			Default: "${ACTIVE_FOLDER}"},
		{Key: "REMOTE_SRC", Label: "Remote file", Type: customactions.PromptText},
	}
}

// TestOpenForm_ResolvesDefaults pins the open contract: every
// Default string is expanded through the editor-state vars before
// the modal is rendered. Without this the user would see literal
// "${ACTIVE_FOLDER}" text in the input — a regression we'd notice
// only after running the action.
func TestOpenForm_ResolvesDefaults(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openForm("Copy from remote", scpPrompts(), func(*App, map[string]string) {})
	if formOf(a) == nil {
		t.Fatal("form should be open")
	}
	got := formOf(a).values["DEST_DIR"]
	if got == "" || got == "${ACTIVE_FOLDER}" {
		t.Fatalf("DEST_DIR default not expanded, got %q", got)
	}
	// Select prompts initialise to the matching option, or the first
	// option when no Default was provided. HOST has no Default so we
	// should land on "cascade".
	if formOf(a).values["HOST"] != "cascade" {
		t.Errorf("HOST initial value = %q, want %q", formOf(a).values["HOST"], "cascade")
	}
}

// TestOpenForm_RebuildsState ensures opening a second form doesn't
// inherit the first form's text or focus. State leaking between
// modal instances is a real footgun; this test pins the rebuild.
func TestOpenForm_RebuildsState(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	a.openForm("First", []customactions.Prompt{
		{Key: "ALPHA", Label: "A", Type: customactions.PromptText, Default: "first"},
	}, nil)
	if formOf(a).values["ALPHA"] != "first" {
		t.Fatalf("first ALPHA = %q", formOf(a).values["ALPHA"])
	}

	a.openForm("Second", []customactions.Prompt{
		{Key: "BETA", Label: "B", Type: customactions.PromptText, Default: "second"},
	}, nil)
	if _, ok := formOf(a).values["ALPHA"]; ok {
		t.Errorf("ALPHA leaked from previous form: %v", formOf(a).values)
	}
	if formOf(a).values["BETA"] != "second" {
		t.Errorf("BETA = %q, want %q", formOf(a).values["BETA"], "second")
	}
	if formOf(a).focus != 0 {
		t.Errorf("focus should reset to 0, got %d", formOf(a).focus)
	}
}

// TestForm_TabCyclesFocus pins the headline keyboard shortcut: Tab
// advances, Shift+Tab retreats, both wrap. Without wrap the user
// hits a "stuck" Tab at the bottom of the form which feels broken.
func TestForm_TabCyclesFocus(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openForm("Test", scpPrompts(), nil)

	steps := []struct {
		key  tcell.Key
		want int
	}{
		{tcell.KeyTab, 1},
		{tcell.KeyTab, 2},
		{tcell.KeyTab, 0},     // wraps
		{tcell.KeyBacktab, 2}, // wraps backward
		{tcell.KeyBacktab, 1},
	}
	for i, s := range steps {
		formOf(a).handleKey(a, tcell.NewEventKey(s.key, 0, tcell.ModNone))
		if formOf(a).focus != s.want {
			t.Errorf("step %d: focus = %d, want %d", i, formOf(a).focus, s.want)
		}
	}
}

// TestForm_SelectCyclesOptions ensures Left / Right cycle options
// and update formValues. The form is the only place a select widget
// lives; if this drifts the SCP host picker silently desyncs from
// what the shell command sees on $HOST.
func TestForm_SelectCyclesOptions(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openForm("Test", scpPrompts(), nil)
	// Focus is on HOST (index 0) by default.

	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone))
	if formOf(a).values["HOST"] != "rager" {
		t.Errorf("after Right: HOST = %q, want %q", formOf(a).values["HOST"], "rager")
	}
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone))
	if formOf(a).values["HOST"] != "cascade" {
		t.Errorf("after Right wrap: HOST = %q, want %q", formOf(a).values["HOST"], "cascade")
	}
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone))
	if formOf(a).values["HOST"] != "rager" {
		t.Errorf("after Left wrap: HOST = %q, want %q", formOf(a).values["HOST"], "rager")
	}
}

// TestForm_TextEditingFlow walks rune-insert → backspace → caret
// motion against a focused text row and confirms formValues stays
// in lockstep with the rune buffer. Both render and submit pull
// from formValues so a desync would silently submit stale data.
func TestForm_TextEditingFlow(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openForm("Test", []customactions.Prompt{
		{Key: "REMOTE_SRC", Label: "Remote file", Type: customactions.PromptText},
	}, nil)

	for _, r := range "/tmp/x" {
		formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	if formOf(a).values["REMOTE_SRC"] != "/tmp/x" {
		t.Fatalf("after typing: %q", formOf(a).values["REMOTE_SRC"])
	}
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	if formOf(a).values["REMOTE_SRC"] != "/tmp/" {
		t.Fatalf("after backspace: %q", formOf(a).values["REMOTE_SRC"])
	}
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone))
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyRune, '~', tcell.ModNone))
	if formOf(a).values["REMOTE_SRC"] != "~/tmp/" {
		t.Fatalf("after Home + insert: %q", formOf(a).values["REMOTE_SRC"])
	}
}

// TestForm_EnterAdvancesThenSubmits is the "press Enter to fly
// through the form" flow: Enter on a non-final row advances focus,
// Enter on the last row fires the callback. Without the advance,
// users would have to alternate Enter/Tab; without the submit, the
// last row would have no way to fire from the keyboard at all.
func TestForm_EnterAdvancesThenSubmits(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	got := map[string]string{}
	called := false
	a.openForm("Test", []customactions.Prompt{
		{Key: "ALPHA", Label: "A", Type: customactions.PromptText, Default: "a"},
		{Key: "BETA", Label: "B", Type: customactions.PromptText, Default: "b"},
	}, func(_ *App, values map[string]string) {
		called = true
		got = values
	})

	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if formOf(a).focus != 1 {
		t.Fatalf("Enter on first row should advance focus, got %d", formOf(a).focus)
	}
	if called {
		t.Fatal("callback should not fire until last-row submit")
	}
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if !called || got["ALPHA"] != "a" || got["BETA"] != "b" {
		t.Fatalf("submit didn't pass values: called=%v got=%v", called, got)
	}
	if formOf(a) != nil {
		t.Error("form should close on submit")
	}
}

// TestForm_EscCancelsWithoutCallback locks the safety contract:
// hitting Esc throws away typed values and never fires the callback.
// Anything else risks a destructive scp running because the user
// fat-fingered Enter while reaching for Esc.
func TestForm_EscCancelsWithoutCallback(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openForm("Test", scpPrompts(), func(*App, map[string]string) { called = true })
	formOf(a).handleKey(a, tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if formOf(a) != nil {
		t.Error("Esc should close form")
	}
	if called {
		t.Error("callback fired on cancel")
	}
}

// TestForm_MouseClicksOnSelectChevrons walks the mouse path users
// will actually take — the editor is mouse-first, so Submit /
// Cancel and the < > select widgets all need to work without the
// keyboard. Pinning the chevron click rects keeps drawForm and
// handleFormMouse from drifting on layout.
func TestForm_MouseClicksOnSelectChevrons(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openForm("Test", scpPrompts(), nil)

	mx, my, mw, _ := formOf(a).rect(a)
	fieldStart := mx + 3
	fieldEnd := mx + mw - 3
	hostInputRow := my + 3 + 1 // first prompt's input row

	// Click the > chevron — should advance from cascade to rager.
	formOf(a).handleMouse(a, fieldEnd-1, hostInputRow, tcell.Button1)
	if formOf(a).values["HOST"] != "rager" {
		t.Errorf("after > click: HOST = %q", formOf(a).values["HOST"])
	}
	// Click the < chevron — should retreat back to cascade.
	formOf(a).handleMouse(a, fieldStart, hostInputRow, tcell.Button1)
	if formOf(a).values["HOST"] != "cascade" {
		t.Errorf("after < click: HOST = %q", formOf(a).values["HOST"])
	}
}

// TestForm_MouseSubmitButton confirms the [ Submit ] button fires
// the callback and closes the modal. Same coverage shape as the
// keyboard test, just via the mouse path the user will actually
// click most of the time.
func TestForm_MouseSubmitButton(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openForm("Test", scpPrompts(), func(*App, map[string]string) { called = true })

	_, submit := formOf(a).buttons(a)
	formOf(a).handleMouse(a, submit.x+submit.w/2, submit.y, tcell.Button1)
	if !called {
		t.Error("Submit click did not fire callback")
	}
	if formOf(a) != nil {
		t.Error("Submit click did not close form")
	}
}

// TestForm_ClickOutsideCancels matches every other modal in the
// editor — clicking outside dismisses without running the callback.
// Without this the form would feel sticky compared to prompt /
// confirm / dirty.
func TestForm_ClickOutsideCancels(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	called := false
	a.openForm("Test", scpPrompts(), func(*App, map[string]string) { called = true })
	formOf(a).handleMouse(a, 0, 0, tcell.Button1)
	if formOf(a) != nil {
		t.Error("click outside should close")
	}
	if called {
		t.Error("callback fired on outside click")
	}
}

// TestAnyModalOpen_IncludesForm guards the router: the editor's
// click-through and key-routing logic short-circuits via
// anyModalOpen, so leaving formOpen out would mean the editor
// happily delivers normal keystrokes to the buffer underneath
// the form modal.
func TestAnyModalOpen_IncludesForm(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if a.anyModalOpen() {
		t.Fatal("startup state should report no modal open")
	}
	a.openForm("Test", scpPrompts(), nil)
	if !a.anyModalOpen() {
		t.Error("form open should report a modal open")
	}
	a.closeModal()
	if a.anyModalOpen() {
		t.Error("after cancel, no modal should be open")
	}
}
