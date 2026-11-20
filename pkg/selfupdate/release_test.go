//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"strings"
	"testing"
)

func TestReleaseFetchFailureClassifier(t *testing.T) {
	cases := []struct{ detail, want, not string }{
		{"error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429", "try again in 5 minutes", ""},
		{"fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 429", "rate limiting", "Network error"},
		{"remote: API rate limit exceeded", "rate limiting", ""},
		{"error: RPC failed; HTTP 503 curl 22", "githubstatus.com", ""},
		{"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com", "✗ Network error", ""},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", "outage", "check your git credentials"},
		{"fatal: Authentication failed for 'https://github.com/x/y.git/'", "Authentication failed", ""},
		{"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "https://github.com/nickheyer/icebath.git", ""},
		{"Host key verification failed.\nfatal: Could not read from remote repository.", "SSH authentication failed", ""},
	}
	for _, tc := range cases {
		msg := ClassifyFetchFailure(tc.detail)
		if !strings.Contains(msg, tc.want) || (tc.not != "" && strings.Contains(msg, tc.not)) {
			t.Fatalf("%q classified %q", tc.detail, msg)
		}
	}
	if msg := ClassifyFetchFailure("fatal: something odd"); msg != "✗ Failed to fetch updates from origin." {
		t.Fatalf("got %q", msg)
	}
}

func TestReleaseVersionPrecedence(t *testing.T) {
	ordered := []string{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.10.0"}
	for i := 1; i < len(ordered); i++ {
		if CompareVersions(ordered[i-1], ordered[i]) != -1 || CompareVersions(ordered[i], ordered[i-1]) != 1 {
			t.Fatalf("%s vs %s", ordered[i-1], ordered[i])
		}
	}
	if CompareVersions("v1.2.3+canary.20260101T000000Z", "v1.2.3") != 0 || CompareVersions("git.abc", "v0.0.1") != -1 {
		t.Fatal("build metadata or unparseable version misordered")
	}
}
