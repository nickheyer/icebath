package install

import (
	"context"
	"testing"
)

func TestParseRepoForms(t *testing.T) {
	cases := []struct {
		raw, web, api string
		official      bool
	}{
		{"https://github.com/nickheyer/icebath.git", "https://github.com/nickheyer/icebath", "https://api.github.com/repos/nickheyer/icebath", true},
		{"git@github.com:NickHeyer/Icebath.git", "https://github.com/NickHeyer/Icebath", "https://api.github.com/repos/NickHeyer/Icebath", true},
		{"ssh://git@github.com/fork/icebath", "https://github.com/fork/icebath", "https://api.github.com/repos/fork/icebath", false},
		{"http://127.0.0.1:9/o/r.git", "http://127.0.0.1:9/o/r", "http://127.0.0.1:9/api/v3/repos/o/r", false},
	}
	for _, c := range cases {
		r, err := ParseRepo(c.raw)
		if err != nil || r.Web != c.web || r.API != c.api || r.Official != c.official {
			t.Fatalf("%s: %+v %v", c.raw, r, err)
		}
	}
	if _, err := ParseRepo("ftp://x/y/z"); err == nil {
		t.Fatal("ftp accepted")
	}
}

func TestChecksumForReadsGoreleaserLines(t *testing.T) {
	body := []byte("aa" + "00000000000000000000000000000000000000000000000000000000000000  icebath-linux-amd64\n" +
		"BB00000000000000000000000000000000000000000000000000000000000000 *icebath-windows-amd64.exe\n")
	if got := ChecksumFor(body, "icebath-linux-amd64"); got != "aa00000000000000000000000000000000000000000000000000000000000000" {
		t.Fatalf("got %q", got)
	}
	if got := ChecksumFor(body, "icebath-windows-amd64.exe"); got != "bb00000000000000000000000000000000000000000000000000000000000000" {
		t.Fatalf("got %q", got)
	}
	if ChecksumFor(body, "missing") != "" {
		t.Fatal("unlisted asset matched")
	}
}

func TestLatestStableReleaseFallsBackFromTheAPIToTheRedirectToTheRefs(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	f := newReleaseFixture(t, []fixtureRelease{{Tag: "v2.0.1", Target: "main"}}, map[string]string{
		"refs/tags/v1.9.0": sha, "refs/tags/v1.10.0": sha, "refs/tags/v1.10.0^{}": sha, "refs/tags/v2.0.0-rc.1": sha, "refs/heads/main": sha,
	})
	r, _ := ParseRepo(f.repo)
	if got := LatestStableRelease(context.Background(), r); got != "v2.0.1" {
		t.Fatalf("api answer %q", got)
	}
	f.noAPI = true
	if got := LatestStableRelease(context.Background(), r); got != "v2.0.1" {
		t.Fatalf("redirect answer %q", got)
	}
	f.releases = nil
	if got := LatestStableRelease(context.Background(), r); got != "v1.10.0" {
		t.Fatalf("refs answer %q", got)
	}
	refs, err := LsRemote(context.Background(), r)
	if err != nil || tagCommit(refs, "v1.10.0") != sha || refs["refs/heads/main"] != sha {
		t.Fatalf("refs %v %v", refs, err)
	}
}

func TestReleaseSelectionByCommitAndBranch(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	f := newReleaseFixture(t, []fixtureRelease{
		{Tag: "v1.1.0", Target: "main", Published: "2026-10-01T00:00:00Z"},
		{Tag: "v1.2.0", Target: "main", Published: "2026-11-01T00:00:00Z"},
		{Tag: "v1.2.0-beta.1", Target: commit, Published: "2026-09-01T00:00:00Z"},
	}, nil)
	r, _ := ParseRepo(f.repo)
	if got, err := releaseForBranch(context.Background(), r, "main"); err != nil || got == nil || got.Tag != "v1.2.0" {
		t.Fatalf("branch pick %+v %v", got, err)
	}
	if got, err := releaseForCommit(context.Background(), r, commit); err != nil || got == nil || got.Tag != "v1.2.0-beta.1" || got.Commit != commit {
		t.Fatalf("commit pick %+v %v", got, err)
	}
	if got, err := releaseForBranch(context.Background(), r, "feature"); err != nil || got != nil {
		t.Fatalf("unknown branch %+v %v", got, err)
	}
}

func TestManifestAndFrameJSONMatchTheScriptProtocol(t *testing.T) {
	want := `{"protocol_version":1,"stages":[{"name":"prerequisites","title":"System prerequisites","category":"runtime","needs_user_input":false},{"name":"repository","title":"Download Icebath Agent","category":"runtime","needs_user_input":false},{"name":"config","title":"Prepare config and skills","category":"configuration","needs_user_input":false},{"name":"products","title":"Install command and app","category":"runtime","needs_user_input":false},{"name":"runtimes","title":"Install Python dependencies","category":"runtime","needs_user_input":false},{"name":"setup","title":"Configure API keys and settings","category":"configuration","needs_user_input":true},{"name":"gateway","title":"Configure gateway service","category":"configuration","needs_user_input":true},{"name":"complete","title":"Finish install","category":"runtime","needs_user_input":false}]}`
	if got := ManifestJSON(false); got != want {
		t.Fatalf("manifest\n%s", got)
	}
	if got := FrameJSON(true, "setup", true, "needs user input"); got != `{"ok":true,"stage":"setup","skipped":true,"reason":"needs user input"}` {
		t.Fatalf("frame %s", got)
	}
	if got := FrameJSON(true, "config", false, ""); got != `{"ok":true,"stage":"config","skipped":false}` {
		t.Fatalf("frame %s", got)
	}
}
