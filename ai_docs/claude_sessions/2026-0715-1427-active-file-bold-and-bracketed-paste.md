# Session: Bold the active file in the tree + bracketed paste (keep pasted formatting)

Session ID: fe2dd54a-be84-419d-b05a-56fc9bba8548
Date: 2026-07-15

## Goals

Two independent asks in one session:

1. "In the file tree, bold the name of the currently active file in the
   editor."
2. Separate issue: "pasted text is not maintaining line formatting."

Both shipped and committed on `main` (not pushed — pushing `main` fires
the release workflow; the user only asked to commit).

---

## 1. Bold the active file in the tree — commit `196182e`

### Approach

Mirror the existing `ActiveFolder` pattern, but for the file open in the
active tab, and sync it at **render time** instead of threading a setter
through every tab-switch path.

- `filetree.Tree.ActiveFile string` — absolute path of the active tab's
  file. Empty when no file is open.
- `Tree.Render` flags a **file** row `activeFile` when
  `!IsDir && ActiveFile != "" && Node.Path == ActiveFile` (the non-empty
  guard stops `""` lighting up an arbitrary row).
- `drawNodeRow` gained an `activeFile` param; when set it adds
  `Bold(true)` to **both** the name style and the per-language glyph
  style — but keeps the row's own fg (file/dirty/muted), deliberately
  NOT the folder's Accent tint, so "active folder" and "active file"
  stay visually distinct. `active` (folder-only) and `activeFile`
  (file-only) are mutually exclusive by construction.
- `app.draw()` re-syncs `a.tree.ActiveFile` from `activeTabPtr()` on
  every frame (empty when nil). Syncing at draw means the bold row is
  correct through **every** path that changes the active tab — open,
  tab-bar click, close, nav back/forward — with zero per-surface push
  calls to maintain.

Tab paths and tree node paths are both absolutized elsewhere in the
codebase, so `Node.Path == ActiveFile` matches directly.

### Files

- `internal/filetree/filetree.go` — `Tree.ActiveFile`, `Render`
  detection, `drawNodeRow` param + bold on rowStyle/glyphStyle.
- `internal/filetree/filetree_test.go` — `TestRender_ActiveFileIsBold`
  (active file bold, sibling not), `TestRender_NoActiveFileNoBoldFile`
  (empty path bolds nothing). Reused existing `findRowY` / `rowHasBold`
  helpers.
- `internal/app/app.go` — the draw-time sync block before
  `a.tree.Render`.

---

## 2. Pasted text losing line formatting — commit `c1c9c92`

### Root cause

The editor **never enabled bracketed paste**, so a terminal paste came
in as raw key events. Two things then mangled it:

- Each pasted **tab** hit `handleKey`'s `case tcell.KeyTab:
  tab.InsertString(tab.IndentUnit)` — so tabs got rewritten to the
  buffer's indent unit (spaces, when the file is space-indented).
  Pasted code lost its exact indentation.
- Every rune ran through the leader / shortcut / Alt-Meta machinery.

There was no auto-indent-on-Enter (Enter just inserts `"\n"`), so line
**breaks** already survived — the damage was specifically to leading
whitespace / tabs.

### tcell bracketed-paste model (v2.13.9)

`scr.EnablePaste()` writes `\x1b[?2004h`. The terminal then wraps a paste
as `EventPaste{start=true}` … content **as ordinary EventKey events**
(runes, `KeyEnter` for newlines, `KeyTab` for tabs) … `EventPaste{end}`.
The `EventPaste.data []byte` field is unexported with no accessor and is
always empty — content is ONLY the interior key events, so you must
accumulate them yourself.

### Fix (new file `internal/app/textpaste.go`)

- `scr.EnablePaste()` added next to `scr.EnableMouse` in `App.New`.
- App state: `pasting bool`, `pasteBuf []rune`.
- Event loop: new `case *tcell.EventPaste: a.handlePaste(e)`.
- `handleKey` top: `if a.pasting { a.accumulatePaste(ev); return }` —
  diverts interior key events verbatim before any shortcut handling.
- `handlePaste(start)` arms accumulation **only if**
  `editorPasteTarget() != nil`; `handlePaste(end)` flushes the buffer
  into that tab with a single `tab.InsertString(string(pasteBuf))`.
- `accumulatePaste`: `KeyRune→rune`, `KeyEnter→'\n'`, `KeyTab→'\t'`,
  everything else dropped.
- `editorPasteTarget() *editor.Tab` — the single source of truth for
  "is the editor the paste target": nil when a modal / find bar / menu /
  focused terminal owns the keyboard, or the active tab is missing or an
  image preview; the tab otherwise. Used to **both** arm and flush, so
  the two can't disagree.

### Why editor-only gating (design)

Only the editor is gated. When something else owns the keyboard,
`pasting` stays false and the interior key events flow through
`handleKey` exactly as before — so single-line inputs (modal, find,
terminal input) are unaffected: **no regression**. Terminals that don't
understand `?2004h` never send the markers, so paste there arrives as
raw keys too — the fix degrades silently, same contract as the LSP /
formatter layers. Verbatim single-insert also makes a paste **one undo
step**. The focused terminal keeps its own Cmd+V (`termPasteClip`) path.

### Files

- `internal/app/textpaste.go` — new; `handlePaste`, `accumulatePaste`,
  `editorPasteTarget` + the design doc block.
- `internal/app/textpaste_test.go` — new; drives the real event path via
  a `feedPaste` helper (start marker → per-rune key events → end marker,
  through `handleKey`):
  - `TestPaste_PreservesLineFormatting` — multi-line indented Go blob
    round-trips byte-for-byte.
  - `TestPaste_TabStaysLiteral` — pasted `\t` stays literal with
    `IndentUnit="    "`; a **contrast** Tab typed outside the paste
    still expands to IndentUnit, proving the difference is the gate.
  - `TestPaste_SingleUndoStep` — one `Undo()` clears the whole paste.
  - `TestPaste_EmptyPasteNoop`, `TestEditorPasteTarget_Gating`
    (tab / focused-terminal / modal / no-tab), `TestPaste_NotGatedWhenModalOpen`.
- `internal/app/app.go` — `EnablePaste()` call, `pasting`/`pasteBuf`
  fields, event-loop case, `handleKey` gate.

---

## Verification

- `go build ./...`, `go vet ./internal/app/`, full `go test ./...`
  (13 pkgs, race detector) all green.
- New diagnostics seen during editing were all **pre-existing** gopls
  "modernize" hints (`minmax`, `rangeint`, `stringsseq`, `forvar`,
  `unusedparams`) whose line numbers merely shifted under the edits —
  none introduced. New test loops match each file's existing idiom.

## Notes / follow-ups (not done)

- Bracketed paste into **modals / find / terminal** still replays as raw
  keys (multi-line paste into a single-line field would fire Enter per
  line). Out of scope this session; could later route those through a
  `textField.insert(flatten(text))`.
- Image-tab gating in `editorPasteTarget` is covered structurally but not
  unit-tested (its `imageMode` constant is unexported to the app pkg).
- `verify` skill not run: driving a real bracketed paste needs a
  terminal emulator sending `\x1b[200~…`; the tests reproduce the exact
  tcell event sequence instead.
