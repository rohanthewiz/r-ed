// =============================================================================
// File: internal/app/finder.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

// Project-wide file finder UI — a centered modal with a search
// input on top and ~10 result rows below. Type to filter, ↑/↓ to
// navigate, Enter to open, Esc to dismiss. Mouse hover highlights;
// click opens.
//
// All the index/scoring lifting lives in internal/finder; this
// file is just the wiring between user input and the buffer/tree
// that already exist. The cached index (App.finder) outlives the
// modal — finderModal is only the transient UI state of one open.

import (
	"path/filepath"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rohanthewiz/r-ed/internal/finder"
)

const (
	// finderModalMaxWidth caps the modal so very wide terminals
	// don't get a full-width strip that's awkward to scan. 80 is
	// the comfortable reading width every IDE settles on.
	finderModalMaxWidth = 80
	// finderResultsVisible is how many result rows we render at
	// once. 10 is the floor for "feels useful" without dominating
	// the screen on small terminals.
	finderResultsVisible = 10
	// finderSearchLimit is how deep we look — we ask for one full
	// "page" extra so users can scroll past the initial chunk.
	finderSearchLimit = 50
)

// finderRebuiltEvent is posted by the background indexer goroutine
// when it finishes a rebuild. The main loop reacts by re-running
// the current query so the visible results refresh — the user
// gets to see "Indexing…" replaced with real matches without
// having to type or wait for the next keystroke.
type finderRebuiltEvent struct {
	when time.Time
}

// When satisfies the tcell.Event interface.
func (e *finderRebuiltEvent) When() time.Time { return e.when }

// finderModal is the transient UI state of one finder session: the
// query field, the highlighted row, and the current result page.
type finderModal struct {
	field    textField
	selected int
	results  []finder.Result
}

// openFinder shows the project-wide file finder. Triggers a
// background rebuild on every open so external file changes are
// reflected even if the periodic invalidation tick hasn't fired.
// (Building when the index is already StateReady is a no-op
// inside the orchestrator's coalesce gate.)
func (a *App) openFinder() {
	m := &finderModal{}
	a.openModal(m)
	scr := a.screen
	if a.finder != nil {
		// Rebuild only when the cache is genuinely stale. A re-open
		// during a still-warm session shouldn't pay for a refresh.
		if a.finder.State() != finder.StateReady {
			a.finder.Rebuild(func() {
				_ = scr.PostEvent(&finderRebuiltEvent{when: time.Now()})
			})
		}
	}
	m.refresh(a)
}

// menuFindFile is the ≡ menu entry that opens the finder. Lives
// alongside menuFind (which is the in-file find bar) — they share
// vocabulary but search different scopes.
func (a *App) menuFindFile() {
	a.closeMenu()
	a.openFinder()
}

// hasFinder is the menu predicate. Always true once the finder
// has been wired in App.New — the row stays enabled even before
// the first index lands so the user can pop the modal and watch
// "Indexing…" tick over.
func (a *App) hasFinder() bool {
	return a.finder != nil
}

// refresh re-runs the current query against the index. Called on
// every edit and on the rebuilt event so a slow first index doesn't
// leave the modal showing stale "Indexing…" forever.
func (m *finderModal) refresh(a *App) {
	if a.finder == nil {
		m.results = nil
		return
	}
	m.results = a.finder.Search(m.field.String(), finderSearchLimit)
	if m.selected >= len(m.results) {
		m.selected = len(m.results) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// invalidateFinder marks the index stale and kicks off a rebuild.
// Called by the file-tree refresh tick and by every fileops
// mutation (create / rename / delete) so the finder doesn't show
// ghost paths or miss freshly-created files.
func (a *App) invalidateFinder() {
	if a.finder == nil {
		return
	}
	a.finder.Invalidate()
	scr := a.screen
	a.finder.Rebuild(func() {
		_ = scr.PostEvent(&finderRebuiltEvent{when: time.Now()})
	})
}

// handleKey routes keyboard input while the finder modal is open.
// The finder-specific bits are arrow-key navigation through results
// and Enter-to-open; text editing is the shared textField.
func (m *finderModal) handleKey(a *App, ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeModal()
	case tcell.KeyEnter:
		m.openSelected(a)
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
	case tcell.KeyDown:
		if m.selected < len(m.results)-1 {
			m.selected++
		}
	default:
		if _, edited := m.field.handleKey(ev); edited {
			m.selected = 0
			m.refresh(a)
		}
	}
}

// handleMouse handles mouse input while the modal is open. Hover
// highlights the row under the cursor; click opens it. Click outside
// the modal dismisses.
func (m *finderModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	mx, my, mw, mh := m.rect(a)
	rowsStart := my + 4
	row := y - rowsStart
	if row >= 0 && row < len(m.results) && x >= mx && x < mx+mw {
		// Hover highlight always tracks the mouse — same behaviour
		// the action menu uses, so users can scrub through results
		// without clicking.
		m.selected = row
	}
	if btn&tcell.Button1 == 0 {
		return
	}
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		a.closeModal()
		return
	}
	if row >= 0 && row < len(m.results) {
		m.selected = row
		m.openSelected(a)
	}
}

// openSelected opens the highlighted result. Closes the modal, then
// resolves the relative path against the project root and opens the
// file. Silent no-op when the result list is empty (e.g. the user
// mashed Enter on a no-match query).
func (m *finderModal) openSelected(a *App) {
	if m.selected < 0 || m.selected >= len(m.results) {
		return
	}
	rel := m.results[m.selected].Path
	a.closeModal()
	abs := filepath.Join(a.rootDir, rel)
	a.openFile(abs)
}

// rect returns the on-screen rectangle of the finder modal. Sized to
// fit a healthy result list while leaving margins on small terminals.
func (m *finderModal) rect(a *App) (x, y, w, h int) {
	w = finderModalMaxWidth
	if w > a.width-4 {
		w = a.width - 4
	}
	if w < 30 {
		w = 30
	}
	// Layout: 1 border + 1 title + 1 divider + 1 input + N results
	// + 1 status + 1 border = N+6 rows.
	h = finderResultsVisible + 6
	if h > a.height-2 {
		h = a.height - 2
	}
	x = (a.width - w) / 2
	y = (a.height - h) / 3 // anchor in upper third — matches VS Code's feel
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return
}

// draw paints the modal: title + Esc hint, input field with
// match-count tail, then either a "Indexing…" line or the result
// rows with highlighted match characters.
//
// Layout (relY):
//
//	0     top border
//	1     title — "Find file in project    esc"
//	2     divider
//	3     input          [ query…           42/12345 ]
//	4..N  result rows
//	N+1   bottom border
func (m *finderModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	hitStyle := tcell.StyleDefault.Background(c.bg).Foreground(a.theme.FindCurrent).Bold(true)
	c.drawFrame(a.screen, mx, my, mw, mh, "Find file")

	// Input row — the right side leaves room for the count tail.
	inputStyle := tcell.StyleDefault.Background(a.theme.BG).Foreground(a.theme.Text)
	m.field.draw(a.screen, my+3, mx+3, mx+mw-12, inputStyle, true)

	// Match count tail.
	state, total, viaGit := finder.StateIdle, 0, false
	if a.finder != nil {
		state, total, viaGit = a.finder.Stats()
	}
	_ = viaGit // reserved for a future "via git" badge — keeping the
	// triple-return so callers don't have to plumb a new method later.
	tail := ""
	switch state {
	case finder.StateBuilding, finder.StateIdle:
		tail = "indexing… "
	case finder.StateErrored:
		tail = "index err "
	case finder.StateReady:
		tail = countLabel(len(m.results), total) + " "
	}
	drawAt(a.screen, mx+mw-1-runeLen(tail), my+3, tail, c.muted)

	// Result rows. We paint the visible window (no scrolling support
	// in v1 — we just cap at finderResultsVisible). When the query
	// has more matches than fit, the user can refine.
	rowsStart := my + 4
	rowsCap := mh - 5 // borders + title + divider + input
	if rowsCap > finderResultsVisible {
		rowsCap = finderResultsVisible
	}
	visible := m.results
	if len(visible) > rowsCap {
		visible = visible[:rowsCap]
	}
	for i := 0; i < rowsCap; i++ {
		ry := rowsStart + i
		if i >= len(visible) {
			// Clear unused rows so a previous query's tail doesn't
			// linger when results shrink.
			for cx := mx + 1; cx < mx+mw-1; cx++ {
				a.screen.SetContent(cx, ry, ' ', nil, c.bgSt)
			}
			continue
		}
		m.drawRow(a, mx, ry, mw, visible[i], i == m.selected, hitStyle, c.muted, c.bg)
	}
}

// drawRow paints one result line: the path, with matched runes
// highlighted via hitStyle, and the row background flipped when it's
// the selected row. The dirname is dimmed so the basename pops — same
// trick the editor's tab bar uses to make the file name the visual
// anchor.
func (m *finderModal) drawRow(a *App, mx, ry, mw int, r finder.Result, selected bool, hitStyle, mutedStyle tcell.Style, modalBG tcell.Color) {
	rowBG := modalBG
	if selected {
		rowBG = a.theme.BG
	}
	rowStyle := tcell.StyleDefault.Background(rowBG).Foreground(a.theme.Text)
	hitOnRow := hitStyle.Background(rowBG)
	mutedOnRow := mutedStyle.Background(rowBG)

	// Background fill so the selected row reads as a single block.
	for cx := mx + 1; cx < mx+mw-1; cx++ {
		a.screen.SetContent(cx, ry, ' ', nil, rowStyle)
	}

	pathRunes := []rune(r.Path)
	// Find the basename split so we can dim the directory part.
	sepIdx := -1
	for i := len(pathRunes) - 1; i >= 0; i-- {
		if pathRunes[i] == '/' {
			sepIdx = i
			break
		}
	}
	matchSet := map[int]bool{}
	for _, hit := range r.MatchedIndexes {
		matchSet[hit] = true
	}

	startCol := mx + 2
	maxCols := mw - 4
	for i, ch := range pathRunes {
		if i >= maxCols {
			break
		}
		st := rowStyle
		if i <= sepIdx {
			st = mutedOnRow
		}
		if matchSet[i] {
			st = hitOnRow
		}
		a.screen.SetContent(startCol+i, ry, ch, nil, st)
	}
}

// countLabel formats the "shown/total" tail. Pulled into its own
// helper so future tweaks (commas, abbreviation past 1k) live in
// one spot. We don't bother showing "shown" when it equals total
// — the count would be redundant and the eye should focus on the
// total.
func countLabel(shown, total int) string {
	if total == 0 {
		return "0"
	}
	if shown == total {
		return itoa(total)
	}
	return itoa(shown) + "/" + itoa(total)
}

// itoa is a tiny non-allocating-ish int→string for the count
// tail. strconv would work fine; keeping a local helper avoids
// pulling strconv into a UI file that otherwise doesn't need it.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
