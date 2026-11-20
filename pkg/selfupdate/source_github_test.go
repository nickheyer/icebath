package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Loopback GitHub refusing every token with 403, its rate limit spent or not, and answering anonymous reads
func sgRefusingAPI(t *testing.T, spent bool) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			remaining := "4999"
			if spent {
				remaining = "0"
			}
			w.Header().Set("x-ratelimit-remaining", remaining)
			w.WriteHeader(403)
			w.Write([]byte(`{"message": "Forbidden"}`))
			return
		}
		w.Write([]byte(`{"sha": "anonymous-ok"}`))
	}))
	t.Cleanup(server.Close)
	for _, name := range plugins.GithubTokenEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("GITHUB_TOKEN", "stale-token-of-a-suspended-account")
	saved := githubTokenSource
	githubTokenSource = func() (string, bool) { return plugins.GithubTokenFromEnv(nil) }
	t.Cleanup(func() { githubTokenSource = saved })
	return server.URL + "/repos/o/r/commits/main"
}

func TestSourceRequestRefusedTokenFallsBackToAnonymous(t *testing.T) {
	target := sgRefusingAPI(t, false)
	text, err := sourceRequest(context.Background(), target, githubJSONAccept)
	if err != nil || text != `{"sha": "anonymous-ok"}` {
		t.Fatalf("request %q %v", text, err)
	}
}

func TestSourceRequestSpentRateLimitStillFails(t *testing.T) {
	target := sgRefusingAPI(t, true)
	_, err := sourceRequest(context.Background(), target, githubJSONAccept)
	var httpErr *plugins.GithubHTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != 403 || !failureAuthenticated(err) {
		t.Fatalf("spent limit answered %v", err)
	}
}

// Local compare endpoint answering payload, counting every request
func sgCompareAPI(t *testing.T, payload string) *atomic.Int32 {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	saved, tokens := sourceGitHubAPI, githubTokenSource
	sourceGitHubAPI = server.URL
	githubTokenSource = func() (string, bool) { return "", false }
	t.Cleanup(func() { sourceGitHubAPI, githubTokenSource = saved, tokens })
	return &calls
}

var sgA, sgB = strings.Repeat("a", 40), strings.Repeat("b", 40)

func TestGithubCompareBehindReturnsAheadBy(t *testing.T) {
	sgCompareAPI(t, `{"ahead_by": 61, "status": "ahead"}`)
	if got := githubCompareBehind(context.Background(), sgA, sgB, OfficialRepository); got != 61 {
		t.Fatalf("behind %d", got)
	}
}

func TestGithubCompareBehindZeroMeansLocalAhead(t *testing.T) {
	sgCompareAPI(t, `{"ahead_by": 0, "status": "behind"}`)
	if got := githubCompareBehind(context.Background(), sgA, sgB, OfficialRepository); got != 0 {
		t.Fatalf("behind %d", got)
	}
}

func TestGithubCompareBehindRejectsShortSHAsWithoutNetwork(t *testing.T) {
	calls := sgCompareAPI(t, `{"ahead_by": 1}`)
	for _, pair := range [][2]string{{"abc123", sgB}, {sgA, ""}, {"", sgB}} {
		if got := githubCompareBehind(context.Background(), pair[0], pair[1], OfficialRepository); got != -1 {
			t.Fatalf("%v counted %d", pair, got)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("short shas reached the network %d times", calls.Load())
	}
}

func TestGithubCompareBehindNetworkFailureIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	saved, tokens := sourceGitHubAPI, githubTokenSource
	sourceGitHubAPI, githubTokenSource = server.URL, func() (string, bool) { return "", false }
	t.Cleanup(func() { sourceGitHubAPI, githubTokenSource = saved, tokens })
	if got := githubCompareBehind(context.Background(), sgA, sgB, OfficialRepository); got != -1 {
		t.Fatalf("offline counted %d", got)
	}
}

func TestGithubCompareBehindRejectsMalformedPayloads(t *testing.T) {
	for _, payload := range []string{`{"status": "diverged"}`, `{"ahead_by": -3}`, `{"ahead_by": "12"}`, `{"ahead_by": true}`, `[]`} {
		sgCompareAPI(t, payload)
		if got := githubCompareBehind(context.Background(), sgA, sgB, OfficialRepository); got != -1 {
			t.Fatalf("%s counted %d", payload, got)
		}
	}
}
