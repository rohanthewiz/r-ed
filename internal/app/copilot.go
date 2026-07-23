// =============================================================================
// File: internal/app/copilot.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// copilot.go is phase 1 of the GitHub Copilot integration: sidecar
// lifecycle and device-flow authentication. It runs the official
// copilot-language-server binary (a native executable GitHub ships for
// every platform r-ed targets) over the same JSON-RPC transport gopls
// uses — internal/lsp's Client is protocol-generic, so no new
// dependency and no second framing layer.
//
// House rules, inherited from the LSP/formatter subsystems:
//
//   - Silent degradation: no binary on PATH, crash, failed handshake —
//     the editor works normally and nothing nags. Installing the binary
//     is the opt-in; the "copilot" config key is the opt-out.
//   - Events only: the spawn/handshake, sign-in calls, and the server's
//     status notifications all run off-loop and post copilot*Events;
//     only the main loop touches App.copilot.
//   - Menu-first: sign in/out and enable/disable live in the ≡ menu.
//
// The sign-in dance (GitHub's device flow, driven via custom LSP-style
// methods the server defines):
//
//	≡ Sign in ──► signIn ──► {userCode, verificationUri, command}
//	                              │
//	   confirm modal shows code ◄─┘  Yes → code copied to clipboard,
//	                                       browser opened, and
//	   workspace/executeCommand(command) — BLOCKS (minutes) until the
//	   user finishes in the browser → copilotSignInDoneEvent
//
// The blocking confirmation is why lsp.Client grew CallWithTimeout: the
// standard 5s budget is three orders of magnitude too short here. While
// the wait is in flight the device code stays visible in the status bar
// (the modal is gone by then, and a code the user can no longer read is
// a dead end).
//
// Credentials are stored by the server itself (shared with Copilot in
// other editors), so sign-in is once per machine, not per session.

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rohanthewiz/r-ed/internal/clipboard"
	"github.com/rohanthewiz/r-ed/internal/editor"
	"github.com/rohanthewiz/r-ed/internal/lsp"
	"github.com/rohanthewiz/r-ed/internal/userconfig"
	"github.com/rohanthewiz/r-ed/internal/version"
)

// copilotServerBinary is the sidecar this integration runs. GitHub
// distributes it as prebuilt native binaries (no Node runtime), which
// is what keeps this compatible with r-ed's single-static-binary
// philosophy: like gopls, it's an external tool we find on PATH, never
// a build dependency.
const copilotServerBinary = "copilot-language-server"

// copilotSignInTimeout bounds the blocking device-flow confirmation.
// GitHub's device codes are valid for ~15 minutes; matching that means
// we never give up on a code the user could still redeem.
const copilotSignInTimeout = 15 * time.Minute

// copilotFallbackVerifyURL is where device codes are redeemed when the
// server's response omits verificationUri (older builds did).
const copilotFallbackVerifyURL = "https://github.com/login/device"

// copilotCopyCode and copilotOpenBrowser are the sign-in flow's two
// side-effects on the host machine, as stubbable vars (the
// builtinCommandsFor pattern): tests must never write the dev machine's
// clipboard or pop a real browser.
var (
	copilotCopyCode    = clipboard.CopyToSystem
	copilotOpenBrowser = openBrowser
)

// copilotConn is the slice of lsp.Client the Copilot layer uses. Unlike
// lspConn it's the generic call surface, not named wrappers — every
// Copilot method is custom, so wrapping each one would just be noise.
// An interface so tests substitute a scripted fake.
type copilotConn interface {
	Call(method string, params, result any) error
	CallWithTimeout(method string, params, result any, timeout time.Duration) error
	Notify(method string, params any) error
	Close()
}

// copilotState is everything the Copilot integration remembers, owned
// by App and mutated only on the main loop.
type copilotState struct {
	client   copilotConn
	starting bool // async spawn + handshake in flight
	dead     bool // unavailable: no binary, crashed, or failed to start

	// enabled mirrors the persisted "copilot" config key. Distinct from
	// dead: disabled is the user's choice and survives in config; dead
	// is this session's verdict about the environment.
	enabled bool

	signedIn bool
	user     string // GitHub login when signed in

	// signInWanted queues a sign-in requested while the async start was
	// still in flight; handleCopilotReady honours it. Without this the
	// very first "Sign in" click would answer "starting, try again" —
	// a papercut on the one interaction every new user hits.
	signInWanted bool

	// pendingCode is the device code awaiting browser confirmation,
	// shown in the status bar for the whole wait — the modal that first
	// displayed it is gone the moment the user clicks Yes.
	pendingCode string

	// statusKind/statusMsg mirror the server's last didChangeStatus
	// notification ("Normal", "Warning", "Error", "Inactive").
	statusKind string
	statusMsg  string

	// ---- Phase 2: ghost-text inline completions (copilot_ghost.go) ----

	// suggest mirrors the persisted "suggestions" config key: whether
	// ghost text is requested and painted at all. Independent of
	// enabled so the sidecar can stay up (sign-in today, chat later)
	// with just the ghost text opted out.
	suggest bool

	// docVersions/docSyncedRev are the per-path didChange version
	// counter and the Tab.EditRev last pushed — the same bookkeeping
	// pair lspState keeps, but flushed lazily at request time.
	docVersions  map[string]int
	docSyncedRev map[string]int

	// armRev remembers the per-path EditRev the completion debounce was
	// last armed for, so only fresh edits (never cursor travel or
	// repeated dispatch-tail passes) restart the countdown.
	armRev map[string]int

	// compTimer is the single completion debounce; reqSeq stamps each
	// request so only the newest response may paint.
	compTimer *time.Timer
	reqSeq    int

	// The visible ghost's accept bookkeeping: which document/revision/
	// cursor it belongs to, the parsed item (range + insertText), and
	// the raw item JSON echoed back in telemetry.
	ghostPath string
	ghostRev  int
	ghostPos  editor.Position
	ghostItem *copilotInlineItem
	ghostRaw  json.RawMessage
}

// -----------------------------------------------------------------------------
// Wire shapes — the server's custom auth methods
// -----------------------------------------------------------------------------

// copilotAuthStatus is the result shape shared by checkStatus and the
// sign-in confirmation: the auth state plus the GitHub login.
type copilotAuthStatus struct {
	Status string `json:"status"` // "OK", "NotSignedIn", "NotAuthorized", ...
	User   string `json:"user"`
}

// signedIn interprets the server's status string. Only "OK"-family
// values mean a usable session; anything unrecognised is treated as
// signed out, which fails safe (the user just signs in again).
func (s copilotAuthStatus) signedIn() bool {
	switch s.Status {
	case "OK", "AlreadySignedIn", "MaybeOk":
		return true
	}
	return false
}

// copilotCommand is the LSP Command object signIn hands back; the
// client echoes it verbatim through workspace/executeCommand to
// confirm. Arguments stay raw — their shape is the server's business.
type copilotCommand struct {
	Title     string            `json:"title"`
	Command   string            `json:"command"`
	Arguments []json.RawMessage `json:"arguments"`
}

// copilotSignInResult is signIn's response: either an already-signed-in
// status, or the device-flow triple (code, URL, confirm command).
type copilotSignInResult struct {
	Status          string         `json:"status"`
	User            string         `json:"user"`
	UserCode        string         `json:"userCode"`
	VerificationURI string         `json:"verificationUri"`
	Command         copilotCommand `json:"command"`
}

// -----------------------------------------------------------------------------
// Custom tcell events — the goroutine → main-loop bridge
// -----------------------------------------------------------------------------

// copilotReadyEvent is posted once the async spawn + handshake
// completes; it carries the live connection and the initial auth state.
type copilotReadyEvent struct {
	when   time.Time
	client copilotConn
	status copilotAuthStatus
}

// When satisfies the tcell.Event interface.
func (e *copilotReadyEvent) When() time.Time { return e.when }

// copilotExitEvent is posted when the server dies or fails to start.
type copilotExitEvent struct {
	when time.Time
}

// When satisfies the tcell.Event interface.
func (e *copilotExitEvent) When() time.Time { return e.when }

// copilotStatusEvent carries a didChangeStatus notification from the
// client's read loop to the main loop.
type copilotStatusEvent struct {
	when    time.Time
	kind    string
	message string
}

// When satisfies the tcell.Event interface.
func (e *copilotStatusEvent) When() time.Time { return e.when }

// copilotSignInEvent carries the signIn response (device code et al).
type copilotSignInEvent struct {
	when time.Time
	res  copilotSignInResult
	err  error
}

// When satisfies the tcell.Event interface.
func (e *copilotSignInEvent) When() time.Time { return e.when }

// copilotSignInDoneEvent lands the blocking confirmation's outcome.
type copilotSignInDoneEvent struct {
	when   time.Time
	status copilotAuthStatus
	err    error
}

// When satisfies the tcell.Event interface.
func (e *copilotSignInDoneEvent) When() time.Time { return e.when }

// copilotSignOutDoneEvent lands a signOut request's outcome.
type copilotSignOutDoneEvent struct {
	when time.Time
	err  error
}

// When satisfies the tcell.Event interface.
func (e *copilotSignOutDoneEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

// copilotReady reports whether the sidecar connection is up and usable.
func (a *App) copilotReady() bool {
	return a.copilot.client != nil && !a.copilot.dead
}

// copilotEnsureStarted kicks off the async sidecar start. Missing
// binary marks the integration dead without a word — the same
// silent-degradation contract as gopls and the formatters. Idempotent;
// disabled, started, starting, and dead states are all no-ops.
func (a *App) copilotEnsureStarted() {
	if !a.copilot.enabled || a.copilot.client != nil || a.copilot.starting || a.copilot.dead || a.screen == nil {
		return
	}
	if _, err := exec.LookPath(copilotServerBinary); err != nil {
		a.copilot.dead = true
		return
	}
	a.copilot.starting = true
	scr := a.screen
	root := a.rootDir
	go func() {
		// onNotify runs on the client's read loop — post, don't touch.
		onNotify := func(method string, params json.RawMessage) {
			if method != "didChangeStatus" {
				return
			}
			var p struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return
			}
			_ = scr.PostEvent(&copilotStatusEvent{when: time.Now(), kind: p.Kind, message: p.Message})
		}
		onExit := func(error) {
			_ = scr.PostEvent(&copilotExitEvent{when: time.Now()})
		}
		client, err := lsp.Start(root, copilotServerBinary, nil, onNotify, onExit)
		if err != nil {
			_ = scr.PostEvent(&copilotExitEvent{when: time.Now()})
			return
		}
		status, err := copilotInitialize(client, root)
		if err != nil {
			client.Close()
			// Mirrors the gopls start path: a timed-out handshake leaves
			// the process alive with no read-loop exit, so post
			// explicitly to settle the state machine.
			_ = scr.PostEvent(&copilotExitEvent{when: time.Now()})
			return
		}
		_ = scr.PostEvent(&copilotReadyEvent{when: time.Now(), client: client, status: status})
	}()
}

// copilotInitialize runs the server's required handshake and returns
// the initial auth state. Runs on the start goroutine, never the main
// loop. The initializationOptions block is NOT optional decoration:
// the server refuses completions for clients that don't identify an
// editorInfo/editorPluginInfo pair.
func copilotInitialize(c *lsp.Client, root string) (copilotAuthStatus, error) {
	var st copilotAuthStatus
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   lsp.PathToURI(root),
		"capabilities": map[string]any{
			"workspace": map[string]any{"workspaceFolders": true},
			// Declares the inline-completion pull model so the server
			// answers textDocument/inlineCompletion (phase 2).
			"textDocument": map[string]any{"inlineCompletion": map[string]any{}},
		},
		"workspaceFolders": []map[string]any{
			{"uri": lsp.PathToURI(root), "name": filepath.Base(root)},
		},
		"initializationOptions": map[string]any{
			"editorInfo":       map[string]any{"name": "r-ed", "version": version.Version},
			"editorPluginInfo": map[string]any{"name": "r-ed-copilot", "version": version.Version},
		},
	}
	if err := c.Call("initialize", params, nil); err != nil {
		return st, err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return st, err
	}
	// The server expects a settings push after initialized; empty means
	// "all defaults" (no proxy, standard telemetry). Fire-and-forget —
	// a lost notification degrades nothing we use yet.
	_ = c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{}})
	if err := c.Call("checkStatus", map[string]any{}, &st); err != nil {
		return st, err
	}
	return st, nil
}

// handleCopilotReady installs the live connection, records the auth
// state, and honours a sign-in the user requested mid-handshake.
func (a *App) handleCopilotReady(e *copilotReadyEvent) {
	if a.copilot.dead || !a.copilot.enabled {
		// Died or was disabled between the ready post and now.
		e.client.Close()
		return
	}
	a.copilot.client = e.client
	a.copilot.starting = false
	a.copilotApplyAuth(e.status)
	// Announce every already-open document — the tabs the user opened
	// while the handshake was in flight — same catch-up handleLSPReady
	// does for gopls.
	for _, t := range a.tabs {
		a.copilotOpenDoc(t)
	}
	if a.copilot.signInWanted {
		a.copilot.signInWanted = false
		if a.copilot.signedIn {
			a.flash("Copilot: already signed in" + copilotUserSuffix(a.copilot.user))
		} else {
			a.copilotBeginSignIn()
		}
	}
}

// copilotApplyAuth stamps an auth result onto the state.
func (a *App) copilotApplyAuth(st copilotAuthStatus) {
	a.copilot.signedIn = st.signedIn()
	if a.copilot.signedIn {
		a.copilot.user = st.User
	} else {
		a.copilot.user = ""
	}
}

// handleCopilotExit marks the integration dead for this session — same
// no-flap, no-auto-restart policy as the LSP layer. Re-enabling via the
// menu toggle is the deliberate restart path.
func (a *App) handleCopilotExit() {
	a.copilotDisconnect()
	a.copilot.dead = true
}

// copilotShutdown tears the sidecar down without marking it dead —
// used by the disable toggle and editor exit, where "unavailable" would
// be the wrong verdict (a re-enable should get a fresh start attempt).
func (a *App) copilotShutdown() {
	a.copilotDisconnect()
}

// copilotDisconnect closes the connection and resets every field that
// only means something while a server is attached — including the
// phase-2 completion machinery: a ghost with no server behind it could
// never report acceptance, so it goes too.
func (a *App) copilotDisconnect() {
	if a.copilot.client != nil {
		a.copilot.client.Close()
	}
	a.copilot.client = nil
	a.copilot.starting = false
	a.copilot.signedIn = false
	a.copilot.user = ""
	a.copilot.signInWanted = false
	a.copilot.pendingCode = ""
	a.copilot.statusKind = ""
	a.copilot.statusMsg = ""
	a.copilotStopCompletionTimer()
	a.copilotClearGhost()
	a.copilot.docVersions = nil
	a.copilot.docSyncedRev = nil
	a.copilot.armRev = nil
}

// handleCopilotStatus records the server's self-reported status. Kept
// as data, not acted on: the auth handlers own sign-in state, and a
// transient Warning shouldn't tear anything down.
func (a *App) handleCopilotStatus(e *copilotStatusEvent) {
	a.copilot.statusKind = e.kind
	a.copilot.statusMsg = e.message
}

// -----------------------------------------------------------------------------
// Sign in / sign out
// -----------------------------------------------------------------------------

// menuCopilotAuth is the ≡ row's dispatch: sign out when signed in,
// otherwise start the sign-in flow. The row is always clickable —
// unlike the LSP rows, which dim, this action explains unavailability
// with a flash, because "Sign in" is the first thing a new user
// reaches for and a silently-dimmed row is a dead end.
func (a *App) menuCopilotAuth() {
	a.closeMenu()
	switch {
	case !a.copilot.enabled:
		a.flash("Copilot is disabled — use ≡ → Enable Copilot first")
	case a.copilot.dead:
		a.flash("Copilot unavailable — install copilot-language-server on PATH, then toggle Copilot off/on")
	case a.copilot.signedIn:
		a.copilotSignOut()
	case a.copilot.client == nil:
		// First interaction races the async start: queue the intent and
		// let handleCopilotReady begin the flow the moment it can.
		a.copilot.signInWanted = true
		a.copilotEnsureStarted()
		if a.copilot.dead {
			// ensureStarted's LookPath just failed — correct the story.
			a.copilot.signInWanted = false
			a.flash("Copilot unavailable — install copilot-language-server on PATH")
		} else {
			a.flash("Copilot is starting — sign-in will open shortly")
		}
	default:
		a.copilotBeginSignIn()
	}
}

// copilotBeginSignIn fires the async signIn request; the device code
// (or an already-signed-in verdict) lands as a copilotSignInEvent.
func (a *App) copilotBeginSignIn() {
	if !a.copilotReady() {
		return
	}
	client := a.copilot.client
	scr := a.screen
	go func() {
		var res copilotSignInResult
		err := client.Call("signIn", map[string]any{}, &res)
		_ = scr.PostEvent(&copilotSignInEvent{when: time.Now(), res: res, err: err})
	}()
}

// handleCopilotSignIn lands the signIn response: either we were signed
// in all along, or we show the device code and ask the user to take it
// to the browser. The modal's Yes both performs the side-effects (copy
// code, open browser) and starts the blocking confirmation.
func (a *App) handleCopilotSignIn(e *copilotSignInEvent) {
	if e.err != nil {
		a.flash("Copilot sign-in failed: " + e.err.Error())
		return
	}
	if st := (copilotAuthStatus{Status: e.res.Status, User: e.res.User}); st.signedIn() {
		a.copilotApplyAuth(st)
		a.flash("Copilot: already signed in" + copilotUserSuffix(a.copilot.user))
		return
	}
	if e.res.UserCode == "" {
		a.flash("Copilot sign-in failed: server returned no device code")
		return
	}
	uri := e.res.VerificationURI
	if uri == "" {
		uri = copilotFallbackVerifyURL
	}
	code, cmd := e.res.UserCode, e.res.Command
	a.openConfirm(
		"GitHub Copilot sign-in",
		fmt.Sprintf("Enter code %s at %s", code, strings.TrimPrefix(uri, "https://")),
		func(app *App) { app.copilotConfirmSignIn(code, uri, cmd) },
	)
}

// copilotConfirmSignIn runs the Yes side of the device-code modal:
// copy the code, open the browser, park the code in the status bar,
// and start the blocking server-side confirmation.
func (a *App) copilotConfirmSignIn(code, uri string, cmd copilotCommand) {
	if !a.copilotReady() {
		return
	}
	// Best-effort conveniences — the code is still on screen (status
	// bar) and the URL was in the modal, so a failure of either
	// side-effect costs nothing but convenience.
	_ = copilotCopyCode(code)
	copilotOpenBrowser(uri)
	a.copilot.pendingCode = code
	a.flash("Copilot: code " + code + " copied — finish signing in in your browser")
	client := a.copilot.client
	scr := a.screen
	go func() {
		var st copilotAuthStatus
		err := client.CallWithTimeout("workspace/executeCommand",
			map[string]any{"command": cmd.Command, "arguments": cmd.Arguments},
			&st, copilotSignInTimeout)
		_ = scr.PostEvent(&copilotSignInDoneEvent{when: time.Now(), status: st, err: err})
	}()
}

// handleCopilotSignInDone lands the confirmation outcome and tells the
// user exactly which of the three endings they got: signed in, signed
// in to an account without Copilot access, or not completed.
func (a *App) handleCopilotSignInDone(e *copilotSignInDoneEvent) {
	a.copilot.pendingCode = ""
	if e.err != nil {
		a.flash("Copilot sign-in failed: " + e.err.Error())
		return
	}
	a.copilotApplyAuth(e.status)
	switch {
	case a.copilot.signedIn:
		a.flash("Copilot: signed in" + copilotUserSuffix(a.copilot.user))
	case e.status.Status == "NotAuthorized":
		a.flash("Copilot: this GitHub account has no Copilot subscription")
	default:
		a.flash("Copilot: sign-in did not complete")
	}
}

// copilotSignOut fires the async signOut request.
func (a *App) copilotSignOut() {
	if !a.copilotReady() {
		return
	}
	client := a.copilot.client
	scr := a.screen
	go func() {
		err := client.Call("signOut", map[string]any{}, nil)
		_ = scr.PostEvent(&copilotSignOutDoneEvent{when: time.Now(), err: err})
	}()
}

// handleCopilotSignOutDone lands the signOut outcome. State is cleared
// even on error — the user asked to be signed out, and an optimistic
// local sign-out that the server missed corrects itself on next start.
func (a *App) handleCopilotSignOutDone(e *copilotSignOutDoneEvent) {
	a.copilot.signedIn = false
	a.copilot.user = ""
	if e.err != nil {
		a.flash("Copilot sign-out: " + e.err.Error())
		return
	}
	a.flash("Copilot: signed out")
}

// -----------------------------------------------------------------------------
// Menu rows + status bar
// -----------------------------------------------------------------------------

// copilotAuthLabel names the auth row for its current direction.
func (a *App) copilotAuthLabel() string {
	if a.copilot.signedIn {
		if a.copilot.user != "" {
			return "Sign out (" + a.copilot.user + ")"
		}
		return "Sign out"
	}
	return "Sign in to GitHub"
}

// copilotToggleLabel names the enable/disable row for its current
// direction, mirroring the auto-save toggle's flip-in-place style.
func (a *App) copilotToggleLabel() string {
	if a.copilot.enabled {
		return "Disable Copilot"
	}
	return "Enable Copilot"
}

// menuToggleCopilot flips the sidecar on or off and persists the
// choice. Enabling clears a dead verdict first — the usual reason for
// death is "binary wasn't installed yet", and the toggle is the
// documented retry path.
func (a *App) menuToggleCopilot() {
	a.closeMenu()
	a.copilot.enabled = !a.copilot.enabled
	if a.copilot.enabled {
		a.copilot.dead = false
		a.copilotEnsureStarted()
		if a.copilot.dead {
			a.flash("Copilot enabled — but copilot-language-server is not on PATH")
		} else {
			a.flash("Copilot enabled")
		}
	} else {
		a.copilotShutdown()
		a.flash("Copilot disabled")
	}
	if err := userconfig.SaveCopilot(userconfig.DefaultPath(), a.copilot.enabled); err != nil {
		a.flash("copilot: " + err.Error())
	}
}

// copilotStatusSegment is the status bar's Copilot fragment: the
// pending device code while a sign-in awaits the browser (the one
// moment the user genuinely needs it on screen), a quiet check mark
// while signed in, and nothing otherwise — absence of Copilot must not
// cost status-bar space.
func (a *App) copilotStatusSegment() string {
	switch {
	case a.copilot.pendingCode != "":
		return "Copilot code " + a.copilot.pendingCode
	case a.copilot.signedIn:
		return "Copilot ✓"
	}
	return ""
}

// copilotUserSuffix formats the " as <login>" tail for flashes, empty
// when the server didn't report a login.
func copilotUserSuffix(user string) string {
	if user == "" {
		return ""
	}
	return " as " + user
}

// openBrowser launches the user's default browser at url from a
// goroutine, best-effort by design: the sign-in modal and status bar
// both show the URL/code, so a headless or misconfigured host just
// means the user navigates by hand — never an error dialog.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	go func() { _ = cmd.Run() }()
}
