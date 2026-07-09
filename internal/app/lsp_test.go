// =============================================================================
// File: internal/app/lsp_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/lsp"
	"github.com/rohanthewiz/r-ed/internal/theme"
)

// fakeLSPConn records every notification the app sends and returns
// canned answers for the two requests, so the whole integration is
// testable without a server process.
type fakeLSPConn struct {
	mu     sync.Mutex
	calls  []string // "didOpen:path:v", "didChange:path:v", "didSave:path", "didClose:path"
	closed bool

	defLocs  []lsp.Location
	defErr   error
	hoverRes *lsp.Hover
	hoverErr error
}

func (f *fakeLSPConn) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

// callLog returns a copy of the recorded calls (mutex-guarded because
// Definition/HoverAt run on request goroutines).
func (f *fakeLSPConn) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLSPConn) DidOpen(path, _ string, version int, _ string) error {
	f.record(fmt.Sprintf("didOpen:%s:%d", filepath.Base(path), version))
	return nil
}

func (f *fakeLSPConn) DidChange(path string, version int, _ string) error {
	f.record(fmt.Sprintf("didChange:%s:%d", filepath.Base(path), version))
	return nil
}

func (f *fakeLSPConn) DidSave(path string) error {
	f.record("didSave:" + filepath.Base(path))
	return nil
}

func (f *fakeLSPConn) DidClose(path string) error {
	f.record("didClose:" + filepath.Base(path))
	return nil
}

func (f *fakeLSPConn) Definition(string, lsp.Position) ([]lsp.Location, error) {
	return f.defLocs, f.defErr
}

func (f *fakeLSPConn) HoverAt(string, lsp.Position) (*lsp.Hover, error) {
	return f.hoverRes, f.hoverErr
}

func (f *fakeLSPConn) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// newLSPTestApp builds a test app with a fake, ready LSP connection
// and one seeded Go file on disk (returned as goPath).
func newLSPTestApp(t *testing.T) (*App, *fakeLSPConn, string) {
	t.Helper()
	dir := t.TempDir()
	goPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n\nfunc main() {\n}\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	fake := &fakeLSPConn{}
	a.lsp.dead = false
	a.lsp.client = fake
	return a, fake, goPath
}

// TestLSPHandles pins the file filter: .go in (case-insensitively),
// everything else out.
func TestLSPHandles(t *testing.T) {
	for path, want := range map[string]bool{
		"/a/b/main.go": true,
		"/a/B/MAIN.GO": true,
		"/a/notes.txt": false,
		"/a/go":        false, // extensionless file named "go"
		"":             false,
	} {
		if got := lspHandles(path); got != want {
			t.Errorf("lspHandles(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestLSPOpenDocAnnounces pins the didOpen path: opening a Go file
// through openFile announces version 1, while non-Go files stay
// invisible to the server.
func TestLSPOpenDocAnnounces(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	txtPath := filepath.Join(a.rootDir, "notes.txt")
	if err := os.WriteFile(txtPath, []byte("hi"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	a.openFile(goPath)
	a.openFile(txtPath)

	calls := fake.callLog()
	if len(calls) != 1 || calls[0] != "didOpen:main.go:1" {
		t.Errorf("calls = %v, want exactly [didOpen:main.go:1]", calls)
	}
	if a.lsp.versions[goPath] != 1 {
		t.Errorf("version = %d, want 1", a.lsp.versions[goPath])
	}
}

// TestHandleLSPReadyAnnouncesOpenTabs pins the handshake catch-up:
// documents opened while the server was still starting get their
// didOpen when the ready event lands.
func TestHandleLSPReadyAnnouncesOpenTabs(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	// Rewind to "still starting": docs opened now can't be announced.
	a.lsp.client = nil
	a.lsp.starting = true
	a.openFile(goPath)

	fake := &fakeLSPConn{}
	a.handleLSPReady(&lspReadyEvent{when: time.Now(), client: fake})

	calls := fake.callLog()
	if len(calls) != 1 || calls[0] != "didOpen:main.go:1" {
		t.Errorf("calls after ready = %v, want the queued didOpen", calls)
	}
	if a.lsp.starting {
		t.Error("starting flag should clear on ready")
	}
}

// TestHandleLSPReadyAfterDeath pins the race guard: a ready event that
// lands after the integration was marked dead must close the fresh
// connection instead of resurrecting a zombie.
func TestHandleLSPReadyAfterDeath(t *testing.T) {
	a, _, _ := newLSPTestApp(t)
	a.lsp.client = nil
	a.lsp.dead = true

	fake := &fakeLSPConn{}
	a.handleLSPReady(&lspReadyEvent{when: time.Now(), client: fake})
	if a.lsp.client != nil {
		t.Error("dead integration must not adopt a late client")
	}
	if !fake.closed {
		t.Error("late client must be closed, not leaked")
	}
}

// TestDebounceSyncFlow drives the edit → debounce → didChange cycle:
// an edit arms the timer, the sync event sends one full-text change at
// version 2, and a second sync with no further edits is a no-op.
func TestDebounceSyncFlow(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	tab := a.activeTabPtr()

	tab.InsertRune('x')
	a.lspAfterEvent()
	if a.lsp.timers[goPath] == nil {
		t.Fatal("edit should arm the debounce timer")
	}

	a.handleLSPSync(&lspSyncEvent{when: time.Now(), path: goPath})
	calls := fake.callLog()
	if len(calls) != 2 || calls[1] != "didChange:main.go:2" {
		t.Fatalf("calls = %v, want didChange at version 2", calls)
	}

	// Quiet document → sync is a no-op.
	a.handleLSPSync(&lspSyncEvent{when: time.Now(), path: goPath})
	if got := fake.callLog(); len(got) != 2 {
		t.Errorf("no-edit sync sent something: %v", got)
	}
}

// TestSaveFlushesBeforeDidSave pins the ordering contract: a save with
// unsynced edits sends didChange first so the server never diagnoses
// stale content against a save marker.
func TestSaveFlushesBeforeDidSave(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	a.activeTabPtr().InsertRune('y')

	a.saveActiveTab()

	calls := fake.callLog()
	want := []string{"didOpen:main.go:1", "didChange:main.go:2", "didSave:main.go"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("calls[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

// TestCloseTabClosesDoc pins tab-close cleanup: the server hears
// didClose and every per-document map drops its entry.
func TestCloseTabClosesDoc(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	a.handleLSPDiags(&lspDiagsEvent{when: time.Now(), path: goPath,
		diags: []lsp.Diagnostic{{Message: "x"}}})

	a.closeTab(0)

	calls := fake.callLog()
	if calls[len(calls)-1] != "didClose:main.go" {
		t.Errorf("calls = %v, want trailing didClose", calls)
	}
	if _, ok := a.lsp.versions[goPath]; ok {
		t.Error("versions entry should be gone")
	}
	if _, ok := a.lsp.diags[goPath]; ok {
		t.Error("diags entry should be gone")
	}
}

// TestHandleLSPDiagsStoreAndRetract pins the store semantics: non-empty
// publishes replace, empty publishes retract.
func TestHandleLSPDiagsStoreAndRetract(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.handleLSPDiags(&lspDiagsEvent{when: time.Now(), path: goPath,
		diags: []lsp.Diagnostic{{Message: "boom", Severity: lsp.SeverityError}}})
	if len(a.lsp.diags[goPath]) != 1 {
		t.Fatal("diagnostic not stored")
	}
	a.handleLSPDiags(&lspDiagsEvent{when: time.Now(), path: goPath, diags: nil})
	if _, ok := a.lsp.diags[goPath]; ok {
		t.Error("empty publish should retract")
	}
}

// TestHandleLSPExitClearsState pins crash cleanup: dead flag set,
// connection dropped, and every stale diagnostic cleared so squiggles
// from a gone server can't linger.
func TestHandleLSPExitClearsState(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	a.lsp.diags = map[string][]lsp.Diagnostic{goPath: {{Message: "x"}}}

	a.handleLSPExit()

	if !a.lsp.dead || a.lsp.client != nil {
		t.Error("exit should mark dead and drop the client")
	}
	if !fake.closed {
		t.Error("exit should close the connection")
	}
	if a.lsp.diags != nil {
		t.Error("exit should clear diagnostics")
	}
}

// TestDiagSourceSpansAndMarks pins the decoration adapter: underline
// spans with severity colors, one gutter dot per line with the worst
// severity winning, zero-width ranges stretched to one visible cell.
func TestDiagSourceSpansAndMarks(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	tab := a.activeTabPtr()
	th := theme.Default()

	a.lsp.diags = map[string][]lsp.Diagnostic{goPath: {
		// Warning first, error second, same line — error must win the mark.
		{Severity: lsp.SeverityWarning, Range: lsp.Range{
			Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 7}}},
		{Severity: lsp.SeverityError, Range: lsp.Range{
			Start: lsp.Position{Line: 0, Character: 8}, End: lsp.Position{Line: 0, Character: 12}}},
		// Zero-width diagnostic on line 2.
		{Severity: lsp.SeverityInfo, Range: lsp.Range{
			Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 5}}},
	}}

	src := lspDiagSource{app: a}
	spans, marks := src.Decorations(tab, th, 0, 10)

	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(spans))
	}
	if !spans[0].Delta.Underline || !spans[0].Delta.SetFG || spans[0].Delta.FG != th.DiagWarning {
		t.Errorf("warning span delta = %+v", spans[0].Delta)
	}
	if spans[2].End.Col != spans[2].Start.Col+1 {
		t.Errorf("zero-width span not stretched: %+v", spans[2])
	}

	sevByLine := map[int]tcell.Color{}
	for _, m := range marks {
		if m.Glyph != '●' {
			t.Errorf("mark glyph = %q, want ●", m.Glyph)
		}
		sevByLine[m.Line] = m.FG
	}
	if len(marks) != 2 {
		t.Fatalf("marks = %d, want one per afflicted line", len(marks))
	}
	if sevByLine[0] != th.DiagError {
		t.Error("line 0 mark should carry the error color (worst severity wins)")
	}
	if sevByLine[2] != th.DiagInfo {
		t.Error("line 2 mark should carry the info color")
	}
}

// TestDiagSourceCulling pins the window/EOF guards: diagnostics
// outside the visible window or past the end of the (edited-shorter)
// buffer produce nothing rather than panicking.
func TestDiagSourceCulling(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	tab := a.activeTabPtr()

	a.lsp.diags = map[string][]lsp.Diagnostic{goPath: {
		{Range: lsp.Range{Start: lsp.Position{Line: 50}, End: lsp.Position{Line: 50}}},   // off-window
		{Range: lsp.Range{Start: lsp.Position{Line: 999}, End: lsp.Position{Line: 999}}}, // past EOF
	}}
	spans, marks := lspDiagSource{app: a}.Decorations(tab, theme.Default(), 0, 10)
	if len(spans) != 0 || len(marks) != 0 {
		t.Errorf("culled window produced spans=%d marks=%d", len(spans), len(marks))
	}
}

// TestDiagStatusSuffix pins the status-bar summary formats, including
// the clean-file empty string that keeps the common case quiet.
func TestDiagStatusSuffix(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.openFile(goPath)

	if got := a.diagStatusSuffix(); got != "" {
		t.Errorf("clean file suffix = %q, want empty", got)
	}
	a.lsp.diags = map[string][]lsp.Diagnostic{goPath: {
		{Severity: lsp.SeverityError}, {Severity: lsp.SeverityError},
		{Severity: lsp.SeverityWarning},
		{Severity: lsp.SeverityHint},
	}}
	if got := a.diagStatusSuffix(); got != " · ✗ 2 ⚠ 1 ℹ 1" {
		t.Errorf("suffix = %q", got)
	}
}

// TestDefinitionJumpAndBack drives the full jump cycle: the definition
// event moves to the target with the origin pushed on the nav stack,
// and Jump back retraces exactly.
func TestDefinitionJumpAndBack(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	otherPath := filepath.Join(a.rootDir, "other.go")
	if err := os.WriteFile(otherPath, []byte("package main\n\nfunc helper() {}\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a.openFile(goPath)
	origin := editor.Position{Line: 2, Col: 5}
	a.activeTabPtr().MoveCursorTo(origin, false)

	a.handleLSPDefinition(&lspDefinitionEvent{
		when: time.Now(), fromPath: goPath, fromPos: origin,
		locs: []lsp.Location{{URI: lsp.PathToURI(otherPath),
			Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}}},
	})

	if tab := a.activeTabPtr(); tab.Path != otherPath {
		t.Fatalf("active tab = %s, want %s", tab.Path, otherPath)
	} else if tab.Cursor != (editor.Position{Line: 2, Col: 5}) {
		t.Errorf("cursor = %+v, want line 2 col 5", tab.Cursor)
	}
	if len(a.lsp.navStack) != 1 {
		t.Fatal("origin not pushed on nav stack")
	}

	a.menuJumpBack()
	if tab := a.activeTabPtr(); tab.Path != goPath || tab.Cursor != origin {
		t.Errorf("jump back landed at %s %+v, want %s %+v", tab.Path, tab.Cursor, goPath, origin)
	}
	if len(a.lsp.navStack) != 0 {
		t.Error("nav stack should be empty after jump back")
	}
}

// TestDefinitionNoResult pins the empty-answer UX: a flash, no jump,
// no nav-stack garbage.
func TestDefinitionNoResult(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.openFile(goPath)

	a.handleLSPDefinition(&lspDefinitionEvent{when: time.Now(), fromPath: goPath})
	if a.statusMsg != "No definition found" {
		t.Errorf("flash = %q", a.statusMsg)
	}
	if len(a.lsp.navStack) != 0 {
		t.Error("failed lookup must not push the nav stack")
	}
}

// TestMenuGoToDefinitionAsync is the end-to-end round trip: menu action
// → request goroutine → posted event → handled on the main loop. Pins
// that the plumbing is actually connected, with a deadline so a broken
// post can't hang the suite.
func TestMenuGoToDefinitionAsync(t *testing.T) {
	a, fake, goPath := newLSPTestApp(t)
	a.openFile(goPath)
	fake.defLocs = []lsp.Location{{URI: lsp.PathToURI(goPath),
		Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 0}}}}

	a.menuGoToDefinition()

	deadline := time.After(2 * time.Second)
	for a.activeTabPtr().Cursor.Line != 2 {
		ch := make(chan tcell.Event, 1)
		go func() { ch <- a.screen.PollEvent() }()
		select {
		case ev := <-ch:
			a.handleEvent(ev)
		case <-deadline:
			t.Fatal("definition event never arrived")
		}
	}
	if len(a.lsp.navStack) != 1 {
		t.Error("async jump should record its origin")
	}
}

// TestHoverOpensModal pins the hover landing: text opens the
// near-cursor modal, an empty answer flashes, and a response for a tab
// the user already left is dropped.
func TestHoverOpensModal(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	a.openFile(goPath)

	a.handleLSPHover(&lspHoverEvent{when: time.Now(), path: goPath, text: "func main()"})
	if _, ok := a.modal.(*hoverModal); !ok {
		t.Fatalf("modal = %T, want *hoverModal", a.modal)
	}
	a.closeModal()

	a.handleLSPHover(&lspHoverEvent{when: time.Now(), path: goPath, text: ""})
	if a.modal != nil {
		t.Error("empty hover should not open a modal")
	}
	if a.statusMsg != "No hover info" {
		t.Errorf("flash = %q", a.statusMsg)
	}

	a.handleLSPHover(&lspHoverEvent{when: time.Now(), path: "/somewhere/else.go", text: "stale"})
	if a.modal != nil {
		t.Error("hover for a left-behind tab must be dropped")
	}
}

// TestHoverLines pins the text flattening: fences stripped, blank
// edges trimmed, and the cap with its ellipsis marker.
func TestHoverLines(t *testing.T) {
	got := hoverLines("```go\nfunc F()\n```\n\ndocs here\n")
	want := []string{"func F()", "", "docs here"}
	if len(got) != len(want) {
		t.Fatalf("hoverLines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}

	long := ""
	for i := 0; i < 20; i++ {
		long += fmt.Sprintf("line %d\n", i)
	}
	capped := hoverLines(long)
	if len(capped) != 13 || capped[12] != "…" {
		t.Errorf("cap: got %d lines, last %q", len(capped), capped[len(capped)-1])
	}

	if hoverLines("  \n\t\n") != nil {
		t.Error("whitespace-only hover should flatten to nothing")
	}
}

// TestLSPPredicates pins the menu enablement: definition/hover need a
// ready server and a Go tab; Jump back needs history.
func TestLSPPredicates(t *testing.T) {
	a, _, goPath := newLSPTestApp(t)
	if a.hasLSPActions() {
		t.Error("no tab open — actions should be disabled")
	}
	a.openFile(goPath)
	if !a.hasLSPActions() {
		t.Error("go tab + ready server — actions should be enabled")
	}
	a.lsp.dead = true
	if a.hasLSPActions() {
		t.Error("dead server — actions should be disabled")
	}
	if a.hasNavBack() {
		t.Error("empty stack — Jump back should be disabled")
	}
	a.pushNav(navLoc{path: goPath})
	if !a.hasNavBack() {
		t.Error("non-empty stack — Jump back should be enabled")
	}
}

// TestPushNavCap pins the stack cap: overflow drops the oldest
// entries, not the newest.
func TestPushNavCap(t *testing.T) {
	a, _, _ := newLSPTestApp(t)
	for i := 0; i < lspNavStackMax+10; i++ {
		a.pushNav(navLoc{pos: editor.Position{Line: i}})
	}
	if len(a.lsp.navStack) != lspNavStackMax {
		t.Fatalf("stack len = %d, want %d", len(a.lsp.navStack), lspNavStackMax)
	}
	if got := a.lsp.navStack[len(a.lsp.navStack)-1].pos.Line; got != lspNavStackMax+9 {
		t.Errorf("newest entry line = %d, want %d", got, lspNavStackMax+9)
	}
}

// TestLSPLeaderBindings pins the three new leader keys onto their
// actions by behavior: Esc-o with history retraces the jump (the
// menu-method equivalence the other leaders are tested by).
func TestLSPLeaderBindings(t *testing.T) {
	for _, key := range []rune{'d', 'i', 'o'} {
		if leaderActionFor(key) == nil {
			t.Errorf("leader %q not bound", key)
		}
	}
}

// TestMenuLSPRows pins the ≡ menu's Code group rows so a layout
// reshuffle can't silently drop them.
func TestMenuLSPRows(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	for _, label := range []string{"Go to definition", "Hover info", "Jump back"} {
		menuItemByLabel(t, a, label) // fails the test if missing
	}
}

// TestPosConversionRoundTrip pins the rune ↔ UTF-16 bridge through a
// non-BMP line — the case that misplaces diagnostics when skipped.
func TestPosConversionRoundTrip(t *testing.T) {
	tab := &editor.Tab{Buffer: editor.NewBuffer("x := \"🙂\" // smile\n")}
	p := editor.Position{Line: 0, Col: 8} // just past the emoji + quote
	lp := lspPosFor(tab, p)
	if lp.Character != 9 { // emoji costs 2 UTF-16 units
		t.Errorf("UTF-16 col = %d, want 9", lp.Character)
	}
	if back := editorPosFor(tab, lp); back != p {
		t.Errorf("round trip = %+v, want %+v", back, p)
	}
}

// TestLSPEndToEndWithRealGopls drives the app-level flow against a
// real server: openFile triggers the async spawn + handshake, the
// ready event announces the document, and a publishDiagnostics for a
// deliberate type error lands in App.lsp.diags. Skipped when gopls
// isn't installed — same convention as the git end-to-end tests.
func TestLSPEndToEndWithRealGopls(t *testing.T) {
	if _, err := exec.LookPath(lspServerBinary); err != nil {
		t.Skip("gopls not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module e2e\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {\n\tnotDefined()\n}\n"), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}

	a := newTestApp(t, dir)
	a.lsp.dead = false // re-enable what newTestApp disabled — this test wants the real thing
	t.Cleanup(a.lspShutdown)

	a.openFile(src)

	// Pump the event loop until diagnostics arrive: ready → didOpen →
	// publishDiagnostics, all via posted events.
	deadline := time.After(60 * time.Second)
	for len(a.lsp.diags[src]) == 0 {
		ch := make(chan tcell.Event, 1)
		go func() { ch <- a.screen.PollEvent() }()
		select {
		case ev := <-ch:
			a.handleEvent(ev)
		case <-deadline:
			t.Fatal("no diagnostics from real gopls within 60s")
		}
	}
	if msg := a.lsp.diags[src][0].Message; !strings.Contains(msg, "notDefined") {
		t.Errorf("diagnostic = %q, want mention of notDefined", msg)
	}
	if !a.hasLSPActions() {
		t.Error("definition/hover should be enabled once the server is up")
	}
}
