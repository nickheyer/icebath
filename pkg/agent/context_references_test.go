package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Git repo with a staged and an unstaged change, the sample_repo fixture
func sampleRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.name", "Hermes Tests")
	git("config", "user.email", "tests@example.com")
	write := func(rel, text string) {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.py", "def alpha():\n    return 'a'\n\ndef beta():\n    return 'b'\n")
	write("src/helper.py", "VALUE = 1\n")
	write("README.md", "# Demo\n")
	write("blob.bin", "\x00\x01\x02binary")
	git("add", ".")
	git("commit", "-m", "initial")
	write("src/main.py", "def alpha():\n    return 'changed'\n\ndef beta():\n    return 'b'\n")
	write("src/helper.py", "VALUE = 2\n")
	git("add", "src/helper.py")
	return repo
}

// Isolated home for the path guards, the active home every reference expansion checks against
func referenceTestHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_SESSION_ID", "")
	os.Unsetenv("ICEBATH_SESSION_ID")
	ClearContextReferenceProvidersForTests()
	t.Cleanup(ClearContextReferenceProvidersForTests)
	return context.Background(), h
}

func preprocess(t *testing.T, ctx context.Context, message, cwd string, contextLength int) *ContextReferenceResult {
	t.Helper()
	result, err := PreprocessContextReferences(ctx, message, ContextReferenceOptions{Cwd: cwd, ContextLength: contextLength})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func preprocessRooted(t *testing.T, ctx context.Context, message, cwd, allowedRoot string, contextLength int) *ContextReferenceResult {
	t.Helper()
	result, err := PreprocessContextReferences(ctx, message, ContextReferenceOptions{Cwd: cwd, ContextLength: contextLength, AllowedRoot: &allowedRoot})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseTypedReferencesIgnoresEmailsAndHandles(t *testing.T) {
	referenceTestHome(t)
	refs := ParseContextReferences("email me at user@example.com and ping @teammate but include @file:src/main.py:1-2 plus @diff and @git:2 and @url:https://example.com/docs", true)
	kinds := make([]string, len(refs))
	for i, r := range refs {
		kinds[i] = r.Kind
	}
	if strings.Join(kinds, ",") != "file,diff,git,url" {
		t.Fatal(kinds)
	}
	if refs[0].Target != "src/main.py" || refs[0].LineStart == nil || *refs[0].LineStart != 1 || refs[0].LineEnd == nil || *refs[0].LineEnd != 2 || refs[2].Target != "2" {
		t.Fatalf("%+v", refs)
	}
}

func TestFolderListingFallsBackWhenRgIsBlocked(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	repo := sampleRepo(t)
	real := quietRunnerFn
	quietRunnerFn = func(cmd []string, cwd string, timeout time.Duration, env []string) (*quietResult, error) {
		if len(cmd) > 0 && cmd[0] == "rg" {
			return nil, errors.New("rg blocked by policy")
		}
		return real(cmd, cwd, timeout, env)
	}
	t.Cleanup(func() { quietRunnerFn = real })
	result := preprocess(t, ctx, "Review @folder:src/", repo, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, "src/") || !strings.Contains(result.Message, "main.py") || !strings.Contains(result.Message, "helper.py") || len(result.Warnings) != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestFolderListingOutsideCwdInsideWidenedAllowedRoot(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	os.MkdirAll(cwd, 0o755)
	writeText(t, filepath.Join(tmp, "shared", "a.txt"), "x\n")
	writeText(t, filepath.Join(tmp, "shared", "sub", "b.txt"), "y\n")
	result := preprocessRooted(t, ctx, "Review @folder:../shared", cwd, tmp, 100_000)
	if !result.Expanded || len(result.Warnings) != 0 || !strings.Contains(result.Message, "\nshared/\n") || strings.Contains(result.Message, tmp) ||
		!strings.Contains(result.Message, "a.txt") || !strings.Contains(result.Message, "b.txt") {
		t.Fatalf("%+v", result)
	}
}

func TestFolderListingInsideCwdUnchanged(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	repo := sampleRepo(t)
	result := preprocess(t, ctx, "Review @folder:src/", repo, 100_000)
	if !result.Expanded || len(result.Warnings) != 0 || !strings.Contains(result.Message, "src/") || !strings.Contains(result.Message, "main.py") {
		t.Fatalf("%+v", result)
	}
}

func TestMissingFileBecomesWarning(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	repo := sampleRepo(t)
	result := preprocess(t, ctx, "Check @file:nope.txt", repo, 100_000)
	if !result.Expanded || len(result.Warnings) != 1 || !strings.Contains(strings.ToLower(result.Message), "not found") {
		t.Fatalf("%+v", result)
	}
}

func TestOversizedTextFileFallsBackToToolReadablePath(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	payload := filepath.Join(tmp, "large.txt")
	writeText(t, payload, "FULL-CONTENT-MARKER\n"+strings.Repeat("x", 8_000))
	result := preprocess(t, ctx, "Inspect @file:large.txt", tmp, 1_000)
	if !result.Expanded || result.Blocked || !strings.Contains(result.Message, payload) || !strings.Contains(result.Message, "too large to inline safely") ||
		!strings.Contains(result.Message, "read_file") || strings.Contains(result.Message, "FULL-CONTENT-MARKER") || len(result.Warnings) != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestFileLineRangeIsAppliedBeforeOversizedFallback(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("x", 200))
	}
	writeText(t, filepath.Join(tmp, "large.txt"), "first line\nsecond line\n"+strings.Join(lines, "\n"))
	result := preprocess(t, ctx, "Inspect @file:large.txt:1-2", tmp, 1_000)
	if !result.Expanded || result.Blocked || !strings.Contains(result.Message, "first line\nsecond line") || strings.Contains(result.Message, "too large to inline safely") {
		t.Fatalf("%+v", result)
	}
}

func refuseFullRead(t *testing.T, reason string) {
	t.Helper()
	previous := referenceReadTextFn
	referenceReadTextFn = func(string) ([]byte, error) { t.Fatal(reason); return nil, nil }
	t.Cleanup(func() { referenceReadTextFn = previous })
}

func TestOversizedFileRefusedWithoutFullRead(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	payload := filepath.Join(tmp, "huge.txt")
	writeText(t, payload, strings.Repeat("x", 100_000))
	refuseFullRead(t, "oversized file was read in full")
	result := preprocess(t, ctx, "Inspect @file:huge.txt", tmp, 1_000)
	if !strings.Contains(result.Message, "too large to inline safely") || !strings.Contains(result.Message, payload) {
		t.Fatalf("%+v", result)
	}
}

func TestLineRangeRefStreamsOnlyTheWindow(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	refuseFullRead(t, "ranged ref read the whole file")
	writeText(t, filepath.Join(tmp, "small.txt"), "first\nsecond\nthird\nfourth\n")
	result := preprocess(t, ctx, "Inspect @file:small.txt:2-3", tmp, 1_000)
	if !strings.Contains(result.Message, "second\nthird") || strings.Contains(result.Message, "first") || strings.Contains(result.Message, "fourth") {
		t.Fatalf("%+v", result)
	}
}

func TestBinarySniffReadsPrefixOnly(t *testing.T) {
	referenceTestHome(t)
	payload := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(payload, append(make([]byte, 10), []byte(strings.Repeat("x", 1_000_000))...), 0o644); err != nil {
		t.Fatal(err)
	}
	var reads []int
	previous := referenceReadPrefixFn
	referenceReadPrefixFn = func(path string, n int) ([]byte, error) {
		data, err := previous(path, n)
		reads = append(reads, len(data))
		return data, err
	}
	t.Cleanup(func() { referenceReadPrefixFn = previous })
	binary, err := isBinaryReferenceFile(payload)
	if err != nil || !binary {
		t.Fatal(binary, err)
	}
	if len(reads) == 0 {
		t.Fatal("the sniff must read")
	}
	for _, n := range reads {
		if n > 4096 {
			t.Fatal(reads)
		}
	}
}

func TestFolderListingCapsLineCountIo(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	assets := filepath.Join(tmp, "assets")
	os.MkdirAll(assets, 0o755)
	if err := os.WriteFile(filepath.Join(assets, "big.txt"), []byte(strings.Repeat("line\n", lineCountMaxBytes/5+2)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(assets, "small.txt"), "a\nb\n")
	result := preprocess(t, ctx, "list @folder:assets", tmp, 10_000_000)
	if !strings.Contains(result.Message, "big.txt") || !strings.Contains(result.Message, "bytes") || !strings.Contains(result.Message, "3 lines") {
		t.Fatalf("%+v", result)
	}
}

func TestLineRangeBoundsReadsOnSingleLineFile(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	writeText(t, filepath.Join(tmp, "oneline.txt"), strings.Repeat("x", 100_000))
	var readSizes, returned []int
	origOpen := referenceOpenTextFn
	referenceOpenTextFn = func(path string) (referenceTextFile, error) {
		fh, err := origOpen(path)
		if err != nil {
			return nil, err
		}
		return &countingTextFile{inner: fh, sizes: &readSizes, returned: &returned}, nil
	}
	t.Cleanup(func() { referenceOpenTextFn = origOpen })
	result := preprocess(t, ctx, "Inspect @file:oneline.txt:1-1", tmp, 1_000)
	if !strings.Contains(result.Message, "too large to inline safely") {
		t.Fatalf("%+v", result)
	}
	if len(readSizes) == 0 || slices.Max(readSizes) > 2001 {
		t.Fatal(readSizes)
	}
	total := 0
	for _, n := range returned {
		total += n
	}
	if total > 2*2001 {
		t.Fatal(returned)
	}
}

// Text handle recording each readline size asked and each piece length returned
type countingTextFile struct {
	inner           referenceTextFile
	sizes, returned *[]int
}

func (c *countingTextFile) readline(size int) (string, error) {
	*c.sizes = append(*c.sizes, size)
	piece, err := c.inner.readline(size)
	*c.returned = append(*c.returned, len(piece))
	return piece, err
}

func (c *countingTextFile) Close() error { return c.inner.Close() }

func TestRunQuietCapsChildOutput(t *testing.T) {
	tmp := t.TempDir()
	flood := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", maxQuietOutputBytes+1000)
	huge, err := runQuiet([]string{"sh", "-c", flood}, tmp, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if huge.returncode == 0 || len(huge.stdout) > maxQuietOutputBytes {
		t.Fatal(huge.returncode, len(huge.stdout))
	}
	exited, err := runQuiet([]string{"sh", "-c", flood + "; exit 0"}, tmp, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exited.returncode == 0 || len(exited.stdout) > maxQuietOutputBytes {
		t.Fatal(exited.returncode, len(exited.stdout))
	}
	ok, err := runQuiet([]string{"sh", "-c", "echo hello"}, tmp, 30*time.Second, nil)
	if err != nil || ok.returncode != 0 || !strings.Contains(ok.stdout, "hello") {
		t.Fatal(ok, err)
	}
}

func TestReferenceCountIsCapped(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	var parts []string
	for i := 0; i < maxExpandedReferences+4; i++ {
		writeText(t, filepath.Join(tmp, fmt.Sprintf("f%d.txt", i)), "data")
		parts = append(parts, fmt.Sprintf("@file:f%d.txt", i))
	}
	result := preprocess(t, ctx, strings.Join(parts, " "), tmp, 10_000_000)
	skipped := 0
	for _, w := range result.Warnings {
		if strings.Contains(w, "not expanded") {
			skipped++
		}
	}
	if skipped != 4 {
		t.Fatal(result.Warnings)
	}
}

func TestMultipleIndividuallySafeFilesStillObeyAggregateLimit(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	tmp := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		writeText(t, filepath.Join(tmp, name), strings.Repeat("x", 1_200))
	}
	result := preprocess(t, ctx, "Inspect @file:first.txt and @file:second.txt", tmp, 1_000)
	if !result.Blocked || result.Expanded || !strings.Contains(strings.Join(result.Warnings, "\n"), "context injection refused") {
		t.Fatalf("%+v", result)
	}
}

func stagedAttachment(t *testing.T, tmp, backend, subdir, name string) (context.Context, string) {
	t.Helper()
	icebathHome := filepath.Join(tmp, ".icebath")
	payload := filepath.Join(icebathHome, subdir, name)
	os.MkdirAll(filepath.Dir(payload), 0o755)
	t.Setenv("ICEBATH_HOME", icebathHome)
	t.Setenv("TERMINAL_ENV", backend)
	ClearContextReferenceProvidersForTests()
	return context.Background(), payload
}

func TestBinaryReferenceBlockMapsHostAttachmentToContainerPath(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stagedAttachment(t, tmp, "docker", "attachments", "archive.zip")
	os.WriteFile(payload, []byte("PK\x03\x04binary-zip-bytes"), 0o644)
	result := preprocess(t, ctx, "Read the attachment @file:"+payload, tmp, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, tools.DefaultContainerBase+"/attachments/archive.zip") || !strings.Contains(result.Message, "binary file, not inlined") {
		t.Fatalf("%+v", result)
	}
}

func TestOversizedTextReferenceMapsHostAttachmentToContainerPath(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stagedAttachment(t, tmp, "docker", "attachments", "large.txt")
	writeText(t, payload, strings.Repeat("x", 8_000))
	result := preprocess(t, ctx, "Read the attachment @file:"+payload, tmp, 1_000)
	if !result.Expanded || result.Blocked {
		t.Fatalf("%+v", result)
	}
	attached := strings.SplitN(result.Message, "--- Attached Context ---", 2)[1]
	if !strings.Contains(attached, tools.DefaultContainerBase+"/attachments/large.txt") || !strings.Contains(result.Message, "too large to inline safely") {
		t.Fatalf("%+v", result)
	}
}

func TestBinaryReferenceBlockKeepsHostPathOnLocalBackend(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stagedAttachment(t, tmp, "local", "attachments", "archive.zip")
	os.WriteFile(payload, []byte("PK\x03\x04binary-zip-bytes"), 0o644)
	result := preprocess(t, ctx, "Read the attachment @file:"+payload, tmp, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, payload) || strings.Contains(result.Message, tools.DefaultContainerBase+"/attachments/") {
		t.Fatalf("%+v", result)
	}
}

const sshWorkspace = "/srv/repos"

// Stages a file under a gateway data home outside the workspace, the ssh topology
func stageOutsideWorkspace(t *testing.T, tmp, subdir, name string) (context.Context, string) {
	t.Helper()
	gatewayHome := filepath.Join(tmp, "gateway-data")
	payload := filepath.Join(gatewayHome, subdir, name)
	os.MkdirAll(filepath.Dir(payload), 0o755)
	t.Setenv("ICEBATH_HOME", gatewayHome)
	t.Setenv("TERMINAL_ENV", "ssh")
	ClearContextReferenceProvidersForTests()
	return context.Background(), payload
}

func TestSshBackendStagedTextAttachmentStillExpands(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stageOutsideWorkspace(t, tmp, "composer-pastes", "Pasted content (3-5.4 KB).txt")
	writeText(t, payload, "pasted body\n")
	result := preprocessRooted(t, ctx, "Summarize @file:"+FormatReferenceValue(payload), sshWorkspace, sshWorkspace, 100_000)
	if !result.Expanded || len(result.Warnings) != 0 || !strings.Contains(result.Message, "pasted body") {
		t.Fatalf("%+v", result)
	}
}

func TestSshBackendStagedBinaryAttachmentPointsAtTheSyncedRemotePath(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stageOutsideWorkspace(t, tmp, "attachments", "archive.zip")
	os.WriteFile(payload, []byte("PK\x03\x04binary-zip-bytes"), 0o644)
	result := preprocessRooted(t, ctx, "Read the attachment @file:"+payload, sshWorkspace, sshWorkspace, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, "binary file, not inlined") || !strings.Contains(result.Message, "~/.icebath/attachments/archive.zip") {
		t.Fatalf("%+v", result)
	}
}

func TestRefOutsideWorkspaceAndStagingDirsIsStillRefused(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "gateway-data"))
	t.Setenv("TERMINAL_ENV", "ssh")
	ClearContextReferenceProvidersForTests()
	secret := filepath.Join(tmp, "elsewhere", "notes.txt")
	writeText(t, secret, "not-for-the-agent\n")
	result := preprocessRooted(t, context.Background(), "@file:"+secret, sshWorkspace, sshWorkspace, 100_000)
	refused := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "outside the allowed workspace") {
			refused = true
		}
	}
	if !refused || strings.Contains(result.Message, "not-for-the-agent") {
		t.Fatalf("%+v", result)
	}
}

func TestLocalBackendStagedAttachmentKeepsTheHostPath(t *testing.T) {
	tmp := t.TempDir()
	ctx, payload := stageOutsideWorkspace(t, tmp, "attachments", "archive.zip")
	os.WriteFile(payload, []byte("PK\x03\x04binary-zip-bytes"), 0o644)
	t.Setenv("TERMINAL_ENV", "local")
	workspace := filepath.Join(tmp, "workspace")
	os.MkdirAll(workspace, 0o755)
	result := preprocessRooted(t, ctx, "Read the attachment @file:"+payload, workspace, workspace, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, "binary file, not inlined") || strings.Contains(result.Message, "~/.icebath/attachments/") {
		t.Fatalf("%+v", result)
	}
}

func TestBlocksCanonicalReadDenylistCredentialStores(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	icebathHome := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", icebathHome)
	ClearContextReferenceProvidersForTests()
	writeText(t, filepath.Join(icebathHome, "auth.json"), `{"openai": "sk-AUTHJSON-SECRET"}`+"\n")
	writeText(t, filepath.Join(icebathHome, ".anthropic_oauth.json"), `{"access_token": "OAUTH-SECRET"}`+"\n")
	writeText(t, filepath.Join(icebathHome, "mcp-tokens", "github.json"), `{"token": "MCP-TOKEN-SECRET"}`+"\n")
	writeText(t, filepath.Join(tmp, ".config", "gcloud", "application_default_credentials.json"), `{"refresh_token": "GCLOUD-SECRET"}`+"\n")
	writeText(t, filepath.Join(tmp, "project", ".env"), "DB_PASSWORD=ENV-SECRET\n")
	result := preprocessRooted(t, context.Background(), "inspect @file:.icebath/auth.json and @file:.icebath/.anthropic_oauth.json and @file:.icebath/mcp-tokens/github.json and @file:project/.env and @file:.config/gcloud/application_default_credentials.json",
		tmp, tmp, 100_000)
	if !result.Expanded {
		t.Fatalf("%+v", result)
	}
	for _, secret := range []string{"sk-AUTHJSON-SECRET", "OAUTH-SECRET", "MCP-TOKEN-SECRET", "ENV-SECRET", "GCLOUD-SECRET"} {
		if strings.Contains(result.Message, secret) {
			t.Fatal(secret)
		}
	}
	sensitive := 0
	for _, w := range result.Warnings {
		if strings.Contains(w, "sensitive credential") {
			sensitive++
		}
	}
	if sensitive != 5 {
		t.Fatal(result.Warnings)
	}
}

func TestCanonicalGuardFailsClosedWhenLookupRaises(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	icebathHome := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", icebathHome)
	ClearContextReferenceProvidersForTests()
	writeText(t, filepath.Join(icebathHome, "auth.json"), `{"openai": "sk-AUTHJSON-SECRET"}`+"\n")
	previous := referenceReadBlockErrorFn
	referenceReadBlockErrorFn = func(context.Context, string) (string, error) {
		return "", &plugins.HostBuiltinError{Type: "RuntimeError", Message: "guard resolution failed"}
	}
	t.Cleanup(func() { referenceReadBlockErrorFn = previous })
	result := preprocessRooted(t, context.Background(), "inspect @file:.icebath/auth.json", tmp, tmp, 100_000)
	if strings.Contains(result.Message, "sk-AUTHJSON-SECRET") {
		t.Fatal(result.Message)
	}
	refused := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "credential deny-list") || strings.Contains(w, "sensitive credential") {
			refused = true
		}
	}
	if !refused {
		t.Fatal(result.Warnings)
	}
}

func TestFormatReferenceValueRoundTripsThroughTheParser(t *testing.T) {
	for _, value := range []string{"/tmp/plain.png", "/Users/me/Library/Application Support/Hermes/composer-images/a.png", `C:\Users\John Doe\Pictures\cat.png`,
		"/tmp/report (final).pdf", "/tmp/it's here.png", `/tmp/say "hi".png`} {
		t.Run(value, func(t *testing.T) {
			matches := findAllReferenceMatches(ReferencePattern, "@file:"+FormatReferenceValue(value))
			if len(matches) == 0 {
				t.Fatal("no match")
			}
			if strings.Trim(matches[0].Group("value"), "`\"'") != value {
				t.Fatal(matches[0].Group("value"))
			}
		})
	}
}

func TestSideThreadExpansionGuardsTheServedProfileHome(t *testing.T) {
	tmp := t.TempDir()
	launchHome := filepath.Join(tmp, "launch")
	servedHome := filepath.Join(launchHome, "profiles", "b")
	writeText(t, filepath.Join(servedHome, "skills", ".hub", "injected.md"), "HUB-CACHE-BODY\n")
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", launchHome)
	ClearContextReferenceProvidersForTests()
	result := preprocessRooted(t, home.WithOverride(context.Background(), servedHome), "read @file:profiles/b/skills/.hub/injected.md", launchHome, launchHome, 100_000)
	if strings.Contains(result.Message, "HUB-CACHE-BODY") {
		t.Fatal(result.Message)
	}
	internal := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "internal Hermes path") {
			internal = true
		}
	}
	if !internal {
		t.Fatal(result.Warnings)
	}
}

func TestComposerPasteOutsideWorkspaceIsAttachedButSiblingDirIsNot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	icebathHome := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", icebathHome)
	ClearContextReferenceProvidersForTests()
	paste := filepath.Join(icebathHome, "composer-pastes", "pasted_content_1.txt")
	writeText(t, paste, "PASTED-BODY-MARKER\n")
	lookalike := filepath.Join(icebathHome, "my-composer-pastes-backup", "secret.txt")
	writeText(t, lookalike, "LOOKALIKE-SECRET\n")
	workspace := filepath.Join(tmp, "project")
	os.MkdirAll(workspace, 0o755)
	result := preprocessRooted(t, context.Background(), fmt.Sprintf("see @file:%s and @file:%s", paste, lookalike), workspace, workspace, 100_000)
	if !result.Expanded || !strings.Contains(result.Message, "PASTED-BODY-MARKER") || strings.Contains(result.Message, "LOOKALIKE-SECRET") ||
		!strings.Contains(strings.Join(result.Warnings, "\n"), "outside the allowed workspace") {
		t.Fatalf("%+v", result)
	}
}

func TestRefsExpandConcurrently(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	const nRefs = 3
	var mu sync.Mutex
	var entered []string
	arrived := make(chan struct{}, nRefs)
	release := make(chan struct{})
	var once sync.Once
	fetcher := func(_ context.Context, url string) (string, error) {
		mu.Lock()
		entered = append(entered, url)
		count := len(entered)
		mu.Unlock()
		arrived <- struct{}{}
		if count == nRefs {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return "", errors.New("references did not expand concurrently: a fetch reached the rendezvous alone")
		}
		return "CONTENT[" + url + "]", nil
	}
	result, err := PreprocessContextReferences(ctx, "see @url:https://a.example/x @url:https://b.example/y @url:https://c.example/z please",
		ContextReferenceOptions{Cwd: t.TempDir(), ContextLength: 100_000, URLFetcher: fetcher})
	if err != nil {
		t.Fatal(err)
	}
	if len(entered) != nRefs || !result.Expanded {
		t.Fatalf("entered %v result %+v", entered, result)
	}
	body := result.Message
	if !(strings.Index(body, "a.example") < strings.Index(body, "b.example") && strings.Index(body, "b.example") < strings.Index(body, "c.example")) {
		t.Fatal("reference blocks must stay in original order")
	}
}

// Minimal concrete providers of the plugin reference tests
type dummyRefProvider struct{ prefix string }

func (p dummyRefProvider) Prefix() string      { return p.prefix }
func (p dummyRefProvider) Description() string { return "test provider" }
func (p dummyRefProvider) Autocomplete(_ context.Context, query string, _ int) ([]ContextCompletionItem, error) {
	return []ContextCompletionItem{NewContextCompletionItem(query+"-result", "", "test")}, nil
}
func (p dummyRefProvider) Expand(_ context.Context, target string) (*string, error) {
	s := "expanded: " + target
	return &s, nil
}

type noneExpandProvider struct{}

func (noneExpandProvider) Prefix() string      { return "skip" }
func (noneExpandProvider) Description() string { return "skip provider" }
func (noneExpandProvider) Autocomplete(context.Context, string, int) ([]ContextCompletionItem, error) {
	return nil, nil
}
func (noneExpandProvider) Expand(context.Context, string) (*string, error) { return nil, nil }

type errorRefProvider struct{}

func (errorRefProvider) Prefix() string      { return "boom" }
func (errorRefProvider) Description() string { return "error provider" }
func (errorRefProvider) Autocomplete(context.Context, string, int) ([]ContextCompletionItem, error) {
	return nil, nil
}
func (errorRefProvider) Expand(context.Context, string) (*string, error) {
	return nil, &plugins.HostBuiltinError{Type: "RuntimeError", Message: "boom!"}
}

func TestRegisterValidProvider(t *testing.T) {
	referenceTestHome(t)
	if err := RegisterContextReferenceProvider(dummyRefProvider{"test"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := GetContextReferenceProviders()["test"]; !ok {
		t.Fatal("registered prefix missing")
	}
}

func TestRegisterRejectsBuiltinPrefix(t *testing.T) {
	referenceTestHome(t)
	for prefix := range BuiltinPrefixes {
		err := RegisterContextReferenceProvider(dummyRefProvider{prefix})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatal(prefix, err)
		}
	}
}

func TestRegisterRejectsDuplicatePrefix(t *testing.T) {
	referenceTestHome(t)
	if err := RegisterContextReferenceProvider(dummyRefProvider{"test"}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterContextReferenceProvider(dummyRefProvider{"test"}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatal(err)
	}
}

func TestRegisterRejectsNonProvider(t *testing.T) {
	referenceTestHome(t)
	err := RegisterContextReference("not a provider")
	var builtin *plugins.HostBuiltinError
	if !errors.As(err, &builtin) || builtin.Type != "TypeError" || !strings.Contains(err.Error(), "must be a ContextReferenceProvider") {
		t.Fatal(err)
	}
}

func TestRegisterRejectsEmptyPrefix(t *testing.T) {
	referenceTestHome(t)
	if err := RegisterContextReferenceProvider(dummyRefProvider{""}); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatal(err)
	}
}

func TestParsePluginReference(t *testing.T) {
	referenceTestHome(t)
	RegisterContextReferenceProvider(dummyRefProvider{"test"})
	refs := ParseContextReferences("check @test:ENG-123 and @file:README.md", true)
	var testRef *ContextReference
	hasFile := false
	for i := range refs {
		if refs[i].Kind == "test" {
			testRef = &refs[i]
		}
		if refs[i].Kind == "file" {
			hasFile = true
		}
	}
	if testRef == nil || !hasFile || testRef.Target != "ENG-123" {
		t.Fatalf("%+v", refs)
	}
}

func TestParsePluginReferenceIgnoredWhenNotRegistered(t *testing.T) {
	referenceTestHome(t)
	if refs := ParseContextReferences("check @test:ENG-123", true); len(refs) != 0 {
		t.Fatalf("%+v", refs)
	}
}

func TestExpandPluginReference(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	RegisterContextReferenceProvider(dummyRefProvider{"test"})
	result := preprocess(t, ctx, "check @test:ENG-123", t.TempDir(), 10000)
	if !result.Expanded || !strings.Contains(result.Message, "expanded: ENG-123") {
		t.Fatalf("%+v", result)
	}
	if strings.Contains(result.Message, "test:ENG-123") && !strings.Contains(result.Message, "Attached Context") {
		t.Fatalf("%+v", result)
	}
}

func TestExpandPluginReturnsNone(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	RegisterContextReferenceProvider(noneExpandProvider{})
	result := preprocess(t, ctx, "check @skip:foo", t.TempDir(), 10000)
	for _, r := range result.References {
		if r.Kind == "skip" && strings.Contains(result.Message, "expanded") {
			t.Fatalf("%+v", result)
		}
	}
}

func TestExpandPluginError(t *testing.T) {
	ctx, _ := referenceTestHome(t)
	RegisterContextReferenceProvider(errorRefProvider{})
	result := preprocess(t, ctx, "check @boom:oops", t.TempDir(), 10000)
	if !result.Expanded || !strings.Contains(result.Message, "plugin expansion error") {
		t.Fatalf("%+v", result)
	}
}

func TestCompletionItemDefaults(t *testing.T) {
	item := NewContextCompletionItem("@issue:1", "", "")
	if item.Text != "@issue:1" || item.Display != "@issue:1" || item.Meta != "" {
		t.Fatalf("%+v", item)
	}
}

func TestHostedContextObjectNameReadsEnginesAndReferenceProviders(t *testing.T) {
	referenceTestHome(t)
	if name, ok := HostedContextObjectName("context_reference", dummyRefProvider{"issue"}); !ok || name != "issue" {
		t.Fatal(name, ok)
	}
	if _, ok := HostedContextObjectName("context_reference", "nope"); ok {
		t.Fatal("a foreign value has no name")
	}
	if _, ok := HostedContextObjectName("context_engine", "nope"); ok {
		t.Fatal("a foreign value is no engine")
	}
}
