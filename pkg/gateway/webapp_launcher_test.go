package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const webappTestLink = "http://127.0.0.1:9120/webapp/launch#ticket=ttttttttttttttttttttttttttttttttttttttttttt"

// Launcher side effects recorded and control answers served per verb
type webappWorld struct {
	t       *testing.T
	root    string
	tmp     string
	home    string
	build   int
	ensure  []string
	control [][2]string
	params  []map[string]any
	opened  []string
	slept   []time.Duration
	answers map[string][]any
	states  []string
}

// Home dir with its config written
func webappTestHome(t *testing.T, path, config string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Namespace of the webapp command with overrides
func webappArgs(overrides map[string]any) map[string]any {
	ns := map[string]any{"no_open": false, "status": false, "sign_out": false, "build_only": false, "force_build": false, "skip_build": false}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

// Recorded launcher world with the Webapp on in the default home
func newWebappWorld(t *testing.T) *webappWorld {
	tmp := t.TempDir()
	w := &webappWorld{t: t, tmp: tmp, states: []string{"absent"}, answers: map[string][]any{
		"webapp-launch":   {map[string]any{"ok": true, "result": map[string]any{"url": webappTestLink, "private": false, "expires_in_seconds": 60}}},
		"webapp-status":   {map[string]any{"ok": true, "result": map[string]any{"state": "serving", "url": strings.Split(webappTestLink, "/webapp")[0], "reason": nil}}},
		"webapp-sign-out": {map[string]any{"ok": true, "result": map[string]any{"signed_out": true}}},
	}}
	saved := wl
	t.Cleanup(func() { wl = saved })
	wl.build = func(context.Context, map[string]any) error { w.build++; return nil }
	wl.ensure = func(_ context.Context, h string) error { w.ensure = append(w.ensure, h); return nil }
	wl.control = func(h, verb string, params map[string]any) (map[string]any, error) {
		w.control = append(w.control, [2]string{h, verb})
		if params == nil {
			params = map[string]any{}
		}
		w.params = append(w.params, params)
		answers := w.answers[verb]
		answer := answers[0]
		if len(answers) > 1 {
			w.answers[verb] = answers[1:]
		}
		if err, ok := answer.(error); ok {
			return nil, err
		}
		return answer.(map[string]any), nil
	}
	wl.open = func(target string) bool { w.opened = append(w.opened, target); return true }
	wl.gatewayState = func(context.Context, string) string {
		state := w.states[0]
		if len(w.states) > 1 {
			w.states = w.states[1:]
		}
		return state
	}
	wl.sleep = func(d time.Duration) { w.slept = append(w.slept, d) }
	w.root = webappTestHome(t, filepath.Join(tmp, "root"), "webapp:\n  enabled: true\n")
	w.useHome(w.root)
	return w
}

// Points the active home at h
func (w *webappWorld) useHome(h string) {
	w.home = h
	w.t.Setenv("ICEBATH_HOME", h)
}

// Answer a verb with one fixed reply
func (w *webappWorld) answer(verb string, replies ...any) { w.answers[verb] = replies }

// Runs the webapp command and returns its exit code, stdout and stderr
func (w *webappWorld) run(ns map[string]any) (int, string, string) {
	w.t.Helper()
	var out, errOut bytes.Buffer
	code, err := CmdWebapp(context.Background(), w.home, ns, "", &out, &errOut)
	if err != nil {
		w.t.Fatalf("CmdWebapp error %v, output %s", err, out.String())
	}
	return code, out.String(), errOut.String()
}

func TestTheWebappOpensFromAOneUseLinkItsGatewayMints(t *testing.T) {
	w := newWebappWorld(t)
	code, out, _ := w.run(webappArgs(nil))
	if code != 0 || w.build != 1 || fmt.Sprint(w.ensure) != fmt.Sprint([]string{w.root}) ||
		fmt.Sprint(w.control) != fmt.Sprint([][2]string{{w.root, "webapp-launch"}}) || fmt.Sprint(w.opened) != fmt.Sprint([]string{webappTestLink}) {
		t.Fatalf("code %d build %d ensure %v control %v opened %v", code, w.build, w.ensure, w.control, w.opened)
	}
	if !strings.Contains(out, "Opened the Webapp in your browser") || strings.Contains(out, "ticket=") {
		t.Fatalf("out %q", out)
	}
}

func TestANamedProfileOpensTheWebappOfTheDefaultGatewayOnThatProfile(t *testing.T) {
	w := newWebappWorld(t)
	w.useHome(webappTestHome(t, filepath.Join(w.root, "profiles", "work"), ""))
	if code, out, _ := w.run(webappArgs(nil)); code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
	if fmt.Sprint(w.ensure) != fmt.Sprint([]string{w.root}) || fmt.Sprint(w.control) != fmt.Sprint([][2]string{{w.root, "webapp-launch"}}) ||
		fmt.Sprint(w.params) != fmt.Sprint([]map[string]any{{"profile": "work"}}) {
		t.Fatalf("ensure %v control %v params %v", w.ensure, w.control, w.params)
	}
}

func TestTheDefaultProfileOpensWithoutAProfileChoice(t *testing.T) {
	w := newWebappWorld(t)
	if code, _, _ := w.run(webappArgs(nil)); code != 0 || len(w.params) != 1 || len(w.params[0]) != 0 {
		t.Fatalf("code %d params %v", code, w.params)
	}
}

func TestAStandaloneProfileOpensTheWebappOfItsOwnGateway(t *testing.T) {
	w := newWebappWorld(t)
	profile := webappTestHome(t, filepath.Join(w.root, "profiles", "solo"), "gateway:\n  standalone: true\nwebapp:\n  enabled: true\n")
	w.useHome(profile)
	if code, _, _ := w.run(webappArgs(nil)); code != 0 || fmt.Sprint(w.ensure) != fmt.Sprint([]string{profile}) {
		t.Fatalf("code %d ensure %v", code, w.ensure)
	}
}

func TestTheLauncherSaysHowToTurnTheWebappOn(t *testing.T) {
	w := newWebappWorld(t)
	webappTestHome(t, w.root, "")
	w.answer("webapp-status", map[string]any{"ok": true, "result": map[string]any{"state": "off", "url": nil, "reason": nil}})
	code, out, _ := w.run(webappArgs(nil))
	if code != 1 || !strings.Contains(out, "webapp.enabled: true") || !strings.Contains(out, filepath.Join(w.root, "config.yaml")) ||
		w.build != 0 || len(w.ensure) != 0 {
		t.Fatalf("code %d out %q build %d ensure %v", code, out, w.build, w.ensure)
	}
}

func TestAGatewayStartedBeforeTheWebappWasTurnedOnAsksForARestart(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-launch", map[string]any{"ok": false, "error": "RuntimeError: The Webapp is off. Set ..."})
	w.answer("webapp-status", map[string]any{"ok": true, "result": map[string]any{"state": "off", "url": nil, "reason": nil}})
	code, out, _ := w.run(webappArgs(nil))
	if code != 1 || !strings.Contains(out, "icebath gateway restart") || len(w.opened) != 0 {
		t.Fatalf("code %d out %q opened %v", code, out, w.opened)
	}
}

func TestAWebappTheGatewayCouldNotServeReportsWhyAndHowToRetry(t *testing.T) {
	w := newWebappWorld(t)
	reason := "could not listen on 127.0.0.1:9120 (Address already in use)"
	w.answer("webapp-launch", map[string]any{"ok": false, "error": "RuntimeError: The Webapp is unavailable: " + reason})
	w.answer("webapp-status", map[string]any{"ok": true, "result": map[string]any{"state": "unavailable", "url": nil, "reason": reason}})
	code, out, _ := w.run(webappArgs(nil))
	if code != 1 || !strings.Contains(out, reason) || !strings.Contains(out, "icebath gateway restart") {
		t.Fatalf("code %d out %q", code, out)
	}
	code, out, _ = w.run(webappArgs(map[string]any{"status": true}))
	if code != 0 || !strings.Contains(out, reason) || !strings.Contains(out, "icebath gateway restart") {
		t.Fatalf("status code %d out %q", code, out)
	}
}

func TestALaunchRefusedForAStaleBundleAsksForABuildNotARestart(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-launch", map[string]any{"ok": false, "error": "RuntimeError: The Webapp is unavailable: " + WebappNotRebuilt})
	w.answer("webapp-status", map[string]any{"ok": true, "result": map[string]any{"state": "unavailable", "url": nil, "reason": WebappNotRebuilt}})
	code, out, _ := w.run(webappArgs(map[string]any{"skip_build": true}))
	if code != 1 || !strings.Contains(out, "Run icebath webapp to rebuild it.") || strings.Contains(out, webappRestartCommand) {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Status answer of a gateway serving the Webapp on loopback
func webappServing() map[string]any {
	return map[string]any{"ok": true, "result": map[string]any{"state": "serving", "url": "http://127.0.0.1:9120", "reason": nil}}
}

func TestAWebappTurnedOffInConfigButStillServedSaysARestartStopsIt(t *testing.T) {
	w := newWebappWorld(t)
	webappTestHome(t, w.root, "webapp:\n  enabled: false\n  host: 0.0.0.0\n")
	w.answer("webapp-status", webappServing())
	code, out, _ := w.run(webappArgs(nil))
	if code != 1 || strings.Contains(out, "The Webapp is off") || !strings.Contains(out, "http://127.0.0.1:9120") ||
		!strings.Contains(out, webappRestartCommand) || w.build != 0 || len(w.ensure) != 0 || len(w.opened) != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestStatusOfAWebappServedAfterTheConfigTurnedItOffSaysARestartStopsIt(t *testing.T) {
	w := newWebappWorld(t)
	webappTestHome(t, w.root, "webapp:\n  enabled: false\n")
	w.answer("webapp-status", webappServing())
	code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
	if code != 0 || !strings.Contains(out, "Serving at http://127.0.0.1:9120") || !strings.Contains(out, webappRestartCommand) {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestBehindTheDashboardSignInTheLauncherOpensTheWebappToSignIn(t *testing.T) {
	w := newWebappWorld(t)
	u := "http://127.0.0.1:9120/"
	w.answer("webapp-launch", map[string]any{"ok": true, "result": map[string]any{"url": u, "private": false, "expires_in_seconds": nil, "sign_in": true}})
	code, out, _ := w.run(webappArgs(nil))
	if code != 0 || fmt.Sprint(w.opened) != fmt.Sprint([]string{u}) || !strings.Contains(strings.ToLower(out), "sign in") {
		t.Fatalf("code %d opened %v out %q", code, w.opened, out)
	}
	code, out, _ = w.run(webappArgs(map[string]any{"no_open": true}))
	if code != 0 || !strings.Contains(out, u) || !strings.Contains(strings.ToLower(out), "sign in") || strings.Contains(out, "one-use") {
		t.Fatalf("no open code %d out %q", code, out)
	}
}

func TestNoOpenPrintsTheLinkInstead(t *testing.T) {
	w := newWebappWorld(t)
	code, out, _ := w.run(webappArgs(map[string]any{"no_open": true}))
	if code != 0 || len(w.opened) != 0 || !strings.Contains(out, webappTestLink) || !strings.Contains(out, "60 seconds") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestABrowserThatWillNotOpenLeavesTheLinkOnScreen(t *testing.T) {
	w := newWebappWorld(t)
	wl.open = func(string) bool { return false }
	if code, out, _ := w.run(webappArgs(nil)); code != 0 || !strings.Contains(out, webappTestLink) {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestBuildOnlyBuildsWithoutAGateway(t *testing.T) {
	w := newWebappWorld(t)
	if code, _, _ := w.run(webappArgs(map[string]any{"build_only": true})); code != 0 || w.build != 1 || len(w.ensure) != 0 || len(w.control) != 0 {
		t.Fatalf("code %d build %d ensure %v control %v", code, w.build, w.ensure, w.control)
	}
}

func TestStatusReadsTheRunningGatewayWithoutStartingOne(t *testing.T) {
	w := newWebappWorld(t)
	code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
	if code != 0 || len(w.ensure) != 0 || fmt.Sprint(w.control) != fmt.Sprint([][2]string{{w.root, "webapp-status"}}) || !strings.Contains(out, "http://127.0.0.1:9120") {
		t.Fatalf("code %d control %v out %q", code, w.control, out)
	}
}

func TestStatusOfAGatewayStillStartingTheWebappSaysSo(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-status", map[string]any{"ok": true, "result": map[string]any{"state": "starting", "url": nil, "reason": nil}})
	code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
	if code != 0 || !strings.Contains(out, "still starting") || strings.Contains(out, "Unavailable") || strings.Contains(out, webappRestartCommand) {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestAGatewayOlderThanTheWebappIsNamedWithTheRestartThatUpdatesIt(t *testing.T) {
	for _, action := range []map[string]any{{"status": true}, {}, {"sign_out": true}} {
		w := newWebappWorld(t)
		for _, verb := range []string{"webapp-status", "webapp-launch", "webapp-sign-out"} {
			w.answer(verb, map[string]any{"ok": false, "error": "unknown verb: '" + verb + "'", "supported_verbs": []any{"identify"}})
		}
		w.states = []string{"ready"}
		code, out, _ := w.run(webappArgs(action))
		if code != 1 || strings.Contains(out, "unknown verb") || !strings.Contains(out, "older") || !strings.Contains(out, webappRestartCommand) {
			t.Fatalf("action %v code %d out %q", action, code, out)
		}
	}
}

func TestStatusWithNoGatewayRunningSaysSo(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-status", &gatewayUnreachable{"no gateway"})
	if code, out, _ := w.run(webappArgs(map[string]any{"status": true})); code != 1 || !strings.Contains(out, "gateway is not running") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestStatusWithNoGatewayRunningAndTheWebappOffSaysHowToTurnItOn(t *testing.T) {
	w := newWebappWorld(t)
	webappTestHome(t, w.root, "")
	w.answer("webapp-status", &gatewayUnreachable{"no gateway"})
	code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
	if code != 1 || !strings.Contains(out, "webapp.enabled: true") || !strings.Contains(out, filepath.Join(w.root, "config.yaml")) ||
		strings.Contains(out, "Run icebath webapp") {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Sign out answer with result
func webappSignOutAnswer(result map[string]any) map[string]any {
	return map[string]any{"ok": true, "result": result}
}

func TestSignOutReplacesTheBrowserCredentialOnTheRunningGateway(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-sign-out", webappSignOutAnswer(map[string]any{"signed_out": true, "serving": true, "sign_in_gate": false, "connections_closed": int64(3)}))
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 0 || fmt.Sprint(w.control) != fmt.Sprint([][2]string{{w.root, "webapp-sign-out"}}) || len(w.ensure) != 0 ||
		!strings.Contains(out, "Signed out of every Webapp tab") || !strings.Contains(out, "closed 3 browser connections") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestSignOutBehindTheSignInGateSaysBrowsersMustSignInAgain(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-sign-out", webappSignOutAnswer(map[string]any{"signed_out": true, "serving": true, "sign_in_gate": true, "connections_closed": int64(1)}))
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	for _, want := range []string{"closed 1 browser connection.", "must sign in again", "tokens", "Native apps", "stay signed in"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("code %d out %q missing %q", code, out, want)
		}
	}
}

func TestSignOutOnAGatewayNotServingTheWebappSaysNoBrowserWasConnected(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-sign-out", webappSignOutAnswer(map[string]any{"signed_out": true, "serving": false, "sign_in_gate": false, "connections_closed": int64(0)}))
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 0 || !strings.Contains(out, "not serving the Webapp") || strings.Contains(out, "Signed out of every") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestSignOutOnAGatewayThatDoesNotReportWhatItClosedAsksForARestart(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-sign-out", webappSignOutAnswer(map[string]any{"signed_out": true}))
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 0 || !strings.Contains(out, webappRestartCommand) || strings.Contains(out, "Signed out of every") {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Stored browser credential of the root home
func (w *webappWorld) storedToken() string {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.root, WebappTokenPath))
	if err != nil {
		w.t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// Mints the root home's browser credential
func (w *webappWorld) mintToken() string {
	w.t.Helper()
	token, err := BrowserSessionToken(context.Background(), w.root, false)
	if err != nil {
		w.t.Fatal(err)
	}
	return token
}

func TestASignOutTheGatewayRefusesIsReportedAsNotDone(t *testing.T) {
	w := newWebappWorld(t)
	before := w.mintToken()
	w.answer("webapp-sign-out", map[string]any{"ok": false, "error": "PermissionError: authenticated private request required"})
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 1 || !strings.Contains(out, "authenticated private request required") || strings.Contains(out, "Signed out") || w.storedToken() != before {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestAGatewayThatWillNotStartIsReported(t *testing.T) {
	w := newWebappWorld(t)
	wl.ensure = func(context.Context, string) error {
		return &gatewayUnreachable{"The gateway did not start (inaccessible: lock owned by another user)."}
	}
	code, out, _ := w.run(webappArgs(nil))
	if code != 1 || !strings.Contains(out, "lock owned by another user") || len(w.control) != 0 {
		t.Fatalf("code %d out %q control %v", code, out, w.control)
	}
}

func TestSigningOutWithNoGatewayRunningStillReplacesTheStoredCredential(t *testing.T) {
	w := newWebappWorld(t)
	before := w.mintToken()
	w.answer("webapp-sign-out", &gatewayUnreachable{"no gateway"})
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 0 || w.storedToken() == before || !strings.Contains(out, "No gateway is running") || !strings.Contains(out, "sign in again") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestSigningOutWithNoGatewayAndACredentialThatCannotBeStoredSaysNobodyWas(t *testing.T) {
	w := newWebappWorld(t)
	if err := os.WriteFile(filepath.Join(w.root, "webapp"), []byte("not a folder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.answer("webapp-sign-out", &gatewayUnreachable{"no gateway"})
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 1 || !strings.Contains(out, "Nobody was signed out") || !strings.Contains(out, filepath.Join(w.root, WebappTokenPath)) {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Sign out answer of a gateway still starting its Webapp
func webappStartingAnswer() map[string]any {
	return map[string]any{"ok": false, "error": "RuntimeError: The Webapp is still starting; try again in a moment."}
}

func TestSignOutWaitsForAStartingGatewayAndSignsOutThere(t *testing.T) {
	for _, first := range []any{webappStartingAnswer(), &gatewayUnreachable{"control socket not up yet"}} {
		w := newWebappWorld(t)
		before := w.mintToken()
		w.answer("webapp-sign-out", first, first, webappSignOutAnswer(map[string]any{"signed_out": true, "serving": true, "sign_in_gate": false, "connections_closed": int64(1)}))
		w.states = []string{"starting"}
		code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
		if code != 0 || !strings.Contains(out, "Signed out of every Webapp tab: closed 1 browser connection.") || len(w.control) != 3 || w.storedToken() != before {
			t.Fatalf("code %d out %q control %v", code, out, w.control)
		}
	}
}

func TestSignOutOnAGatewayThatStaysStartingSaysNobodyWasSignedOut(t *testing.T) {
	w := newWebappWorld(t)
	before := w.mintToken()
	w.answer("webapp-sign-out", webappStartingAnswer())
	w.states = []string{"starting"}
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	var slept time.Duration
	for _, d := range w.slept {
		slept += d
	}
	if code != 1 || !strings.Contains(out, "still starting") || !strings.Contains(strings.ToLower(out), "nobody was signed out") ||
		strings.Contains(out, "Signed out") || w.storedToken() != before || slept < 20*time.Second {
		t.Fatalf("code %d out %q slept %v", code, out, slept)
	}
}

func TestSignOutOnAGatewayThatDoesNotAnswerSignsNobodyOut(t *testing.T) {
	w := newWebappWorld(t)
	before := w.mintToken()
	w.answer("webapp-sign-out", &gatewayUnreachable{"timed out"})
	w.states = []string{"inaccessible"}
	code, out, _ := w.run(webappArgs(map[string]any{"sign_out": true}))
	if code != 1 || !strings.Contains(strings.ToLower(out), "nobody was signed out") || strings.Contains(out, "Signed out") || w.storedToken() != before {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestAHomeWhoseGatewayNeverRanReadsAsHavingNone(t *testing.T) {
	idle := webappTestHome(t, filepath.Join(t.TempDir(), "idle"), "")
	if state := webappGatewayState(context.Background(), idle); state != "absent" {
		t.Fatalf("state %q", state)
	}
}

func TestAHermesHomeAtADeletedProfileIsRefusedLikeItsProfileFlag(t *testing.T) {
	for _, deleted := range []string{"removed", "tombstoned"} {
		for _, action := range []map[string]any{{"sign_out": true}, {"status": true}, {}} {
			w := newWebappWorld(t)
			gone := filepath.Join(w.root, "profiles", "gone")
			if deleted == "tombstoned" {
				webappTestHome(t, gone, "")
				if err := home.MarkProfileDeleted(gone); err != nil {
					t.Fatal(err)
				}
			}
			before := w.mintToken()
			w.useHome(gone)
			code, out, errOut := w.run(webappArgs(action))
			if code != 1 || !strings.Contains(errOut, "Error: Profile 'gone' does not exist. Create it with: icebath profile create gone") ||
				strings.Contains(out, "Signed out") || len(w.control) != 0 || len(w.ensure) != 0 || w.build != 0 || w.storedToken() != before {
				t.Fatalf("%s %v code %d out %q err %q", deleted, action, code, out, errOut)
			}
		}
	}
}

func TestALinkForTheUserToOpenByHandIsPrintedAndNeverHandedToABrowserLauncher(t *testing.T) {
	w := newWebappWorld(t)
	w.answer("webapp-launch", map[string]any{"ok": true, "result": map[string]any{"url": webappTestLink, "private": true, "expires_in_seconds": int64(60)}})
	wl.open = func(target string) bool { w.opened = append(w.opened, target); return false }
	code, out, _ := w.run(webappArgs(nil))
	for _, u := range w.opened {
		if strings.Contains(u, "ticket") {
			t.Fatalf("opened %q", u)
		}
	}
	if code != 0 || !strings.Contains(out, webappTestLink) || !strings.Contains(out, "keep it private") || !strings.Contains(out, "60 seconds") || !strings.Contains(out, "once") {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Whether output names every secure context feature plain HTTP loses
func saysWhatPlainHTTPLoses(out string) bool {
	lowered := strings.ToLower(out)
	for _, part := range []string{"plain http", "bot screen", "microphone", "wake word", "reading the clipboard", "notifications", "https"} {
		if !strings.Contains(lowered, part) {
			return false
		}
	}
	return true
}

func TestAWebappOpenedOverPlainHTTPOnTheLANSaysWhatTheBrowserLoses(t *testing.T) {
	for _, noOpen := range []bool{false, true} {
		w := newWebappWorld(t)
		w.answer("webapp-launch", map[string]any{"ok": true, "result": map[string]any{"url": "http://192.168.1.20:9120/", "private": false, "expires_in_seconds": nil, "sign_in": true}})
		if code, out, _ := w.run(webappArgs(map[string]any{"no_open": noOpen})); code != 0 || !saysWhatPlainHTTPLoses(out) {
			t.Fatalf("no open %v code %d out %q", noOpen, code, out)
		}
	}
}

// Records a LAN sign in registration for the address and pins it as this machine's LAN address
func (w *webappWorld) registerLANSignIn(address string, port int) {
	w.t.Helper()
	record := filepath.Join(w.root, "webapp", "sign-in-registration.json")
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		w.t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"portal": "https://portal.example", "client_id": "agent:x",
		"callback": fmt.Sprintf("http://%s:%d/auth/callback", address, port)})
	if err := os.WriteFile(record, append(raw, '\n'), 0o644); err != nil {
		w.t.Fatal(err)
	}
	wl.lanAddress = func() string { return address }
}

func TestStatusOfAWebappBrowsersOpenOverPlainHTTPOnTheLANSaysWhatTheBrowserLoses(t *testing.T) {
	for _, browserAddress := range []string{"public_url", "lan_sign_in"} {
		w := newWebappWorld(t)
		if browserAddress == "public_url" {
			webappTestHome(t, w.root, "webapp:\n  enabled: true\n  host: 0.0.0.0\n  public_url: http://192.168.1.20:9120\n")
		} else {
			webappTestHome(t, w.root, "webapp:\n  enabled: true\n  host: 0.0.0.0\n")
			w.registerLANSignIn("192.168.1.20", 9120)
		}
		code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
		if code != 0 || !strings.Contains(out, "Serving at http://127.0.0.1:9120") || !saysWhatPlainHTTPLoses(out) {
			t.Fatalf("%s code %d out %q", browserAddress, code, out)
		}
	}
}

func TestStatusGetsNoPlainHTTPWarningWhenBrowsersOpenASecureAddress(t *testing.T) {
	for _, publicURL := range []string{"", "https://icebath.example.ts.net"} {
		w := newWebappWorld(t)
		config := "webapp:\n  enabled: true\n  host: 0.0.0.0\n"
		if publicURL != "" {
			config += "  public_url: " + publicURL + "\n"
		}
		webappTestHome(t, w.root, config)
		if code, out, _ := w.run(webappArgs(map[string]any{"status": true})); code != 0 || strings.Contains(strings.ToLower(out), "plain http") {
			t.Fatalf("%q code %d out %q", publicURL, code, out)
		}
	}
}

func TestStatusOfAPrivateWebappIgnoresALANSignInRegistrationItKept(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		w := newWebappWorld(t)
		webappTestHome(t, w.root, "webapp:\n  enabled: true\n  host: "+host+"\n")
		w.registerLANSignIn("192.168.1.20", 9120)
		code, out, _ := w.run(webappArgs(map[string]any{"status": true}))
		if code != 0 || !strings.Contains(out, "Serving at http://127.0.0.1:9120") || strings.Contains(strings.ToLower(out), "plain http") {
			t.Fatalf("%s code %d out %q", host, code, out)
		}
	}
}

func TestLoopbackAndHTTPSAddressesAreSecureContextsAndGetNoWarning(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:9120/", "http://127.0.0.2:9120/", "http://localhost:9120/",
		"http://icebath.localhost:9120/", "http://[::1]:9120/", "https://icebath.example.ts.net/"} {
		w := newWebappWorld(t)
		w.answer("webapp-launch", map[string]any{"ok": true, "result": map[string]any{"url": u, "private": false, "expires_in_seconds": nil, "sign_in": true}})
		if code, out, _ := w.run(webappArgs(map[string]any{"no_open": true})); code != 0 || strings.Contains(strings.ToLower(out), "plain http") {
			t.Fatalf("%s code %d out %q", u, code, out)
		}
	}
}

func TestAOneUseLinkOnLoopbackGetsNoPlainHTTPWarning(t *testing.T) {
	w := newWebappWorld(t)
	_, out1, _ := w.run(webappArgs(map[string]any{"no_open": true}))
	_, out2, _ := w.run(webappArgs(map[string]any{"status": true}))
	if strings.Contains(strings.ToLower(out1+out2), "plain http") {
		t.Fatalf("out %q %q", out1, out2)
	}
}
