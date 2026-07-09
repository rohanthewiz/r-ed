// =============================================================================
// File: internal/app/lsp.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-09
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// lsp.go bridges the minimal LSP client (internal/lsp) into the editor:
// server lifecycle, document sync, diagnostics, go-to-definition, and
// hover. It follows the same house rules as every other subsystem:
//
//   - Silent degradation (format.go's rule): no gopls on PATH, server
//     crash, request failure — the editor keeps working, nothing nags.
//   - Custom tcell events for goroutine → main-loop messaging: the
//     client's read loop, the start handshake, the debounce timers,
//     and the definition/hover calls all run off-loop and post events;
//     only the main loop touches lspState.
//
// Lifecycle: the first opened .go file kicks off an async start
// (LookPath → spawn → initialize handshake on a goroutine). When
// lspReadyEvent lands, every already-open Go tab gets its didOpen —
// so documents opened during the handshake aren't lost. Edits re-arm
// a per-document debounce timer; the timer posts lspSyncEvent and the
// main loop sends one full-text didChange. Saves flush any pending
// change first so the server never sees a didSave for stale content.
//
//	openFile(.go) ──► lspEnsureStarted ──goroutine──► spawn + initialize
//	                                                        │
//	   didOpen all open .go tabs  ◄── lspReadyEvent ◄───────┘
//	   keystroke → EditRev bump → debounce timer ──► lspSyncEvent → didChange
//	   gopls ──publishDiagnostics──► lspDiagsEvent → App.lsp.diags → gutter/underline

package app

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/lsp"
	"github.com/rohanthewiz/r-ed/internal/theme"
)

// lspSyncDebounce is how long after the last edit the didChange fires.
// Long enough to coalesce a typing burst into one sync, short enough
// that diagnostics feel live when the user pauses.
const lspSyncDebounce = 300 * time.Millisecond

// lspNavStackMax caps the back-navigation stack. Fifty jumps of
// history is more than anyone retraces; the cap just stops a long
// session from growing the slice forever.
const lspNavStackMax = 50

// lspServerBinary is the language server the editor knows how to run.
// Go-only for now — the lspHandles/languageID seams are where a second
// language would plug in.
const lspServerBinary = "gopls"

// lspConn is the slice of the lsp.Client surface the app layer uses.
// An interface so tests can substitute a recording fake without
// spawning a real server process.
type lspConn interface {
	DidOpen(path, languageID string, version int, text string) error
	DidChange(path string, version int, text string) error
	DidSave(path string) error
	DidClose(path string) error
	Definition(path string, pos lsp.Position) ([]lsp.Location, error)
	HoverAt(path string, pos lsp.Position) (*lsp.Hover, error)
	Close()
}

// navLoc is one entry of the back-navigation stack: where the cursor
// was before a go-to-definition jump.
type navLoc struct {
	path string
	pos  editor.Position
}

// lspState is everything the LSP integration remembers, owned by App
// and mutated only on the main loop. Maps are lazily created so tests
// that assemble an App by hand need no extra setup.
type lspState struct {
	client   lspConn
	starting bool // async spawn+initialize in flight
	dead     bool // unavailable: no binary, crashed, or failed to start

	versions  map[string]int // per-path didChange version counter
	syncedRev map[string]int // per-path Tab.EditRev last sent to the server
	timers    map[string]*time.Timer

	// diags is keyed by absolute path. gopls publishes for any file in
	// the workspace, not just open ones; keeping them all costs little
	// and means a file opened later shows its problems immediately.
	diags map[string][]lsp.Diagnostic

	navStack []navLoc
}

// -----------------------------------------------------------------------------
// Custom tcell events — the goroutine → main-loop bridge
// -----------------------------------------------------------------------------

// lspReadyEvent is posted once the async spawn + initialize handshake
// completes successfully; it carries the live connection.
type lspReadyEvent struct {
	when   time.Time
	client lspConn
}

// When satisfies the tcell.Event interface.
func (e *lspReadyEvent) When() time.Time { return e.when }

// lspExitEvent is posted when the server dies or fails to start.
type lspExitEvent struct {
	when time.Time
}

// When satisfies the tcell.Event interface.
func (e *lspExitEvent) When() time.Time { return e.when }

// lspDiagsEvent carries one document's fresh diagnostics from the
// client's read loop to the main loop.
type lspDiagsEvent struct {
	when  time.Time
	path  string
	diags []lsp.Diagnostic
}

// When satisfies the tcell.Event interface.
func (e *lspDiagsEvent) When() time.Time { return e.when }

// lspSyncEvent is posted by a debounce timer: "this document has been
// quiet for lspSyncDebounce — sync it now".
type lspSyncEvent struct {
	when time.Time
	path string
}

// When satisfies the tcell.Event interface.
func (e *lspSyncEvent) When() time.Time { return e.when }

// lspDefinitionEvent carries a definition response. fromPath/fromPos
// remember where the request was made so the nav stack records the
// true origin even if the user moved while the request was in flight.
type lspDefinitionEvent struct {
	when     time.Time
	fromPath string
	fromPos  editor.Position
	locs     []lsp.Location
	err      error
}

// When satisfies the tcell.Event interface.
func (e *lspDefinitionEvent) When() time.Time { return e.when }

// lspHoverEvent carries a hover response, already flattened to text.
type lspHoverEvent struct {
	when time.Time
	path string
	text string
	err  error
}

// When satisfies the tcell.Event interface.
func (e *lspHoverEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

// lspHandles reports whether the LSP layer covers this file. Go-only
// today; a future second server extends this and languageIDFor.
func lspHandles(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

// languageIDFor returns the LSP languageId for a handled path.
func languageIDFor(string) string { return "go" }

// lspReady reports whether the connection is up and usable.
func (a *App) lspReady() bool {
	return a.lsp.client != nil && !a.lsp.dead
}

// lspEnsureStarted kicks off the async server start the first time a
// handled file opens. Missing binary marks the integration dead
// without a word to the user — same silent-degradation contract as
// missing formatters. Idempotent; later calls are no-ops.
func (a *App) lspEnsureStarted() {
	if a.lsp.client != nil || a.lsp.starting || a.lsp.dead || a.screen == nil {
		return
	}
	if _, err := exec.LookPath(lspServerBinary); err != nil {
		a.lsp.dead = true
		return
	}
	a.lsp.starting = true
	scr := a.screen
	root := a.rootDir
	go func() {
		// onNotify runs on the client's read loop — post, don't touch.
		onNotify := func(method string, params json.RawMessage) {
			if method != "textDocument/publishDiagnostics" {
				return
			}
			var p lsp.PublishDiagnosticsParams
			if err := json.Unmarshal(params, &p); err != nil {
				return
			}
			path := lsp.URIToPath(p.URI)
			if path == "" {
				return
			}
			_ = scr.PostEvent(&lspDiagsEvent{when: time.Now(), path: path, diags: p.Diagnostics})
		}
		onExit := func(error) {
			_ = scr.PostEvent(&lspExitEvent{when: time.Now()})
		}
		client, err := lsp.Start(root, lspServerBinary, nil, onNotify, onExit)
		if err != nil {
			_ = scr.PostEvent(&lspExitEvent{when: time.Now()})
			return
		}
		if err := client.Initialize(root); err != nil {
			client.Close()
			// The failed handshake already fires onExit via the read
			// loop in most cases, but a timeout leaves the process
			// running — post explicitly so the state machine settles.
			_ = scr.PostEvent(&lspExitEvent{when: time.Now()})
			return
		}
		_ = scr.PostEvent(&lspReadyEvent{when: time.Now(), client: client})
	}()
}

// handleLSPReady installs the live connection and announces every
// already-open handled document — the tabs the user opened while the
// handshake was still in flight.
func (a *App) handleLSPReady(e *lspReadyEvent) {
	if a.lsp.dead {
		// Server died between the ready post and now (or the editor
		// shut the integration down). Don't resurrect.
		e.client.Close()
		return
	}
	a.lsp.client = e.client
	a.lsp.starting = false
	for _, t := range a.tabs {
		a.lspOpenDoc(t)
	}
}

// handleLSPExit marks the integration dead and clears every
// diagnostic — stale squiggles from a crashed server would otherwise
// linger forever with nothing left to retract them. Deliberately no
// auto-restart: a crashing server would flap, and the user can
// restart the editor when they've fixed their gopls install.
func (a *App) handleLSPExit() {
	if a.lsp.client != nil {
		a.lsp.client.Close()
	}
	a.lsp.client = nil
	a.lsp.starting = false
	a.lsp.dead = true
	a.lsp.diags = nil
	a.lspStopTimers()
}

// lspShutdown tears the connection down on editor exit.
func (a *App) lspShutdown() {
	a.lspStopTimers()
	if a.lsp.client != nil {
		a.lsp.client.Close()
		a.lsp.client = nil
	}
	a.lsp.dead = true
}

// lspStopTimers cancels every pending debounce timer.
func (a *App) lspStopTimers() {
	for path, tm := range a.lsp.timers {
		tm.Stop()
		delete(a.lsp.timers, path)
	}
}

// -----------------------------------------------------------------------------
// Document sync
// -----------------------------------------------------------------------------

// lspOpenDoc announces one tab's document to the server (and starts
// the server on first need). Safe to call for any tab — non-Go files,
// images, and untitled tabs are skipped.
func (a *App) lspOpenDoc(t *editor.Tab) {
	if t == nil || t.Path == "" || t.IsImage() || !lspHandles(t.Path) {
		return
	}
	a.lspEnsureStarted()
	if !a.lspReady() {
		return // queued implicitly: handleLSPReady re-announces open tabs
	}
	if a.lsp.versions == nil {
		a.lsp.versions = map[string]int{}
	}
	if a.lsp.syncedRev == nil {
		a.lsp.syncedRev = map[string]int{}
	}
	a.lsp.versions[t.Path] = 1
	a.lsp.syncedRev[t.Path] = t.EditRev
	_ = a.lsp.client.DidOpen(t.Path, languageIDFor(t.Path), 1, t.Buffer.String())
}

// lspCloseDoc announces a closed tab and drops its bookkeeping. The
// diagnostics entry goes too — the server will retract them anyway,
// and holding paint data for an invisible file helps nobody.
func (a *App) lspCloseDoc(path string) {
	if path == "" || !lspHandles(path) {
		return
	}
	if tm := a.lsp.timers[path]; tm != nil {
		tm.Stop()
		delete(a.lsp.timers, path)
	}
	delete(a.lsp.versions, path)
	delete(a.lsp.syncedRev, path)
	delete(a.lsp.diags, path)
	if a.lspReady() {
		_ = a.lsp.client.DidClose(path)
	}
}

// lspDidSave flushes any unsent buffer content and then announces the
// save. The flush matters: with a pending debounce the server would
// otherwise see didSave for text it hasn't been given yet and
// diagnose a phantom version of the file.
func (a *App) lspDidSave(t *editor.Tab) {
	if t == nil || !a.lspReady() || !lspHandles(t.Path) {
		return
	}
	a.lspFlushChange(t)
	_ = a.lsp.client.DidSave(t.Path)
}

// lspAfterEvent runs after every event dispatch and (re-)arms the
// debounce timer for any open document whose EditRev moved past what
// the server has. Cheap — a handful of integer compares — so calling
// it unconditionally keeps the trigger logic in one place instead of
// sprinkled through every mutation path.
func (a *App) lspAfterEvent() {
	if !a.lspReady() || a.screen == nil {
		return
	}
	for _, t := range a.tabs {
		if t.Path == "" || t.IsImage() || !lspHandles(t.Path) {
			continue
		}
		if _, open := a.lsp.versions[t.Path]; !open {
			continue // never announced (opened pre-ready and missed? — didOpen handles)
		}
		if t.EditRev == a.lsp.syncedRev[t.Path] {
			continue
		}
		a.lspArmTimer(t.Path)
	}
}

// lspArmTimer starts (or restarts) the per-document debounce clock.
// Restarting on every further edit is what makes it a debounce rather
// than a throttle: the sync fires once the document goes quiet.
func (a *App) lspArmTimer(path string) {
	if a.lsp.timers == nil {
		a.lsp.timers = map[string]*time.Timer{}
	}
	if tm := a.lsp.timers[path]; tm != nil {
		tm.Reset(lspSyncDebounce)
		return
	}
	scr := a.screen
	a.lsp.timers[path] = time.AfterFunc(lspSyncDebounce, func() {
		_ = scr.PostEvent(&lspSyncEvent{when: time.Now(), path: path})
	})
}

// handleLSPSync is the debounce firing on the main loop: send the
// document's current full text if it's still open and still ahead of
// the server. The timer entry is dropped so the next edit arms a
// fresh one.
func (a *App) handleLSPSync(e *lspSyncEvent) {
	delete(a.lsp.timers, e.path)
	t := a.tabByPath(e.path)
	if t == nil {
		return
	}
	a.lspFlushChange(t)
}

// lspFlushChange sends one full-text didChange if the buffer is ahead
// of what the server has seen. No-op when already in sync.
func (a *App) lspFlushChange(t *editor.Tab) {
	if t == nil || !a.lspReady() {
		return
	}
	if _, open := a.lsp.versions[t.Path]; !open {
		return
	}
	if t.EditRev == a.lsp.syncedRev[t.Path] {
		return
	}
	a.lsp.versions[t.Path]++
	a.lsp.syncedRev[t.Path] = t.EditRev
	_ = a.lsp.client.DidChange(t.Path, a.lsp.versions[t.Path], t.Buffer.String())
}

// tabByPath returns the open tab backing path, or nil. Events resolve
// tabs by path rather than index because tabs reorder and close while
// background work is in flight — same rule formatDoneEvent follows.
func (a *App) tabByPath(path string) *editor.Tab {
	for _, t := range a.tabs {
		if t.Path == path {
			return t
		}
	}
	return nil
}

// handleLSPDiags stores one document's fresh diagnostics. An empty
// list retracts — gopls sends that when the last problem is fixed.
func (a *App) handleLSPDiags(e *lspDiagsEvent) {
	if a.lsp.diags == nil {
		a.lsp.diags = map[string][]lsp.Diagnostic{}
	}
	if len(e.diags) == 0 {
		delete(a.lsp.diags, e.path)
		return
	}
	a.lsp.diags[e.path] = e.diags
}

// -----------------------------------------------------------------------------
// Position conversion — the editor speaks runes, LSP speaks UTF-16
// -----------------------------------------------------------------------------

// lspPosFor converts a buffer position to an LSP position.
func lspPosFor(t *editor.Tab, p editor.Position) lsp.Position {
	return lsp.Position{
		Line:      p.Line,
		Character: lsp.UTF16Col(t.Buffer.LineRunes(p.Line), p.Col),
	}
}

// editorPosFor converts an LSP position into a clamped buffer position
// for tab t.
func editorPosFor(t *editor.Tab, p lsp.Position) editor.Position {
	pos := editor.Position{
		Line: p.Line,
		Col:  lsp.RuneCol(t.Buffer.LineRunes(p.Line), p.Character),
	}
	return t.Buffer.Clamp(pos)
}

// -----------------------------------------------------------------------------
// Go-to-definition + back navigation
// -----------------------------------------------------------------------------

// hasLSPActions is the menu predicate for definition / hover: the
// server is up and the active tab is a document it understands.
func (a *App) hasLSPActions() bool {
	t := a.activeTabPtr()
	return t != nil && t.Path != "" && !t.IsImage() && lspHandles(t.Path) && a.lspReady()
}

// hasNavBack is the menu predicate for Jump back.
func (a *App) hasNavBack() bool { return len(a.lsp.navStack) > 0 }

// menuGoToDefinition fires an async definition request for the symbol
// under the cursor. The result lands as an lspDefinitionEvent.
func (a *App) menuGoToDefinition() {
	a.closeMenu()
	t := a.activeTabPtr()
	if t == nil || !a.hasLSPActions() {
		return
	}
	client := a.lsp.client
	scr := a.screen
	path, from := t.Path, t.Cursor
	pos := lspPosFor(t, from)
	a.lspFlushChange(t) // definition must resolve against what's on screen
	go func() {
		locs, err := client.Definition(path, pos)
		_ = scr.PostEvent(&lspDefinitionEvent{
			when: time.Now(), fromPath: path, fromPos: from, locs: locs, err: err,
		})
	}()
}

// handleLSPDefinition lands a definition response: jump to the first
// location, recording where we came from so Esc-o can retrace.
func (a *App) handleLSPDefinition(e *lspDefinitionEvent) {
	if e.err != nil || len(e.locs) == 0 {
		a.flash("No definition found")
		return
	}
	target := lsp.URIToPath(e.locs[0].URI)
	if target == "" {
		a.flash("Definition is not in a plain file")
		return
	}
	a.openFile(target)
	t := a.activeTabPtr()
	if t == nil || t.Path != target {
		return // openFile failed and flashed its own error
	}
	a.pushNav(navLoc{path: e.fromPath, pos: e.fromPos})
	t.MoveCursorTo(editorPosFor(t, e.locs[0].Range.Start), false)
}

// pushNav records a jump origin, capping the stack so it can't grow
// without bound over a long session.
func (a *App) pushNav(loc navLoc) {
	a.lsp.navStack = append(a.lsp.navStack, loc)
	if len(a.lsp.navStack) > lspNavStackMax {
		a.lsp.navStack = a.lsp.navStack[len(a.lsp.navStack)-lspNavStackMax:]
	}
}

// menuJumpBack pops the navigation stack and returns the cursor to
// where the last definition jump started.
func (a *App) menuJumpBack() {
	a.closeMenu()
	n := len(a.lsp.navStack)
	if n == 0 {
		a.flash("Nowhere to jump back to")
		return
	}
	loc := a.lsp.navStack[n-1]
	a.lsp.navStack = a.lsp.navStack[:n-1]
	a.openFile(loc.path)
	if t := a.activeTabPtr(); t != nil && t.Path == loc.path {
		t.MoveCursorTo(loc.pos, false)
	}
}

// -----------------------------------------------------------------------------
// Hover
// -----------------------------------------------------------------------------

// menuHoverInfo fires an async hover request for the symbol under the
// cursor; the response lands as an lspHoverEvent.
func (a *App) menuHoverInfo() {
	a.closeMenu()
	t := a.activeTabPtr()
	if t == nil || !a.hasLSPActions() {
		return
	}
	client := a.lsp.client
	scr := a.screen
	path := t.Path
	pos := lspPosFor(t, t.Cursor)
	a.lspFlushChange(t)
	go func() {
		h, err := client.HoverAt(path, pos)
		text := ""
		if h != nil {
			text = h.HoverText()
		}
		_ = scr.PostEvent(&lspHoverEvent{when: time.Now(), path: path, text: text, err: err})
	}()
}

// handleLSPHover lands a hover response: open the near-cursor modal,
// or flash when the server has nothing to say. A response for a tab
// the user has already left is dropped — popping a modal about a
// different file would be disorienting.
func (a *App) handleLSPHover(e *lspHoverEvent) {
	t := a.activeTabPtr()
	if t == nil || t.Path != e.path {
		return
	}
	lines := hoverLines(e.text)
	if e.err != nil || len(lines) == 0 {
		a.flash("No hover info")
		return
	}
	a.openModal(&hoverModal{lines: lines})
}

// hoverLines flattens hover text for the modal: markdown code fences
// dropped (the modal is monospace already — fence markers are pure
// noise), trailing blank lines trimmed, and length capped so a huge
// doc comment can't swallow the screen.
func hoverLines(text string) []string {
	const maxLines = 12
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			continue
		}
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	// Trim leading/trailing blanks left behind by dropped fences.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) > maxLines {
		out = append(out[:maxLines], "…")
	}
	return out
}

// -----------------------------------------------------------------------------
// Diagnostics — decoration source + status bar summary
// -----------------------------------------------------------------------------

// diagSeverityColor maps an LSP severity to its theme color. Unknown /
// omitted severities are treated as errors per the spec.
func diagSeverityColor(th theme.Theme, severity int) tcell.Color {
	switch severity {
	case lsp.SeverityWarning:
		return th.DiagWarning
	case lsp.SeverityInfo, lsp.SeverityHint:
		return th.DiagInfo
	default:
		return th.DiagError
	}
}

// lspDiagSource adapts App.lsp.diags into decorations: an underline
// span per diagnostic plus one gutter dot per afflicted line. It
// registers after gitDiffSource so a diagnostic dot outranks a git
// mark on the same line — "this line is broken" matters more than
// "this line changed".
type lspDiagSource struct {
	app *App
}

// Decorations emits the visible window's diagnostic spans and marks.
// Line indexes are validated against the live buffer because
// diagnostics lag edits by a debounce + type-check — a stale range
// past EOF must cull, not panic.
func (s lspDiagSource) Decorations(t *editor.Tab, th theme.Theme, firstLine, lastLine int) ([]editor.Span, []editor.GutterMark) {
	diags := s.app.lsp.diags[t.Path]
	if len(diags) == 0 {
		return nil, nil
	}
	var spans []editor.Span
	// Track the worst (lowest-numbered) severity per marked line so
	// one gutter cell summarises overlapping diagnostics honestly.
	lineSev := map[int]int{}
	for _, d := range diags {
		start, end := d.Range.Start.Line, d.Range.End.Line
		if end < firstLine || start > lastLine || start >= t.Buffer.LineCount() {
			continue
		}
		sev := d.Severity
		if sev == 0 {
			sev = lsp.SeverityError
		}
		sp := editor.Span{
			Start: editorPosFor(t, d.Range.Start),
			End:   editorPosFor(t, d.Range.End),
			Delta: editor.StyleDelta{Underline: true, SetFG: true, FG: diagSeverityColor(th, sev)},
		}
		// A zero-width range (some servers point at a single position)
		// still deserves a visible cell — stretch it one rune right.
		if sp.Start == sp.End {
			sp.End.Col++
		}
		spans = append(spans, sp)
		if cur, ok := lineSev[start]; !ok || sev < cur {
			lineSev[start] = sev
		}
	}
	var marks []editor.GutterMark
	for line, sev := range lineSev {
		if line < firstLine || line > lastLine {
			continue
		}
		marks = append(marks, editor.GutterMark{Line: line, Glyph: '●', FG: diagSeverityColor(th, sev)})
	}
	return spans, marks
}

// diagCounts tallies the active tab's diagnostics by bucket for the
// status bar: errors, warnings, and everything milder lumped together.
func (a *App) diagCounts() (errs, warns, infos int) {
	t := a.activeTabPtr()
	if t == nil {
		return 0, 0, 0
	}
	for _, d := range a.lsp.diags[t.Path] {
		switch d.Severity {
		case lsp.SeverityWarning:
			warns++
		case lsp.SeverityInfo, lsp.SeverityHint:
			infos++
		default:
			errs++
		}
	}
	return errs, warns, infos
}

// diagStatusSuffix renders the status-bar summary (" · ✗ 2 ⚠ 1"), or
// "" when the active tab is clean — the common case stays uncluttered.
func (a *App) diagStatusSuffix() string {
	errs, warns, infos := a.diagCounts()
	if errs == 0 && warns == 0 && infos == 0 {
		return ""
	}
	// One " · " lead-in, then each non-zero bucket with its glyph.
	var b strings.Builder
	b.WriteString(" ·")
	if errs > 0 {
		fmt.Fprintf(&b, " ✗ %d", errs)
	}
	if warns > 0 {
		fmt.Fprintf(&b, " ⚠ %d", warns)
	}
	if infos > 0 {
		fmt.Fprintf(&b, " ℹ %d", infos)
	}
	return b.String()
}
