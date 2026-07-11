// =============================================================================
// File: internal/app/terminal_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the embedded terminal panel. Every test runs against
// fakeTermEval (newTestApp stubs newTermEvaluator) so no grsh session
// ever executes a real command — the same isolation contract as the
// LSP fake and the stubbed builtin formatter.

package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/grsh"
)

// fakeTermEval satisfies termEvaluator with recordable, controllable
// behavior. Eval calls arrive on a goroutine (submitTermCommand spawns
// one), so the recorded state is mutex-guarded and tests poll via
// waitEvals.
type fakeTermEval struct {
	mu          sync.Mutex
	evals       []string
	evalErr     error
	needsMore   func(string) bool
	interrupts  int
	kills       int
	interruptOK bool
	lastStatus  int
	cwd         string
	notes       []string
}

func (f *fakeTermEval) Eval(src string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evals = append(f.evals, src)
	return f.evalErr
}

func (f *fakeTermEval) NeedsMore(src string) bool {
	if f.needsMore != nil {
		return f.needsMore(src)
	}
	return false
}

func (f *fakeTermEval) Interrupt() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts++
	return f.interruptOK
}

func (f *fakeTermEval) Kill() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills++
	return true
}

func (f *fakeTermEval) LastStatus() int { return f.lastStatus }

func (f *fakeTermEval) Cwd() string {
	if f.cwd == "" {
		return "/tmp/fake"
	}
	return f.cwd
}

func (f *fakeTermEval) Notifications() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.notes
	f.notes = nil
	return n
}

// waitEvals polls until the fake has recorded n Eval calls — the submit
// path runs Eval on a goroutine, so assertions must wait for it.
func (f *fakeTermEval) waitEvals(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.evals) >= n {
			out := append([]string(nil), f.evals...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d Eval call(s)", n)
	return nil
}

// openTestTerm opens the panel on a test app and returns the injected
// fake for inspection.
func openTestTerm(t *testing.T, a *App) *fakeTermEval {
	t.Helper()
	a.menuToggleTerminal()
	f, ok := a.term.sess.(*fakeTermEval)
	if !ok {
		t.Fatalf("expected fakeTermEval, got %T", a.term.sess)
	}
	return f
}

// typeTermLine feeds a string into the focused panel one rune event at
// a time — the same path real keystrokes take.
func typeTermLine(a *App, s string) {
	for _, r := range s {
		a.handleTermKey(tcell.NewEventKey(tcell.KeyRune, r, 0))
	}
}

// TestTermToggle pins the ≡ toggle's contract: opening focuses the
// input, builds the session lazily, and claims the bottom strip from
// the git panel; closing keeps the session for a warm reopen.
func TestTermToggle(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.gitPanel.open = true

	a.menuToggleTerminal()
	if !a.term.open || !a.term.focused {
		t.Fatal("toggle should open and focus the panel")
	}
	if a.term.sess == nil {
		t.Fatal("opening should build the session")
	}
	if a.gitPanel.open {
		t.Error("opening the terminal should collapse the git panel (single-occupancy strip)")
	}

	sess := a.term.sess
	a.menuToggleTerminal()
	if a.term.open || a.term.focused {
		t.Fatal("second toggle should close and unfocus")
	}
	if a.term.sess != sess {
		t.Error("closing should keep the session (variables and cwd survive a hide)")
	}
}

// TestGitPanelToggleClosesTerminal is the other half of the
// single-occupancy rule.
func TestGitPanelToggleClosesTerminal(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.gitIsRepo = true
	a.menuToggleGitPanel()
	if !a.gitPanel.open {
		t.Fatal("git panel should open")
	}
	if a.term.open || a.term.focused {
		t.Error("opening the git panel should collapse the terminal")
	}
}

// TestLeaderTerminal pins the Esc-` nicety: an open-but-unfocused
// panel grabs focus instead of closing.
func TestLeaderTerminal(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.term.focused = false

	a.leaderTerminal()
	if !a.term.open || !a.term.focused {
		t.Fatal("leader on unfocused panel should focus, not close")
	}
	a.leaderTerminal()
	if a.term.open {
		t.Fatal("leader on focused panel should close it")
	}
}

// TestTermSubmitEvalsCommand walks the happy path: typed command is
// echoed to the scrollback, Eval receives it, and the done event
// clears the running flag.
func TestTermSubmitEvalsCommand(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)

	typeTermLine(a, "echo hi")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))

	if evals := f.waitEvals(t, 1); evals[0] != "echo hi" {
		t.Errorf("Eval got %q, want %q", evals[0], "echo hi")
	}
	if !a.term.running {
		t.Error("running should be set while the Eval is in flight")
	}
	var echoed bool
	for _, ln := range a.term.lines {
		if ln.kind == termCmd && strings.Contains(ln.text, "echo hi") {
			echoed = true
		}
	}
	if !echoed {
		t.Error("submitted command should be echoed into the scrollback")
	}

	a.handleTermDone(&termDoneEvent{when: time.Now()})
	if a.term.running {
		t.Error("done event should clear running")
	}
}

// TestTermSubmitWhileRunningIsBlocked pins the busy rule: Enter during
// an in-flight Eval must not stack a second one.
func TestTermSubmitWhileRunningIsBlocked(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)
	a.term.running = true

	typeTermLine(a, "echo nope")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	f.mu.Lock()
	n := len(f.evals)
	f.mu.Unlock()
	if n != 0 {
		t.Errorf("busy panel evaluated anyway: %d call(s)", n)
	}
}

// TestTermContinuation pins the NeedsMore loop: incomplete units
// accumulate (with the … prompt) and evaluate as one chunk when the
// classifier is satisfied — blocks span prompts like the real REPL.
func TestTermContinuation(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)
	f.needsMore = func(src string) bool { return !strings.Contains(src, "}") }

	typeTermLine(a, "if true {")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	if a.term.running {
		t.Fatal("incomplete unit must not evaluate")
	}
	if len(a.term.pending) != 1 {
		t.Fatalf("pending = %d lines, want 1", len(a.term.pending))
	}
	if got := a.termPrompt(); got != "… " {
		t.Errorf("continuation prompt = %q, want %q", got, "… ")
	}

	typeTermLine(a, "}")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	evals := f.waitEvals(t, 1)
	if want := "if true {\n}"; evals[0] != want {
		t.Errorf("Eval got %q, want %q", evals[0], want)
	}
	if a.term.pending != nil {
		t.Error("pending should clear once the unit evaluates")
	}
}

// TestTermOutputWriterCoalesces verifies the writer buffers chunks and
// the drain path lands them in the scrollback with line splitting.
func TestTermOutputWriterCoalesces(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)

	a.term.writer.Write([]byte("alpha\nbra"))
	a.term.writer.Write([]byte("vo\n"))
	a.handleTermOutput()

	if len(a.term.lines) != 2 || a.term.lines[0].text != "alpha" || a.term.lines[1].text != "bravo" {
		t.Fatalf("scrollback = %+v, want alpha/bravo", a.term.lines)
	}
	if a.term.partial != "" {
		t.Errorf("partial = %q, want empty", a.term.partial)
	}
}

// TestTermPartialLine pins the unterminated-tail behavior: output
// without a newline stays in partial (rendered live) until completed.
func TestTermPartialLine(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)

	a.termAppendOutput("prompt? ")
	if a.term.partial != "prompt? " || len(a.term.lines) != 0 {
		t.Fatalf("partial = %q lines = %d, want tail held back", a.term.partial, len(a.term.lines))
	}
	a.termAppendOutput("yes\n")
	if len(a.term.lines) != 1 || a.term.lines[0].text != "prompt? yes" {
		t.Fatalf("lines = %+v, want one joined line", a.term.lines)
	}
}

// TestStripTermANSI covers the three escape families child output uses:
// SGR color (CSI), window-title OSC, and two-byte escapes.
func TestStripTermANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;38;5;196mbold\x1b[m!", "bold!"},
		{"\x1b]0;title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link", "link"},
		{"a\x1bcb", "ab"},
		{"cut\x1b", "cut"},
	}
	for _, c := range cases {
		if got := stripTermANSI(c.in); got != c.want {
			t.Errorf("stripTermANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTermCarriageReturnOverwrite pins progress-bar semantics: a bare
// \r rewinds the in-progress line instead of committing it.
func TestTermCarriageReturnOverwrite(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.termAppendOutput("50%\r100%\n")
	if len(a.term.lines) != 1 || a.term.lines[0].text != "100%" {
		t.Fatalf("lines = %+v, want single %q", a.term.lines, "100%")
	}
}

// TestTermTabExpansion checks tabs land on termTabStop columns — Go
// tool output aligns with 8-column tabs.
func TestTermTabExpansion(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.termAppendOutput("ok\tdone\n")
	if got := a.term.lines[0].text; got != "ok      done" {
		t.Errorf("expanded = %q, want %q", got, "ok      done")
	}
}

// TestTermHistory walks Up/Down navigation including the draft stash:
// the half-typed line survives a trip into history and back.
func TestTermHistory(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)

	for _, cmd := range []string{"first", "second"} {
		typeTermLine(a, cmd)
		a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
		f.waitEvals(t, 1)
		a.handleTermDone(&termDoneEvent{when: time.Now()})
		f.mu.Lock()
		f.evals = nil
		f.mu.Unlock()
	}

	typeTermLine(a, "dra")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyUp, 0, 0))
	if got := a.term.input.String(); got != "second" {
		t.Fatalf("Up = %q, want %q", got, "second")
	}
	a.handleTermKey(tcell.NewEventKey(tcell.KeyUp, 0, 0))
	if got := a.term.input.String(); got != "first" {
		t.Fatalf("Up Up = %q, want %q", got, "first")
	}
	a.handleTermKey(tcell.NewEventKey(tcell.KeyDown, 0, 0))
	a.handleTermKey(tcell.NewEventKey(tcell.KeyDown, 0, 0))
	if got := a.term.input.String(); got != "dra" {
		t.Fatalf("back down = %q, want stashed draft %q", got, "dra")
	}
}

// TestTermScrollFollowsTail pins the yank rule: a view at the bottom
// follows new output; a view scrolled up to read stays put.
func TestTermScrollFollowsTail(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)

	for i := 0; i < 60; i++ {
		a.termAppendOutput("line\n")
	}
	if a.term.scroll != a.termMaxScroll() {
		t.Fatalf("scroll = %d, want pinned to max %d", a.term.scroll, a.termMaxScroll())
	}

	a.termPanelScroll(-5)
	held := a.term.scroll
	a.termAppendOutput("more\n")
	if a.term.scroll != held {
		t.Errorf("scroll moved %d → %d; reading position must not be yanked", held, a.term.scroll)
	}
}

// TestTermPanelGeometry checks the editor gives up exactly the panel's
// height and the hit-test agrees with the rectangle.
func TestTermPanelGeometry(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	_, _, _, editorBefore := a.editorRect()
	openTestTerm(t, a)

	_, _, _, editorAfter := a.editorRect()
	if want := editorBefore - a.termPanelHeight(); editorAfter != want {
		t.Errorf("editor h = %d, want %d", editorAfter, want)
	}
	px, py, pw, ph := a.termPanelRect()
	if py+ph != a.height-1 {
		t.Errorf("panel bottom = %d, want flush against status bar row %d", py+ph, a.height-1)
	}
	if !a.termPanelContains(px, py) || !a.termPanelContains(px+pw-1, py+ph-1) {
		t.Error("contains() disagrees with the panel corners")
	}
	if a.termPanelContains(px, py-1) || a.termPanelContains(px+pw, py) {
		t.Error("contains() claims cells outside the panel")
	}
}

// TestTermResizeClamp pins the resize band: never shorter than the
// minimum, never tall enough to squeeze the editor below its floor.
func TestTermResizeClamp(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)

	a.resizeTermPanel(1)
	if got := a.termPanelHeight(); got != termPanelMinHeight {
		t.Errorf("height after tiny resize = %d, want floor %d", got, termPanelMinHeight)
	}
	a.resizeTermPanel(1000)
	if got, max := a.termPanelHeight(), a.maxTermPanelHeight(); got != max {
		t.Errorf("height after huge resize = %d, want ceiling %d", got, max)
	}
}

// TestTermExitClosesPanel pins the `exit` contract: the panel closes
// and the session is discarded so the next open starts fresh.
func TestTermExitClosesPanel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	prev := termExitCode
	termExitCode = func(error) (int, bool) { return 3, true }
	t.Cleanup(func() { termExitCode = prev })

	a.handleTermDone(&termDoneEvent{when: time.Now(), err: errors.New("exit 3")})
	if a.term.open || a.term.focused {
		t.Error("exit should close the panel")
	}
	if a.term.sess != nil {
		t.Error("exit should discard the session")
	}
}

// TestTermEvalErrorSurfaces checks a failed Eval lands in the
// scrollback as an error-styled line rather than vanishing.
func TestTermEvalErrorSurfaces(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.handleTermDone(&termDoneEvent{when: time.Now(), err: errors.New("boom")})
	var found bool
	for _, ln := range a.term.lines {
		if ln.kind == termErr && strings.Contains(ln.text, "boom") {
			found = true
		}
	}
	if !found {
		t.Error("eval error should append a termErr scrollback line")
	}
}

// TestTermInterruptEscalation pins the ⏹ ladder: SIGINT first, SIGKILL
// on the second press, and the done event resets the ladder.
func TestTermInterruptEscalation(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)
	f.interruptOK = true
	a.term.running = true

	a.termInterrupt()
	if f.interrupts != 1 || f.kills != 0 || !a.term.interrupted {
		t.Fatalf("first press: interrupts=%d kills=%d flag=%v", f.interrupts, f.kills, a.term.interrupted)
	}
	a.termInterrupt()
	if f.kills != 1 {
		t.Fatalf("second press should Kill, kills=%d", f.kills)
	}
	a.handleTermDone(&termDoneEvent{when: time.Now()})
	if a.term.interrupted {
		t.Error("done should reset the escalation ladder")
	}
}

// TestTermUnfocusOnEditorClick pins the mouse-first focus model: a
// press outside the panel returns the keyboard to the editor.
func TestTermUnfocusOnEditorClick(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	if !a.term.focused {
		t.Fatal("panel should open focused")
	}
	// Press in the editor body (below the tab bar, right of the
	// sidebar, above the panel), then release.
	a.handleMouse(tcell.NewEventMouse(a.sidebarW()+5, 3, tcell.Button1, 0))
	a.handleMouse(tcell.NewEventMouse(a.sidebarW()+5, 3, tcell.ButtonNone, 0))
	if a.term.focused {
		t.Error("editor click should unfocus the terminal")
	}
	if !a.term.open {
		t.Error("editor click should not close the panel")
	}
}

// TestTermPromptStates walks the prompt gutter through its four faces.
func TestTermPromptStates(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)

	if got := a.termPrompt(); got != "❯ " {
		t.Errorf("idle prompt = %q", got)
	}
	f.lastStatus = 2
	if got := a.termPrompt(); got != "[2] ❯ " {
		t.Errorf("status prompt = %q", got)
	}
	f.lastStatus = 0
	a.term.pending = []string{"if x {"}
	if got := a.termPrompt(); got != "… " {
		t.Errorf("continuation prompt = %q", got)
	}
	a.term.running = true
	if got := a.termPrompt(); got != "⋯ " {
		t.Errorf("running prompt = %q", got)
	}
}

// TestTermPasteClip checks Cmd+V routes the text clipboard into the
// input line, dropping newlines (the input is single-line).
func TestTermPasteClip(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.clipBuf = "ls -la\n/tmp"
	a.termPasteClip()
	if got := a.term.input.String(); got != "ls -la/tmp" {
		t.Errorf("pasted input = %q, want control runes dropped", got)
	}
}

// TestTermNotificationsDrain checks finished-background-job lines land
// in the scrollback after an Eval completes.
func TestTermNotificationsDrain(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	f := openTestTerm(t, a)
	f.notes = []string{"[1]  Done       sleep 1 &"}
	a.handleTermDone(&termDoneEvent{when: time.Now()})
	var found bool
	for _, ln := range a.term.lines {
		if strings.Contains(ln.text, "Done") {
			found = true
		}
	}
	if !found {
		t.Error("job notification should append to the scrollback")
	}
}

// TestTermScrollbackTrim bounds the buffer: past termScrollbackMax the
// oldest lines fall off and the count holds steady.
func TestTermScrollbackTrim(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	for i := 0; i < termScrollbackMax+50; i++ {
		a.term.lines = append(a.term.lines, termLine{text: "x"})
	}
	a.termTrimScrollback()
	if len(a.term.lines) != termScrollbackMax {
		t.Errorf("scrollback = %d lines, want trimmed to %d", len(a.term.lines), termScrollbackMax)
	}
}

// TestAbbrevHomePath pins the ~ abbreviation used by the header cwd.
func TestAbbrevHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory in this environment")
	}
	if got := abbrevHomePath(home); got != "~" {
		t.Errorf("home = %q, want ~", got)
	}
	sub := filepath.Join(home, "projs")
	if got := abbrevHomePath(sub); got != "~"+string(filepath.Separator)+"projs" {
		t.Errorf("sub = %q", got)
	}
	if got := abbrevHomePath("/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("outside home = %q, want unchanged", got)
	}
}

// TestTermMenuRowAndLabel checks the ≡ row exists in the View group
// and its label tracks panel state.
func TestTermMenuRowAndLabel(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if got := a.termToggleLabel(); got != "Show terminal" {
		t.Errorf("closed label = %q", got)
	}
	a.menuToggleTerminal()
	if got := a.termToggleLabel(); got != "Hide terminal" {
		t.Errorf("open label = %q", got)
	}
	items, _, _ := a.menuLayout()
	var found bool
	for _, it := range items {
		if it.labelFor != nil && it.labelFor(a) == "Hide terminal" {
			found = true
		}
	}
	if !found {
		t.Error("terminal toggle row missing from the ≡ menu")
	}
}

// TestDrawTermPanelSmoke draws the open panel on a simulation screen
// and asserts the header title and prompt actually rendered.
func TestDrawTermPanelSmoke(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTestTerm(t, a)
	a.termAppendOutput("hello from grsh\n")
	a.draw()
	a.screen.Show() // flush to the simulation buffer GetContents reads

	if !screenContainsText(a.screen, "Terminal") {
		t.Error("header title not rendered")
	}
	if !screenContainsText(a.screen, "hello from grsh") {
		t.Error("scrollback line not rendered")
	}
	if !screenContainsText(a.screen, "❯") {
		t.Error("prompt glyph not rendered")
	}
}

// TestTermRealGrshIntegration swaps the real grsh session back in and
// runs one harmless command end-to-end: submit → embedded Eval →
// coalescing writer. This is the only test allowed to execute a real
// command (`echo` — no filesystem or network effects) so a grsh upgrade
// that breaks the embedding contract fails here, not in the user's lap.
func TestTermRealGrshIntegration(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	newTermEvaluator = func(out io.Writer) termEvaluator {
		return grsh.NewSession(grsh.Options{Stdout: out, Stderr: out})
	}
	a.menuToggleTerminal()

	typeTermLine(a, "echo integration-ok")
	a.handleTermKey(tcell.NewEventKey(tcell.KeyEnter, 0, 0))

	// The Eval goroutine streams into the writer; poll its buffer
	// directly instead of running the event loop.
	deadline := time.Now().Add(5 * time.Second)
	var got strings.Builder
	for time.Now().Before(deadline) {
		got.WriteString(a.term.writer.drain())
		if strings.Contains(got.String(), "integration-ok") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("real grsh output never arrived; captured %q", got.String())
}

// screenContainsText scans a simulation screen row-wise for a substring.
func screenContainsText(scr tcell.Screen, want string) bool {
	sim, ok := scr.(tcell.SimulationScreen)
	if !ok {
		return false
	}
	cells, w, h := sim.GetContents()
	for row := 0; row < h; row++ {
		var b strings.Builder
		for col := 0; col < w; col++ {
			c := cells[row*w+col]
			if len(c.Runes) > 0 {
				b.WriteRune(c.Runes[0])
			}
		}
		if strings.Contains(b.String(), want) {
			return true
		}
	}
	return false
}
