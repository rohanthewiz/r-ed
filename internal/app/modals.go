// =============================================================================
// File: internal/app/modals.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// modals.go houses the editor's small secondary modals: a single-line text
// prompt (used for Rename and New File), a Yes/No confirmation (used for
// Delete, and in "info" flavour for reporting command output), the
// Save/Discard/Cancel unsaved-changes dialog, and the right-click context
// menu over the file tree. Each implements the modal interface from
// modal.go and owns its own state; App.modal holds whichever one is up.

package app

import (
	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/filetree"
)

// Layout constants for the secondary modals. Width is wide enough to hold a
// reasonably long filename or sentence; heights are fixed by the row layout
// in the draw functions below.
const (
	promptModalWidth   = 54
	promptModalHeight  = 9
	confirmModalWidth  = 54
	confirmModalHeight = 9

	// contextMenuWidth is fixed so the popup geometry stays predictable —
	// the labels are short enough to fit comfortably.
	contextMenuWidth = 19
)

// closeAllModals dismisses every overlay in one shot — the active modal,
// the main action menu, and the find bar — and parks any in-flight drag /
// auto-scroll state. openModal calls it first so overlays stay mutually
// exclusive and a stale drag from before the modal opened can't keep
// extending a selection underneath it.
func (a *App) closeAllModals() {
	a.menuOpen = false
	a.modal = nil
	a.findOpen = false
	a.findValue = nil
	a.findCursor = 0
	a.findScroll = 0
	a.hoveredMenuRow = -1
	a.dragMode = ""
	a.stopAutoScroll()
}

// anyModalOpen reports whether any overlay surface is on screen. Used by
// the main event router to short-circuit normal editor input. The find
// bar is included so a key/mouse handler can use this to know "is the
// user mid-task in some overlay surface".
func (a *App) anyModalOpen() bool {
	return a.menuOpen || a.modal != nil || a.findOpen
}

// modalChrome bundles the style set every modal draws with, so each
// draw method derives them once from the theme instead of re-deriving
// five styles inline.
type modalChrome struct {
	bg     tcell.Color
	bgSt   tcell.Style
	border tcell.Style
	title  tcell.Style
	muted  tcell.Style
	body   tcell.Style
}

// chrome builds the shared modal style set from the active theme.
func (a *App) chrome() modalChrome {
	bg := a.theme.LineHL
	return modalChrome{
		bg:     bg,
		bgSt:   tcell.StyleDefault.Background(bg).Foreground(a.theme.Text),
		border: tcell.StyleDefault.Background(bg).Foreground(a.theme.Subtle),
		title:  tcell.StyleDefault.Background(bg).Foreground(a.theme.Accent).Bold(true),
		muted:  tcell.StyleDefault.Background(bg).Foreground(a.theme.Muted),
		body:   tcell.StyleDefault.Background(bg).Foreground(a.theme.Text),
	}
}

// drawFrame paints the standard modal shell: filled background, border,
// title row with the trailing "esc" hint, and the divider under the
// title. Every titled modal opens its draw method with this.
func (c modalChrome) drawFrame(scr tcell.Screen, mx, my, mw, mh int, title string) {
	fillRect(scr, mx, my, mw, mh, c.bgSt)
	drawBorder(scr, mx, my, mw, mh, c.border)
	drawHDivider(scr, mx, my+2, mw, c.border)
	drawAt(scr, mx+1, my+1, " "+title, c.title)
	hint := "esc "
	drawAt(scr, mx+mw-1-runeLen(hint), my+1, hint, c.muted)
}

// centeredRect centers a w×h rectangle in the window, clamped to (0,0)
// so a modal taller/wider than a tiny terminal still starts on screen.
func (a *App) centeredRect(w, h int) (x, y, ww, hh int) {
	x = (a.width - w) / 2
	y = (a.height - h) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y, w, h
}

// -----------------------------------------------------------------------------
// Prompt modal (text input + OK / Cancel)
// -----------------------------------------------------------------------------

// promptModal is a single-line text input with OK / Cancel. callback
// runs with the trimmed value when the user confirms with Enter or
// clicks OK; an empty submit is ignored.
type promptModal struct {
	title    string
	hint     string
	field    textField
	callback func(*App, string)
}

// openPrompt shows a single-line text input modal. title is the heading,
// hint is a small subtitle (e.g. "in /path/to/folder"), initial pre-fills
// the input field.
func (a *App) openPrompt(title, hint, initial string, callback func(*App, string)) {
	a.openModal(&promptModal{
		title:    title,
		hint:     hint,
		field:    newTextField(initial),
		callback: callback,
	})
}

// submit runs the prompt's callback with the current value (trimmed of
// surrounding whitespace) and closes the modal. An empty value is rejected
// silently — the user can still cancel with Esc.
func (m *promptModal) submit(a *App) {
	value := trimSpace(m.field.String())
	if value == "" {
		return
	}
	a.closeModal()
	if m.callback != nil {
		m.callback(a, value)
	}
}

// handleKey processes keyboard input while the prompt modal is open:
// Enter submits; Esc cancels; everything else is standard single-line
// editing handled by the shared textField.
func (m *promptModal) handleKey(a *App, ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeModal()
	case tcell.KeyEnter:
		m.submit(a)
	default:
		m.field.handleKey(ev)
	}
}

// buttons returns the Cancel / OK button rects — the one geometry source
// both draw and handleMouse consume.
func (m *promptModal) buttons(a *App) (cancel, ok btnRect) {
	mx, my, _, _ := m.rect(a)
	return btnRect{x: mx + 14, y: my + 6, w: 10}, btnRect{x: mx + 30, y: my + 6, w: 8}
}

// fieldSpan returns the input row and its [start, end) columns.
func (m *promptModal) fieldSpan(a *App) (y, start, end int) {
	mx, my, mw, _ := m.rect(a)
	return my + 4, mx + 3, mx + mw - 3
}

// handleMouse processes mouse input while the prompt modal is open.
// Clicks on OK / Cancel run the corresponding action; clicks outside the
// modal cancel; clicks on the input field reposition the cursor.
func (m *promptModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	if btn&tcell.Button1 == 0 {
		return
	}
	mx, my, mw, mh := m.rect(a)
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		a.closeModal()
		return
	}
	cancel, ok := m.buttons(a)
	switch {
	case cancel.contains(x, y):
		a.closeModal()
		return
	case ok.contains(x, y):
		m.submit(a)
		return
	}
	fy, start, end := m.fieldSpan(a)
	if y == fy {
		m.field.clickAt(start, end, x)
	}
}

// rect returns the on-screen rectangle of the prompt modal, centered in
// the window.
func (m *promptModal) rect(a *App) (x, y, w, h int) {
	return a.centeredRect(promptModalWidth, promptModalHeight)
}

// draw renders the prompt modal: a centered box with a title row, a
// single-line text input, and a Cancel / OK button row.
//
// Rows (relY):
//
//	0   top border
//	1   title — "<title>   esc"
//	2   divider
//	3   hint (greyed)
//	4   input field    [ value ]
//	5   blank
//	6   buttons        [ Cancel ]   [  OK  ]
//	7   blank
//	8   bottom border
func (m *promptModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	c.drawFrame(a.screen, mx, my, mw, mh, m.title)

	if m.hint != "" {
		drawAt(a.screen, mx+2, my+3, m.hint, c.muted)
	}

	fy, start, end := m.fieldSpan(a)
	inputStyle := tcell.StyleDefault.Background(a.theme.BG).Foreground(a.theme.Text)
	m.field.draw(a.screen, fy, start, end, inputStyle, true)

	cancel, ok := m.buttons(a)
	drawButton(a.screen, cancel.x, cancel.y, "[ Cancel ]", c.bg, a.theme.Text, false)
	drawButton(a.screen, ok.x, ok.y, "[  OK  ]", c.bg, a.theme.Accent, true)
}

// -----------------------------------------------------------------------------
// Confirm modal (Yes / No, or single-OK "info" flavour)
// -----------------------------------------------------------------------------

// confirmModal is a Yes/No confirmation. callback runs only when the
// user picks Yes. cancelHook, when set, runs when the modal is dismissed
// any other way — No, Esc, or a click outside — so flows like the
// format-trust prompt can react to a negative answer. The info flag
// flips it into a single-button "OK" report (multi-line body in lines)
// for passive output like a failed custom action's stderr.
type confirmModal struct {
	title      string
	message    string
	hover      int // 0 = No (the safe default), 1 = Yes
	callback   func(*App)
	cancelHook func(*App)
	info       bool
	lines      []string
}

// openConfirm shows a Yes/No confirmation modal. message is the body text
// shown to the user; callback runs only when the user picks Yes. The default
// focus lands on No so an accidental Enter is harmless — important for
// destructive actions like Delete. The returned modal lets callers arm
// cancelHook (format-trust deny, format-install decline) without a
// side-channel field on App.
func (a *App) openConfirm(title, message string, callback func(*App)) *confirmModal {
	m := &confirmModal{title: title, message: message, callback: callback}
	a.openModal(m)
	return m
}

// openInfo opens the confirm modal in single-button "OK" flavour for
// passive reporting — most importantly, the full stderr from a failed
// custom action where the status-bar flash isn't enough room. lines is
// drawn one row per entry inside the modal body. Empty input falls
// back to a single "(no output captured)" line so the dialog never
// looks broken.
func (a *App) openInfo(title string, lines []string) {
	if len(lines) == 0 {
		lines = []string{"(no output captured)"}
	}
	a.openModal(&confirmModal{title: title, info: true, lines: lines})
}

// yes runs the confirm callback and closes the modal.
func (m *confirmModal) yes(a *App) {
	a.closeModal()
	if m.callback != nil {
		m.callback(a)
	}
}

// cancel dismisses the confirm modal without running the callback,
// firing cancelHook (if armed) after the close so the hook can open a
// follow-up modal without this one stomping it.
func (m *confirmModal) cancel(a *App) {
	a.closeModal()
	if m.cancelHook != nil {
		m.cancelHook(a)
	}
}

// handleKey processes keyboard input while the confirm modal is open.
// Left / Right toggle focus between [No] and [Yes]; Enter activates the
// focused button; Esc cancels.
func (m *confirmModal) handleKey(a *App, ev *tcell.EventKey) {
	if m.info {
		// Info modal has only one button. Any "I'm done" key dismisses;
		// cycling between buttons doesn't apply because there's only one.
		switch ev.Key() {
		case tcell.KeyEsc, tcell.KeyEnter, tcell.KeyTab:
			a.closeModal()
		}
		return
	}
	switch ev.Key() {
	case tcell.KeyEsc:
		m.cancel(a)
	case tcell.KeyEnter:
		if m.hover == 1 {
			m.yes(a)
		} else {
			m.cancel(a)
		}
	case tcell.KeyLeft, tcell.KeyTab:
		// Tab cycles between buttons; Left moves to No.
		if ev.Key() == tcell.KeyTab {
			m.hover = 1 - m.hover
		} else {
			m.hover = 0
		}
	case tcell.KeyRight:
		m.hover = 1
	}
}

// buttons returns the No / Yes button rects. The hit widths are a shade
// wider than the drawn labels on purpose — they preserve the historical
// click zones so a near-miss on a short label still lands.
func (m *confirmModal) buttons(a *App) (no, yes btnRect) {
	mx, my, _, _ := m.rect(a)
	return btnRect{x: mx + 14, y: my + 5, w: 8}, btnRect{x: mx + 28, y: my + 5, w: 10}
}

// okButton returns the single centered OK button rect used in info mode.
func (m *confirmModal) okButton(a *App) btnRect {
	mx, my, mw, mh := m.rect(a)
	return btnRect{x: mx + (mw-10)/2, y: my + mh - 3, w: 10}
}

// handleMouse processes mouse input for the confirm modal. Hovering
// a button highlights it; clicking it activates. Clicks outside the modal
// cancel.
func (m *confirmModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	mx, my, mw, mh := m.rect(a)
	if m.info {
		// Single OK button. Outside the modal dismisses too — same
		// convention as the rest of the modals.
		if btn&tcell.Button1 == 0 {
			return
		}
		if x < mx || x >= mx+mw || y < my || y >= my+mh {
			a.closeModal()
			return
		}
		if m.okButton(a).contains(x, y) {
			a.closeModal()
		}
		return
	}
	no, yes := m.buttons(a)
	// Hover tracking — works for any move, with a button bit set or not.
	switch {
	case no.contains(x, y):
		m.hover = 0
	case yes.contains(x, y):
		m.hover = 1
	}
	if btn&tcell.Button1 == 0 {
		return
	}
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		m.cancel(a)
		return
	}
	switch {
	case no.contains(x, y):
		m.cancel(a)
	case yes.contains(x, y):
		m.yes(a)
	}
}

// rect returns the on-screen rectangle of the confirm modal, centered in
// the window. In info mode the modal grows wider so a command's stderr
// lines fit without aggressive truncation, and taller to fit the line
// list — the chrome (border, title, divider, button row) takes 7 rows
// on top of the body.
func (m *confirmModal) rect(a *App) (x, y, w, h int) {
	w = confirmModalWidth
	h = confirmModalHeight
	if m.info {
		w = 84
		bodyRows := len(m.lines)
		if bodyRows < 1 {
			bodyRows = 1
		}
		h = bodyRows + 7
	}
	return a.centeredRect(w, h)
}

// draw renders the Yes/No modal (or the info flavour).
//
// Rows (relY):
//
//	0   top border
//	1   title — "<title>   esc"
//	2   divider
//	3   blank
//	4   message (centered)
//	5   buttons          [  No  ]    [  Yes  ]
//	6   blank
//	7   blank
//	8   bottom border
func (m *confirmModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	c.drawFrame(a.screen, mx, my, mw, mh, m.title)

	if m.info {
		// Info mode: left-aligned multi-line body + a single centered
		// OK button. The body is left-aligned because scp/ssh stderr
		// usually starts with file paths that read poorly when centered.
		for i, line := range m.lines {
			if runeLen(line) > mw-4 {
				line = string([]rune(line)[:mw-4])
			}
			drawAt(a.screen, mx+2, my+3+i, line, c.body)
		}
		ok := m.okButton(a)
		drawButton(a.screen, ok.x, ok.y, "[  OK  ]", c.bg, a.theme.Accent, true)
		a.screen.HideCursor()
		return
	}

	// Message — centered, truncated if too long.
	msg := m.message
	if runeLen(msg) > mw-4 {
		msg = msg[:mw-4]
	}
	drawAt(a.screen, mx+(mw-runeLen(msg))/2, my+4, msg, c.body)

	// Buttons. Default focus is No so an accidental Enter is non-destructive.
	no, yes := m.buttons(a)
	drawButton(a.screen, no.x, no.y, "[  No  ]", c.bg, a.theme.Text, m.hover == 0)
	drawButton(a.screen, yes.x, yes.y, "[ Yes ]", c.bg, a.theme.Error, m.hover == 1)

	a.screen.HideCursor()
}

// -----------------------------------------------------------------------------
// Save / Discard / Cancel modal (unsaved-changes prompt)
// -----------------------------------------------------------------------------

// dirtyModalWidth and dirtyModalHeight pin the unsaved-changes modal's
// geometry. Wider than the Yes/No confirm so the three buttons sit
// comfortably on one row with breathing space between them.
const (
	dirtyModalWidth  = 60
	dirtyModalHeight = 9
)

// dirtyModal is the unsaved-changes dialog. saveCB runs when the user
// picks Save (typically: save the tab(s), then proceed); discardCB runs
// when the user picks Discard (skip saving, proceed anyway). Cancel just
// dismisses without running anything. hover indexes the button row:
// 0 = Cancel (safe default for an accidental Enter), 1 = Discard, 2 = Save.
type dirtyModal struct {
	title     string
	message   string
	hover     int
	saveCB    func(*App)
	discardCB func(*App)
}

// openDirtyClose shows the unsaved-changes modal. Default focus is Cancel
// so a stray Enter is non-destructive — same safety pattern the delete
// confirm uses.
func (a *App) openDirtyClose(title, message string, saveCB, discardCB func(*App)) {
	a.openModal(&dirtyModal{title: title, message: message, saveCB: saveCB, discardCB: discardCB})
}

// discard runs the discard callback and dismisses the modal.
func (m *dirtyModal) discard(a *App) {
	a.closeModal()
	if m.discardCB != nil {
		m.discardCB(a)
	}
}

// save runs the save callback and dismisses the modal.
func (m *dirtyModal) save(a *App) {
	a.closeModal()
	if m.saveCB != nil {
		m.saveCB(a)
	}
}

// activate runs the focused button's action — used by Enter and by
// keyboard-driven activations.
func (m *dirtyModal) activate(a *App) {
	switch m.hover {
	case 0:
		a.closeModal()
	case 1:
		m.discard(a)
	case 2:
		m.save(a)
	}
}

// handleKey processes keyboard input while the dirty-close modal is
// open. Left/Right and Tab cycle focus across the three buttons; Enter
// activates the focused button; Esc cancels.
func (m *dirtyModal) handleKey(a *App, ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeModal()
	case tcell.KeyEnter:
		m.activate(a)
	case tcell.KeyLeft:
		if m.hover > 0 {
			m.hover--
		}
	case tcell.KeyRight:
		if m.hover < 2 {
			m.hover++
		}
	case tcell.KeyTab:
		m.hover = (m.hover + 1) % 3
	}
}

// buttons returns the (Cancel, Discard, Save) button rects — the single
// geometry source for draw and hit-testing. The trio is spaced to
// center inside the 60-cell modal.
func (m *dirtyModal) buttons(a *App) [3]btnRect {
	mx, my, _, _ := m.rect(a)
	return [3]btnRect{
		{x: mx + 5, y: my + 5, w: 10},  // [ Cancel ]
		{x: mx + 22, y: my + 5, w: 11}, // [ Discard ]
		{x: mx + 42, y: my + 5, w: 8},  // [ Save ]
	}
}

// buttonAt maps a screen cell to a button index (0=Cancel, 1=Discard,
// 2=Save) or -1 when the cell misses every button.
func (m *dirtyModal) buttonAt(a *App, x, y int) int {
	for i, b := range m.buttons(a) {
		if b.contains(x, y) {
			return i
		}
	}
	return -1
}

// handleMouse processes mouse input for the dirty-close modal.
// Hovering a button highlights it; clicking activates. A click outside
// the modal cancels — same as the confirm modal.
func (m *dirtyModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	// Hover tracking — works for any move with a button bit set or not.
	if idx := m.buttonAt(a, x, y); idx >= 0 {
		m.hover = idx
	}
	if btn&tcell.Button1 == 0 {
		return
	}
	mx, my, mw, mh := m.rect(a)
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		a.closeModal()
		return
	}
	switch m.buttonAt(a, x, y) {
	case 0:
		a.closeModal()
	case 1:
		m.discard(a)
	case 2:
		m.save(a)
	}
}

// rect returns the on-screen rectangle of the dirty-close modal,
// centered in the window.
func (m *dirtyModal) rect(a *App) (x, y, w, h int) {
	return a.centeredRect(dirtyModalWidth, dirtyModalHeight)
}

// draw renders the Save / Discard / Cancel modal.
//
// Rows (relY):
//
//	0   top border
//	1   title — "<title>   esc"
//	2   divider
//	3   blank
//	4   message (centered)
//	5   buttons    [ Cancel ]    [ Discard ]    [ Save ]
//	6   blank
//	7   blank
//	8   bottom border
func (m *dirtyModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)
	c := a.chrome()
	c.drawFrame(a.screen, mx, my, mw, mh, m.title)

	// Message — centered, truncated if too long.
	msg := m.message
	if runeLen(msg) > mw-4 {
		msg = msg[:mw-4]
	}
	drawAt(a.screen, mx+(mw-runeLen(msg))/2, my+4, msg, c.body)

	// Buttons. Cancel is neutral, Discard is red (destructive),
	// Save is the editor's accent so it reads as the productive default.
	b := m.buttons(a)
	drawButton(a.screen, b[0].x, b[0].y, "[ Cancel ]", c.bg, a.theme.Text, m.hover == 0)
	drawButton(a.screen, b[1].x, b[1].y, "[ Discard ]", c.bg, a.theme.Error, m.hover == 1)
	drawButton(a.screen, b[2].x, b[2].y, "[ Save ]", c.bg, a.theme.Accent, m.hover == 2)

	a.screen.HideCursor()
}

// -----------------------------------------------------------------------------
// Tree right-click context menu
// -----------------------------------------------------------------------------

// contextItem is one row in the tree's right-click context menu. action runs
// against the node the menu was opened for; enabled gates whether the row is
// clickable.
type contextItem struct {
	label   string
	action  func(*App, *filetree.Node)
	enabled func(*App, *filetree.Node) bool
}

// contextModal is the small right-click popup over the file tree,
// anchored at (x, y) and acting on node.
type contextModal struct {
	x, y  int
	node  *filetree.Node
	items []contextItem
	hover int
}

// openTreeContext opens the context menu near (x, y) for node n. The items
// shown depend on whether n is a file or a folder. Renaming or deleting the
// project root is intentionally not allowed.
func (a *App) openTreeContext(n *filetree.Node, x, y int) {
	items := []contextItem{}
	if n.IsDir {
		items = append(items, contextItem{label: "New File", action: ctxNewFile})
	}
	if n != a.tree.Root {
		items = append(items, contextItem{label: "Rename", action: ctxRename})
		items = append(items, contextItem{label: "Delete", action: ctxDelete})
	}
	// Zip works on files, folders, and the root alike — the archive
	// is read-only on the source, so no root guard is needed.
	items = append(items, contextItem{label: "Zip", action: ctxZip})
	items = append(items, contextItem{label: "Copy rel path", action: ctxCopyRelativePath})
	items = append(items, contextItem{label: "Copy abs path", action: ctxCopyAbsolutePath})

	cx, cy := a.placeContext(x, y, len(items))
	a.openModal(&contextModal{x: cx, y: cy, node: n, items: items})
}

// placeContext picks an on-screen origin for the context menu. Anchors on
// the click point, but flips left or up when that would put part of the
// popup off-screen.
func (a *App) placeContext(x, y, count int) (int, int) {
	w := contextMenuWidth
	h := count + 2
	cx := x
	cy := y
	if cx+w > a.width {
		cx = x - w + 1
	}
	if cy+h > a.height {
		cy = y - h + 1
	}
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	return cx, cy
}

// rect returns the on-screen rectangle of the context menu.
func (m *contextModal) rect(a *App) (x, y, w, h int) {
	return m.x, m.y, contextMenuWidth, len(m.items) + 2
}

// handleKey processes keyboard input for the context menu.
func (m *contextModal) handleKey(a *App, ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeModal()
	case tcell.KeyDown:
		if m.hover < len(m.items)-1 {
			m.hover++
		}
	case tcell.KeyUp:
		if m.hover > 0 {
			m.hover--
		}
	case tcell.KeyEnter:
		m.activate(a)
	}
}

// handleMouse processes mouse input for the context menu. Hovering
// a row highlights it; clicking activates. Any click outside the popup
// dismisses it.
func (m *contextModal) handleMouse(a *App, x, y int, btn tcell.ButtonMask) {
	mx, my, mw, mh := m.rect(a)
	if x >= mx && x < mx+mw && y > my && y < my+mh-1 {
		m.hover = y - my - 1
	}
	if btn&tcell.Button1 == 0 {
		return
	}
	if x < mx || x >= mx+mw || y < my || y >= my+mh {
		a.closeModal()
		return
	}
	if y > my && y < my+mh-1 {
		m.hover = y - my - 1
		m.activate(a)
	}
}

// activate runs the currently highlighted context item against the
// node the menu was opened for.
func (m *contextModal) activate(a *App) {
	if m.hover < 0 || m.hover >= len(m.items) {
		return
	}
	item := m.items[m.hover]
	a.closeModal()
	if item.action != nil && m.node != nil {
		item.action(a, m.node)
	}
}

// draw renders the right-click context menu at its anchor.
func (m *contextModal) draw(a *App) {
	mx, my, mw, mh := m.rect(a)

	bg := a.theme.LineHL
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Text)
	borderStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Subtle)
	hoverBg := a.theme.Selection
	hoverStyle := tcell.StyleDefault.Background(hoverBg).Foreground(a.theme.Text).Bold(true)
	hoverChevStyle := tcell.StyleDefault.Background(hoverBg).Foreground(a.theme.AccentSoft).Bold(true)
	chevStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.AccentSoft)

	fillRect(a.screen, mx, my, mw, mh, bgStyle)
	drawBorder(a.screen, mx, my, mw, mh, borderStyle)

	for i, item := range m.items {
		cy := my + 1 + i
		hovered := i == m.hover
		if hovered {
			for cx := mx + 1; cx < mx+mw-1; cx++ {
				a.screen.SetContent(cx, cy, ' ', nil, hoverStyle)
			}
			drawAt(a.screen, mx+2, cy, "▸", hoverChevStyle)
			drawAt(a.screen, mx+4, cy, item.label, hoverStyle)
		} else {
			drawAt(a.screen, mx+2, cy, "▸", chevStyle)
			drawAt(a.screen, mx+4, cy, item.label, bgStyle)
		}
	}

	a.screen.HideCursor()
}

// -----------------------------------------------------------------------------
// Drawing helpers shared across modals.
// -----------------------------------------------------------------------------

// fillRect paints a rectangle of (w x h) cells starting at (x, y) with the
// given style.
func fillRect(scr tcell.Screen, x, y, w, h int, st tcell.Style) {
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, st)
		}
	}
}

// drawBorder draws a single-line box border around the rectangle.
func drawBorder(scr tcell.Screen, x, y, w, h int, st tcell.Style) {
	scr.SetContent(x, y, '┌', nil, st)
	scr.SetContent(x+w-1, y, '┐', nil, st)
	scr.SetContent(x, y+h-1, '└', nil, st)
	scr.SetContent(x+w-1, y+h-1, '┘', nil, st)
	for cx := x + 1; cx < x+w-1; cx++ {
		scr.SetContent(cx, y, '─', nil, st)
		scr.SetContent(cx, y+h-1, '─', nil, st)
	}
	for cy := y + 1; cy < y+h-1; cy++ {
		scr.SetContent(x, cy, '│', nil, st)
		scr.SetContent(x+w-1, cy, '│', nil, st)
	}
}

// drawHDivider draws a horizontal divider with ├ ┤ end caps inside an
// existing border.
func drawHDivider(scr tcell.Screen, x, y, w int, st tcell.Style) {
	scr.SetContent(x, y, '├', nil, st)
	scr.SetContent(x+w-1, y, '┤', nil, st)
	for cx := x + 1; cx < x+w-1; cx++ {
		scr.SetContent(cx, y, '─', nil, st)
	}
}

// drawButton renders a "button" — really just bracketed label — at (x, y).
// Active buttons get a tinted background so they read as the focused option.
func drawButton(scr tcell.Screen, x, y int, label string, modalBG tcell.Color, fg tcell.Color, focused bool) {
	bg := modalBG
	st := tcell.StyleDefault.Background(bg).Foreground(fg).Bold(true)
	if focused {
		// Focused button: invert — the label sits on a tinted block.
		st = tcell.StyleDefault.Background(fg).Foreground(modalBG).Bold(true)
	}
	col := 0
	for _, r := range label {
		scr.SetContent(x+col, y, r, nil, st)
		col++
	}
}

// runeLen returns the visible cell count of s (one cell per rune).
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// trimSpace strips ASCII whitespace from both ends of s. Tiny dependency-free
// substitute for strings.TrimSpace so this file doesn't grow imports.
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
