// =============================================================================
// File: internal/app/formmodal.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// formmodal.go renders the multi-field secondary modal — a vertical form
// with one row per prompt. Custom actions opt in by listing prompts in
// their config; before the action's shell command runs, the editor opens
// this modal, lets the user fill in each field, and only on Submit does
// the action actually execute. The submitted values are exported to the
// shell as env vars named after the prompt keys.
//
// Layout intentionally mirrors the prompt modal it lives next to — same
// border, divider, "esc" hint, button row — so the two read as siblings.
// What's different: a flexible-height field stack and per-row focus state.

package app

import (
	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/customactions"
)

// Layout constants for the form modal. The width matches the prompt
// modal so the visual rhythm of the editor's secondary surfaces stays
// consistent, and the height grows with the prompt count rather than
// being pinned — see rect for the math.
const (
	formModalWidth = 60

	// formRowHeight is two lines per prompt: one for the label, one
	// for the input. Compact enough that 3-4 fields stay on a normal
	// terminal without scrolling.
	formRowHeight = 2

	// formChromeHeight is the cells the modal spends on borders,
	// title row, divider, button row, and trailing border — added on
	// top of the prompt rows. Pulled out so the height calc reads
	// "rows * row-height + chrome" rather than a magic number.
	formChromeHeight = 7
)

// formRow is the live state of one prompt row: the shared textField for
// text prompts, or the selected option index for select prompts.
type formRow struct {
	prompt customactions.Prompt
	field  textField // text rows only
	selIdx int       // select rows only
}

// formModal collects the values for a custom action's prompts. values
// is the canonical (Prompt.Key → value) store, updated on every edit so
// render and submit read the same state. callback receives the map only
// on Submit; Cancel discards everything.
type formModal struct {
	title    string
	rows     []formRow
	values   map[string]string
	focus    int // which prompt row owns the keyboard
	callback func(*App, map[string]string)
}

// openForm displays the form modal for a custom action. The caller
// passes the prompt list straight from customactions.Action — we
// expand each Default through the editor-state vars here so the
// modal sees only fully-resolved strings.
//
// All row state is rebuilt from prompts so a previous form can't leak
// data into the next one.
func (a *App) openForm(title string, prompts []customactions.Prompt, callback func(*App, map[string]string)) {
	if len(prompts) == 0 {
		return
	}
	vars := a.captureActionVars()

	m := &formModal{
		title:    title,
		rows:     make([]formRow, len(prompts)),
		values:   make(map[string]string, len(prompts)),
		callback: callback,
	}
	for i, p := range prompts {
		row := formRow{prompt: p}
		switch p.Type {
		case customactions.PromptText:
			val := vars.expand(p.Default)
			row.field = newTextField(val)
			m.values[p.Key] = val
		case customactions.PromptSelect:
			// Select state is the option index. Default matches by
			// equality against an option string when present; otherwise
			// we land on the first option so the user always sees a
			// valid choice on open.
			expanded := vars.expand(p.Default)
			for j, opt := range p.Options {
				if opt == expanded {
					row.selIdx = j
					break
				}
			}
			m.values[p.Key] = p.Options[row.selIdx]
		}
		m.rows[i] = row
	}
	a.openModal(m)
}

// submit hands the collected (Key, Value) map back to the caller and
// closes the modal. Empty fields are still passed through — the shell
// command author chooses whether to treat empty as "skip" or as an
// error. Forcing non-empty here would block legitimate cases like
// "REMOTE_OPTS defaults to nothing."
func (m *formModal) submit(a *App) {
	a.closeModal()
	if m.callback != nil {
		m.callback(a, m.values)
	}
}

// rect computes the on-screen rectangle. Height grows with the prompt
// count — 4 prompts is 4*2 + 7 = 15 rows — and the centering clamps to
// (0,0) so a tall form in a small terminal still starts on screen.
func (m *formModal) rect(a *App) (x, y, w, h int) {
	return a.centeredRect(formModalWidth, formChromeHeight+formRowHeight*max1(len(m.rows)))
}

// max1 keeps the height calc at least one row tall even if a buggy
// caller managed to open the form with no prompts. Without it the
// chrome rows would still draw against a 0-prompt height, which
// looks like a broken modal.
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// buttons returns the (Cancel, Submit) button rects — the single
// geometry source for draw and mouse hit-testing.
func (m *formModal) buttons(a *App) (cancel, submit btnRect) {
	mx, my, mw, mh := m.rect(a)
	btnY := my + mh - 3
	return btnRect{x: mx + 4, y: btnY, w: 10}, btnRect{x: mx + mw - 10 - 4, y: btnY, w: 10}
}

// rowSpan returns the label row, input row, and the input field's
// [start, end) columns for prompt row i.
func (m *formModal) rowSpan(a *App, i int) (labelRow, inputRow, fieldStart, fieldEnd int) {
	mx, my, mw, _ := m.rect(a)
	labelRow = my + 3 + i*formRowHeight
	return labelRow, labelRow + 1, mx + 3, mx + mw - 3
}

// draw renders the modal: title, divider, one (label, input) pair per
// prompt, and a [ Cancel ] / [ Submit ] button row. The focused row's
// input is highlighted; selects show the current option with chevrons,
// text rows show their value with a caret.
func (m *formModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	c.drawFrame(a.screen, mx, my, mw, mh, m.title)

	a.screen.HideCursor()
	for i := range m.rows {
		row := &m.rows[i]
		p := row.prompt
		labelRow, inputRow, fieldStart, fieldEnd := m.rowSpan(a, i)
		focused := i == m.focus

		labelStyle := c.muted
		if focused {
			labelStyle = c.title
		}
		drawAt(a.screen, mx+2, labelRow, p.Label, labelStyle)

		inputBg := a.theme.BG
		if focused {
			inputBg = a.theme.Subtle
		}
		inputStyle := tcell.StyleDefault.Background(inputBg).Foreground(a.theme.Text)

		switch p.Type {
		case customactions.PromptText:
			row.field.draw(a.screen, inputRow, fieldStart, fieldEnd, inputStyle, focused)
		case customactions.PromptSelect:
			// Render as "‹  option  ›" centered-ish. The chevrons double
			// as click targets via handleMouse; we offset them inward
			// from the field edges so they don't sit on top of the
			// input border.
			for cx := fieldStart - 1; cx <= fieldEnd; cx++ {
				a.screen.SetContent(cx, inputRow, ' ', nil, inputStyle)
			}
			drawAt(a.screen, fieldStart, inputRow, "<", inputStyle)
			drawAt(a.screen, fieldEnd-1, inputRow, ">", inputStyle)
			opt := ""
			if row.selIdx >= 0 && row.selIdx < len(p.Options) {
				opt = p.Options[row.selIdx]
			}
			startX := fieldStart + (fieldEnd-fieldStart-runeLen(opt))/2
			drawAt(a.screen, startX, inputRow, opt, inputStyle)
		}
	}

	cancel, submit := m.buttons(a)
	drawButton(a.screen, cancel.x, cancel.y, "[ Cancel ]", c.bg, a.theme.Text, false)
	drawButton(a.screen, submit.x, submit.y, "[ Submit ]", c.bg, a.theme.Accent, true)
}

// handleKey routes keystrokes while the form is open. Tab moves focus
// forward, Shift+Tab back. On a text row, printable runes / arrow keys /
// Backspace edit the buffer. On a select row, Left / Right cycle
// options. Enter on the last row submits; Enter on any other row
// advances focus (so a user racing through the form can hold Tab/Enter
// to fill it out).
func (m *formModal) handleKey(a *App, ev *tcell.EventKey) {
	if len(m.rows) == 0 {
		return
	}
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeModal()
		return
	case tcell.KeyTab:
		m.moveFocus(+1)
		return
	case tcell.KeyBacktab:
		m.moveFocus(-1)
		return
	case tcell.KeyEnter:
		if m.focus == len(m.rows)-1 {
			m.submit(a)
			return
		}
		m.moveFocus(+1)
		return
	}

	row := &m.rows[m.focus]
	switch row.prompt.Type {
	case customactions.PromptSelect:
		m.handleSelectKey(ev, row)
	case customactions.PromptText:
		if _, edited := row.field.handleKey(ev); edited {
			m.values[row.prompt.Key] = row.field.String()
		}
	}
}

// handleSelectKey advances or retreats the select's option index on
// Left/Up and Right/Down, wrapping at the ends, and mirrors the choice
// into values so render and submit stay in sync.
func (m *formModal) handleSelectKey(ev *tcell.EventKey, row *formRow) {
	p := row.prompt
	switch ev.Key() {
	case tcell.KeyLeft, tcell.KeyUp:
		row.selIdx--
		if row.selIdx < 0 {
			row.selIdx = len(p.Options) - 1
		}
		m.values[p.Key] = p.Options[row.selIdx]
	case tcell.KeyRight, tcell.KeyDown:
		row.selIdx = (row.selIdx + 1) % len(p.Options)
		m.values[p.Key] = p.Options[row.selIdx]
	}
}

// moveFocus shifts the focused row by delta and wraps. Wrapping matches
// every list-with-keyboard surface in the editor (action menu, finder,
// this modal) so users never hit a "stuck at the top" surprise.
func (m *formModal) moveFocus(delta int) {
	n := len(m.rows)
	if n == 0 {
		return
	}
	i := (m.focus + delta) % n
	if i < 0 {
		i += n
	}
	m.focus = i
}

// handleMouse routes mouse clicks: a click on a prompt row's label or
// input area moves focus there; clicks on a select's < or > chevrons
// cycle the option; clicks on Submit / Cancel resolve the modal; clicks
// outside dismiss it.
func (m *formModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	if btn&tcell.Button1 == 0 {
		return
	}
	mx, my, mw, mh := m.rect(a)
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		a.closeModal()
		return
	}

	cancel, submit := m.buttons(a)
	switch {
	case cancel.contains(x, y):
		a.closeModal()
		return
	case submit.contains(x, y):
		m.submit(a)
		return
	}

	for i := range m.rows {
		row := &m.rows[i]
		p := row.prompt
		labelRow, inputRow, fieldStart, fieldEnd := m.rowSpan(a, i)
		if y != labelRow && y != inputRow {
			continue
		}
		m.focus = i

		if y == inputRow && p.Type == customactions.PromptSelect {
			switch x {
			case fieldStart:
				row.selIdx--
				if row.selIdx < 0 {
					row.selIdx = len(p.Options) - 1
				}
				m.values[p.Key] = p.Options[row.selIdx]
			case fieldEnd - 1:
				row.selIdx = (row.selIdx + 1) % len(p.Options)
				m.values[p.Key] = p.Options[row.selIdx]
			}
			return
		}
		if y == inputRow && p.Type == customactions.PromptText {
			row.field.clickAt(fieldStart, fieldEnd, x)
		}
		return
	}
}
