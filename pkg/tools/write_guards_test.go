package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Asker recording every approval prompt and answering with a mutable choice
type wgAsker struct {
	mu     sync.Mutex
	answer string
	calls  [][]Question
}

func (a *wgAsker) Ask(_ context.Context, qs []Question) (FormResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, qs)
	return FormResult{Outcome: "answered", Answers: []Answer{{Status: "answered", Selected: []string{a.answer}}}}, nil
}

func (a *wgAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// File tool env with no safe root and every headless session flag cleared
func wgEnv(t *testing.T) (*Env, string) {
	t.Helper()
	for _, k := range []string{"ICEBATH_WRITE_SAFE_ROOT", "ICEBATH_CRON_SESSION", "ICEBATH_KANBAN_TASK", "ICEBATH_SESSION_PLATFORM",
		"ICEBATH_PLATFORM", "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_GATEWAY_SESSION", "ICEBATH_SESSION_KEY"} {
		t.Setenv(k, "")
	}
	return newFileToolTestEnv(t)
}

// Env with a CLI approval asker installed whose answer starts as deny
func wgApprovalEnv(t *testing.T) (*Env, string, *wgAsker) {
	t.Helper()
	env, ws := wgEnv(t)
	asker := &wgAsker{answer: "deny"}
	env.Ask = asker
	return env, ws, asker
}

// Home relative path expanded through HOME the way os.path.expanduser does
func wgExpandUser(rel string) string {
	h := os.Getenv("HOME")
	if h == "" {
		h = userHomeDir()
	}
	return filepath.Join(h, filepath.FromSlash(rel))
}

// Python is_write_denied over the file safety classifier
func wgIsWriteDenied(env *Env, path string) bool {
	return classifyWriteDenial(env, path, false) != ""
}

// Writes the profile config.yaml and drops the cached tree
func wgWriteConfig(t *testing.T, env *Env, body string) {
	t.Helper()
	if err := os.MkdirAll(env.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Points the env at another active home the way a per turn home override does
func wgUseHome(t *testing.T, env *Env, h string) {
	t.Helper()
	env.Home = h
	config.ResetCache()
}

// write_file_tool on the default task decoded
func wgWrite(t *testing.T, env *Env, path, content string) map[string]any {
	t.Helper()
	return ftDecode(t, writeFileTool(context.Background(), env, path, content, "default", false, ""))
}

// patch_tool on the default task decoded
func wgPatch(t *testing.T, env *Env, a patchArgs) map[string]any {
	t.Helper()
	return ftDecode(t, patchTool(context.Background(), env, a, "default", nil))
}

func wgStr(s string) *string { return &s }

func wgMustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wgRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wgExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func wgHasError(r map[string]any) bool {
	return truthy(r["error"])
}

// Asserts a tool result carries a BLOCKED refusal
func wgAssertBlocked(t *testing.T, r map[string]any) {
	t.Helper()
	if !wgHasError(r) || !strings.Contains(pyStr(r["error"]), "BLOCKED") {
		t.Fatalf("expected BLOCKED error, got %v", r)
	}
}

func wgAssertNoError(t *testing.T, r map[string]any) {
	t.Helper()
	if wgHasError(r) {
		t.Fatalf("unexpected error %v", r)
	}
}

func wgRealpath(p string) string { return resolveNonStrict(p) }

func TestWriteGuardsStaticDenyTempFileNotDeniedByDefault(t *testing.T) {
	env, ws := wgEnv(t)
	if wgIsWriteDenied(env, filepath.Join(ws, "regular.txt")) {
		t.Fatal("temp file must not be denied")
	}
}

func TestWriteGuardsStaticDenyEtcShadowDenied(t *testing.T) {
	env, _ := wgEnv(t)
	if !wgIsWriteDenied(env, "/etc/shadow") {
		t.Fatal("/etc/shadow must be denied")
	}
}

func TestWriteGuardsSshConfigNotHardDenied(t *testing.T) {
	env, _ := wgEnv(t)
	if wgIsWriteDenied(env, wgExpandUser(".ssh/config")) {
		t.Fatal("~/.ssh/config must not be hard denied")
	}
}

func TestWriteGuardsSshConfigWriteDeniedErrorIsEmpty(t *testing.T) {
	env, _ := wgEnv(t)
	if msg := writeDeniedError(env, wgExpandUser(".ssh/config"), "Write", false); msg != "" {
		t.Fatalf("got %q", msg)
	}
}

func TestWriteGuardsSshConfigIsApprovalRequired(t *testing.T) {
	env, _ := wgEnv(t)
	if !isWriteApprovalRequired(env, wgExpandUser(".ssh/config")) {
		t.Fatal("~/.ssh/config must be approval required")
	}
}

func TestWriteGuardsSshPrivateKeysStillHardDenied(t *testing.T) {
	env, _ := wgEnv(t)
	for _, name := range []string{"id_rsa", "id_ed25519", "authorized_keys"} {
		p := wgExpandUser(".ssh/" + name)
		if !wgIsWriteDenied(env, p) {
			t.Fatalf("%s must be denied", name)
		}
		if isWriteApprovalRequired(env, p) {
			t.Fatalf("%s must not be merely approval gated", name)
		}
	}
}

func TestWriteGuardsSshOtherDirFilesStillHardDenied(t *testing.T) {
	env, _ := wgEnv(t)
	for _, rel := range []string{".ssh/id_rsa.pub", ".ssh/secret_key"} {
		if !wgIsWriteDenied(env, wgExpandUser(rel)) {
			t.Fatalf("%s must be denied", rel)
		}
	}
}

func TestWriteGuardsRegularFileNotApprovalRequired(t *testing.T) {
	env, ws := wgEnv(t)
	if isWriteApprovalRequired(env, filepath.Join(ws, "notes.txt")) {
		t.Fatal("regular file must not be approval required")
	}
}

func TestWriteGuardsSafeRootWritesInsideAllowed(t *testing.T) {
	env, ws := wgEnv(t)
	safeRoot := filepath.Join(ws, "workspace")
	child := filepath.Join(safeRoot, "subdir", "file.txt")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", safeRoot)
	if wgIsWriteDenied(env, child) {
		t.Fatal("write inside safe root must be allowed")
	}
}

func TestWriteGuardsSafeRootWithTildeExpansion(t *testing.T) {
	env, ws := wgEnv(t)
	safeRoot := filepath.Join(ws, "workspace")
	if err := os.MkdirAll(safeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", safeRoot)
	if wgIsWriteDenied(env, filepath.Join(safeRoot, "file.txt")) {
		t.Fatal("write inside safe root must be allowed")
	}
}

func TestWriteGuardsSafeRootDoesNotOverrideStaticDeny(t *testing.T) {
	env, _ := wgEnv(t)
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", wgExpandUser(""))
	if !wgIsWriteDenied(env, wgExpandUser(".ssh/id_rsa")) {
		t.Fatal("static deny must win over the safe root")
	}
}

func TestWriteGuardsMultiRootWriteInsideFirstRootAllowed(t *testing.T) {
	env, ws := wgEnv(t)
	rootA, rootB := filepath.Join(ws, "workspace_a"), filepath.Join(ws, "workspace_b")
	child := filepath.Join(rootA, "subdir", "file.txt")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", rootA+string(os.PathListSeparator)+rootB)
	if wgIsWriteDenied(env, child) {
		t.Fatal("write inside the first root must be allowed")
	}
}

func TestWriteGuardsMultiRootTrailingSeparatorIgnored(t *testing.T) {
	env, ws := wgEnv(t)
	root := filepath.Join(ws, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", root+string(os.PathListSeparator))
	if wgIsWriteDenied(env, filepath.Join(root, "file.txt")) {
		t.Fatal("trailing separator must be ignored")
	}
}

func TestWriteGuardsMultiRootStaticDenyStillWins(t *testing.T) {
	env, ws := wgEnv(t)
	root := filepath.Join(ws, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", root+string(os.PathListSeparator)+wgExpandUser(""))
	if !wgIsWriteDenied(env, wgExpandUser(".ssh/id_rsa")) {
		t.Fatal("static deny must win with multiple roots")
	}
}

func TestWriteGuardsMultiRootDuplicateRootsDeduplicated(t *testing.T) {
	env, ws := wgEnv(t)
	root := filepath.Join(ws, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", root+string(os.PathListSeparator)+root)
	if wgIsWriteDenied(env, filepath.Join(root, "file.txt")) {
		t.Fatal("duplicate roots must still allow writes inside")
	}
}

func TestWriteGuardsDeniedErrorCredentialPathMessage(t *testing.T) {
	env, _ := wgEnv(t)
	msg := writeDeniedError(env, wgExpandUser(".ssh/id_rsa"), "Write", false)
	if msg == "" || !strings.Contains(msg, "protected system/credential file") || strings.Contains(msg, "ICEBATH_WRITE_SAFE_ROOT") {
		t.Fatalf("got %q", msg)
	}
}

func TestWriteGuardsDeniedErrorSafeRootMessage(t *testing.T) {
	env, ws := wgEnv(t)
	safeRoot := filepath.Join(ws, "workspace")
	if err := os.MkdirAll(safeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", safeRoot)
	msg := writeDeniedError(env, filepath.Join(ws, "outside.txt"), "Write", false)
	if msg == "" || !strings.Contains(msg, "outside ICEBATH_WRITE_SAFE_ROOT") || !strings.Contains(msg, safeRoot) ||
		strings.Contains(msg, "protected system/credential file") {
		t.Fatalf("got %q", msg)
	}
}

func TestWriteGuardsDeniedErrorAllowedPathEmpty(t *testing.T) {
	env, ws := wgEnv(t)
	if msg := writeDeniedError(env, filepath.Join(ws, "ok.txt"), "Write", false); msg != "" {
		t.Fatalf("got %q", msg)
	}
}

func TestWriteGuardsSensitivePathSystemPrefixesBlocked(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"etc_hosts", "/etc/hosts"}, {"private_etc_hosts", "/private/etc/hosts"},
		{"private_etc_ssh_config", "/private/etc/ssh/sshd_config"}, {"private_var", "/private/var/db/something"},
		{"boot", "/boot/grub/grub.cfg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := wgEnv(t)
			msg, err := checkSensitivePath(env, tc.path, "default")
			if err != nil {
				t.Fatal(err)
			}
			if msg == "" {
				t.Fatalf("%s must be blocked", tc.path)
			}
		})
	}
}

func TestWriteGuardsSensitivePathSafePathAllowed(t *testing.T) {
	env, _ := wgEnv(t)
	msg, err := checkSensitivePath(env, "/tmp/safe_file.txt", "default")
	if err != nil || msg != "" {
		t.Fatalf("got %q %v", msg, err)
	}
}

func TestWriteGuardsProtectedDenyBlocksWrite(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "SOUL.md", ".cursorrules"} {
		t.Run(name, func(t *testing.T) {
			env, ws, asker := wgApprovalEnv(t)
			target := filepath.Join(ws, name)
			res := wgWrite(t, env, target, "injected")
			wgAssertBlocked(t, res)
			if wgExists(target) {
				t.Fatal("denied write must not land")
			}
			if asker.count() != 1 {
				t.Fatalf("calls %d", asker.count())
			}
		})
	}
}

func TestWriteGuardsProtectedApproveOnceAllowsWrite(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	asker.answer = "once"
	target := filepath.Join(ws, "AGENTS.md")
	wgAssertNoError(t, wgWrite(t, env, target, "approved content"))
	if wgRead(t, target) != "approved content" || asker.count() != 1 {
		t.Fatalf("content %q calls %d", wgRead(t, target), asker.count())
	}
}

func TestWriteGuardsProtectedPromptsEvenUnderYolo(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	prev := yoloModeFrozen
	yoloModeFrozen = true
	t.Cleanup(func() { yoloModeFrozen = prev })
	target := filepath.Join(ws, "AGENTS.md")
	wgAssertBlocked(t, wgWrite(t, env, target, "injected"))
	if wgExists(target) {
		t.Fatal("denied write must not land")
	}
	if asker.count() != 1 {
		t.Fatalf("yolo bypassed the protected gate, calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedSecondWritePromptsAgain(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	asker.answer = "once"
	target := filepath.Join(ws, "AGENTS.md")
	wgWrite(t, env, target, "injected")
	wgWrite(t, env, target, "second")
	if asker.count() != 2 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedCLIPromptToldNoScopePersists(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	asker.answer = "once"
	wgWrite(t, env, filepath.Join(ws, "SOUL.md"), "injected")
	if asker.count() == 0 {
		t.Fatal("no prompt shown")
	}
	if got := asker.calls[0][0].Choices; !reflect.DeepEqual(got, []string{"once", "deny"}) {
		t.Fatalf("prompt advertised a persisted scope %v", got)
	}
}

func TestWriteGuardsProtectedRegularFileNeverPrompts(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	wgAssertNoError(t, wgWrite(t, env, filepath.Join(ws, "notes.md"), "hello"))
	if asker.count() != 0 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedNoHumanFailsClosed(t *testing.T) {
	env, ws := wgEnv(t)
	target := filepath.Join(ws, "AGENTS.md")
	wgAssertBlocked(t, wgWrite(t, env, target, "injected"))
	if wgExists(target) {
		t.Fatal("write without a human must not land")
	}
}

func TestWriteGuardsProtectedConfigDisabledSkipsGate(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	wgWriteConfig(t, env, "security:\n  protected_instruction_files: false\n")
	wgAssertNoError(t, wgWrite(t, env, filepath.Join(ws, "AGENTS.md"), "ok"))
	if asker.count() != 0 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedExtraPatternsFromConfig(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	wgWriteConfig(t, env, "security:\n  protected_instruction_files: true\n  protected_instruction_extra_patterns:\n    - \"*.mdc\"\n")
	wgAssertBlocked(t, wgWrite(t, env, filepath.Join(ws, "rules.mdc"), "injected"))
}

func TestWriteGuardsProtectedSymlinkToProtectedFileGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	real := filepath.Join(ws, "AGENTS.md")
	wgMustWrite(t, real, "original")
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	wgAssertBlocked(t, wgWrite(t, env, link, "injected"))
	if wgRead(t, real) != "original" {
		t.Fatal("symlinked protected file was rewritten")
	}
}

func TestWriteGuardsProtectedCaseVariantGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	wgAssertBlocked(t, wgWrite(t, env, filepath.Join(ws, "agents.MD"), "injected"))
}

func TestWriteGuardsProtectedRelativeTraversalGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	if err := os.MkdirAll(filepath.Join(ws, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	wgAssertBlocked(t, wgWrite(t, env, "./x/../AGENTS.md", "injected"))
	if wgExists(filepath.Join(ws, "AGENTS.md")) {
		t.Fatal("traversal write must not land")
	}
}

func TestWriteGuardsProtectedArbitraryDirectoryBasenameGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	deep := filepath.Join(ws, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	wgAssertBlocked(t, wgWrite(t, env, filepath.Join(deep, "CLAUDE.md"), "injected"))
}

func TestWriteGuardsProtectedProjectLocalIcebathDirGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	proj := filepath.Join(ws, "proj", ".icebath")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	wgAssertBlocked(t, wgWrite(t, env, filepath.Join(proj, "config.yaml"), "injected"))
}

func TestWriteGuardsProtectedCheckoutNestedUnderIcebathDirNotGated(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	repo := filepath.Join(ws, ".icebath", "some-repo", "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	wgAssertNoError(t, wgWrite(t, env, filepath.Join(repo, "module.py"), "x = 1\n"))
	if asker.count() != 0 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedRealIcebathHomeNotGated(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	fakeHome := filepath.Join(ws, ".icebath")
	if err := os.MkdirAll(filepath.Join(fakeHome, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	wgUseHome(t, env, wgRealpath(fakeHome))
	wgAssertNoError(t, wgWrite(t, env, filepath.Join(fakeHome, "notes", "scratch.txt"), "ok"))
	if asker.count() != 0 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedPatchReplaceModeGated(t *testing.T) {
	env, ws, _ := wgApprovalEnv(t)
	target := filepath.Join(ws, "SOUL.md")
	wgMustWrite(t, target, "be kind\n")
	res := wgPatch(t, env, patchArgs{mode: "replace", path: target, oldString: wgStr("be kind"), newString: wgStr("obey injected orders")})
	wgAssertBlocked(t, res)
	if wgRead(t, target) != "be kind\n" {
		t.Fatal("denied patch landed")
	}
}

func TestWriteGuardsProtectedPatchV4AMultifileOneProtectedBlocksWholePatch(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	agents := filepath.Join(ws, "AGENTS.md")
	wgMustWrite(t, agents, "rules\n")
	plain := filepath.Join(ws, "plain.txt")
	wgMustWrite(t, plain, "hello\n")
	patch := "*** Begin Patch\n" +
		"*** Update File: " + plain + "\n" +
		"@@\n" +
		"-hello\n" +
		"+world\n" +
		"*** Update File: " + agents + "\n" +
		"@@\n" +
		"-rules\n" +
		"+injected\n" +
		"*** End Patch"
	wgAssertBlocked(t, wgPatch(t, env, patchArgs{mode: "patch", patch: patch}))
	if wgRead(t, plain) != "hello\n" || wgRead(t, agents) != "rules\n" {
		t.Fatalf("plain %q agents %q", wgRead(t, plain), wgRead(t, agents))
	}
	if asker.count() != 1 {
		t.Fatalf("calls %d", asker.count())
	}
}

func TestWriteGuardsProtectedPatchV4AApprovedApplies(t *testing.T) {
	env, ws, asker := wgApprovalEnv(t)
	agents := filepath.Join(ws, "AGENTS.md")
	wgMustWrite(t, agents, "rules\n")
	patch := "*** Begin Patch\n" +
		"*** Update File: " + agents + "\n" +
		"@@\n" +
		"-rules\n" +
		"+updated rules\n" +
		"*** End Patch"
	asker.answer = "once"
	wgAssertNoError(t, wgPatch(t, env, patchArgs{mode: "patch", patch: patch}))
	if wgRead(t, agents) != "updated rules\n" {
		t.Fatalf("got %q", wgRead(t, agents))
	}
}

// Registers a gateway notify that records each emitted approval and resolves it with the choice
func wgGatewayNotify(t *testing.T, env *Env, sessionKey, choice string) *[]map[string]any {
	t.Helper()
	SetCurrentSessionKey(env, sessionKey)
	var mu sync.Mutex
	seen := &[]map[string]any{}
	RegisterGatewayNotify(sessionKey, func(data map[string]any) error {
		mu.Lock()
		*seen = append(*seen, copyMap(data))
		mu.Unlock()
		ResolveGatewayApproval(sessionKey, choice, false, "", "")
		return nil
	})
	t.Cleanup(func() {
		UnregisterGatewayNotify(sessionKey)
		ResetCurrentSessionKey(env)
	})
	return seen
}

func TestWriteGuardsProtectedGatewayNotifyResolveOnceAllows(t *testing.T) {
	env, ws := wgEnv(t)
	seen := wgGatewayNotify(t, env, "protected-files-test-session", "once")
	target := filepath.Join(ws, "AGENTS.md")
	wgAssertNoError(t, wgWrite(t, env, target, "gateway approved"))
	if wgRead(t, target) != "gateway approved" {
		t.Fatalf("got %q", wgRead(t, target))
	}
	if len(*seen) != 1 {
		t.Fatalf("notifications %d", len(*seen))
	}
	data := (*seen)[0]
	if data["allow_permanent"] != false || data["allow_session"] != false {
		t.Fatalf("buttons offered a persistent scope %v", data)
	}
}

func TestWriteGuardsProtectedGatewayPayloadRendersOnlyOnceAndDeny(t *testing.T) {
	env, ws := wgEnv(t)
	sessionKey := "protected-files-payload-session"
	seen := wgGatewayNotify(t, env, sessionKey, "once")
	wgWrite(t, env, filepath.Join(ws, "SOUL.md"), "gateway approved")
	if len(*seen) != 1 {
		t.Fatalf("notifications %d", len(*seen))
	}
	data := (*seen)[0]
	command, _ := data["command"].(string)
	description, _ := data["description"].(string)
	patternKey, _ := data["pattern_key"].(string)
	req, err := newApprovalRequest(command, description, patternKey, stringList(data["pattern_keys"]), sessionKey, "gateway",
		data["allow_session"] != false, data["allow_permanent"] != false, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.AllowedChoices, []string{"once", "deny"}) {
		t.Fatalf("rendered choices %v", req.AllowedChoices)
	}
}

// Real shaped icebath root with a named worker profile and a root marker
func wgProfileLayout(t *testing.T, base string) (string, string) {
	t.Helper()
	root := filepath.Join(base, "home")
	profile := filepath.Join(root, "profiles", "worker")
	if err := os.MkdirAll(filepath.Join(profile, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	wgMustWrite(t, filepath.Join(root, "config.yaml"), "model:\n  default: x\n")
	return root, profile
}

func TestWriteGuardsProfileScopeExemptsRootDirectFiles(t *testing.T) {
	env, _, asker := wgApprovalEnv(t)
	base := t.TempDir()
	root, profile := wgProfileLayout(t, base)
	wgUseHome(t, env, profile)
	exempt := icebathExemptHomes(env)
	if !inList(exempt, wgRealpath(root)) {
		t.Fatalf("root %s missing from exempt homes %v", wgRealpath(root), exempt)
	}
	for _, name := range []string{"LEDGER.md", "MEMORY.md", "SOUL.md", "AGENTS.md"} {
		if r := protectedInstructionReason(env, filepath.Join(root, name), "default", true, nil); r != "" {
			t.Fatalf("%s gated with %q", name, r)
		}
	}
	ledger := filepath.Join(root, "LEDGER.md")
	wgAssertNoError(t, wgWrite(t, env, ledger, "caliber fixed"))
	if wgRead(t, ledger) != "caliber fixed" || asker.count() != 0 {
		t.Fatalf("content %q calls %d", wgRead(t, ledger), asker.count())
	}
}

func TestWriteGuardsProfileOnlyRealRootExempt(t *testing.T) {
	env, _, asker := wgApprovalEnv(t)
	base := t.TempDir()
	_, profile := wgProfileLayout(t, base)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".icebath"), 0o755); err != nil {
		t.Fatal(err)
	}
	wgUseHome(t, env, profile)
	if protectedInstructionReason(env, filepath.Join(repo, ".icebath", "config.yaml"), "default", true, nil) == "" {
		t.Fatal("checkout .icebath/config.yaml must stay gated")
	}
	if r := protectedInstructionReason(env, filepath.Join(repo, "AGENTS.md"), "default", true, nil); r != "AGENTS.md" {
		t.Fatalf("got %q", r)
	}
	target := filepath.Join(repo, ".icebath", "config.yaml")
	wgAssertBlocked(t, wgWrite(t, env, target, "gate: off\n"))
	if wgExists(target) {
		t.Fatal("denied write must not land")
	}
	if asker.count() != 1 {
		t.Fatalf("calls %d", asker.count())
	}
	fakeProfile := filepath.Join(base, "not-a-hermes-root", "profiles", "worker")
	if err := os.MkdirAll(fakeProfile, 0o755); err != nil {
		t.Fatal(err)
	}
	wgUseHome(t, env, fakeProfile)
	if got := icebathExemptHomes(env); !reflect.DeepEqual(got, []string{wgRealpath(fakeProfile)}) {
		t.Fatalf("got %v", got)
	}
}

// Alpha and beta profiles under one root, each with its own config.yaml
func wgAlphaBeta(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "home")
	a, b := filepath.Join(root, "profiles", "alpha"), filepath.Join(root, "profiles", "beta")
	for _, h := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(h, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		wgMustWrite(t, filepath.Join(h, "config.yaml"), "model:\n  default: original\n")
	}
	return a, b
}

func TestWriteGuardsMultiplexHomeGetterTracksActiveProfile(t *testing.T) {
	env, _ := wgEnv(t)
	a, b := wgAlphaBeta(t)
	wgUseHome(t, env, a)
	if got := realIcebathHome(env); got != wgRealpath(a) {
		t.Fatalf("alpha got %q", got)
	}
	wgUseHome(t, env, b)
	if got := realIcebathHome(env); got != wgRealpath(b) {
		t.Fatalf("beta got %q", got)
	}
}

func TestWriteGuardsMultiplexConfigHardBlockRefusesBetaConfig(t *testing.T) {
	env, _ := wgEnv(t)
	a, b := wgAlphaBeta(t)
	wgUseHome(t, env, a)
	icebathConfigResolved(env)
	wgUseHome(t, env, b)
	msg, err := checkSensitivePath(env, filepath.Join(b, "config.yaml"), "default")
	if err != nil {
		t.Fatal(err)
	}
	if msg == "" || !strings.Contains(msg, "Refusing to write to icebath config file") {
		t.Fatalf("got %q", msg)
	}
}

// Two profile icebath layout with the active home on the security profile
func wgFakeIcebath(t *testing.T, env *Env) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "fake-icebath")
	wgMustWrite(t, filepath.Join(root, "skills", "shared-skill", "SKILL.md"), "---\nname: shared-skill\ndescription: default copy.\n---\n")
	secHome := filepath.Join(root, "profiles", "icebath-security")
	coderHome := filepath.Join(root, "profiles", "coder")
	for _, d := range []string{filepath.Join(secHome, "skills"), filepath.Join(coderHome, "skills")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", secHome)
	wgUseHome(t, env, secHome)
	return root, secHome
}

func TestWriteGuardsCrossProfileWriteAllowedGuardRetired(t *testing.T) {
	env, _ := wgEnv(t)
	root, _ := wgFakeIcebath(t, env)
	target := filepath.Join(root, "skills", "shared-skill", "SKILL.md")
	wgAssertNoError(t, ftDecode(t, readFileTool(context.Background(), env, target, nil, nil, "default")))
	wgAssertNoError(t, wgWrite(t, env, target, "cross-profile write, allowed"))
	if wgRead(t, target) != "cross-profile write, allowed" {
		t.Fatalf("got %q", wgRead(t, target))
	}
}

func TestWriteGuardsCrossProfilePatchBypass(t *testing.T) {
	env, _ := wgEnv(t)
	root, _ := wgFakeIcebath(t, env)
	target := filepath.Join(root, "skills", "shared-skill", "SKILL.md")
	res := wgPatch(t, env, patchArgs{mode: "replace", path: target, oldString: wgStr("default copy."), newString: wgStr("user-directed update."), crossProfile: true})
	wgAssertNoError(t, res)
	if !strings.Contains(wgRead(t, target), "user-directed update.") {
		t.Fatalf("got %q", wgRead(t, target))
	}
}

func TestWriteGuardsCrossProfileV4APatchWritesThrough(t *testing.T) {
	env, _ := wgEnv(t)
	root, _ := wgFakeIcebath(t, env)
	target := filepath.Join(root, "skills", "shared-skill", "SKILL.md")
	v4a := "*** Begin Patch\n" +
		"*** Update File: " + target + "\n" +
		"@@\n" +
		"-default copy.\n" +
		"+v4a cross-profile write, allowed.\n" +
		"*** End Patch"
	wgAssertNoError(t, wgPatch(t, env, patchArgs{mode: "patch", patch: v4a}))
	if !strings.Contains(wgRead(t, target), "v4a cross-profile write, allowed.") {
		t.Fatalf("got %q", wgRead(t, target))
	}
}

const wgBlind = "Your patch changed this file"

// Dispatches a registered tool by name for one task and decodes its result
func wgDispatch(t *testing.T, env *Env, name, taskID string, args map[string]any) map[string]any {
	t.Helper()
	def := Default.Get(name)
	if def == nil {
		t.Fatalf("tool %s not registered", name)
	}
	return ftDecode(t, def.Handler(context.Background(), &Call{Name: name, Args: args, TaskID: taskID, Env: env}))
}

// Dispatch with path set, paging and content merged from the extra arguments
func wgCall(t *testing.T, env *Env, name, path, taskID string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"path": path}
	for k, v := range extra {
		args[k] = v
	}
	return wgDispatch(t, env, name, taskID, args)
}

func wgOk(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	if _, has := r["error"]; has {
		t.Fatalf("unexpected error %v", r)
	}
	return r
}

func wgNoErrorKey(t *testing.T, r map[string]any, what string) {
	t.Helper()
	if _, has := r["error"]; has {
		t.Fatalf("%s: %v", what, r)
	}
}

func wgBWrite(t *testing.T, env *Env, path, taskID, content string) map[string]any {
	t.Helper()
	return wgCall(t, env, "write_file", path, taskID, map[string]any{"content": content})
}

func wgBRead(t *testing.T, env *Env, path, taskID string, paging map[string]any) map[string]any {
	t.Helper()
	return wgOk(t, wgCall(t, env, "read_file", path, taskID, paging))
}

func wgBReplace(t *testing.T, env *Env, path, taskID, old, nw string) map[string]any {
	t.Helper()
	return wgOk(t, wgCall(t, env, "patch", path, taskID, map[string]any{"old_string": old, "new_string": nw}))
}

type wgUpdate struct{ path, old, nw string }

func wgV4A(t *testing.T, env *Env, taskID string, updates ...wgUpdate) map[string]any {
	t.Helper()
	var body strings.Builder
	for _, u := range updates {
		body.WriteString("*** Update File: " + u.path + "\n@@\n-" + u.old + "\n+" + u.nw + "\n")
	}
	return wgOk(t, wgDispatch(t, env, "patch", taskID, map[string]any{"mode": "patch", "patch": "*** Begin Patch\n" + body.String() + "*** End Patch"}))
}

// Asserts a stale write refusal and returns its error text
func wgBlocked(t *testing.T, r map[string]any) string {
	t.Helper()
	if r["stale_write_blocked"] != true {
		t.Fatalf("expected stale_write_blocked, got %v", r)
	}
	return pyStr(r["error"])
}

// Per test task id whose file tool state is dropped when the test ends
func wgTask(t *testing.T) string {
	t.Helper()
	task := "own-patch-" + t.Name()
	t.Cleanup(func() { ClearFileOpsCache(task) })
	return task
}

// Sets the file mtime to the given stamp and leaves atime unchanged
func wgSetMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, time.Time{}, mtime); err != nil {
		t.Fatal(err)
	}
}

func wgMtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestWriteGuardsBaselinePartialRereadKeepsUnchangedFullReadOrWrite(t *testing.T) {
	env, ws := wgEnv(t)
	original := "first\nsecond\nthird\n"
	for _, source := range []string{"read", "write", "pages"} {
		func() {
			task := "known-" + source
			path := filepath.Join(ws, source+".txt")
			defer ClearFileOpsCache(task)
			if source == "write" {
				wgNoErrorKey(t, wgCall(t, env, "write_file", path, task, map[string]any{"content": original}), source)
			} else {
				wgMustWrite(t, path, original)
				if source == "pages" {
					for _, offset := range []int{1, 2, 3} {
						wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, map[string]any{"offset": offset, "limit": 1}), source)
					}
				} else {
					wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, nil), source)
				}
			}
			wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, map[string]any{"offset": 2, "limit": 2}), source)
			written := wgCall(t, env, "write_file", path, task, map[string]any{"content": "replacement\n"})
			wgNoErrorKey(t, written, source)
			if wgRead(t, path) != "replacement\n" {
				t.Fatalf("%s: got %q", source, wgRead(t, path))
			}
		}()
	}
}

func TestWriteGuardsBaselinePartialReadCannotRefreshChangedFullBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	original := "first\nsecond\nthird\n"
	for _, source := range []string{"write", "pages"} {
		func() {
			path := filepath.Join(ws, "changed-"+source+".txt")
			task := "snapshot-" + source
			defer ClearFileOpsCache(task)
			if source == "write" {
				wgNoErrorKey(t, wgCall(t, env, "write_file", path, task, map[string]any{"content": original}), source)
			} else {
				wgMustWrite(t, path, original)
				wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, map[string]any{"offset": 1, "limit": 1}), source)
			}
			stamp := wgMtime(t, path)
			wgMustWrite(t, path, "other\nsecond\nthird\n")
			wgSetMtime(t, path, stamp)
			wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, map[string]any{"offset": 2, "limit": 2}), source)
			refused := wgCall(t, env, "write_file", path, task, map[string]any{"content": "replacement\n"})
			if !truthy(refused["stale_write_blocked"]) {
				t.Fatalf("%s: %v", source, refused)
			}
			if wgRead(t, path) != "other\nsecond\nthird\n" {
				t.Fatalf("%s: got %q", source, wgRead(t, path))
			}
			wgNoErrorKey(t, wgCall(t, env, "read_file", path, task, nil), source)
			wgNoErrorKey(t, wgCall(t, env, "write_file", path, task, map[string]any{"content": "merged\n"}), source)
			if wgRead(t, path) != "merged\n" {
				t.Fatalf("%s: got %q", source, wgRead(t, path))
			}
		}()
	}

	maxLine := ToolOutputLimits(env).MaxLineLength
	path := filepath.Join(ws, "clamped.txt")
	long := strings.Repeat("x", maxLine+10)
	wgMustWrite(t, path, long+"\nlast\n")
	defer ClearFileOpsCache("clamped")
	wgNoErrorKey(t, wgCall(t, env, "read_file", path, "clamped", nil), "clamped")
	refused := wgCall(t, env, "write_file", path, "clamped", map[string]any{"content": "replacement\n"})
	if !truthy(refused["stale_write_blocked"]) {
		t.Fatalf("clamped: %v", refused)
	}
	if !strings.HasPrefix(wgRead(t, path), long) {
		t.Fatal("clamped file was overwritten")
	}
}

func TestWriteGuardsBaselineOwnReplacePatchAfterFullReadKeepsBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "probe.txt")
	wgOk(t, wgBWrite(t, env, path, task, "first=old\nsecond=old\n"))
	wgBRead(t, env, path, task, nil)
	wgBReplace(t, env, path, task, "first=old", "first=new")
	written := wgBWrite(t, env, path, task, "first=new\nsecond=new\n")
	if _, has := written["error"]; has || truthy(written["stale_write_blocked"]) {
		t.Fatalf("got %v", written)
	}
	if wgRead(t, path) != "first=new\nsecond=new\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselineOwnPatchAfterOwnWriteKeepsBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "written.txt")
	wgOk(t, wgBWrite(t, env, path, task, "a=1\nb=1\n"))
	wgBReplace(t, env, path, task, "a=1", "a=2")
	wgBReplace(t, env, path, task, "b=1", "b=2")
	wgOk(t, wgBWrite(t, env, path, task, "a=3\nb=3\n"))
	if wgRead(t, path) != "a=3\nb=3\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselineOwnV4APatchKeepsBaselineForEveryFile(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	one, two := filepath.Join(ws, "one.txt"), filepath.Join(ws, "two.txt")
	for _, p := range []string{one, two} {
		wgMustWrite(t, p, "alpha\nbeta\n")
		wgBRead(t, env, p, task, nil)
	}
	wgV4A(t, env, task, wgUpdate{one, "alpha", "ALPHA"})
	wgOk(t, wgBWrite(t, env, one, task, "single\n"))
	if wgRead(t, one) != "single\n" {
		t.Fatalf("got %q", wgRead(t, one))
	}

	wgBRead(t, env, one, task, nil)
	wgV4A(t, env, task, wgUpdate{one, "single", "SINGLE"}, wgUpdate{two, "alpha", "ALPHA"})
	for _, p := range []string{one, two} {
		want := "replaced " + filepath.Base(p) + "\n"
		wgOk(t, wgBWrite(t, env, p, task, want))
		if wgRead(t, p) != want {
			t.Fatalf("got %q", wgRead(t, p))
		}
	}

	wgV4A(t, env, task, wgUpdate{one, "replaced " + filepath.Base(one), "first"}, wgUpdate{one, "first", "second"})
	if wgRead(t, one) != "second\n" {
		t.Fatalf("got %q", wgRead(t, one))
	}
	wgOk(t, wgBWrite(t, env, one, task, "chained\n"))

	added := filepath.Join(ws, "added.txt")
	wgOk(t, wgDispatch(t, env, "patch", task, map[string]any{"mode": "patch",
		"patch": "*** Begin Patch\n*** Add File: " + added + "\n+created\n*** End Patch"}))
	wgOk(t, wgBWrite(t, env, added, task, "replaced\n"))
	if wgRead(t, added) != "replaced\n" {
		t.Fatalf("got %q", wgRead(t, added))
	}
}

func TestWriteGuardsBaselineOwnPatchKeepsCRLFAndBOMFilesWritable(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "crlf.txt")
	wgMustWrite(t, path, "\xef\xbb\xbfone\r\ntwo\r\n")
	wgBRead(t, env, path, task, nil)
	wgBReplace(t, env, path, task, "one", "ONE")
	if got := wgRead(t, path); got != "\xef\xbb\xbfONE\r\ntwo\r\n" {
		t.Fatalf("got %q", got)
	}
	wgOk(t, wgBWrite(t, env, path, task, "three\n"))
}

func TestWriteGuardsBaselineBlindPatchNeverGainsBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "blind.txt")
	wgMustWrite(t, path, "one\ntwo\nthree\n")
	wgBReplace(t, env, path, task, "one", "ONE")
	if e := wgBlocked(t, wgBWrite(t, env, path, task, "x\n")); !strings.Contains(e, wgBlind) {
		t.Fatalf("got %q", e)
	}
	if wgRead(t, path) != "ONE\ntwo\nthree\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}

	wgMustWrite(t, path, "one\ntwo\nthree\n")
	wgBRead(t, env, path, task, map[string]any{"offset": 1, "limit": 1})
	wgBReplace(t, env, path, task, "two", "TWO")
	if e := wgBlocked(t, wgBWrite(t, env, path, task, "x\n")); !strings.Contains(e, wgBlind) {
		t.Fatalf("got %q", e)
	}
	if wgRead(t, path) != "one\nTWO\nthree\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}

	wgBRead(t, env, path, task, nil)
	wgOk(t, wgBWrite(t, env, path, task, "merged\n"))
	if wgRead(t, path) != "merged\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselineSiblingPatchVoidsOtherTaskAndGivesPatcherNothing(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "shared.txt")
	wgMustWrite(t, path, "left\nright\n")
	sibling := task + "-sibling"
	defer ClearFileOpsCache(sibling)
	wgBRead(t, env, path, task, nil)
	wgBReplace(t, env, path, sibling, "right", "RIGHT")

	mine := wgBlocked(t, wgBWrite(t, env, path, task, "mine\n"))
	if strings.Contains(mine, wgBlind) || !strings.Contains(mine, sibling) {
		t.Fatalf("mine %q", mine)
	}
	if e := wgBlocked(t, wgBWrite(t, env, path, sibling, "theirs\n")); !strings.Contains(e, wgBlind) {
		t.Fatalf("theirs %q", e)
	}
	if wgRead(t, path) != "left\nRIGHT\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}

	wgBRead(t, env, path, task, nil)
	wgOk(t, wgBWrite(t, env, path, task, "mine\n"))
	wgBRead(t, env, path, sibling, nil)
	wgOk(t, wgBWrite(t, env, path, sibling, "theirs\n"))
	if wgRead(t, path) != "theirs\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselineExternalEditAfterOwnPatchRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		preserveMtime bool
	}{{"mtime-moves", false}, {"mtime-restored", true}} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := wgEnv(t)
			task := wgTask(t)
			path := filepath.Join(ws, "external.txt")
			wgMustWrite(t, path, "one\ntwo\n")
			wgBRead(t, env, path, task, nil)
			wgBReplace(t, env, path, task, "one", "ONE")
			stamp := wgMtime(t, path)
			wgMustWrite(t, path, "ONE\nEXTERNAL\n")
			if tc.preserveMtime {
				wgSetMtime(t, path, stamp)
			} else {
				wgSetMtime(t, path, stamp.Add(time.Second))
			}
			if e := wgBlocked(t, wgBWrite(t, env, path, task, "clobber\n")); strings.Contains(e, wgBlind) {
				t.Fatalf("got %q", e)
			}
			if wgRead(t, path) != "ONE\nEXTERNAL\n" {
				t.Fatalf("got %q", wgRead(t, path))
			}
			wgBRead(t, env, path, task, nil)
			wgOk(t, wgBWrite(t, env, path, task, "merged\n"))
			if wgRead(t, path) != "merged\n" {
				t.Fatalf("got %q", wgRead(t, path))
			}
		})
	}
}

// Routes the task's backend through a scripted patch_replace until the returned undo runs
func wgScriptPatchReplace(t *testing.T, env *Env, task string, fn func(real fileToolOps, path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult) func() {
	t.Helper()
	real, err := getFileOpsShared(env, task)
	if err != nil {
		t.Fatal(err)
	}
	prev := getFileOps
	ops := &scriptedFileOps{fileToolOps: real.ops}
	ops.patchReplaceFn = func(path, oldString, newString string, replaceAll bool, probe *editProbe) *PatchResult {
		return fn(real.ops, path, oldString, newString, replaceAll, probe)
	}
	installScriptedFileOps(t, env, ops, nil)
	return func() { getFileOps = prev }
}

func TestWriteGuardsBaselineWriteLandingAfterPatchWriteGivesNoBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "raced-after.txt")
	wgMustWrite(t, path, "one\ntwo\n")
	wgBRead(t, env, path, task, nil)
	undo := wgScriptPatchReplace(t, env, task, func(real fileToolOps, p, o, n string, all bool, probe *editProbe) *PatchResult {
		result := real.patchReplace(p, o, n, all, probe)
		stamp := wgMtime(t, path)
		wgMustWrite(t, path, "ONE\nRACED\n")
		wgSetMtime(t, path, stamp)
		return result
	})
	wgBReplace(t, env, path, task, "one", "ONE")
	undo()
	wgBlocked(t, wgBWrite(t, env, path, task, "clobber\n"))
	if wgRead(t, path) != "ONE\nRACED\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselineWriteLandingBeforePatchReadGivesNoBaseline(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "raced-before.txt")
	wgMustWrite(t, path, "one\ntwo\n")
	wgBRead(t, env, path, task, nil)
	undo := wgScriptPatchReplace(t, env, task, func(real fileToolOps, p, o, n string, all bool, probe *editProbe) *PatchResult {
		wgMustWrite(t, path, "one\nUNSEEN\n")
		return real.patchReplace(p, o, n, all, probe)
	})
	wgBReplace(t, env, path, task, "one", "ONE")
	undo()
	if wgRead(t, path) != "ONE\nUNSEEN\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
	wgBlocked(t, wgBWrite(t, env, path, task, "clobber\n"))
	if wgRead(t, path) != "ONE\nUNSEEN\n" {
		t.Fatalf("got %q", wgRead(t, path))
	}
}

func TestWriteGuardsBaselinePagedReadCarriesOnlyWhenEveryPageRead(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	content := b.String()
	complete, missing := filepath.Join(ws, "complete.txt"), filepath.Join(ws, "missing.txt")
	for _, p := range []string{complete, missing} {
		wgMustWrite(t, p, content)
		if !truthy(wgBRead(t, env, p, task, nil)["truncated"]) {
			t.Fatalf("%s first page not truncated", p)
		}
	}
	wgBRead(t, env, complete, task, map[string]any{"offset": 2001})

	for _, p := range []string{complete, missing} {
		wgBReplace(t, env, p, task, "line 1\n", "LINE 1\n")
	}
	wgOk(t, wgBWrite(t, env, complete, task, "merged\n"))
	if wgRead(t, complete) != "merged\n" {
		t.Fatalf("got %q", wgRead(t, complete))
	}
	if e := wgBlocked(t, wgBWrite(t, env, missing, task, "merged\n")); !strings.Contains(e, wgBlind) {
		t.Fatalf("got %q", e)
	}
	if !strings.HasPrefix(wgRead(t, missing), "LINE 1\nline 2\n") {
		t.Fatal("missing file was overwritten")
	}
}

func TestWriteGuardsBaselineOwnPatchAfterCompactionKeepsBaseline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reread bool
	}{{"no-reread", false}, {"reread", true}} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := wgEnv(t)
			task := wgTask(t)
			path := filepath.Join(ws, "compacted.txt")
			wgMustWrite(t, path, "one\ntwo\n")
			wgBRead(t, env, path, task, nil)
			ResetFileDedup(task)
			if tc.reread {
				wgBRead(t, env, path, task, nil)
			}
			wgBReplace(t, env, path, task, "one", "ONE")
			wgOk(t, wgBWrite(t, env, path, task, "merged\n"))
			if wgRead(t, path) != "merged\n" {
				t.Fatalf("got %q", wgRead(t, path))
			}
		})
	}
}

func TestWriteGuardsBaselineThirdPatchFailureHintRequiresFullReread(t *testing.T) {
	env, ws := wgEnv(t)
	task := wgTask(t)
	path := filepath.Join(ws, "anchor.txt")
	wgMustWrite(t, path, "alpha\n")
	var failed map[string]any
	for range 3 {
		failed = wgDispatch(t, env, "patch", task, map[string]any{"path": path, "old_string": "missing", "new_string": "x"})
	}
	hint := pyStr(failed["_hint"])
	if !strings.Contains(hint, "failure #3") || !strings.Contains(hint, "re-read the file in full, then use write_file to replace it") ||
		strings.Contains(hint, "use write_file to replace the entire file") {
		t.Fatalf("hint %q", hint)
	}
}
