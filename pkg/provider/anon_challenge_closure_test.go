package provider

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Captures challenge stderr for the test
func anonChallengeStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := challengeStderr
	challengeStderr = buf
	t.Cleanup(func() { challengeStderr = prev })
	return buf
}

// Restores the announce, terminal and browser seams after the test
func anonPresentSeams(t *testing.T) {
	t.Helper()
	announce, terminal, browser, getenv := announceChallengeFn, presentInTerminalFn, openBrowserFn, oauthGetenv
	t.Cleanup(func() {
		announceChallengeFn, presentInTerminalFn, openBrowserFn, oauthGetenv = announce, terminal, browser, getenv
		SetFreeTierGatewayHooks(FreeTierGatewayHooks{})
	})
}

// Records announced challenges, answering with the given broadcast result
func anonRecordAnnounced(answer bool) *[]BrowserChallenge {
	var mu sync.Mutex
	seen := &[]BrowserChallenge{}
	announceChallengeFn = func(env PoolEnv, c BrowserChallenge) bool {
		mu.Lock()
		*seen = append(*seen, c)
		mu.Unlock()
		return answer
	}
	return seen
}

func anonTestChallenge(rawURL, message string) BrowserChallenge {
	return BrowserChallenge{URL: rawURL, Required: true, ExpiresIn: 600 * time.Second, Interval: 2 * time.Second, Message: message}
}

func anonBackground(env PoolEnv) PoolEnv {
	env.Ctx = withBackgroundCaller(env.ctx())
	return env
}

// An isolated profile with no portal, for presenting tests
func anonPresentEnv(t *testing.T) PoolEnv {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"ICEBATH_DESKTOP", "ICEBATH_DASHBOARD_SESSION_TOKEN", "SSH_TTY", "SSH_CLIENT"} {
		t.Setenv(name, "")
	}
	resetChallengeForTests()
	t.Cleanup(resetChallengeForTests)
	anonPresentSeams(t)
	return modelsPoolEnv(context.Background())
}

func TestChallengeAHostFailureWakesTheWaiterButStillRequiresAnAuthoritativeMint(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	presentChallengeFn = func(env PoolEnv, c BrowserChallenge) {
		if !RecordHostOutcome(env, c.URL, c.Attempt, "error") {
			t.Error("host outcome not recorded")
		}
	}
	_, err := anonResolve(t, env)
	if ae := anonCodeOf(t, err); !strings.Contains(ae.Error(), "couldn't finish") {
		t.Fatal(ae.Error())
	}
	if portal.count("/api/anonymous/token") != 2 || portal.count("/api/anonymous/challenge/status") != 1 {
		t.Fatal(portal.paths())
	}
	if _, err := resolveNousRuntimeCredentials(anonDesktopEnv(env), false, ""); err == nil {
		t.Fatal("a second caller must fail fast")
	}
	if portal.count("/api/anonymous/challenge/status") != 1 {
		t.Fatal(portal.paths())
	}
}

func TestChallengeAPollTheServiceRefusesEndsTheWaitInsteadOfRunningOutTheClock(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired, p.challengeNeverClears = true, true
		p.challengeStatuses = []string{"pending"}
		p.routes = map[string]func(*http.Request, map[string]any) (int, map[string]any){
			"/api/anonymous/challenge/status": func(*http.Request, map[string]any) (int, map[string]any) {
				if len(p.challengeStatuses) > 0 {
					status := p.challengeStatuses[0]
					p.challengeStatuses = p.challengeStatuses[1:]
					return 200, map[string]any{"status": status}
				}
				return 404, map[string]any{"error": "unknown_token"}
			},
		}
	})
	_, err := anonResolve(t, env)
	if ae := anonCodeOf(t, err); ae.Code != AnonChallengeRequired {
		t.Fatal(ae.Code)
	}
	if portal.count("/api/anonymous/challenge/status") != 2 {
		t.Fatal(portal.paths())
	}
}

func TestChallengeTheStatusPollTrustsWhatTheCredentialWasMintedWith(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	anonClaimedTrial(t, env)
	anonPatchNous(t, env, map[string]any{"tls": map[string]any{"insecure": true}})
	var mu sync.Mutex
	var polledWith []oauthVerify
	real := nousHTTPClientFn
	t.Cleanup(func() { nousHTTPClientFn = real })
	nousHTTPClientFn = func(e PoolEnv, timeout time.Duration, verify oauthVerify) *http.Client {
		mu.Lock()
		polledWith = append(polledWith, verify)
		mu.Unlock()
		return real(e, timeout, verify)
	}
	portal.set(func(p *anonFakePortal) { p.challengeRequired = true })
	if _, err := resolveNousRuntimeCredentials(anonDesktopEnv(env), false, ""); err != nil {
		t.Fatal(err)
	}
	if portal.count("/api/anonymous/challenge/status") < 1 || len(polledWith) == 0 {
		t.Fatal(portal.paths(), polledWith)
	}
	for _, verify := range polledWith {
		if !verify.Insecure {
			t.Fatalf("%+v", polledWith)
		}
	}
}

func TestChallengeAStaleStatusCannotHideAClearanceFromTheFinalMint(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	challengeWait = 0
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired, p.challengeNeverClears = true, true
		p.routes = map[string]func(*http.Request, map[string]any) (int, map[string]any){
			"/api/anonymous/challenge/status": func(*http.Request, map[string]any) (int, map[string]any) {
				p.challengeRequired = false
				return 200, map[string]any{"status": "pending"}
			},
		}
	})
	creds, err := anonResolve(t, env)
	if err != nil || anyString(creds["api_key"]) == "" {
		t.Fatalf("%v %v", creds, err)
	}
	if portal.count("/api/anonymous/token") != 2 || PendingChallenge(env) != nil {
		t.Fatal(portal.paths(), PendingChallenge(env))
	}
}

func TestChallengeTheManagedToolTokenPathWorksAChallengeToo(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil || !isGuestState(state) {
		t.Fatalf("%v %v", state, err)
	}
	portal.set(func(p *anonFakePortal) {
		p.honoursPurpose, p.challengeRequired = false, true
		p.challengeStatuses = []string{"pending"}
	})
	if token, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); err != nil || token == "" {
		t.Fatalf("%q %v", token, err)
	}
	if len(*presented) != 1 {
		t.Fatal(*presented)
	}
}

func TestChallengeABackgroundCallerNeverPrintsOrOpensABrowser(t *testing.T) {
	env := anonPresentEnv(t)
	stderr := anonChallengeStderr(t)
	openBrowserFn = func(string) error {
		t.Error("opened a browser in the background")
		return nil
	}
	presentChallenge(anonBackground(env), anonTestChallenge("https://portal.example.test/challenge?code=t", "A quick check."))
	if stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
}

func TestChallengeTheKeepaliveAndTheStatusPaintAreBackgroundCallers(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil || !isGuestState(state) {
		t.Fatalf("%v %v", state, err)
	}
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	if RefreshNousAuthKeepaliveOnce(env, NousKeepaliveOptions{}) {
		t.Fatal("keepalive refreshed a guest")
	}
	computeNousAuthStatus(env)
	if portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatal(portal.paths())
	}
}

func TestChallengeAPollErrorIsABlipNotAVerdict(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatusErrors = []int{429, 500}
		p.challengeStatuses = []string{"pending"}
	})
	creds, err := anonResolve(t, env)
	if err != nil || anyString(creds["api_key"]) == "" || len(*presented) != 1 {
		t.Fatalf("%v %v %d", creds, err, len(*presented))
	}
}

func TestChallengeAnInheritedDesktopFlagIsNotTheDesktopSurface(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonPresentSeams(t)
	stderr := anonChallengeStderr(t)
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	t.Setenv("SSH_TTY", "/dev/pts/0")
	openBrowserFn = func(string) error { return syscall.ENOENT }
	if _, err := anonConnectorsToken(t, env); err != nil {
		t.Fatal(err)
	}
	last := portal.tokenRequests[len(portal.tokenRequests)-1]["body"].(map[string]any)["client"].(map[string]any)
	if last["surface"] != "cli" {
		t.Fatal(last)
	}
	link := portal.URL + "/challenge?code=t"
	presentChallenge(env, anonTestChallenge(link, "m"))
	if !strings.Contains(stderr.String(), link) {
		t.Fatal(stderr.String())
	}
}

func TestChallengeOptionalATerminalNeverOpensAnything(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonPresentSeams(t)
	presented := anonRecordPresented(t)
	announced := anonRecordAnnounced(false)
	portal.set(func(p *anonFakePortal) { p.honoursPurpose, p.optionalChallenge = false, true })
	if token, err := anonConnectorsToken(t, env); err != nil || token == "" {
		t.Fatalf("%q %v", token, err)
	}
	if len(*presented) != 0 || len(*announced) != 0 {
		t.Fatal(*presented, *announced)
	}
}

func TestChallengeOptionalTheDesktopBackendAnnouncesItAndWaitsOnNothing(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonPresentSeams(t)
	anonRecordPresented(t)
	announced := anonRecordAnnounced(false)
	portal.set(func(p *anonFakePortal) { p.optionalChallenge = true })
	creds, err := anonResolve(t, env)
	if err != nil || anyString(creds["api_key"]) == "" {
		t.Fatalf("%v %v", creds, err)
	}
	if len(*announced) != 1 || (*announced)[0].Required || portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatal(*announced, portal.paths())
	}
	if pending := PendingChallenge(env); pending == nil || pending["url"] != portal.challengeURL {
		t.Fatal(pending)
	}
}

func TestChallengeOptionalReplayedOffersKeepTheOriginalExpiry(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRestoreMonotonic(t)
	clock := time.Unix(100, 0)
	anonMonotonic = func() time.Time { return clock }
	challenge := BrowserChallenge{URL: portal.challengeURL, Required: false, ExpiresIn: 60 * time.Second, Interval: 2 * time.Second, Message: "check"}
	recordChallenge(env, challenge, false)
	clock = clock.Add(20 * time.Second)
	recordChallenge(env, challenge, false)
	if got := PendingChallenge(env)["expires_in"]; got != 40 {
		t.Fatal(got)
	}
	retry := recordChallenge(env, challenge, true)
	if RecordHostOutcome(env, challenge.URL, 0, "done") {
		t.Fatal("an old attempt's result ended the retry")
	}
	if got := PendingChallenge(env)["attempt"]; got != retry.Attempt {
		t.Fatal(got, retry.Attempt)
	}
	clock = clock.Add(41 * time.Second)
	if pending := PendingChallenge(env); pending != nil {
		t.Fatal(pending)
	}
}

func TestChallengePresentingTheDesktopBackendAnnouncesAndNeverOpensABrowser(t *testing.T) {
	env := anonPresentEnv(t)
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "spawn-token")
	announced := anonRecordAnnounced(true)
	var opened []BrowserChallenge
	presentInTerminalFn = func(_ PoolEnv, c BrowserChallenge) { opened = append(opened, c) }
	challenge := anonTestChallenge("https://portal.example.test/challenge?code=t", "m")
	presentChallenge(env, challenge)
	if !reflect.DeepEqual(*announced, []BrowserChallenge{challenge}) || len(opened) != 0 {
		t.Fatal(*announced, opened)
	}
}

func TestChallengePresentingTheStdioTUIGatewayHandsTheLinkToItsClient(t *testing.T) {
	env := anonPresentEnv(t)
	stderr := anonChallengeStderr(t)
	type sentEvent struct {
		event   string
		payload map[string]any
	}
	var sent []sentEvent
	SetFreeTierGatewayHooks(FreeTierGatewayHooks{StdioIsRPCChannel: func() bool { return true },
		BroadcastGlobal: func(event string, payload map[string]any) error {
			sent = append(sent, sentEvent{event, payload})
			return nil
		}})
	challenge := anonTestChallenge("https://portal.example.test/challenge?code=t", "m")
	presentChallenge(anonBackground(env), challenge)
	if len(sent) != 0 {
		t.Fatal("a keepalive tick never pops a browser", sent)
	}
	presentChallenge(env, challenge)
	if !reflect.DeepEqual(sent, []sentEvent{{ChallengeEvent, challenge.AsPayload()}}) || stderr.Len() != 0 {
		t.Fatal(sent, stderr.String())
	}
}

func TestChallengePresentingAStdioWriteFailureFallsBackToTheTerminal(t *testing.T) {
	env := anonPresentEnv(t)
	SetFreeTierGatewayHooks(FreeTierGatewayHooks{StdioIsRPCChannel: func() bool { return true },
		BroadcastGlobal: func(string, map[string]any) error { return syscall.ENOSPC }})
	var opened []BrowserChallenge
	presentInTerminalFn = func(_ PoolEnv, c BrowserChallenge) { opened = append(opened, c) }
	challenge := anonTestChallenge("https://portal.example.test/challenge?code=t", "m")
	presentChallenge(env, challenge)
	if !reflect.DeepEqual(opened, []BrowserChallenge{challenge}) {
		t.Fatal(opened)
	}
}

func TestChallengePresentingATerminalOpensOneTabPerTicket(t *testing.T) {
	env := anonPresentEnv(t)
	stderr := anonChallengeStderr(t)
	oauthGetenv = func(name string) string {
		switch name {
		case "DISPLAY":
			return ":0"
		case "BROWSER":
			return "firefox"
		}
		return ""
	}
	var opened []string
	openBrowserFn = func(u string) error {
		opened = append(opened, u)
		return nil
	}
	challenge := anonTestChallenge("https://portal.example.test/challenge?code=once", "m")
	presentChallenge(env, challenge)
	presentChallenge(env, challenge)
	if !reflect.DeepEqual(opened, []string{challenge.URL}) || strings.Count(stderr.String(), challenge.URL) != 2 {
		t.Fatal(opened, stderr.String())
	}
}

func TestChallengePresentingATerminalPrintsTheLinkAndSkipsTheBrowserOverSSH(t *testing.T) {
	env := anonPresentEnv(t)
	stderr := anonChallengeStderr(t)
	t.Setenv("SSH_TTY", "/dev/pts/0")
	openBrowserFn = func(string) error {
		t.Error("opened a browser over SSH")
		return nil
	}
	challenge := anonTestChallenge("https://portal.example.test/challenge?code=t", "A quick check.")
	presentChallenge(env, challenge)
	if out := stderr.String(); !strings.Contains(out, "A quick check.") || !strings.Contains(out, challenge.URL) {
		t.Fatal(out)
	}
}

func anonPurposes(p *anonFakePortal) []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []any{}
	for _, r := range p.tokenRequests {
		out = append(out, r["body"].(map[string]any)["purpose"])
	}
	return out
}

func anonTokenRequests(p *anonFakePortal) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokenRequests)
}

func anonScope(token string) any { return DecodeJWTClaims(token)["scope"] }

func anonForgetMemo() {
	resolveTokenCacheMu.Lock()
	resolveTokenCache = map[string]resolvedNousToken{}
	resolveTokenCacheMu.Unlock()
}

// Writes live or expired inference and connectors tokens into the guest state, ttl nil leaving a slot alone
func anonPersistGuestTokens(t *testing.T, env PoolEnv, inference, connectors *int) map[string]string {
	t.Helper()
	at := func(s int) string {
		return time.Now().UTC().Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano)
	}
	tokens := map[string]string{}
	fields := map[string]any{}
	if inference != nil {
		tokens["inference"] = anonJWT(map[string]any{"exp": time.Now().Unix() + int64(*inference)})
		fields["access_token"], fields["scope"], fields["token_type"] = tokens["inference"], "inference:invoke", "Bearer"
		fields["inference_base_url"], fields["expires_at"] = anonWelcome, at(*inference)
	}
	if connectors != nil {
		tokens["connectors"] = anonJWT(map[string]any{"scope": "connectors:invoke", "exp": time.Now().Unix() + int64(*connectors)})
		fields["connectors_token"] = map[string]any{"access_token": tokens["connectors"], "expires_at": at(*connectors)}
	}
	anonPatchNous(t, env, fields)
	return tokens
}

// Installs the portal with a connectors guest minted, as the nas fixture
func anonConnectorsGuest(t *testing.T) (*anonFakePortal, PoolEnv) {
	t.Helper()
	portal, env := installAnonPortal(t)
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil || !isGuestState(state) {
		t.Fatalf("%v %v", state, err)
	}
	return portal, env
}

// Installs the portal with the welcome trial claimed, as the trial fixture, returning the desktop env
func anonTrialGuest(t *testing.T) (*anonFakePortal, PoolEnv) {
	t.Helper()
	portal, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	return portal, anonDesktopEnv(env)
}

func anonMustToken(t *testing.T, env PoolEnv) string {
	t.Helper()
	token, err := ResolveNousAccessToken(env, NousAccessTokenOptions{})
	if err != nil || token == "" {
		t.Fatalf("%q %v", token, err)
	}
	return token
}

func TestConnectorsCallersNeverChallengedEvenWhileTheInferenceMintWouldBe(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	token := anonMustToken(t, env)
	if read, err := ReadNousAccessToken(env); err != nil || read != token {
		t.Fatalf("%q %v", read, err)
	}
	if anonScope(token) != "connectors:invoke" || !reflect.DeepEqual(anonPurposes(portal), []any{"connectors"}) {
		t.Fatal(anonScope(token), anonPurposes(portal))
	}
	if len(*presented) != 0 || portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatal(*presented, portal.paths())
	}
}

func TestConnectorsCallersPickTheLiveSlotOrMintConnectorsOnly(t *testing.T) {
	ttl := func(v int) *int { return &v }
	cases := []struct {
		name                  string
		inference, connectors *int
		serves                string
	}{
		{"expired inference live connectors", ttl(-60), ttl(900), "connectors"},
		{"live inference expired connectors", ttl(900), ttl(-60), "inference"},
		{"both expired", ttl(-60), ttl(-60), ""},
		{"only an expired inference token", ttl(-60), nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			portal, env := anonConnectorsGuest(t)
			presented := anonRecordPresented(t)
			held := anonPersistGuestTokens(t, env, c.inference, c.connectors)
			portal.set(func(p *anonFakePortal) { p.challengeRequired = true })
			token := anonMustToken(t, env)
			anonForgetMemo()
			if again, err := ReadNousAccessToken(env); err != nil || again != token {
				t.Fatalf("%q then %q %v", token, again, err)
			}
			if c.serves != "" {
				if token != held[c.serves] || anonTokenRequests(portal) != 0 {
					t.Fatalf("%q %d", token, anonTokenRequests(portal))
				}
			} else {
				slot, _ := currentNousState(env)["connectors_token"].(map[string]any)
				if anonScope(token) != "connectors:invoke" || !reflect.DeepEqual(anonPurposes(portal), []any{"connectors"}) || slot["access_token"] != token {
					t.Fatal(anonPurposes(portal), slot)
				}
			}
			if len(*presented) != 0 {
				t.Fatal(*presented)
			}
		})
	}
}

func TestConnectorsCallersTheConnectorsTokenIsPersistedBesideTheInferenceSlot(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	token := anonMustToken(t, env)
	state := currentNousState(env)
	slot, _ := state["connectors_token"].(map[string]any)
	if slot["access_token"] != token || anyString(state["access_token"]) != "" {
		t.Fatal(state)
	}
	anonForgetMemo()
	if anonMustToken(t, env) != token || anonTokenRequests(portal) != 1 {
		t.Fatal(anonTokenRequests(portal))
	}
}

func TestConnectorsCallersAccountReadsUseTheConnectorsToken(t *testing.T) {
	portal, env := anonTrialGuest(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired = true })
	anonMustToken(t, env)
	ResetNousPortalAccountInfoCache()
	t.Cleanup(ResetNousPortalAccountInfoCache)
	info := GetNousPortalAccountInfo(env.ctx(), false)
	if !info.LoggedIn || info.Source != "jwt" || info.AccountTier != "anonymous" {
		t.Fatalf("%+v", info)
	}
	if anonTokenRequests(portal) != 1 || len(*presented) != 0 {
		t.Fatal(anonTokenRequests(portal), *presented)
	}
}

func TestConnectorsCallersARefusalSurfacesAsItDoesToday(t *testing.T) {
	for _, code := range []string{"signin_required", "access_denied"} {
		t.Run(code, func(t *testing.T) {
			portal, env := anonConnectorsGuest(t)
			portal.set(func(p *anonFakePortal) {
				p.tokenResponse = &anonCanned{status: 403, body: map[string]any{"error": code}}
			})
			_, err := ResolveNousAccessToken(env, NousAccessTokenOptions{})
			ae := anonCodeOf(t, err)
			if ae.Code != AnonSigninRequired || ae.Retryable == nil || *ae.Retryable || ae.Error() != AnonFailureCopy[AnonSigninRequired] {
				t.Fatalf("%+v", ae)
			}
		})
	}
}

func TestConnectorsInferenceCallersStillWorkTheChallengeAndNeverTakeTheConnectorsToken(t *testing.T) {
	portal, env := anonTrialGuest(t)
	presented := anonRecordPresented(t)
	connectors := anonMustToken(t, env)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatuses = []string{"pending"}
	})
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil || creds["api_key"] == connectors || anonScope(anyString(creds["api_key"])) != "inference:invoke" {
		t.Fatalf("%v %v", creds, err)
	}
	if len(*presented) != 1 || (*presented)[0].URL != portal.challengeURL {
		t.Fatal(*presented)
	}
	if !reflect.DeepEqual(anonPurposes(portal), []any{"connectors", "inference", "inference"}) {
		t.Fatal(anonPurposes(portal))
	}
}

func TestConnectorsInferenceCallersAHeldInferenceTokenSatisfiesConnectorsCallers(t *testing.T) {
	portal, env := anonTrialGuest(t)
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil {
		t.Fatal(err)
	}
	anonForgetMemo()
	if anonMustToken(t, env) != creds["api_key"] || anonTokenRequests(portal) != 1 {
		t.Fatal(anonTokenRequests(portal))
	}
}

func TestConnectorsInferenceCallersAConnectorsMintLeavesAPendingInferenceChallengeAlone(t *testing.T) {
	portal, env := anonTrialGuest(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	if _, err := resolveNousRuntimeCredentials(anonBackground(env), false, ""); err == nil {
		t.Fatal("a background caller waited the challenge out")
	}
	if pending := PendingChallenge(env); pending == nil || pending["url"] != portal.challengeURL {
		t.Fatal(pending)
	}
	anonMustToken(t, env)
	if pending := PendingChallenge(env); pending == nil || pending["url"] != portal.challengeURL {
		t.Fatal(pending)
	}
}

func TestConnectorsInferenceCallersAConnectorsCallerNeverQueuesBehindAnInferenceChallenge(t *testing.T) {
	portal, env := anonTrialGuest(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatuses = []string{"pending"}
	})
	var mu sync.Mutex
	var duringWait []string
	presentChallengeFn = func(PoolEnv, BrowserChallenge) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			token, _ := ResolveNousAccessToken(env, NousAccessTokenOptions{})
			mu.Lock()
			duringWait = append(duringWait, token)
			mu.Unlock()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil || anyString(creds["api_key"]) == "" {
		t.Fatalf("%v %v", creds, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(duringWait) != 1 || anonScope(duringWait[0]) != "connectors:invoke" {
		t.Fatal(duringWait)
	}
}

func TestConnectorsInferenceCallersAnInferenceGiveUpCooldownDoesNotFailConnectorsCallersFast(t *testing.T) {
	portal, env := anonTrialGuest(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	challengeWait = 0
	if _, err := resolveNousRuntimeCredentials(env, false, ""); err == nil {
		t.Fatal("the challenge never cleared")
	}
	if !challengeGaveUpRecently(env) || anonScope(anonMustToken(t, env)) != "connectors:invoke" {
		t.Fatal("connectors caller failed fast")
	}
}

func TestConnectorsNoInferenceIntentAConnectorsOnlyGuestNeverMintsInference(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	presented := anonRecordPresented(t)
	if _, err := resolveNousRuntimeCredentials(env, false, ""); err == nil {
		t.Fatal("a connectors guest got an inference token")
	}
	if anonTokenRequests(portal) != 0 || len(*presented) != 0 || !isGuestState(currentNousState(env)) {
		t.Fatal(anonTokenRequests(portal), *presented)
	}
}

func TestConnectorsNoInferenceIntentTheKeepaliveNeverMintsForAGuest(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	anonMustToken(t, env)
	if RefreshNousAuthKeepaliveOnce(env, NousKeepaliveOptions{}) || !reflect.DeepEqual(anonPurposes(portal), []any{"connectors"}) {
		t.Fatal(anonPurposes(portal))
	}
}

func TestConnectorsNoInferenceIntentAModelCatalogReadNeverWaitsOnAChallenge(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) { p.challengeRequired, p.challengeNeverClears = true, true })
	rows, ok := nousCatalog(env.ctx(), "nous", false)
	curated := GetCuratedNousModelIDs(env.ctx())
	if len(curated) > 0 {
		if !ok || !reflect.DeepEqual(rows, plainRows(curated)) {
			t.Fatal(rows, curated)
		}
	} else if ok {
		t.Fatal(rows)
	}
	if portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatal(portal.paths())
	}
}

func TestConnectorsOlderNasAnInferenceTokenForAConnectorsMintFillsTheInferenceSlot(t *testing.T) {
	portal, env := anonTrialGuest(t)
	portal.set(func(p *anonFakePortal) { p.honoursPurpose = false })
	token := anonMustToken(t, env)
	state := currentNousState(env)
	if anonScope(token) != "inference:invoke" || state["access_token"] != token || state["connectors_token"] != nil {
		t.Fatal(state)
	}
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil || creds["api_key"] != token || anonTokenRequests(portal) != 1 {
		t.Fatal(creds, err, anonTokenRequests(portal))
	}
}

func TestConnectorsOlderNasAChallengedConnectorsMintFallsBackToTheChallengeFlow(t *testing.T) {
	portal, env := anonConnectorsGuest(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.honoursPurpose, p.challengeRequired = false, true
		p.challengeStatuses = []string{"pending"}
	})
	if scope := anonScope(anonMustToken(t, env)); scope != "inference:invoke" || len(*presented) != 1 {
		t.Fatal(scope, *presented)
	}
}

func TestConnectorsPersistedStateWrittenByCurrentReleasesLoadsUnchanged(t *testing.T) {
	portal, env := anonTrialGuest(t)
	ttl := 900
	token := anonPersistGuestTokens(t, env, &ttl, nil)["inference"]
	if PeekNousAccessToken(env) != token || anonMustToken(t, env) != token {
		t.Fatal("connectors read")
	}
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil || creds["api_key"] != token || anonTokenRequests(portal) != 0 {
		t.Fatal(creds, err, anonTokenRequests(portal))
	}
}
