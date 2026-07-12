// =============================================================================
// File: internal/app/terminal.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// terminal.go implements the embedded terminal panel — a grsh session
// (github.com/rohanthewiz/grsh: bash-style shell + Go expressions in one
// language) hosted in a strip above the status bar. It is a REPL panel,
// not a PTY: grsh is compiled in and streams plain text, so the panel
// works identically over SSH+tmux and never fights tcell for the tty.
// Full-screen child programs (vim, htop) are out of scope by design —
// r-ed users already live in a terminal and can split a pane for those.
//
// House patterns in play:
//   - NOT a modal: the panel owns part of the layout like the git panel,
//     but unlike the git panel it needs the keyboard, so it carries a
//     `focused` flag — clicks inside focus it, clicks anywhere else give
//     the keyboard back to the editor. Esc stays global (leaders and the
//     double-Esc menu work from inside the terminal).
//   - Single-occupancy bottom strip: the terminal and the git panel
//     swap, never stack. Two resizable strips would need circular
//     height-clamp math on small windows; exclusivity keeps both
//     panels' invariants independent, and Esc-g / Esc-` flip between
//     them instantly.
//   - Custom tcell events: grsh runs each Eval on a goroutine; output
//     arrives via termOutputEvent (through a coalescing writer so heavy
//     output can't flood the event queue) and completion via
//     termDoneEvent. Only the main loop mutates panel state.
//   - The stop button replaces Ctrl+C (no Ctrl shortcuts, ever): ⏹
//     sends SIGINT to the foreground process group; a second click
//     escalates to SIGKILL.

package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/grsh"

	"github.com/rohanthewiz/r-ed/internal/userconfig"
)

const (
	// termPanelMinHeight keeps the panel usable (header + a few
	// scrollback rows + the input row); termPanelMaxHeight caps auto
	// mode the same way the git panel does — the editor stays the main
	// surface.
	termPanelMinHeight = 5
	termPanelMaxHeight = 18

	// termPanelMinEditorRows is how much editor a user-driven resize
	// must leave standing — same contract as gitPanelMinEditorRows.
	termPanelMinEditorRows = 5

	// termPanelResizeStep is the Esc-= / Esc-- growth per press while
	// the terminal (rather than the git panel) owns the bottom strip.
	// In the left-dock layout the same step applies to columns.
	termPanelResizeStep = 2

	// termPanelMinWidth keeps a left-docked strip usable — enough
	// columns for a prompt plus a short command. There is no max
	// constant: the ceiling is derived live from the window so the
	// editor always keeps minEditorAfterDrag columns (see
	// maxTermPanelWidth).
	termPanelMinWidth = 24

	// termScrollbackMax bounds the in-memory scrollback. Beyond it the
	// oldest lines fall off — a build spewing megabytes must not grow
	// the editor's heap without limit.
	termScrollbackMax = 5000

	// termTabStop is the column multiple output tabs expand to. Go
	// tooling output leans on 8-column tabs; rendering literal \t via
	// tcell would show a single odd cell instead of alignment.
	termTabStop = 8
)

// termEvaluator is the seam between the panel and grsh, mirroring the
// lsp fakeLSPConn trick: production code always talks to a real
// *grsh.Session, tests inject a fake so no command ever executes.
type termEvaluator interface {
	Eval(src string) error
	NeedsMore(src string) bool
	Interrupt() bool
	Kill() bool
	LastStatus() int
	Cwd() string
	Notifications() []string
}

// newTermEvaluator builds the production grsh session. A package var —
// not a method — so newTestApp can stub it exactly like
// builtinCommandFor: tests must never spawn a session that can run real
// commands on the dev machine.
var newTermEvaluator = func(out io.Writer) termEvaluator {
	return grsh.NewSession(grsh.Options{Stdout: out, Stderr: out})
}

// termExitCode unwraps grsh's exit request. Indirected for the same
// reason as newTermEvaluator: the fake evaluator can't fabricate grsh's
// internal exit error, so tests stub this to simulate `exit`.
var termExitCode = grsh.ExitCode

// termLineKind selects the style a scrollback line renders with.
type termLineKind int

const (
	termOut termLineKind = iota // child stdout/stderr
	termCmd                     // echoed prompt + command
	termErr                     // eval error (grsh.UserMessage)
)

// termLine is one committed scrollback row.
type termLine struct {
	text string
	kind termLineKind
}

// termPanelState is the panel's whole state, mutated only on the main
// loop. Scrollback, history, and the session itself survive a collapse —
// reopening puts the user back in the same shell.
type termPanelState struct {
	open    bool
	focused bool // keyboard routes to the input line while true

	// height is the user-chosen row count from a header drag or the
	// resize leaders; 0 means "auto" (a third of the screen). Session-
	// only, like the git panel and sidebarWidth. Applies while the
	// panel is bottom-docked.
	height int

	// width is the left-dock twin of height: user-chosen column count
	// from a splitter drag or the resize leaders, 0 for "auto" (a
	// third of the screen width). Session-only.
	width int

	sess   termEvaluator
	writer *termWriter

	lines   []termLine
	partial string // output tail not yet terminated by \n
	scroll  int    // first visible scrollback row

	input     textField
	history   []string
	histIdx   int    // == len(history) while editing a fresh line
	histDraft string // in-progress input stashed while browsing history

	// pending accumulates continuation lines while grsh reports the
	// unit incomplete (unclosed block, trailing pipe) — same
	// NeedsMore loop the standalone REPL runs.
	pending []string

	running     bool // an Eval is in flight
	interrupted bool // ⏹ already clicked once; next click escalates to Kill
}

// -----------------------------------------------------------------------------
// Output plumbing — goroutine side
// -----------------------------------------------------------------------------

// termOutputEvent pokes the main loop to drain the coalescing writer.
// It carries no data on purpose: chunks stay in the writer's buffer so
// any number of writes cost at most one queued event.
type termOutputEvent struct{ when time.Time }

// When satisfies the tcell.Event interface.
func (e *termOutputEvent) When() time.Time { return e.when }

// termDoneEvent reports one Eval's completion (err nil on success).
type termDoneEvent struct {
	when time.Time
	err  error
}

// When satisfies the tcell.Event interface.
func (e *termDoneEvent) When() time.Time { return e.when }

// termWriter is the io.Writer grsh streams into. Writes land in a
// mutex-guarded buffer; at most one termOutputEvent is in flight at a
// time (posted=true), so a command printing thousands of small chunks
// coalesces instead of overflowing tcell's bounded event queue.
type termWriter struct {
	scr    tcell.Screen
	mu     sync.Mutex
	buf    []byte
	posted bool
}

// Write appends p and posts the drain event if none is pending. Safe
// for the copier goroutines os/exec runs — that's the grsh embedding
// contract's "writers must be goroutine-safe" clause.
func (w *termWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	shouldPost := !w.posted
	w.posted = true
	w.mu.Unlock()
	if shouldPost {
		if err := w.scr.PostEvent(&termOutputEvent{when: time.Now()}); err != nil {
			// Queue full: clear the flag so a later write retries the
			// post — otherwise the tail of the output is stranded in
			// the buffer until the next command runs.
			w.mu.Lock()
			w.posted = false
			w.mu.Unlock()
		}
	}
	return len(p), nil
}

// drain returns everything buffered since the last drain and re-arms
// posting. Called only from the main loop's termOutputEvent handler.
func (w *termWriter) drain() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := string(w.buf)
	w.buf = w.buf[:0]
	w.posted = false
	return s
}

// -----------------------------------------------------------------------------
// Toggle + session lifecycle
// -----------------------------------------------------------------------------

// menuToggleTerminal is the ≡ menu entry point: a strict open/close
// toggle (the Esc-` leader adds a focus-first nicety on top). Opening
// claims the bottom strip from the git panel and focuses the input.
func (a *App) menuToggleTerminal() {
	a.closeMenu()
	a.term.open = !a.term.open
	a.term.focused = a.term.open
	if a.term.open {
		// Single-occupancy bottom strip — only applies while the
		// terminal actually competes for the bottom; a left-docked
		// strip coexists with the git panel.
		if !a.termDockLeft {
			a.gitPanel.open = false
		}
		a.ensureTermSession()
	}
}

// menuToggleTermDock flips the terminal between the classic bottom
// strip and the left-docked vertical strip (which also sends the file
// tree to the right edge), persisting the choice like the auto-save
// toggle does. Re-entering the bottom layout restores the strip's
// single-occupancy rule, so an open git panel yields.
func (a *App) menuToggleTermDock() {
	a.closeMenu()
	a.termDockLeft = !a.termDockLeft
	if !a.termDockLeft && a.term.open {
		a.gitPanel.open = false
	}
	if a.termDockLeft {
		a.flash("Terminal docks left · file tree on the right")
	} else {
		a.flash("Terminal docks at the bottom · file tree on the left")
	}
	dock := userconfig.TermDockBottom
	if a.termDockLeft {
		dock = userconfig.TermDockLeft
	}
	if err := userconfig.SaveTermDock(userconfig.DefaultPath(), dock); err != nil {
		a.flash("config: " + err.Error())
	}
}

// termDockToggleLabel names the layout the toggle will switch TO —
// the same action-not-state convention as the other toggle rows.
func (a *App) termDockToggleLabel() string {
	if a.termDockLeft {
		return "Dock terminal at bottom"
	}
	return "Dock terminal left (tree right)"
}

// leaderTerminal is the Esc-` binding: an open-but-unfocused panel
// grabs focus first, so the leader doubles as "jump to terminal";
// otherwise it toggles like the menu row.
func (a *App) leaderTerminal() {
	if a.term.open && !a.term.focused {
		a.term.focused = true
		return
	}
	a.menuToggleTerminal()
}

// termToggleLabel is the dynamic ≡ menu label, mirroring the sidebar
// and git panel toggles.
func (a *App) termToggleLabel() string {
	if a.term.open {
		return "Hide terminal"
	}
	return "Show terminal"
}

// ensureTermSession lazily builds the grsh session on first open. The
// session persists across hide/show — variables and cwd survive, same
// as a real shell — and is only torn down by an explicit `exit`.
func (a *App) ensureTermSession() {
	if a.term.sess != nil {
		return
	}
	w := &termWriter{scr: a.screen}
	a.term.writer = w
	a.term.sess = newTermEvaluator(w)
	a.term.histIdx = 0
}

// -----------------------------------------------------------------------------
// Command submission + async results
// -----------------------------------------------------------------------------

// submitTermCommand runs the input line: echo it to the scrollback,
// fold it into any pending continuation, and either wait for more input
// (grsh says the unit is incomplete) or launch the Eval goroutine.
func (a *App) submitTermCommand() {
	if a.term.sess == nil {
		return
	}
	if a.term.running {
		a.flash("terminal is busy — ⏹ to stop the running command")
		return
	}
	input := a.term.input.String()
	a.termAppendLine(termLine{text: a.termPrompt() + input, kind: termCmd})
	a.term.input = newTextField("")
	if strings.TrimSpace(input) != "" {
		a.term.history = append(a.term.history, input)
	}
	a.term.histIdx = len(a.term.history)
	a.term.histDraft = ""

	a.term.pending = append(a.term.pending, input)
	src := strings.Join(a.term.pending, "\n")
	if strings.TrimSpace(src) == "" {
		a.term.pending = nil
		return
	}
	if a.term.sess.NeedsMore(src) {
		return // keep accumulating; prompt switches to the … gutter
	}
	a.term.pending = nil
	a.term.running = true
	a.term.interrupted = false

	// Snapshot for the goroutine: the session is safe to Eval off-loop
	// (that's its embedding contract) but App state is not, so the
	// closure touches nothing else and reports back via the event.
	sess, scr := a.term.sess, a.screen
	go func() {
		err := sess.Eval(src)
		_ = scr.PostEvent(&termDoneEvent{when: time.Now(), err: err})
	}()
}

// handleTermOutput drains the coalescing writer into the scrollback.
// Runs on the main loop only.
func (a *App) handleTermOutput() {
	if a.term.writer == nil {
		return
	}
	a.termAppendOutput(a.term.writer.drain())
}

// handleTermDone finishes one Eval: surface the error (or honor an
// `exit`), drain background-job notifications, and re-sync the
// workspace — shell commands create and modify files, and the tree
// shouldn't wait for the 10s tick to notice.
func (a *App) handleTermDone(e *termDoneEvent) {
	a.term.running = false
	a.term.interrupted = false
	if code, ok := termExitCode(e.err); ok {
		// `exit` ends the session, not the editor: close the panel and
		// drop the session so the next open starts a fresh shell.
		a.term.open = false
		a.term.focused = false
		a.term.sess = nil
		a.term.lines = nil
		a.term.partial = ""
		a.term.pending = nil
		a.flash("terminal session ended (exit " + itoa(code) + ")")
		return
	}
	if e.err != nil {
		a.termAppendLine(termLine{text: grsh.UserMessage(e.err), kind: termErr})
	}
	a.termDrainNotifications()
	a.refreshTreeNow()
}

// termDrainNotifications appends finished background-job lines
// ("[1]  Done  cmd &"). Called after each Eval and from the 10s tick so
// a job finishing while the shell is idle still gets announced.
func (a *App) termDrainNotifications() {
	if a.term.sess == nil {
		return
	}
	for _, note := range a.term.sess.Notifications() {
		a.termAppendLine(termLine{text: note, kind: termOut})
	}
}

// termInterrupt is the ⏹ button: SIGINT first, SIGKILL on the second
// press — the mouse-first stand-in for Ctrl+C / kill -9.
func (a *App) termInterrupt() {
	if a.term.sess == nil || !a.term.running {
		return
	}
	if a.term.interrupted {
		if a.term.sess.Kill() {
			a.flash("kill sent")
		}
		return
	}
	if a.term.sess.Interrupt() {
		a.term.interrupted = true
		a.flash("interrupt sent — ⏹ again to force kill")
	} else {
		// Builtins and pure-Go evaluation have no process to signal;
		// say so instead of silently doing nothing.
		a.flash("nothing to interrupt (builtin or Go code running)")
	}
}

// -----------------------------------------------------------------------------
// Scrollback
// -----------------------------------------------------------------------------

// termAppendOutput folds a raw output chunk into the scrollback:
// ANSI escapes stripped (Chroma owns color in this editor; child SGR
// would fight the theme), \r\n and \n commit lines, a bare \r rewinds
// the partial line (progress-bar semantics), and tabs expand to
// termTabStop columns.
func (a *App) termAppendOutput(chunk string) {
	if chunk == "" {
		return
	}
	atBottom := a.termAtBottom()
	for _, r := range stripTermANSI(chunk) {
		switch r {
		case '\n':
			a.term.lines = append(a.term.lines, termLine{text: a.term.partial, kind: termOut})
			a.term.partial = ""
		case '\r':
			a.term.partial = ""
		case '\t':
			pad := termTabStop - runeLen(a.term.partial)%termTabStop
			a.term.partial += strings.Repeat(" ", pad)
		default:
			a.term.partial += string(r)
		}
	}
	a.termTrimScrollback()
	if atBottom {
		a.term.scroll = a.termMaxScroll()
	}
}

// termAppendLine commits one already-formed line (command echo, error,
// job note), flushing any partial output first so ordering is honest.
func (a *App) termAppendLine(ln termLine) {
	atBottom := a.termAtBottom()
	if a.term.partial != "" {
		a.term.lines = append(a.term.lines, termLine{text: a.term.partial, kind: termOut})
		a.term.partial = ""
	}
	a.term.lines = append(a.term.lines, ln)
	a.termTrimScrollback()
	if atBottom {
		a.term.scroll = a.termMaxScroll()
	}
}

// termTrimScrollback drops the oldest lines past termScrollbackMax,
// shifting the scroll anchor so the view doesn't visually jump.
func (a *App) termTrimScrollback() {
	if over := len(a.term.lines) - termScrollbackMax; over > 0 {
		a.term.lines = a.term.lines[over:]
		a.term.scroll -= over
		if a.term.scroll < 0 {
			a.term.scroll = 0
		}
	}
}

// termVisibleRows is how many scrollback rows the panel shows — the
// panel minus the header rule and the input row.
func (a *App) termVisibleRows() int {
	_, _, _, ph := a.termPanelRect()
	if v := ph - 2; v > 0 {
		return v
	}
	return 1
}

// termContentRows counts renderable rows: committed lines plus the
// in-progress partial line.
func (a *App) termContentRows() int {
	n := len(a.term.lines)
	if a.term.partial != "" {
		n++
	}
	return n
}

// termMaxScroll is the scroll offset that pins the newest row to the
// bottom of the viewport. Hard clamp, no overscroll — a shell reads
// bottom-up and void rows below the last output would look like lag.
func (a *App) termMaxScroll() int {
	if max := a.termContentRows() - a.termVisibleRows(); max > 0 {
		return max
	}
	return 0
}

// termAtBottom reports whether the view is pinned to the newest output.
// Sampled BEFORE appending so new output follows the tail only when the
// user was already there — wheel-scrolling up to read never gets yanked
// back down (the cursorMoved rule, applied to a shell).
func (a *App) termAtBottom() bool {
	return a.term.scroll >= a.termMaxScroll()
}

// termPanelScroll wheels the scrollback by delta rows, hard-clamped.
func (a *App) termPanelScroll(delta int) {
	a.term.scroll += delta
	if max := a.termMaxScroll(); a.term.scroll > max {
		a.term.scroll = max
	}
	if a.term.scroll < 0 {
		a.term.scroll = 0
	}
}

// stripTermANSI removes ANSI escape sequences from child output: CSI
// (ESC [ … final byte in @–~), OSC (ESC ] … BEL or ESC \), and two-byte
// ESC+char forms. The panel renders through the editor theme, so child
// styling is noise — and unstripped CSI bytes would render as garbage
// cells.
func stripTermANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != 0x1b {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			break
		}
		switch s[i+1] {
		case '[': // CSI: parameters/intermediates end at a byte in @–~
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j // loop's i++ steps past the final byte
		case ']': // OSC: runs to BEL or ESC \
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++
			}
			i = j
		default: // two-byte escape (ESC c, ESC =, …)
			i++
		}
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// History
// -----------------------------------------------------------------------------

// termHistoryMove steps through past commands with Up/Down. The
// in-progress line is stashed on the way up and restored when the user
// walks back below the newest entry — readline's draft behavior.
func (a *App) termHistoryMove(delta int) {
	h := &a.term
	if len(h.history) == 0 {
		return
	}
	idx := h.histIdx + delta
	if idx < 0 || idx > len(h.history) {
		return
	}
	if h.histIdx == len(h.history) {
		h.histDraft = h.input.String()
	}
	h.histIdx = idx
	if idx == len(h.history) {
		h.input = newTextField(h.histDraft)
		return
	}
	h.input = newTextField(h.history[idx])
}

// -----------------------------------------------------------------------------
// Geometry — one source for draw AND mouse routing
// -----------------------------------------------------------------------------

// termPanelHeight returns the panel's row count for the current window:
// user height wins, auto mode takes a third of the screen, both
// re-clamped live so a terminal resize can't squeeze the editor out.
// Same shape as gitPanelHeight — the two panels are peers.
func (a *App) termPanelHeight() int {
	h := a.term.height
	if h == 0 {
		h = a.height / 3
		if h > termPanelMaxHeight {
			h = termPanelMaxHeight
		}
	}
	if h < termPanelMinHeight {
		h = termPanelMinHeight
	}
	if max := a.maxTermPanelHeight(); h > max {
		h = max
	}
	return h
}

// maxTermPanelHeight is the tallest the panel may grow while the editor
// keeps its minimum working rows.
func (a *App) maxTermPanelHeight() int {
	max := a.height - 2 - termPanelMinEditorRows
	if a.findOpen {
		max -= findBarHeight
	}
	if max < termPanelMinHeight {
		max = termPanelMinHeight
	}
	return max
}

// resizeTermPanel records a user-chosen height, clamped to the legal
// band, and re-clamps the scroll against the new viewport.
func (a *App) resizeTermPanel(target int) {
	if target < termPanelMinHeight {
		target = termPanelMinHeight
	}
	if max := a.maxTermPanelHeight(); target > max {
		target = max
	}
	atBottom := a.termAtBottom()
	a.term.height = target
	a.termPanelScroll(0) // re-clamp
	if atBottom {
		a.term.scroll = a.termMaxScroll()
	}
}

// dragTermPanelTo resizes so the header rule tracks the mouse row —
// the same glued-to-the-cursor feel as the other splitters.
func (a *App) dragTermPanelTo(y int) {
	bottom := a.height - 1
	if a.findOpen {
		bottom -= findBarHeight
	}
	a.resizeTermPanel(bottom - y)
}

// growTermPanel / shrinkTermPanel let Esc-= / Esc-- resize whichever
// bottom panel is open (the git panel handlers no-op when it's the
// terminal's turn, and vice versa). A left-docked strip grows in
// columns instead of rows — the leader keeps meaning "more terminal".
func (a *App) growTermPanel() {
	if !a.term.open {
		return
	}
	if a.termDockLeft {
		a.resizeTermPanelWidth(a.termPanelWidth() + termPanelResizeStep)
		return
	}
	a.resizeTermPanel(a.termPanelHeight() + termPanelResizeStep)
}

// shrinkTermPanel steps the panel shorter / narrower; see growTermPanel.
func (a *App) shrinkTermPanel() {
	if !a.term.open {
		return
	}
	if a.termDockLeft {
		a.resizeTermPanelWidth(a.termPanelWidth() - termPanelResizeStep)
		return
	}
	a.resizeTermPanel(a.termPanelHeight() - termPanelResizeStep)
}

// growBottomPanel / shrinkBottomPanel are the Esc-= / Esc-- targets:
// each panel's grow/shrink no-ops unless it is the one open, and the
// bottom strip is single-occupancy, so exactly one (or neither) acts.
func (a *App) growBottomPanel() {
	a.growGitPanel()
	a.growTermPanel()
}

// shrinkBottomPanel steps the open bottom panel shorter; see
// growBottomPanel.
func (a *App) shrinkBottomPanel() {
	a.shrinkGitPanel()
	a.shrinkTermPanel()
}

// termStripW is the total column count a left-docked terminal strip
// consumes (panel + its splitter column): zero when the panel is
// closed or bottom-docked. The layout helpers pivot on this the way
// they pivot on sidebarW.
func (a *App) termStripW() int {
	if !a.termDockLeft || !a.term.open {
		return 0
	}
	return a.termPanelWidth()
}

// termSplitterX returns the left-docked strip's resize handle column
// (its rightmost cell), or -1 when there is no vertical strip to
// resize — mirroring splitterX's contract for the sidebar.
func (a *App) termSplitterX() int {
	if sw := a.termStripW(); sw > 0 {
		return sw - 1
	}
	return -1
}

// termPanelWidth returns the left-docked strip's column count for the
// current window: user width wins, auto mode takes a third of the
// screen, both re-clamped live so a terminal resize can't squeeze the
// editor out. The width twin of termPanelHeight.
func (a *App) termPanelWidth() int {
	w := a.term.width
	if w == 0 {
		w = a.width / 3
	}
	if w < termPanelMinWidth {
		w = termPanelMinWidth
	}
	if max := a.maxTermPanelWidth(); w > max {
		w = max
	}
	return w
}

// maxTermPanelWidth is the widest the left-docked strip may grow while
// the editor keeps its minimum working columns next to the sidebar.
func (a *App) maxTermPanelWidth() int {
	max := a.width - a.sidebarW() - minEditorAfterDrag
	if max < termPanelMinWidth {
		max = termPanelMinWidth
	}
	return max
}

// resizeTermPanelWidth records a user-chosen strip width, clamped to
// the legal band — the splitter-drag twin of resizeTermPanel.
func (a *App) resizeTermPanelWidth(target int) {
	if target < termPanelMinWidth {
		target = termPanelMinWidth
	}
	if max := a.maxTermPanelWidth(); target > max {
		target = max
	}
	atBottom := a.termAtBottom()
	a.term.width = target
	a.termPanelScroll(0) // re-clamp against the (unchanged) row count
	if atBottom {
		a.term.scroll = a.termMaxScroll()
	}
}

// termPanelRect returns the panel's on-screen rectangle. Bottom dock:
// editor-width, directly above the find bar (when open) and the status
// bar. Left dock: a full-height strip on the left edge, one column
// narrower than the strip — the rightmost column belongs to the
// splitter, same convention as sidebarRect.
func (a *App) termPanelRect() (x, y, w, h int) {
	if a.termDockLeft {
		return 0, 0, a.termStripW() - 1, a.height - 1
	}
	sw := a.leftBlockW()
	h = a.termPanelHeight()
	y = a.height - 1 - h
	if a.findOpen {
		y -= findBarHeight
	}
	return sw, y, a.width - sw - a.rightBlockW(), h
}

// termPanelContains reports whether (x, y) falls inside the open panel.
func (a *App) termPanelContains(x, y int) bool {
	px, py, pw, ph := a.termPanelRect()
	return x >= px && x < px+pw && y >= py && y < py+ph
}

// termCloseRect is the ✕ button's rectangle in the header row —
// computed once so draw and hit-testing can't drift (btnRect rule).
func (a *App) termCloseRect() btnRect {
	px, py, pw, _ := a.termPanelRect()
	return btnRect{x: px + pw - 4, y: py, w: 3}
}

// termStopRect is the ⏹ button's rectangle, left of the ✕. Only live
// while a command runs; draw and hit-test both gate on term.running.
func (a *App) termStopRect() btnRect {
	c := a.termCloseRect()
	return btnRect{x: c.x - 4, y: c.y, w: 3}
}

// termInputSpan returns the input row's y and the field's [start, end)
// columns after the prompt — the one geometry source for drawing,
// caret placement, and click-to-position.
func (a *App) termInputSpan() (y, start, end int) {
	px, py, pw, ph := a.termPanelRect()
	y = py + ph - 1
	start = px + 1 + runeLen(a.termPrompt())
	end = px + pw - 1
	if start > end {
		start = end
	}
	return
}

// termPrompt is the input row's gutter text: a run indicator while a
// command is in flight, the continuation ellipsis while grsh wants more
// lines, and otherwise a status-decorated caret glyph.
func (a *App) termPrompt() string {
	switch {
	case a.term.running:
		return "⋯ "
	case len(a.term.pending) > 0:
		return "… "
	case a.term.sess != nil && a.term.sess.LastStatus() != 0:
		return "[" + itoa(a.term.sess.LastStatus()) + "] ❯ "
	default:
		return "❯ "
	}
}

// termTitleCwd renders the session's working directory for the header,
// home-abbreviated and left-truncated to fit narrow panels. The cwd
// lives in the header rather than the prompt so the input row keeps
// its columns for the command.
func (a *App) termTitleCwd(maxW int) string {
	if a.term.sess == nil {
		return ""
	}
	// A narrow strip (the left dock can shrink to termPanelMinWidth)
	// may leave no room at all — drop the cwd entirely rather than
	// letting an untruncatable string paint past the panel edge.
	if maxW < 4 {
		return ""
	}
	cwd := abbrevHomePath(a.term.sess.Cwd())
	if runeLen(cwd) > maxW {
		r := []rune(cwd)
		cwd = "…" + string(r[len(r)-maxW+1:])
	}
	return cwd
}

// abbrevHomePath shortens a path under $HOME to the familiar ~ form.
func abbrevHomePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// -----------------------------------------------------------------------------
// Keyboard + mouse
// -----------------------------------------------------------------------------

// handleTermKey processes a keystroke while the panel has focus. Esc
// never reaches here (the global handler consumes it first), so leaders
// and the double-Esc menu keep working from inside the terminal.
func (a *App) handleTermKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEnter:
		a.submitTermCommand()
	case tcell.KeyUp:
		a.termHistoryMove(-1)
	case tcell.KeyDown:
		a.termHistoryMove(1)
	case tcell.KeyPgUp:
		a.termPanelScroll(-a.termVisibleRows())
	case tcell.KeyPgDn:
		a.termPanelScroll(a.termVisibleRows())
	default:
		a.term.input.handleKey(ev)
	}
}

// termPanelPress routes an initial left press inside the panel and
// reports whether it started a header resize drag. Body clicks focus
// the input; a click on the input row also repositions the caret.
func (a *App) termPanelPress(x, y int) (startDrag bool) {
	if a.termCloseRect().contains(x, y) {
		a.term.open = false
		a.term.focused = false
		return false
	}
	if a.term.running && a.termStopRect().contains(x, y) {
		a.termInterrupt()
		return false
	}
	_, py, _, _ := a.termPanelRect()
	if y == py && !a.termDockLeft {
		return true // header rule outside the buttons: grab handle
	}
	a.term.focused = true
	iy, start, end := a.termInputSpan()
	if y == iy {
		a.term.input.clickAt(start, end, x)
	}
	return false
}

// termPasteClip inserts the text clipboard into the input line — the
// Cmd+V path while the terminal has focus (pasting a path or snippet
// into a command is the common case; the editor buffer is not the
// target when the shell owns the keyboard).
func (a *App) termPasteClip() {
	if a.clipBuf == "" {
		return
	}
	for _, r := range a.clipBuf {
		if r == '\n' || r == '\r' || r < 0x20 {
			continue // input is single-line; drop control runes
		}
		a.term.input.handleKey(tcell.NewEventKey(tcell.KeyRune, r, 0))
	}
}

// -----------------------------------------------------------------------------
// Drawing
// -----------------------------------------------------------------------------

// drawTermPanel paints the panel: header rule with title, cwd and
// buttons; the scrollback; and the prompt + input row. When the panel
// has focus the hardware cursor moves to the input caret (drawn after
// the editor, so the later ShowCursor wins).
func (a *App) drawTermPanel() {
	px, py, pw, ph := a.termPanelRect()
	th := a.theme

	headerSt := tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Subtle)
	titleSt := tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Accent).Bold(true)
	cwdSt := tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Muted)
	bodyBG := tcell.StyleDefault.Background(th.BG)

	// Header rule: title + cwd on the left, ⏹ (while running) and ✕ on
	// the right. The rule doubles as the resize grab handle.
	for cx := px; cx < px+pw; cx++ {
		a.screen.SetContent(cx, py, '─', nil, headerSt)
	}
	title := " Terminal "
	drawAt(a.screen, px+1, py, title, titleSt)
	cwd := a.termTitleCwd(pw - runeLen(title) - 12)
	if cwd != "" {
		drawAt(a.screen, px+1+runeLen(title), py, "· "+cwd+" ", cwdSt)
	}
	closeBtn := a.termCloseRect()
	drawAt(a.screen, closeBtn.x, closeBtn.y, " ✕ ", titleSt)
	if a.term.running {
		stopBtn := a.termStopRect()
		drawAt(a.screen, stopBtn.x, stopBtn.y, " ⏹ ",
			tcell.StyleDefault.Background(th.SidebarBG).Foreground(th.Error).Bold(true))
	}

	// Scrollback rows.
	for row := 0; row < ph-2; row++ {
		ry := py + 1 + row
		for cx := px; cx < px+pw; cx++ {
			a.screen.SetContent(cx, ry, ' ', nil, bodyBG)
		}
		a.drawTermRow(a.term.scroll+row, px+1, ry, pw-2)
	}

	// Input row: prompt gutter + editable field.
	iy, start, end := a.termInputSpan()
	for cx := px; cx < px+pw; cx++ {
		a.screen.SetContent(cx, iy, ' ', nil, bodyBG)
	}
	promptSt := tcell.StyleDefault.Background(th.BG).Foreground(th.Accent).Bold(true)
	if a.term.running {
		promptSt = tcell.StyleDefault.Background(th.BG).Foreground(th.Muted)
	}
	drawAt(a.screen, px+1, iy, a.termPrompt(), promptSt)
	inputSt := tcell.StyleDefault.Background(th.BG).Foreground(th.Text)
	a.term.input.draw(a.screen, iy, start, end, inputSt, a.term.focused)
}

// drawTermRow paints scrollback row idx (committed lines first, then
// the partial tail), truncated to the pane width.
func (a *App) drawTermRow(idx, x, ry, w int) {
	if w <= 0 || idx < 0 {
		return
	}
	th := a.theme
	var text string
	st := tcell.StyleDefault.Background(th.BG).Foreground(th.Text)
	switch {
	case idx < len(a.term.lines):
		ln := a.term.lines[idx]
		text = ln.text
		switch ln.kind {
		case termCmd:
			st = st.Foreground(th.Accent).Bold(true)
		case termErr:
			st = st.Foreground(th.Error)
		}
	case idx == len(a.term.lines) && a.term.partial != "":
		text = a.term.partial
	default:
		return
	}
	if runeLen(text) > w {
		text = string([]rune(text)[:w-1]) + "…"
	}
	drawAt(a.screen, x, ry, text, st)
}
