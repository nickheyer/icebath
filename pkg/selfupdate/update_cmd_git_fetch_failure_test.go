package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const ugRateLimitStderr = "error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429\n" +
	"fatal: expected flush after ref listing"

const ugCurl429Stderr = "fatal: unable to access 'https://github.com/NousResearch/hermes-agent.git/':" +
	" The requested URL returned error: 429"

func TestClassifyFetchFailureHttp429RpcFailureReportsRateLimit(t *testing.T) {
	msg := ClassifyFetchFailure(ugRateLimitStderr)
	if !strings.Contains(msg, "rate limiting") || !strings.Contains(msg, "try again in 5 minutes") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureCurlUnableToAccess429IsRateLimitNotNetwork(t *testing.T) {
	msg := ClassifyFetchFailure(ugCurl429Stderr)
	if !strings.Contains(msg, "rate limiting") || strings.Contains(msg, "Network error") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureRateLimitPhraseWithoutCode(t *testing.T) {
	if msg := ClassifyFetchFailure("fatal: GitHub rate limit exceeded"); !strings.Contains(msg, "rate limiting") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailure5xxReportsOutage(t *testing.T) {
	msg := ClassifyFetchFailure("fatal: unable to access 'https://github.com/x.git/': The requested URL returned error: 503")
	if !strings.Contains(msg, "outage") || !strings.Contains(msg, "githubstatus.com") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureDnsFailureReportsNetworkError(t *testing.T) {
	msg := ClassifyFetchFailure("fatal: unable to access 'https://github.com/x.git/': Could not resolve host: github.com")
	if !strings.HasPrefix(msg, "✗ Network error") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureUsernamePrompt401ReportsGithubNotUserCredentials(t *testing.T) {
	msg := ClassifyFetchFailure("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	if !strings.Contains(msg, "GitHub") || !strings.Contains(msg, "outage") || strings.Contains(msg, "check your git credentials") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureAuthFailure(t *testing.T) {
	if msg := ClassifyFetchFailure("fatal: Authentication failed for 'https://github.com/x.git/'"); !strings.Contains(msg, "Authentication failed") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureSshPublickeyDenialReportsSshAuthNotGeneric(t *testing.T) {
	msg := ClassifyFetchFailure("git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.")
	if !strings.Contains(msg, "SSH authentication failed") || !strings.Contains(msg, officialRepoURL) {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureOnlyACheckoutGitRejectsGetsTheInstallerRepair(t *testing.T) {
	ugEnv(t)
	tmp := t.TempDir()
	fetch := func(cwd string, source ...string) string {
		cmd := exec.Command("git", append([]string{"fetch"}, source...)...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+tmp)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		_ = cmd.Run()
		return stderr.String()
	}
	damaged := filepath.Join(tmp, "custom-dir", "icebath-agent")
	if err := os.MkdirAll(filepath.Join(damaged, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out bytes.Buffer
	if err := printFetchFailure(ctx, &out, fetch(damaged, "origin"), []string{"git"}, damaged); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"damaged", "hermes-agent.nousresearch.com/install.", damaged} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	healthy, remote := filepath.Join(tmp, "healthy"), filepath.Join(tmp, "remote")
	ugGit(t, tmp, "init", "-q", healthy)
	ugWrite(t, filepath.Join(remote, ".git"), "gitdir: "+filepath.Join(tmp, "gone")+"\n")
	relayed := "remote: fatal: not a git repository (or any of the parent directories): .git"
	for _, stderr := range []string{fetch(healthy, remote), relayed} {
		out.Reset()
		if err := printFetchFailure(ctx, &out, stderr, []string{"git"}, healthy); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "damaged") {
			t.Fatalf("a healthy checkout was called damaged:\n%s", out.String())
		}
	}
}

func TestClassifyFetchFailureSshHostKeyFailureReportsSshAuth(t *testing.T) {
	msg := ClassifyFetchFailure(strings.Repeat("@", 59) + "\nHost key verification failed.\nfatal: Could not read from remote repository.")
	if !strings.Contains(msg, "SSH authentication failed") {
		t.Fatal(msg)
	}
}

func TestClassifyFetchFailureUnknownFallsBackToGeneric(t *testing.T) {
	if msg := ClassifyFetchFailure("fatal: something novel"); msg != "✗ Failed to fetch updates from origin." {
		t.Fatal(msg)
	}
}

func ugPrintedFetchFailure(t *testing.T, stderr string) string {
	t.Helper()
	var out bytes.Buffer
	if err := printFetchFailure(context.Background(), &out, stderr, nil, ""); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestPrintFetchFailurePrintsDiagnosisAndBoundedRawStderr(t *testing.T) {
	out := ugPrintedFetchFailure(t, ugRateLimitStderr)
	for _, want := range []string{"rate limiting", "HTTP 429", "error: RPC failed", "fatal: expected flush after ref listing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPrintFetchFailureLimitsRawStderrToFifteenLines(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	out := ugPrintedFetchFailure(t, strings.Join(lines, "\n"))
	for _, want := range []string{"  line 0", "  line 14", "  ... (5 more lines omitted)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "  line 15") {
		t.Fatalf("output shows a sixteenth line:\n%s", out)
	}
}

func TestPrintFetchFailureEmptyStderrPrintsOnlyDiagnosis(t *testing.T) {
	got := strings.Split(strings.TrimSpace(ugPrintedFetchFailure(t, "")), "\n")
	if !slices.Equal(got, []string{"✗ Failed to fetch updates from origin."}) {
		t.Fatalf("output = %q", got)
	}
}
