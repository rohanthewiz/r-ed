// =============================================================================
// File: internal/editor/lineops_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-11
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

package editor

import (
	"strings"
	"testing"
)

// lineOpsTab builds an in-memory tab over the given lines with the
// cursor parked at the origin — the shared fixture for line-op tests.
func lineOpsTab(t *testing.T, content string) *Tab {
	t.Helper()
	tab := &Tab{Buffer: NewBuffer(content)}
	tab.initUndo()
	return tab
}

// TestDuplicateLines_SingleLine pins the basic gesture: the cursor's
// line is copied below it and the cursor rides down onto the copy, so
// repeated presses stack copies.
func TestDuplicateLines_SingleLine(t *testing.T) {
	tab := lineOpsTab(t, "alpha\nbravo\ncharlie")
	tab.Cursor = Position{Line: 1, Col: 3}
	tab.Anchor = tab.Cursor

	tab.DuplicateLines()
	if got := tab.Buffer.String(); got != "alpha\nbravo\nbravo\ncharlie" {
		t.Fatalf("duplicate: got %q", got)
	}
	if tab.Cursor != (Position{Line: 2, Col: 3}) {
		t.Fatalf("cursor should ride down onto the copy, got %+v", tab.Cursor)
	}
	if !tab.Dirty || tab.EditRev == 0 {
		t.Fatal("duplicate must mark the buffer dirty and bump EditRev")
	}
}

// TestDuplicateLines_SelectionBlock verifies a multi-line selection
// duplicates every touched line as one block and the selection moves
// down onto the copy.
func TestDuplicateLines_SelectionBlock(t *testing.T) {
	tab := lineOpsTab(t, "a\nb\nc\nd")
	tab.Anchor = Position{Line: 0, Col: 1}
	tab.Cursor = Position{Line: 1, Col: 1}

	tab.DuplicateLines()
	if got := tab.Buffer.String(); got != "a\nb\na\nb\nc\nd" {
		t.Fatalf("block duplicate: got %q", got)
	}
	if tab.Anchor.Line != 2 || tab.Cursor.Line != 3 {
		t.Fatalf("selection should move onto the copy, got anchor %+v cursor %+v", tab.Anchor, tab.Cursor)
	}
	// One structural undo step restores the original.
	if !tab.Undo() || tab.Buffer.String() != "a\nb\nc\nd" {
		t.Fatalf("undo should restore original, got %q", tab.Buffer.String())
	}
}

// TestMoveLines_UpAndDown pins the block shift in both directions,
// including cursor/anchor travelling with the block.
func TestMoveLines_UpAndDown(t *testing.T) {
	tab := lineOpsTab(t, "a\nb\nc")
	tab.Cursor = Position{Line: 1}
	tab.Anchor = tab.Cursor

	if !tab.MoveLines(1) {
		t.Fatal("move down should succeed")
	}
	if got := tab.Buffer.String(); got != "a\nc\nb" {
		t.Fatalf("move down: got %q", got)
	}
	if tab.Cursor.Line != 2 {
		t.Fatalf("cursor should travel with the line, got line %d", tab.Cursor.Line)
	}
	if !tab.MoveLines(-1) || tab.Buffer.String() != "a\nb\nc" {
		t.Fatalf("move back up should restore order, got %q", tab.Buffer.String())
	}
}

// TestMoveLines_EdgesAreNoOps verifies pushing a block past either end
// of the buffer refuses without mutating anything.
func TestMoveLines_EdgesAreNoOps(t *testing.T) {
	tab := lineOpsTab(t, "a\nb")
	tab.Cursor = Position{Line: 0}
	tab.Anchor = tab.Cursor
	if tab.MoveLines(-1) {
		t.Fatal("moving the first line up must refuse")
	}
	tab.Cursor = Position{Line: 1}
	tab.Anchor = tab.Cursor
	if tab.MoveLines(1) {
		t.Fatal("moving the last line down must refuse")
	}
	if tab.Dirty || tab.EditRev != 0 {
		t.Fatal("refused moves must not dirty the buffer")
	}
}

// TestMoveLines_SelectionBlockCoalescedUndo moves a two-line selection
// down twice and verifies (a) the block moves as a unit with the
// selection intact, and (b) the nudge burst undoes in a single step —
// the undoGroupLineMove coalescing contract.
func TestMoveLines_SelectionBlockCoalescedUndo(t *testing.T) {
	tab := lineOpsTab(t, "a\nb\nc\nd")
	tab.Anchor = Position{Line: 0}
	tab.Cursor = Position{Line: 1, Col: 1}

	tab.MoveLines(1)
	tab.MoveLines(1)
	if got := tab.Buffer.String(); got != "c\nd\na\nb" {
		t.Fatalf("double move down: got %q", got)
	}
	if tab.Anchor.Line != 2 || tab.Cursor.Line != 3 {
		t.Fatalf("selection should travel with the block, got anchor %+v cursor %+v", tab.Anchor, tab.Cursor)
	}
	if !tab.Undo() {
		t.Fatal("undo should have something to do")
	}
	if got := tab.Buffer.String(); got != "a\nb\nc\nd" {
		t.Fatalf("one undo should revert the whole nudge burst, got %q", got)
	}
	if tab.CanUndo() {
		t.Fatal("burst should have been a single coalesced undo step")
	}
}

// TestLineOps_ImageTabsAreNoOps verifies both gestures short-circuit on
// read-only image tabs, matching every other mutating Tab method.
func TestLineOps_ImageTabsAreNoOps(t *testing.T) {
	tab := lineOpsTab(t, "a\nb")
	tab.Mode = imageMode
	tab.DuplicateLines()
	if tab.MoveLines(1) {
		t.Fatal("MoveLines on an image tab must refuse")
	}
	if tab.Buffer.String() != "a\nb" || tab.Dirty {
		t.Fatalf("image tab buffer must stay untouched, got %q", tab.Buffer.String())
	}
}

// TestSelectedLineRange normalises a backwards (cursor-above-anchor)
// selection into an ascending line span.
func TestSelectedLineRange(t *testing.T) {
	tab := lineOpsTab(t, strings.Repeat("x\n", 5))
	tab.Anchor = Position{Line: 4}
	tab.Cursor = Position{Line: 2}
	first, last := tab.selectedLineRange()
	if first != 2 || last != 4 {
		t.Fatalf("want [2,4], got [%d,%d]", first, last)
	}
}
