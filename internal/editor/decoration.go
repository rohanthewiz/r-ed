// =============================================================================
// File: internal/editor/decoration.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-08
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// decoration.go is the editor's span/overlay system. Decoration sources
// produce Spans (a buffer range plus a partial style override) and
// GutterMarks (a per-line glyph in the gutter's mark column); Render
// merges them over the base syntax grid each frame.
//
// The design exists so every "paint something over the code" feature —
// selection, find matches today; git diff marks and LSP diagnostics in
// later phases — goes through one merge path instead of each growing
// its own ad-hoc branch inside Render's inner loop (which is exactly
// how the pre-refactor selection/find code worked). A new overlay is a
// new DecorationSource, not a Render edit.
//
//	sources (in precedence order)          Render, per visible row
//	┌──────────────────────────┐
//	│ Tab.DecoSources (git,    │   spans   ┌──────────────────────────┐
//	│ LSP, … — phases 4/5)     ├──────────►│ base syntax styles       │
//	│ selectionSource (builtin)│           │ + line-highlight bg      │
//	│ findSource     (builtin) │   marks   │ + span deltas, in order  │
//	└──────────────────────────┘──────────►│ gutter mark column       │
//	                                       └──────────────────────────┘
//
// Later sources win where spans overlap, so the built-ins run last:
// user-interaction feedback (selection, find) must stay visible on top
// of ambient annotations like diagnostics.

package editor

import (
	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/r-ed/internal/theme"
)

// StyleDelta is a partial style override. Colors apply only when their
// Set* flag is true; attribute bits are OR-ed in. Deltas compose — a
// find-match background can land on top of an LSP underline without
// either knowing about the other — which is the property a plain
// tcell.Style replacement wouldn't give us.
type StyleDelta struct {
	FG, BG       tcell.Color
	SetFG, SetBG bool
	Bold         bool
	Italic       bool
	Underline    bool
}

// Apply layers the delta over st and returns the result.
func (d StyleDelta) Apply(st tcell.Style) tcell.Style {
	if d.SetFG {
		st = st.Foreground(d.FG)
	}
	if d.SetBG {
		st = st.Background(d.BG)
	}
	if d.Bold {
		st = st.Bold(true)
	}
	if d.Italic {
		st = st.Italic(true)
	}
	if d.Underline {
		st = st.Underline(true)
	}
	return st
}

// Span is one styled buffer range: [Start, End) in the same rune-indexed,
// half-open convention selections use, so producers can hand selection or
// match ranges straight through without off-by-one adjustment.
type Span struct {
	Start, End Position
	Delta      StyleDelta
}

// colRange projects the span onto one line, returning the [start, end)
// rune-column window it covers there. ok=false when the span doesn't
// touch the line at all. lineLen caps the window on interior and start
// lines of a multi-line span (the span conceptually covers the newline,
// but there's no cell to paint for it).
func (s Span) colRange(line, lineLen int) (start, end int, ok bool) {
	if line < s.Start.Line || line > s.End.Line {
		return 0, 0, false
	}
	start = 0
	if line == s.Start.Line {
		start = s.Start.Col
	}
	end = lineLen
	if line == s.End.Line && s.End.Col < end {
		end = s.End.Col
	}
	if start >= end {
		return 0, 0, false
	}
	return start, end, true
}

// GutterMark is a per-line glyph drawn in the mark column — the single
// cell between the line numbers and the code. One mark per line; when
// several sources mark the same line, the highest-precedence (latest)
// source wins, same rule as span styling.
type GutterMark struct {
	Line  int
	Glyph rune
	FG    tcell.Color
}

// DecorationSource produces spans and gutter marks for the visible line
// window [firstLine, lastLine]. Sources are consulted once per render,
// so producers whose data is expensive to compute (git diffs, LSP
// diagnostics) should cache upstream and treat this as a cheap read.
// The theme is passed in because Tab doesn't hold one — Render receives
// it per frame, and sources need it to pick their colors.
type DecorationSource interface {
	Decorations(t *Tab, th theme.Theme, firstLine, lastLine int) ([]Span, []GutterMark)
}

// collectDecorations gathers spans and marks from every source for the
// visible window. External sources (Tab.DecoSources) run first, the
// interaction built-ins last, so precedence is: syntax < external
// annotations < selection < find. See the file comment for why.
func (t *Tab) collectDecorations(th theme.Theme, firstLine, lastLine int) ([]Span, []GutterMark) {
	var spans []Span
	var marks []GutterMark
	sources := make([]DecorationSource, 0, len(t.DecoSources)+2)
	sources = append(sources, t.DecoSources...)
	sources = append(sources, selectionSource{}, findSource{})
	for _, src := range sources {
		s, m := src.Decorations(t, th, firstLine, lastLine)
		spans = append(spans, s...)
		marks = append(marks, m...)
	}
	return spans, marks
}

// -----------------------------------------------------------------------------
// Built-in sources — the two pre-existing overlays, migrated onto the
// span system to prove the design carries real weight.
// -----------------------------------------------------------------------------

// selectionSource emits one span covering the active selection.
type selectionSource struct{}

// Decorations returns the selection as a single background span, or
// nothing when no selection is active or it lies entirely off-screen.
func (selectionSource) Decorations(t *Tab, th theme.Theme, firstLine, lastLine int) ([]Span, []GutterMark) {
	if !t.HasSelection() {
		return nil, nil
	}
	start, end := PosOrdered(t.Anchor, t.Cursor)
	if end.Line < firstLine || start.Line > lastLine {
		return nil, nil
	}
	return []Span{{
		Start: start,
		End:   end,
		Delta: StyleDelta{SetBG: true, BG: th.Selection},
	}}, nil
}

// findSource emits one span per visible find match. The current match
// (FindIndex) gets the louder FindCurrent treatment — background plus
// inverted foreground — so the eye lands on it among the soft tints.
type findSource struct{}

// Decorations converts the tab's cached match list into spans, culled
// to the visible window so a 10k-match query doesn't cost anything for
// the 40 rows actually on screen.
func (findSource) Decorations(t *Tab, th theme.Theme, firstLine, lastLine int) ([]Span, []GutterMark) {
	if len(t.FindMatches) == 0 {
		return nil, nil
	}
	var spans []Span
	for i, m := range t.FindMatches {
		if m.Line < firstLine || m.Line > lastLine {
			continue
		}
		delta := StyleDelta{SetBG: true, BG: th.FindMatch}
		if i == t.FindIndex {
			delta = StyleDelta{SetBG: true, BG: th.FindCurrent, SetFG: true, FG: th.BG}
		}
		spans = append(spans, Span{
			Start: Position{Line: m.Line, Col: m.Col},
			End:   Position{Line: m.Line, Col: m.Col + m.Width},
			Delta: delta,
		})
	}
	return spans, nil
}
