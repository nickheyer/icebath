package provider

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readGuardState(t *testing.T, g *NousRateGuard) nousRateState {
	t.Helper()
	raw, err := os.ReadFile(g.statePath())
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	var st nousRateState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state decode: %v", err)
	}
	return st
}

func TestRecordsWithHeaderReset(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests-1h", "1800")
	if err := g.Record(h, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	st := readGuardState(t, g)
	if math.Abs(st.ResetSeconds-1800) > 2 || st.ResetAt <= float64(time.Now().UnixNano())/1e9 {
		t.Fatalf("state %+v", st)
	}
}

func TestFallsBackToErrorContextResetAt(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	future := time.Now().Add(900 * time.Second)
	if err := g.Record(nil, future, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	st := readGuardState(t, g)
	if math.Abs(st.ResetAt-float64(future.UnixNano())/1e9) > 1 {
		t.Fatalf("reset_at %v want %v", st.ResetAt, float64(future.UnixNano())/1e9)
	}
}

func TestThe429BodysWaitOutranksTheHourlyWindowEnd(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir(), Anonymous: true}
	h := http.Header{}
	h.Set("x-ratelimit-remaining-requests-1h", "37")
	h.Set("x-ratelimit-reset-requests-1h", "3594")
	if err := g.Record(h, time.Now().Add(410*time.Second), 300*time.Second); err != nil {
		t.Fatal(err)
	}
	st := readGuardState(t, g)
	if math.Abs(st.ResetSeconds-410) > 2 {
		t.Fatalf("reset_seconds %v", st.ResetSeconds)
	}
	if filepath.Base(g.statePath()) != "nous-anonymous.json" {
		t.Fatalf("anonymous state path %s", g.statePath())
	}
}

func TestCustomDefaultCooldown(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	if err := g.Record(nil, time.Time{}, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	if st := readGuardState(t, g); math.Abs(st.ResetSeconds-120) > 2 {
		t.Fatalf("reset_seconds %v", st.ResetSeconds)
	}
}

func TestRemainingSecondsWhenActive(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests-1h", "600")
	if err := g.Record(h, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	remaining, ok := g.Remaining()
	if !ok || remaining <= 595*time.Second || remaining > 605*time.Second {
		t.Fatalf("remaining %v %v", remaining, ok)
	}
}

func TestRemainingNoneWhenExpired(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(g.statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	raw, _ := json.Marshal(map[string]any{"reset_at": now - 10, "recorded_at": now - 100})
	if err := os.WriteFile(g.statePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Remaining(); ok {
		t.Fatal("expired state should report none")
	}
	if _, err := os.Stat(g.statePath()); !os.IsNotExist(err) {
		t.Fatal("expired state file should be removed")
	}
}

func TestClearsExistingFile(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	h := http.Header{}
	h.Set("retry-after", "600")
	if err := g.Record(h, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Remaining(); !ok {
		t.Fatal("expected active state")
	}
	if err := g.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Remaining(); ok {
		t.Fatal("state should be cleared")
	}
	if err := g.Clear(); err != nil {
		t.Fatalf("clearing twice must not fail: %v", err)
	}
}

func TestParseResetSecondsHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Reset-Requests-1h", "1200")
	if d, ok := parseResetSeconds(h); !ok || d != 1200*time.Second {
		t.Fatalf("case insensitive %v %v", d, ok)
	}
	if _, ok := parseResetSeconds(nil); ok {
		t.Fatal("nil headers parsed")
	}
	if _, ok := parseResetSeconds(http.Header{}); ok {
		t.Fatal("empty headers parsed")
	}
	zero := http.Header{}
	zero.Set("x-ratelimit-reset-requests-1h", "0")
	if _, ok := parseResetSeconds(zero); ok {
		t.Fatal("zero value parsed")
	}
}

func TestIsGenuineNousRateLimit(t *testing.T) {
	exhausted := headersFrom(map[string]string{
		"x-ratelimit-limit-requests-1h": "800", "x-ratelimit-remaining-requests-1h": "0", "x-ratelimit-reset-requests-1h": "3100",
		"x-ratelimit-limit-requests": "200", "x-ratelimit-remaining-requests": "198", "x-ratelimit-reset-requests": "40",
	})
	if !IsGenuineNousRateLimit(exhausted, nil, time.Now()) {
		t.Fatal("exhausted hourly bucket should be genuine")
	}
	if IsGenuineNousRateLimit(nil, nil, time.Now()) || IsGenuineNousRateLimit(http.Header{}, nil, time.Now()) {
		t.Fatal("bare 429 should be upstream")
	}
	ct := http.Header{}
	ct.Set("content-type", "application/json")
	if IsGenuineNousRateLimit(ct, nil, time.Now()) {
		t.Fatal("content type only should be upstream")
	}
	prior := ParseRateLimitHeaders(headersFrom(map[string]string{
		"x-ratelimit-limit-requests-1h": "800", "x-ratelimit-remaining-requests-1h": "750", "x-ratelimit-reset-requests-1h": "2000",
		"x-ratelimit-limit-requests": "200", "x-ratelimit-remaining-requests": "180", "x-ratelimit-reset-requests": "30",
		"x-ratelimit-limit-tokens": "800000", "x-ratelimit-remaining-tokens": "790000", "x-ratelimit-reset-tokens": "30",
		"x-ratelimit-limit-tokens-1h": "8000000", "x-ratelimit-remaining-tokens-1h": "7900000", "x-ratelimit-reset-tokens-1h": "2000",
	}), "nous", time.Now())
	if IsGenuineNousRateLimit(nil, prior, time.Now()) {
		t.Fatal("healthy last known state should stay upstream")
	}
}

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(raw) + ".sig"
}

func TestWelcomeHostRateLimitMessageNamesTheSlashCommand(t *testing.T) {
	homeDir := t.TempDir()
	g := &NousRateGuard{Home: homeDir, Anonymous: true}
	h := http.Header{}
	h.Set("retry-after", "600")
	if err := g.Record(h, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	route := Route{Provider: "nous", Model: GuestModel, BaseURL: "https://welcome-api.nousresearch.com/v1", APIKey: makeJWT(t, map[string]any{"account_tier": "anonymous"}), Mode: ModeChatCompletions}
	var statuses, buffered []string
	client := &fakeClient{route: route}
	_, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Home: homeDir, BufferStatus: func(s string) { statuses = append(statuses, s) }, Verbose: func(lines ...string) { buffered = append(buffered, lines...) }})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailNousRateLimited {
		t.Fatalf("expected nous rate limited failure, got %v", err)
	}
	expected := "You've used up the allowance for chatting without signing in. It refreshes in " + FriendlyWait(600) + ". Sign in for a bigger allowance, it's free. To sign in: /login."
	if len(statuses) != 1 || statuses[0] != "⏳ "+expected {
		t.Fatalf("statuses %q", statuses)
	}
	if de.Message != expected || de.FreeTierKind != "rate_limited" {
		t.Fatalf("message %q kind %q", de.Message, de.FreeTierKind)
	}
	if len(buffered) != 1 || buffered[0] != "⏳ "+expected+" Trying fallback..." {
		t.Fatalf("buffered %q", buffered)
	}
	if client.calls != 0 {
		t.Fatal("the guard must skip the call")
	}
}

func TestReadUsesUTF8BOMTolerantDecoding(t *testing.T) {
	g := &NousRateGuard{Home: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(g.statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "\xef\xbb\xbf" + `{"reset_at": ` + jsonInt(time.Now().Unix()+3600) + `, "provider": "中文", "recorded_at": 0}`
	if err := os.WriteFile(g.statePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if remaining, ok := g.Remaining(); !ok || remaining <= 0 {
		t.Fatalf("remaining %v %v", remaining, ok)
	}
}

func jsonInt(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
