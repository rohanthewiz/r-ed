// =============================================================================
// File: internal/app/copilot_test.go
// Author: Rohan Allison <rohanthewiz@gmail.com>
// Created: 2026-07-22
// Copyright: 2026 Rohan Allison. All rights reserved.
// =============================================================================

// Tests for the Copilot sidecar integration (copilot.go). The real
// copilot-language-server is never spawned here — newTestApp marks the
// integration dead, and these tests inject fakeCopilotConn, the same
// isolation contract the LSP tests keep with fakeLSPConn. Flows that
// legitimately hop through a goroutine (signIn, the blocking
// confirmation) are asserted with a bounded wait on the fake's
// recorded calls.

package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCopilotConn is a scripted, race-safe copilotConn: it records
// every Call/Notify (they arrive from sign-in goroutines, hence the
// mutex) and answers from canned per-method results.
type fakeCopilotConn struct {
	mu       sync.Mutex
	calls    []string
	notifies []string
	closed   bool
	results  map[string]any   // method → value marshalled into the caller's result
	errs     map[string]error // method → scripted failure
}

// Call records the method and plays back the scripted response.
func (f *fakeCopilotConn) Call(method string, params, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	res := f.results[method]
	err := f.errs[method]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if result != nil && res != nil {
		b, mErr := json.Marshal(res)
		if mErr != nil {
			return mErr
		}
		return json.Unmarshal(b, result)
	}
	return nil
}

// CallWithTimeout defers to Call — the fake never blocks, so the
// deadline is irrelevant here.
func (f *fakeCopilotConn) CallWithTimeout(method string, params, result any, _ time.Duration) error {
	return f.Call(method, params, result)
}

// Notify records the notification method.
func (f *fakeCopilotConn) Notify(method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifies = append(f.notifies, method)
	return nil
}

// Close records that the connection was torn down.
func (f *fakeCopilotConn) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// called reports whether method has been requested yet (race-safe).
func (f *fakeCopilotConn) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, method)
}

// isClosed reports the torn-down flag (race-safe).
func (f *fakeCopilotConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// waitForCopilot polls cond with a deadline — the bounded-wait bridge
// for flows that hop through a goroutine before touching the fake.
func waitForCopilot(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// newCopilotTestApp is newTestApp with the Copilot integration revived:
// enabled, not dead, and connected to the given fake.
func newCopilotTestApp(t *testing.T, fake *fakeCopilotConn) *App {
	t.Helper()
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = false
	a.copilot.client = fake
	return a
}

// TestCopilotAuthStatusSignedIn pins which server status strings count
// as a usable session — anything unrecognised must fail safe to
// signed-out, never signed-in.
func TestCopilotAuthStatusSignedIn(t *testing.T) {
	cases := map[string]bool{
		"OK":              true,
		"AlreadySignedIn": true,
		"MaybeOk":         true,
		"NotSignedIn":     false,
		"NotAuthorized":   false,
		"":                false,
		"SomethingNew":    false,
	}
	for status, want := range cases {
		if got := (copilotAuthStatus{Status: status}).signedIn(); got != want {
			t.Errorf("signedIn(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestHandleCopilotReady_InstallsAndAppliesAuth pins the happy start
// path: the ready event installs the connection, clears the starting
// flag, and stamps the initial auth state.
func TestHandleCopilotReady_InstallsAndAppliesAuth(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = false
	a.copilot.starting = true
	fake := &fakeCopilotConn{}
	a.handleCopilotReady(&copilotReadyEvent{
		when: time.Now(), client: fake,
		status: copilotAuthStatus{Status: "OK", User: "octocat"},
	})
	if a.copilot.client != fake || a.copilot.starting {
		t.Fatal("ready event did not install the connection")
	}
	if !a.copilot.signedIn || a.copilot.user != "octocat" {
		t.Fatalf("auth not applied: signedIn=%v user=%q", a.copilot.signedIn, a.copilot.user)
	}
}

// TestHandleCopilotReady_DeadOrDisabledClosesClient pins the race
// guard: a ready event landing after death or a disable must close the
// fresh connection, not resurrect the integration.
func TestHandleCopilotReady_DeadOrDisabledClosesClient(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = true
	fake := &fakeCopilotConn{}
	a.handleCopilotReady(&copilotReadyEvent{when: time.Now(), client: fake})
	if !fake.isClosed() || a.copilot.client != nil {
		t.Fatal("ready-after-dead must close the client and install nothing")
	}
}

// TestHandleCopilotReady_HonoursQueuedSignIn pins the signInWanted
// bridge: a sign-in clicked during the handshake begins automatically
// once the server is up.
func TestHandleCopilotReady_HonoursQueuedSignIn(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = false
	a.copilot.signInWanted = true
	fake := &fakeCopilotConn{
		results: map[string]any{"signIn": map[string]any{"status": "PromptUserDeviceFlow", "userCode": "AB12-CD34"}},
	}
	a.handleCopilotReady(&copilotReadyEvent{
		when: time.Now(), client: fake,
		status: copilotAuthStatus{Status: "NotSignedIn"},
	})
	if a.copilot.signInWanted {
		t.Fatal("signInWanted should be consumed by the ready handler")
	}
	waitForCopilot(t, "queued signIn call", func() bool { return fake.called("signIn") })
}

// TestMenuCopilotAuth_ExplainsUnavailability pins the row's always-
// clickable contract: disabled and dead states flash a reason instead
// of silently doing nothing.
func TestMenuCopilotAuth_ExplainsUnavailability(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = false
	a.menuCopilotAuth()
	if !strings.Contains(a.statusMsg, "disabled") {
		t.Fatalf("disabled flash = %q, want a 'disabled' explanation", a.statusMsg)
	}
	a.copilot.enabled = true
	a.copilot.dead = true
	a.menuCopilotAuth()
	if !strings.Contains(a.statusMsg, "unavailable") {
		t.Fatalf("dead flash = %q, want an 'unavailable' explanation", a.statusMsg)
	}
}

// TestMenuCopilotAuth_SignedInSignsOut pins the row's flip: with a
// live signed-in session the same action requests signOut.
func TestMenuCopilotAuth_SignedInSignsOut(t *testing.T) {
	fake := &fakeCopilotConn{}
	a := newCopilotTestApp(t, fake)
	a.copilot.signedIn = true
	a.copilot.user = "octocat"
	a.menuCopilotAuth()
	waitForCopilot(t, "signOut call", func() bool { return fake.called("signOut") })
}

// TestMenuCopilotAuth_QueuesWhileStarting pins the no-client branch:
// the intent is queued (for handleCopilotReady) rather than lost, and
// the user is told the flow will continue.
func TestMenuCopilotAuth_QueuesWhileStarting(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = false
	a.copilot.starting = true // pretend the async spawn is in flight
	a.menuCopilotAuth()
	if !a.copilot.signInWanted {
		t.Fatal("sign-in intent must be queued while the server starts")
	}
	if !strings.Contains(a.statusMsg, "starting") {
		t.Fatalf("flash = %q, want a 'starting' notice", a.statusMsg)
	}
}

// TestHandleCopilotSignIn_OpensModalWithCode pins the device-code leg:
// the response opens a confirm modal whose message carries the code,
// and Yes performs the side-effects and starts the confirmation call.
func TestHandleCopilotSignIn_OpensModalWithCode(t *testing.T) {
	fake := &fakeCopilotConn{
		results: map[string]any{"workspace/executeCommand": map[string]any{"status": "OK", "user": "octocat"}},
	}
	a := newCopilotTestApp(t, fake)

	var copiedCode, openedURL string
	copilotCopyCode = func(s string) error { copiedCode = s; return nil }
	copilotOpenBrowser = func(u string) { openedURL = u }

	a.handleCopilotSignIn(&copilotSignInEvent{
		when: time.Now(),
		res: copilotSignInResult{
			Status:          "PromptUserDeviceFlow",
			UserCode:        "AB12-CD34",
			VerificationURI: "https://github.com/login/device",
			Command:         copilotCommand{Command: "github.copilot.finishDeviceFlow"},
		},
	})
	m, ok := a.modal.(*confirmModal)
	if !ok {
		t.Fatalf("expected confirm modal, got %T", a.modal)
	}
	if !strings.Contains(m.message, "AB12-CD34") || !strings.Contains(m.message, "github.com/login/device") {
		t.Fatalf("modal message = %q, want code and URL", m.message)
	}

	m.yes(a)
	if copiedCode != "AB12-CD34" || openedURL != "https://github.com/login/device" {
		t.Fatalf("side-effects: copied=%q opened=%q", copiedCode, openedURL)
	}
	if a.copilot.pendingCode != "AB12-CD34" {
		t.Fatalf("pendingCode = %q, want the device code parked for the status bar", a.copilot.pendingCode)
	}
	waitForCopilot(t, "executeCommand confirmation", func() bool { return fake.called("workspace/executeCommand") })
}

// TestHandleCopilotSignIn_AlreadySignedIn pins the short-circuit: an
// already-signed-in response applies auth with no modal.
func TestHandleCopilotSignIn_AlreadySignedIn(t *testing.T) {
	a := newCopilotTestApp(t, &fakeCopilotConn{})
	a.handleCopilotSignIn(&copilotSignInEvent{
		when: time.Now(),
		res:  copilotSignInResult{Status: "AlreadySignedIn", User: "octocat"},
	})
	if a.modal != nil {
		t.Fatal("no modal should open when already signed in")
	}
	if !a.copilot.signedIn || a.copilot.user != "octocat" {
		t.Fatalf("auth not applied: signedIn=%v user=%q", a.copilot.signedIn, a.copilot.user)
	}
}

// TestHandleCopilotSignInDone covers the three endings — signed in,
// account without access, and a hard failure — and that the pending
// code always leaves the status bar.
func TestHandleCopilotSignInDone(t *testing.T) {
	a := newCopilotTestApp(t, &fakeCopilotConn{})

	a.copilot.pendingCode = "AB12-CD34"
	a.handleCopilotSignInDone(&copilotSignInDoneEvent{
		when: time.Now(), status: copilotAuthStatus{Status: "OK", User: "octocat"},
	})
	if !a.copilot.signedIn || a.copilot.pendingCode != "" {
		t.Fatalf("success: signedIn=%v pendingCode=%q", a.copilot.signedIn, a.copilot.pendingCode)
	}
	if !strings.Contains(a.statusMsg, "octocat") {
		t.Fatalf("success flash = %q, want the login named", a.statusMsg)
	}

	a.copilot.pendingCode = "AB12-CD34"
	a.handleCopilotSignInDone(&copilotSignInDoneEvent{
		when: time.Now(), status: copilotAuthStatus{Status: "NotAuthorized"},
	})
	if a.copilot.signedIn || a.copilot.pendingCode != "" {
		t.Fatal("NotAuthorized must not read as signed in")
	}
	if !strings.Contains(a.statusMsg, "subscription") {
		t.Fatalf("NotAuthorized flash = %q, want the subscription explanation", a.statusMsg)
	}

	a.copilot.pendingCode = "AB12-CD34"
	a.handleCopilotSignInDone(&copilotSignInDoneEvent{when: time.Now(), err: errors.New("boom")})
	if a.copilot.pendingCode != "" {
		t.Fatal("pendingCode must clear even on failure")
	}
	if !strings.Contains(a.statusMsg, "boom") {
		t.Fatalf("failure flash = %q, want the error surfaced", a.statusMsg)
	}
}

// TestHandleCopilotSignOutDone pins that sign-out clears local auth
// state even when the server call failed — the user asked out.
func TestHandleCopilotSignOutDone(t *testing.T) {
	a := newCopilotTestApp(t, &fakeCopilotConn{})
	a.copilot.signedIn = true
	a.copilot.user = "octocat"
	a.handleCopilotSignOutDone(&copilotSignOutDoneEvent{when: time.Now(), err: errors.New("nope")})
	if a.copilot.signedIn || a.copilot.user != "" {
		t.Fatal("sign-out must clear auth state even on server error")
	}
}

// TestHandleCopilotExit pins the crash path: connection gone, state
// reset, and the dead verdict set so nothing flaps into a restart.
func TestHandleCopilotExit(t *testing.T) {
	fake := &fakeCopilotConn{}
	a := newCopilotTestApp(t, fake)
	a.copilot.signedIn = true
	a.copilot.pendingCode = "AB12-CD34"
	a.handleCopilotExit()
	if !fake.isClosed() || a.copilot.client != nil {
		t.Fatal("exit must close and drop the connection")
	}
	if !a.copilot.dead || a.copilot.signedIn || a.copilot.pendingCode != "" {
		t.Fatal("exit must mark dead and reset session state")
	}
}

// TestMenuToggleCopilot_DisableShutsDownAndPersists pins the off flip:
// the sidecar is torn down (without a dead verdict, so re-enabling can
// retry) and the preference lands in config.json.
func TestMenuToggleCopilot_DisableShutsDownAndPersists(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	fake := &fakeCopilotConn{}
	a := newCopilotTestApp(t, fake)

	a.menuToggleCopilot()
	if a.copilot.enabled || !fake.isClosed() || a.copilot.client != nil {
		t.Fatal("disable must flip the flag and shut the sidecar down")
	}
	if a.copilot.dead {
		t.Fatal("a deliberate disable must not be recorded as dead")
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "r-ed", "config.json"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(data), `"copilot": "off"`) {
		t.Fatalf("config = %s, want copilot off persisted", data)
	}
}

// TestMenuToggleCopilot_EnableRetriesLookup pins the on flip as the
// documented retry path: a dead verdict is cleared and, with no binary
// on PATH, quietly re-earned — plus the flash says the binary is
// missing so the user knows what to install.
func TestMenuToggleCopilot_EnableRetriesLookup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // guarantees LookPath misses
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = false
	a.copilot.dead = true

	a.menuToggleCopilot()
	if !a.copilot.enabled {
		t.Fatal("enable must flip the flag")
	}
	if !a.copilot.dead {
		t.Fatal("with no binary on PATH the retry should re-mark dead")
	}
	if !strings.Contains(a.statusMsg, "not on PATH") {
		t.Fatalf("flash = %q, want the missing-binary explanation", a.statusMsg)
	}
}

// TestCopilotEnsureStarted_GuardsHold pins the no-op guards: disabled,
// dead, and already-connected states must never attempt a spawn (the
// starting flag is the observable).
func TestCopilotEnsureStarted_GuardsHold(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	a.copilot.enabled = false
	a.copilot.dead = false
	a.copilotEnsureStarted()
	if a.copilot.starting {
		t.Fatal("disabled integration must not start")
	}

	a.copilot.enabled = true
	a.copilot.dead = true
	a.copilotEnsureStarted()
	if a.copilot.starting {
		t.Fatal("dead integration must not start")
	}

	a.copilot.dead = false
	a.copilot.client = &fakeCopilotConn{}
	a.copilotEnsureStarted()
	if a.copilot.starting {
		t.Fatal("connected integration must not start again")
	}
}

// TestCopilotEnsureStarted_MissingBinaryGoesDeadSilently pins the
// silent-degradation contract: no binary on PATH → dead, no goroutine,
// no flash.
func TestCopilotEnsureStarted_MissingBinaryGoesDeadSilently(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // guarantees LookPath misses
	a := newTestApp(t, t.TempDir())
	a.copilot.enabled = true
	a.copilot.dead = false
	before := a.statusMsg
	a.copilotEnsureStarted()
	if !a.copilot.dead || a.copilot.starting {
		t.Fatalf("want silent dead verdict, got dead=%v starting=%v", a.copilot.dead, a.copilot.starting)
	}
	if a.statusMsg != before {
		t.Fatalf("missing binary must not flash, got %q", a.statusMsg)
	}
}

// TestCopilotStatusSegment pins the status bar fragment's three
// states: pending device code wins, signed-in shows the quiet mark,
// absence costs nothing.
func TestCopilotStatusSegment(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if got := a.copilotStatusSegment(); got != "" {
		t.Fatalf("idle segment = %q, want empty", got)
	}
	a.copilot.signedIn = true
	if got := a.copilotStatusSegment(); got != "Copilot ✓" {
		t.Fatalf("signed-in segment = %q", got)
	}
	a.copilot.pendingCode = "AB12-CD34"
	if got := a.copilotStatusSegment(); !strings.Contains(got, "AB12-CD34") {
		t.Fatalf("pending segment = %q, want the device code", got)
	}
}

// TestCopilotAuthAndToggleLabels pins the flip-in-place menu labels.
func TestCopilotAuthAndToggleLabels(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if got := a.copilotAuthLabel(); got != "Sign in to GitHub" {
		t.Fatalf("signed-out label = %q", got)
	}
	a.copilot.signedIn = true
	a.copilot.user = "octocat"
	if got := a.copilotAuthLabel(); got != "Sign out (octocat)" {
		t.Fatalf("signed-in label = %q", got)
	}
	a.copilot.enabled = true
	if got := a.copilotToggleLabel(); got != "Disable Copilot" {
		t.Fatalf("enabled toggle label = %q", got)
	}
	a.copilot.enabled = false
	if got := a.copilotToggleLabel(); got != "Enable Copilot" {
		t.Fatalf("disabled toggle label = %q", got)
	}
}

// TestHandleCopilotStatus pins that didChangeStatus is recorded as
// data without disturbing auth state.
func TestHandleCopilotStatus(t *testing.T) {
	a := newCopilotTestApp(t, &fakeCopilotConn{})
	a.copilot.signedIn = true
	a.handleCopilotStatus(&copilotStatusEvent{when: time.Now(), kind: "Warning", message: "quota low"})
	if a.copilot.statusKind != "Warning" || a.copilot.statusMsg != "quota low" {
		t.Fatal("status notification not recorded")
	}
	if !a.copilot.signedIn {
		t.Fatal("a status notification must not clear auth state")
	}
}
