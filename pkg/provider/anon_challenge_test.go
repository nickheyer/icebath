package provider

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// Records every presented challenge in place of presenting it
func anonRecordPresented(t *testing.T) *[]BrowserChallenge {
	t.Helper()
	var mu sync.Mutex
	seen := &[]BrowserChallenge{}
	presentChallengeFn = func(env PoolEnv, c BrowserChallenge) {
		mu.Lock()
		*seen = append(*seen, c)
		mu.Unlock()
	}
	return seen
}

// A free model call by a guest that claimed the welcome trial on the desktop backend
func anonResolve(t *testing.T, env PoolEnv) (map[string]any, error) {
	t.Helper()
	anonClaimedTrial(t, env)
	return resolveNousRuntimeCredentials(anonDesktopEnv(env), false, "")
}

// A connectors token read, the one exchange a terminal guest makes
func anonConnectorsToken(t *testing.T, env PoolEnv) (string, error) {
	t.Helper()
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || !isGuestState(state) {
		t.Fatalf("guest: %v %v", state, err)
	}
	return ResolveNousAccessToken(env, NousAccessTokenOptions{})
}

func TestChallengeExchangeReportsSurfaceVersionAndTheBrowserCapability(t *testing.T) {
	portal, env := installAnonPortal(t)
	if _, err := anonConnectorsToken(t, env); err != nil {
		t.Fatal(err)
	}
	sent := portal.tokenRequests[len(portal.tokenRequests)-1]
	client := sent["body"].(map[string]any)["client"].(map[string]any)
	if caps, _ := client["capabilities"].([]any); len(caps) != 1 || caps[0] != "browser-v1" {
		t.Fatalf("capabilities %v", client["capabilities"])
	}
	if client["surface"] != "cli" || client["name"] != "icebath-agent" {
		t.Fatalf("client %v", client)
	}
	ua := anyString(sent["user_agent"])
	if !strings.HasPrefix(ua, "icebath-agent/") || !strings.Contains(ua, "(cli;") {
		t.Fatalf("user agent %q", ua)
	}
}

func TestChallengeIsPresentedPolledAndTheExchangeRetried(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatuses = []string{"pending", "pending", "needs_interaction"}
	})
	var stderr bytes.Buffer
	challengeStderr = &stderr
	creds, err := anonResolve(t, env)
	if err != nil || anyString(creds["api_key"]) == "" {
		t.Fatalf("%v %v", creds, err)
	}
	if len(*presented) != 1 || (*presented)[0].URL != portal.challengeURL {
		t.Fatalf("presented %v", *presented)
	}
	if !(*presented)[0].Required || (*presented)[0].Message != "A quick check first." {
		t.Fatalf("presented %v", (*presented)[0])
	}
	if portal.count("/api/anonymous/token") != 2 || portal.count("/api/anonymous/challenge/status") != 4 {
		t.Fatalf("paths %v", portal.paths())
	}
	if PendingChallenge(anonDesktopEnv(env)) != nil {
		t.Fatal("a cleared challenge must not stay pending")
	}
}

func TestChallengeWaitHappensOutsideTheAuthLocks(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatuses = []string{"pending"}
	})
	var lockErr error
	presentChallengeFn = func(env PoolEnv, c BrowserChallenge) {
		unlock, err := nousSharedStoreLock(env, 500*time.Millisecond)
		if err != nil {
			lockErr = err
			return
		}
		unlock()
	}
	creds, err := anonResolve(t, env)
	if err != nil || anyString(creds["api_key"]) == "" || lockErr != nil {
		t.Fatalf("%v %v %v", creds, err, lockErr)
	}
}

func TestChallengeThatNeverClearsIsARetryableErrorNotAHang(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeNeverClears = true
	})
	challengeWait = 50 * time.Millisecond
	challengeSleep = func(time.Duration) { time.Sleep(20 * time.Millisecond) }
	_, err := anonResolve(t, env)
	ae := anonCodeOf(t, err)
	if ae.Code != AnonChallengeRequired || !anonRetryable(ae) || !strings.Contains(ae.Error(), "browser") {
		t.Fatalf("%v %v", ae.Code, ae)
	}
	if _, err := resolveNousRuntimeCredentials(anonDesktopEnv(env), false, ""); anonCodeOf(t, err) == nil {
		t.Fatal("a queued caller must fail fast")
	}
	if len(*presented) != 1 {
		t.Fatalf("presented %d", len(*presented))
	}
}

func TestChallengeSecondChallengeAfterTheRetryIsNotLoopedOn(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeStatuses = []string{"failed"}
		p.afterHandle = func(p *anonFakePortal) { p.challengeRequired = true }
	})
	_, err := anonResolve(t, env)
	if ae := anonCodeOf(t, err); ae.Code != AnonChallengeRequired {
		t.Fatalf("code %q", ae.Code)
	}
	if portal.count("/api/anonymous/token") != 2 {
		t.Fatalf("paths %v", portal.paths())
	}
}

func TestChallengeBackgroundCallerAnnouncesAndReturnsAtOnce(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	anonClaimedTrial(t, env)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeNeverClears = true
	})
	background := anonDesktopEnv(env)
	background.Ctx = withBackgroundCaller(background.Ctx)
	_, err := resolveNousRuntimeCredentials(background, false, "")
	ae := anonCodeOf(t, err)
	if ae.Code != AnonChallengeRequired || !anonRetryable(ae) {
		t.Fatalf("%v", ae)
	}
	if len(*presented) != 1 || portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatalf("presented %d paths %v", len(*presented), portal.paths())
	}
	if pending := PendingChallenge(background); pending == nil || pending["url"] != portal.challengeURL {
		t.Fatalf("pending %v", pending)
	}
}

func TestChallengeMessagingGatewayNeverWaitsOnAChallenge(t *testing.T) {
	portal, env := installAnonPortal(t)
	SetFreeTierGatewayHooks(FreeTierGatewayHooks{OwnsRuntimeLock: func() bool { return true }})
	challengeWait = 50 * time.Millisecond
	var stderr bytes.Buffer
	challengeStderr = &stderr
	portal.set(func(p *anonFakePortal) {
		p.honoursPurpose = false
		p.challengeRequired = true
		p.challengeNeverClears = true
	})
	_, err := anonConnectorsToken(t, env)
	ae := anonCodeOf(t, err)
	if ae.Code != AnonChallengeRequired || !anonRetryable(ae) {
		t.Fatalf("%v", ae)
	}
	if portal.count("/api/anonymous/challenge/status") != 0 {
		t.Fatalf("paths %v", portal.paths())
	}
}

func TestChallengeA428OnSignUpIsStillTheProofOfWorkVerdict(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 428, body: map[string]any{"error": "challenge_required", "challenges": []any{}}}
	})
	_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if ae := anonCodeOf(t, err); ae.Code != AnonPowRequired {
		t.Fatalf("code %q", ae.Code)
	}
}

func TestChallengeAProofOfWorkVerdictOff428StaysTheProofOfWorkVerdict(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 400, body: map[string]any{"error": "pow_invalid"}}
	})
	_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if ae := anonCodeOf(t, err); ae.Code != AnonPowRequired || anonRetryable(ae) {
		t.Fatalf("%v", ae)
	}
}

func TestChallengeA403OnSignUpStaysRetryable(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 403, body: map[string]any{"error": "access_denied"}}
	})
	_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if ae := anonCodeOf(t, err); ae.Code != AnonServerError || !anonRetryable(ae) {
		t.Fatalf("%v", ae)
	}
}

func TestChallengeURLOffThePortalOriginIsRefused(t *testing.T) {
	portal, env := installAnonPortal(t)
	presented := anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.challengeRequired = true
		p.challengeURL = "https://evil.example/challenge?code=x"
	})
	_, err := anonResolve(t, env)
	if ae := anonCodeOf(t, err); ae.Code != AnonSigninRequired || anonRetryable(ae) {
		t.Fatalf("%v", ae)
	}
	if len(*presented) != 0 {
		t.Fatalf("presented %v", *presented)
	}
}

func TestChallengeSameOriginMeansSchemeAndHost(t *testing.T) {
	portal := "https://portal.example.test"
	for _, u := range []any{"javascript:alert(1)", "file:///etc/passwd", portal + ".evil.example/challenge",
		strings.Replace(portal, "https://", "http://", 1) + "/challenge", "", nil} {
		payload := map[string]any{"challenges": []any{map[string]any{"type": "browser", "url": u}}}
		if got := parseBrowserChallenge(nousProcessEnv(), payload, portal); got != nil {
			t.Fatalf("%v parsed as %v", u, got)
		}
	}
}

func TestChallengeAnUnknownChallengeTypeFallsBackToSignIn(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRecordPresented(t)
	portal.set(func(p *anonFakePortal) {
		p.tokenResponse = &anonCanned{status: 428, body: map[string]any{"error": "challenge_required", "message": "Attest your device.",
			"challenges": []any{map[string]any{"type": "attestation-v9", "url": p.URL + "/x"}}}}
	})
	_, err := anonResolve(t, env)
	ae := anonCodeOf(t, err)
	if ae.Code != AnonSigninRequired || !strings.Contains(ae.Error(), "Sign in") || strings.Contains(ae.Error(), "Attest") {
		t.Fatalf("%v", ae)
	}
}

func TestChallengeAnUnknown428IsASignInNotTheProofOfWorkSentence(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.tokenResponse = &anonCanned{status: 428, body: map[string]any{"error": "something_new"}}
	})
	_, err := anonResolve(t, env)
	ae := anonCodeOf(t, err)
	if ae.Code != AnonSigninRequired || strings.Contains(ae.Error(), "proof of work") {
		t.Fatalf("%v", ae)
	}
}

func TestChallengeARefusalUsesTheServicesOwnWords(t *testing.T) {
	for _, name := range []string{"signin_required", "access_denied"} {
		t.Run(name, func(t *testing.T) {
			portal, env := installAnonPortal(t)
			portal.set(func(p *anonFakePortal) {
				p.tokenResponse = &anonCanned{status: 403, body: map[string]any{"error": name, "message": "Not from here, sorry."}}
			})
			_, err := anonResolve(t, env)
			ae := anonCodeOf(t, err)
			if ae.Code != AnonSigninRequired || ae.Error() != "Not from here, sorry." {
				t.Fatalf("%v", ae)
			}
		})
	}
}

func TestChallengeNetworkNumbersAreKeptFiniteAndSane(t *testing.T) {
	portal := "https://portal.example.test"
	parsed := func(expiresIn any) *BrowserChallenge {
		return parseBrowserChallenge(nousProcessEnv(), map[string]any{"challenges": []any{map[string]any{"type": "browser",
			"url": portal + "/challenge?code=t", "expires_in": expiresIn}}}, portal)
	}
	if parsed(math.Inf(1)).ExpiresIn != 600*time.Second || parsed(1e12).ExpiresIn != 900*time.Second ||
		parsed(0.0).ExpiresIn != 30*time.Second || parsed(true).ExpiresIn != 600*time.Second {
		t.Fatal("expires_in must be clamped")
	}
	if parsed(math.Inf(1)).AsPayload()["expires_in"] != 600 {
		t.Fatal("payload expires_in")
	}
}

func TestChallengeControlCharactersNeverReachTheTerminal(t *testing.T) {
	if got := serverMessage("Check\x1b[2J this"); got != "Check [2J this" {
		t.Fatalf("%q", got)
	}
	portal := "https://portal.example.test"
	payload := map[string]any{"challenges": []any{map[string]any{"type": "browser", "url": portal + "/challenge?code=\x1b]0;x"}}}
	if parseBrowserChallenge(nousProcessEnv(), payload, portal) != nil {
		t.Fatal("a control character URL must be refused")
	}
}

func TestChallengeDeadCodeOnlyForTheDeadType(t *testing.T) {
	dead := &AnonCredentialDeadError{Auth: anonErr("x", AnonAccountLocked, 0, false)}
	if code, ok := anonCredentialDeadCode(dead); !ok || code != AnonAccountLocked {
		t.Fatalf("%q %v", code, ok)
	}
	if _, ok := anonCredentialDeadCode(errors.New("other")); ok {
		t.Fatal("a plain error is not dead")
	}
}
