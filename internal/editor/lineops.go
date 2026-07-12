// =============================================================================
// File: internal/editor/lineops.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// lineops.go implements whole-line editing gestures: duplicate the
// current line (or every line the selection touches) and shift that
// same block up or down. Both operate on line blocks rather than
// character ranges because that's the editing intent behind the keys
// bound to them (Ctrl-D / Alt-Up / Alt-Down): "work with this line",
// regardless of where in the line the caret happens to sit.

package editor

// selectedLineRange returns the inclusive [first, last] span of lines
// the selection touches, or the cursor's line twice when nothing is
// selected. Line-block operations share this so they all agree on what
// "the current lines" means.
func (t *Tab) selectedLineRange() (first, last int) {
	first, last = t.Anchor.Line, t.Cursor.Line
	if first > last {
		first, last = last, first
	}
	return first, last
}

// DuplicateLines copies the selected line block (or the cursor's line)
// and inserts the copy directly below the block. Cursor, anchor — and
// with them any selection — shift down onto the copy, so repeated
// presses stack copies the way other editors do. Always a structural
// undo step: duplicating is an explicit gesture, never part of a
// typing burst. No-op on image tabs.
func (t *Tab) DuplicateLines() {
	if t.IsImage() {
		return
	}
	first, last := t.selectedLineRange()
	t.pushUndo(undoGroupStructural)

	n := last - first + 1
	block := make([]string, n)
	copy(block, t.Buffer.Lines[first:last+1])
	// append(block, tail...) allocates a fresh slice (block is at full
	// capacity), so the outer append can safely overwrite the tail
	// region of the original backing array.
	t.Buffer.Lines = append(t.Buffer.Lines[:last+1], append(block, t.Buffer.Lines[last+1:]...)...)

	t.Cursor.Line += n
	t.Anchor.Line += n
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	t.EditRev++
}

// MoveLines shifts the selected line block (or the cursor's line) one
// line up (delta = -1) or down (delta = +1), carrying cursor, anchor,
// and selection along with it. Returns false without touching the
// buffer when the block is already pressed against the edge it's being
// pushed toward. Consecutive moves coalesce into a single undo step —
// nudging a line five rows down should undo in one hop, not five.
func (t *Tab) MoveLines(delta int) bool {
	if t.IsImage() || (delta != -1 && delta != 1) {
		return false
	}
	first, last := t.selectedLineRange()
	if delta < 0 && first == 0 {
		return false
	}
	if delta > 0 && last >= t.Buffer.LineCount()-1 {
		return false
	}
	t.pushUndo(undoGroupLineMove)

	lines := t.Buffer.Lines
	if delta < 0 {
		// The line above hops over the block to sit below it. copy is
		// memmove-safe, so the overlapping shift is fine.
		moved := lines[first-1]
		copy(lines[first-1:last], lines[first:last+1])
		lines[last] = moved
	} else {
		// The line below hops over the block to sit above it.
		moved := lines[last+1]
		copy(lines[first+1:last+2], lines[first:last+1])
		lines[first] = moved
	}

	t.Cursor.Line += delta
	t.Anchor.Line += delta
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	t.EditRev++
	return true
}
