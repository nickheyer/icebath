package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Credential ladder GITHUB_TOKEN, GH_TOKEN, a blank value falling through, then the gh CLI probed once
func TestGithubTokenLadder(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "  ", "GH_TOKEN": " ghp_user "}
	if tok, ok := GithubTokenFromEnv(func(k string) string { return env[k] }); !ok || tok != "ghp_user" {
		t.Fatalf("env token %q", tok)
	}
	env["GITHUB_TOKEN"] = "ghp_first"
	if tok, _ := GithubTokenFromEnv(func(k string) string { return env[k] }); tok != "ghp_first" {
		t.Fatalf("GITHUB_TOKEN first %q", tok)
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\necho x >> "+count+"\n[ \"$1 $2\" = \"auth token\" ] && echo ' gho_cli '\n")
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	ghCLIMu.Lock()
	ghCLIProbed, ghCLICache = false, ""
	ghCLIMu.Unlock()
	t.Cleanup(func() { ghCLIMu.Lock(); ghCLIProbed, ghCLICache = false, ""; ghCLIMu.Unlock() })
	for i := 0; i < 3; i++ {
		if tok, ok := GithubToken(); !ok || tok != "gho_cli" {
			t.Fatalf("gh token %q", tok)
		}
	}
	if raw, _ := os.ReadFile(count); strings.Count(string(raw), "x") != 1 {
		t.Fatalf("gh spawned %q", raw)
	}
	t.Setenv("GH_TOKEN", "ghp_env")
	if tok, _ := GithubToken(); tok != "ghp_env" {
		t.Fatalf("env beats gh %q", tok)
	}
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\nexit 1\n")
	ghCLIMu.Lock()
	ghCLIProbed = false
	ghCLIMu.Unlock()
	t.Setenv("GH_TOKEN", "")
	if tok, ok := GithubToken(); ok || tok != "" {
		t.Fatalf("failed gh is anonymous %q", tok)
	}
	t.Setenv("PATH", t.TempDir())
	ghCLIMu.Lock()
	ghCLIProbed = false
	ghCLIMu.Unlock()
	if _, ok := GithubToken(); ok {
		t.Fatal("missing gh is anonymous")
	}
}

// Rate limit, outage and transport lines of describe_github_failure over a local api.github.com
func TestDescribeGithubFailure(t *testing.T) {
	limited := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range limited {
			w.Header().Set(k, v)
		}
		switch r.URL.Path {
		case "/limited":
			w.WriteHeader(403)
		case "/busy":
			w.WriteHeader(429)
		case "/down":
			w.WriteHeader(502)
		case "/missing":
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	get := func(path string) error {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if e := GithubHTTPErrorFromResponse(resp); e != nil {
			return e
		}
		return nil
	}
	now := 1000.0
	limited["x-ratelimit-remaining"] = "0"
	limited["x-ratelimit-reset"] = " 1600 "
	anon := DescribeGithubFailure(get("/limited"), false, &now)
	if anon != "GitHub API rate limit reached (HTTP 403): anonymous requests are limited to 60 per hour per network address, shared with everyone behind the same connection. It resets in about 10 minutes; setting GITHUB_TOKEN in the environment lifts the limit." {
		t.Fatalf("anonymous %q", anon)
	}
	limited["x-ratelimit-reset"] = "1030"
	if got := DescribeGithubFailure(get("/busy"), true, &now); got != "GitHub API rate limit reached for your GITHUB_TOKEN (HTTP 429) — it resets in about a minute." {
		t.Fatalf("authenticated %q", got)
	}
	limited["x-ratelimit-reset"] = "900"
	if got := DescribeGithubFailure(get("/limited"), true, &now); !strings.HasSuffix(got, "it resets within an hour.") {
		t.Fatalf("past reset %q", got)
	}
	limited["x-ratelimit-reset"] = "soon"
	if got := DescribeGithubFailure(get("/limited"), true, nil); !strings.HasSuffix(got, "it resets within an hour.") {
		t.Fatalf("bad reset %q", got)
	}
	limited["x-ratelimit-remaining"] = "12"
	if got := DescribeGithubFailure(get("/limited"), false, &now); got != "api.github.com answered HTTP 403." {
		t.Fatalf("plain 403 %q", got)
	}
	if got := DescribeGithubFailure(get("/down"), false, nil); got != "GitHub is having trouble (HTTP 502 from api.github.com) — check githubstatus.com and try again later." {
		t.Fatalf("outage %q", got)
	}
	if got := DescribeGithubFailure(get("/missing"), false, nil); got != "api.github.com answered HTTP 404." {
		t.Fatalf("missing %q", got)
	}
	if get("/ok") != nil {
		t.Fatal("200 is no error")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	_, err := (&http.Client{Timeout: 50 * time.Millisecond}).Get(slow.URL)
	if got := DescribeGithubFailure(err, false, nil); got != "api.github.com did not answer within 10 seconds." {
		t.Fatalf("timeout %q", got)
	}
	refused := &url.Error{Op: "Get", URL: "https://api.github.com/x", Err: errors.New("connection refused")}
	if got := DescribeGithubFailure(refused, false, nil); got != "Connection to api.github.com failed (connection refused) — check your connection, firewall or proxy." {
		t.Fatalf("refused %q", got)
	}
	if got := DescribeGithubFailure(&url.Error{Err: errors.New("read: Timed Out")}, false, nil); got != "api.github.com did not answer within 10 seconds." {
		t.Fatalf("timed out text %q", got)
	}
	if got := DescribeGithubFailure(context.DeadlineExceeded, false, nil); got != "api.github.com did not answer within 10 seconds." {
		t.Fatalf("deadline %q", got)
	}
	if got := DescribeGithubFailure(errors.New("boom"), false, nil); got != "api.github.com: boom" {
		t.Fatalf("other %q", got)
	}
	if pyFloorDiv(-1, 60) != -1 || pyFloorDiv(659, 60) != 10 || pyFloorDiv(119.5, 60) != 1 {
		t.Fatal("floor division parity")
	}
}
