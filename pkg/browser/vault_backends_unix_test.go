//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	extraChildModes["fake-bw"] = runFakeBW
	extraChildModes["fake-bw-env"] = runFakeBWEnv
	extraChildModes["fake-op"] = runFakeOp
}

func appendFakeLog(name string, record any) {
	line, _ := json.Marshal(record)
	f, err := os.OpenFile(filepath.Join(os.Getenv("FAKE_DIR"), name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(3)
	}
	f.Write(append(line, '\n'))
	f.Close()
}

func envOrNil(name string) any {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return nil
}

func argvStarts(argv []string, first, second string) bool {
	return len(argv) >= 2 && argv[0] == first && argv[1] == second
}

// Stand in bw with the real CLI's password contract, logging argv, stdin and where the master password travelled
func runFakeBW() int {
	argv := os.Args[1:]
	stdin, _ := io.ReadAll(os.Stdin)
	var pw any
	pwEnv := ""
	if i := slices.Index(argv, "--passwordenv"); i >= 0 && i+1 < len(argv) {
		pwEnv = argv[i+1]
		pw = envOrNil(pwEnv)
	}
	appendFakeLog("bw.log", map[string]any{"argv": argv, "stdin": string(stdin), "BW_SESSION": envOrNil("BW_SESSION"), "pw": pw})
	if argvStarts(argv, "unlock", "--raw") {
		if pwEnv == "" {
			os.Stderr.WriteString("Master password is required. Try again in interactive mode or provide a password file or environment variable.\n")
			return 1
		}
		if os.Getenv(pwEnv) != "correct horse" {
			os.Stderr.WriteString("Invalid master password.\n")
			return 1
		}
		fmt.Println("SESSION-TOKEN-123")
		return 0
	}
	if os.Getenv("BW_SESSION") != "SESSION-TOKEN-123" {
		os.Stderr.WriteString("Vault is locked.\n")
		return 1
	}
	if argvStarts(argv, "list", "items") {
		fmt.Println(`[{"id": "abc", "type": 1, "name": "Example", "creationDate": "2026-01-01T00:00:00Z", ` +
			`"login": {"username": "jane@example.com", "uris": [{"uri": "https://example.com/login"}]}}, ` +
			`{"id": "note", "type": 2, "name": "Secure note"}]`)
		return 0
	}
	if argvStarts(argv, "get", "password") {
		fmt.Println("plain sentence nobody would flag 7")
		return 0
	}
	return 2
}

// Stand in bw that logs the environment of every invocation
func runFakeBWEnv() int {
	argv := os.Args[1:]
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	appendFakeLog("bw.log", map[string]any{"argv": argv, "env": env})
	if argvStarts(argv, "unlock", "--raw") {
		fmt.Println("dummy-bw-session-7f3a")
		return 0
	}
	if argvStarts(argv, "list", "items") {
		fmt.Println("[]")
		return 0
	}
	return 2
}

// Stand in op holding the service account rule that item get must name a vault
func runFakeOp() int {
	argv := os.Args[1:]
	appendFakeLog("op.log", argv)
	if argvStarts(argv, "item", "list") {
		items := []any{}
		for _, id := range []string{"item-a", "item-b", "item-c"} {
			items = append(items, map[string]any{"id": id, "title": id, "vault": map[string]any{"id": "vault-" + id[len(id)-1:]},
				"urls": []any{map[string]any{"href": "https://example.com/login"}}})
		}
		out, _ := json.Marshal(items)
		fmt.Println(string(out))
		return 0
	}
	if argvStarts(argv, "item", "get") {
		if os.Getenv("OP_SERVICE_ACCOUNT_TOKEN") != "" && !slices.Contains(argv, "--vault") {
			os.Stderr.WriteString("a vault query must be provided when this command is called by a service account\n")
			return 1
		}
		if slices.Contains(argv, "item-c") {
			os.Stderr.WriteString("error initializing client: Validation: (failed to session.DecodeSACredentials)\n")
			return 1
		}
		if slices.Contains(argv, "--otp") {
			fmt.Println("123456")
		} else {
			fmt.Println("dummy-password")
		}
		return 0
	}
	return 2
}

// Fake manager executable logging beside itself, the directory it logs into returned with it
func fakeManager(t *testing.T, mode string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return fakeExe(t, mode, "FAKE_DIR='"+dir+"'"), dir
}

func readFakeLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

// The fake bw as the only enabled backend of a locked profile
func fakeBWBackend(t *testing.T) (context.Context, *tools.Env, *BitwardenLoginBackend, string) {
	t.Helper()
	ctx, env := vaultProfile(t)
	exe, dir := fakeManager(t, "fake-bw")
	backend := newBitwardenLoginBackend(ctx, map[string]any{"enabled": true, "binary_path": exe})
	pin(t, &enabledLoginBackendsFn, func(context.Context) ([]LoginBackend, error) { return []LoginBackend{backend}, nil })
	return ctx, env, backend, filepath.Join(dir, "bw.log")
}

func TestLockedManagerIsReportedNotPromptedWhenHeadless(t *testing.T) {
	ctx, env, _, log := fakeBWBackend(t)
	t.Setenv("ICEBATH_CRON_SESSION", "1")
	SetUnlockPromptCallback(ctx, func(string, string) string { return "correct horse" })
	defer SetUnlockPromptCallback(ctx, nil)
	listText, err := browserVaultList(ctx, env)
	listed := loadResult(t, listText, err)
	if items, _ := anyGet(listed, "items").([]any); len(items) != 0 {
		t.Fatalf("items %v", items)
	}
	wantLocked := `[{"backend": "bitwarden", "display_name": "Bitwarden", "unlock": "unavailable_in_this_session"}]`
	if got := jsonText(anyGet(listed, "locked")); got != wantLocked {
		t.Fatalf("locked %s", got)
	}
	fillText, err := browserVaultFill(ctx, env, "bw:abc", "t")
	filled := loadResult(t, fillText, err)
	if anyGet(filled, "success") != false || anyGet(filled, "error_type") != "unlock_unavailable" {
		t.Fatalf("fill %s", fillText)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("bw must not be invoked at all while locked in a headless session")
	}
	if IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("bitwarden unlocked")
	}
}

func TestUnlockUsesVendorPasswordenvContractThenFillRoutesByPrefix(t *testing.T) {
	ctx, env, backend, log := fakeBWBackend(t)
	prompts := [][2]string{}
	SetUnlockPromptCallback(ctx, func(name, display string) string {
		prompts = append(prompts, [2]string{name, display})
		return "correct horse"
	})
	defer SetUnlockPromptCallback(ctx, nil)
	pin(t, &vaultCurrentPageOrigin, func(context.Context, *tools.Env, string) string { return "https://example.com" })
	pin(t, &vaultEvalJS, func(context.Context, *tools.Env, string, string) *pyjson.Object {
		return pyjson.ObjectOf("success", true, "result", `[{"tag": "input", "type": "password", "name": "password", "id": "pw", `+
			`"autocomplete": "current-password", "visible": true}]`)
	})
	secretScripts := []string{}
	pin(t, &vaultEvalJSSecret, func(_ context.Context, _ *tools.Env, _ string, expression string) *pyjson.Object {
		secretScripts = append(secretScripts, expression)
		return pyjson.ObjectOf("success", true, "result", `{"filled": 1}`)
	})
	fillText, err := browserVaultFill(ctx, env, "bw:abc", "t")
	out := loadResult(t, fillText, err)
	out.Pop("next")
	if got := jsonText(out); got != `{"success": true, "filled_fields": 1, "backend": "bitwarden", "kind": "login", "origin": "https://example.com"}` {
		t.Fatalf("fill %s", got)
	}
	if !reflect.DeepEqual(prompts, [][2]string{{"bitwarden", "Bitwarden"}}) {
		t.Fatalf("prompts %v", prompts)
	}
	listText, err := browserVaultList(ctx, env)
	listed := loadResult(t, listText, err)
	first := anyGet(listed, "items").([]any)[0]
	if anyGet(first, "handle") != "bw:abc" || anyGet(first, "identifier") != "jane@example.com" {
		t.Fatalf("listed %s", listText)
	}
	if strings.Contains(listText, "plain sentence nobody would flag 7") {
		t.Fatal("listing carries the password")
	}
	if len(secretScripts) == 0 || !strings.Contains(secretScripts[len(secretScripts)-1], "plain sentence nobody would flag 7") {
		t.Fatal("the password did not reach the fill script")
	}

	calls := readFakeLog(t, log)
	unlockCalls := 0
	for _, c := range calls {
		argv := stringsOf(c["argv"])
		if argvStarts(argv, "unlock", "--raw") {
			unlockCalls++
			if c["pw"] != "correct horse" || c["stdin"] != "" {
				t.Fatalf("unlock call %v", c)
			}
		}
		if strings.Contains(strings.Join(argv, " "), "correct horse") {
			t.Fatal("master password must never be argv")
		}
		if argv[0] != "unlock" && c["BW_SESSION"] != "SESSION-TOKEN-123" {
			t.Fatalf("call without the session token %v", c)
		}
	}
	if unlockCalls != 1 {
		t.Fatalf("unlock calls %d", unlockCalls)
	}
	if _, ok := os.LookupEnv(bitwardenMasterEnv); ok {
		t.Fatal("master password env var is child-only")
	}

	own := os.Getenv("ICEBATH_HOME")
	name := "bitwarden"
	os.Setenv("ICEBATH_HOME", filepath.Join(filepath.Dir(log), "other-profile"))
	lockedElsewhere := !backend.IsUnlocked()
	LockVault(ctx, &name)
	os.Setenv("ICEBATH_HOME", own)
	if !lockedElsewhere {
		t.Fatal("another profile saw this profile's session")
	}
	if !backend.IsUnlocked() {
		t.Fatal("another profile locked this profile's session")
	}
	LockVault(ctx, &name)
	if backend.IsUnlocked() {
		t.Fatal("still unlocked after lock")
	}
	if _, ok := os.LookupEnv("BW_SESSION"); ok {
		t.Fatal("session token must never touch the process env")
	}
}

func TestLockDuringUnlockWinsAndOnlyTheOwningSessionReleaseDropsAToken(t *testing.T) {
	ctx, _, backend, _ := fakeBWBackend(t)
	name := "bitwarden"
	gen := BeginVaultUnlock(ctx, "bitwarden")
	LockVault(ctx, &name)
	if StoreVaultSessionToken(ctx, "bitwarden", "LATE-TOKEN", &gen) {
		t.Fatal("a late token committed after the lock")
	}
	if backend.IsUnlocked() {
		t.Fatal("unlocked by a discarded token")
	}
	sid := "sess-A"
	SetVaultSessionID(ctx, &sid)
	defer SetVaultSessionID(ctx, nil)
	if err := backend.Unlock("correct horse"); err != nil {
		t.Fatal(err)
	}
	if !backend.IsUnlocked() {
		t.Fatal("not unlocked")
	}
	ReleaseVaultSession("sess-B")
	if !backend.IsUnlocked() {
		t.Fatal("a sibling session release dropped the token")
	}
	ReleaseVaultSession("sess-A")
	if backend.IsUnlocked() {
		t.Fatal("the owning session release kept the token")
	}
}

func TestBitwardenMultiUriItemBindsEverySavedWebOrigin(t *testing.T) {
	backend := newBitwardenLoginBackend(context.Background(), map[string]any{"enabled": true})
	itemsJSON := `[{"id": "multi", "type": 1, "name": "Amazon", "creationDate": "2026-01-01T00:00:00Z",
		"login": {"username": "jane@example.com", "uris": [
			{"uri": "https://amazon.co.uk/signin"},
			{"uri": "https://www.amazon.co.uk"},
			{"uri": "https://eu.account.amazon.com"},
			{"uri": "androidapp://com.amazon.shopping"},
			{"uri": "not a url"},
			{"uri": "https://never.amazon.co.uk", "match": 5}]}}]`
	backend.unlocked = func() bool { return true }
	backend.run = func(...string) (string, error) { return itemsJSON, nil }
	metas, err := backend.ListItems()
	if err != nil || len(metas) != 1 {
		t.Fatalf("metas %v %v", metas, err)
	}
	if metas[0].Origin != "https://amazon.co.uk" {
		t.Fatalf("origin %v", metas[0].Origin)
	}
	want := []string{"https://amazon.co.uk", "https://www.amazon.co.uk", "https://eu.account.amazon.com"}
	if !reflect.DeepEqual(metas[0].AllowedOrigins, want) {
		t.Fatalf("allowed %v", metas[0].AllowedOrigins)
	}
}

func TestOnepasswordMultiUrlItemBindsEverySavedWebOrigin(t *testing.T) {
	backend := newOnePasswordLoginBackend(context.Background(), map[string]any{"enabled": true})
	itemsJSON := `[{"id": "multi", "title": "Amazon", "created_at": "2026-01-01T00:00:00Z",
		"additional_information": "jane@example.com",
		"urls": [{"href": "https://amazon.co.uk"}, {"href": "https://www.amazon.co.uk"},
			{"href": "https://eu.account.amazon.com"}, {"href": "androidapp://com.amazon.shopping"},
			{"href": "::not parseable::"}]},
		{"id": "single", "title": "Shop", "created_at": "2026-01-01T00:00:00Z",
		"urls": [{"href": "https://shop.example.com"}]}]`
	backend.unlocked = func() bool { return true }
	backend.run = func(...string) (string, error) { return itemsJSON, nil }
	listed, err := backend.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	multi, single := findMeta(listed, "op:multi"), findMeta(listed, "op:single")
	if multi == nil || single == nil {
		t.Fatalf("listed %v", listed)
	}
	if multi.Origin != "https://amazon.co.uk" || !reflect.DeepEqual(multi.AllowedOrigins,
		[]string{"https://amazon.co.uk", "https://www.amazon.co.uk", "https://eu.account.amazon.com"}) {
		t.Fatalf("multi %+v", multi)
	}
	if single.Origin != "https://shop.example.com" || !reflect.DeepEqual(single.AllowedOrigins, []string{"https://shop.example.com"}) {
		t.Fatalf("single %+v", single)
	}
	if got := allOrigins([]string{"https://a.com/x", "https://a.com/y"}); !reflect.DeepEqual(got, []string{"https://a.com"}) {
		t.Fatalf("all origins %v", got)
	}
	if got := webOrigins([]string{"androidapp://com.x"}); !reflect.DeepEqual(got, []string{"androidapp://com.x"}) {
		t.Fatalf("web origins %v", got)
	}
}

func TestOnepasswordBackendEnvForwardsConfigDirectory(t *testing.T) {
	t.Setenv("OP_CONFIG_DIR", "/tmp/op-config")
	backend := newOnePasswordLoginBackend(context.Background(), map[string]any{"enabled": true})
	if got := backend.env("")["OP_CONFIG_DIR"]; got != "/tmp/op-config" {
		t.Fatalf("OP_CONFIG_DIR %q", got)
	}
}

// Fresh backend per call over the fake op with a service account token, as the handle lookup builds one
func fakeOpServiceAccount(t *testing.T) (func() *OnePasswordLoginBackend, string) {
	t.Helper()
	exe, dir := fakeManager(t, "fake-op")
	pin(t, &vaultGetSecret, func(_ context.Context, name, def string) string {
		if name == "OP_SERVICE_ACCOUNT_TOKEN" {
			return "dummy-sa-token"
		}
		return def
	})
	return func() *OnePasswordLoginBackend {
		return newOnePasswordLoginBackend(context.Background(), map[string]any{"binary_path": exe})
	}, filepath.Join(dir, "op.log")
}

func readOpLog(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatal(err)
		}
		out = append(out, argv)
	}
	return out
}

func TestOnepasswordServiceAccountReadsNameTheItemsOwnVault(t *testing.T) {
	newBackend, log := fakeOpServiceAccount(t)
	fill := newBackend()
	if meta, err := fill.GetMeta("op:item-b"); err != nil || meta == nil {
		t.Fatalf("meta %v %v", meta, err)
	}
	if password, err := fill.ResolvePassword("op:item-b"); err != nil || password != "dummy-password" {
		t.Fatalf("password %q %v", password, err)
	}
	if code, ok, err := newBackend().ResolveOTP("op:item-a"); err != nil || !ok || code != "123456" {
		t.Fatalf("otp %q %v %v", code, ok, err)
	}
	calls := readOpLog(t, log)
	for i, argv := range calls {
		if argvStarts(argv, "item", "list") {
			calls[i] = argv[:2]
		}
	}
	want := [][]string{{"item", "list"}, {"item", "get", "item-b", "--vault", "vault-b", "--fields", "label=password", "--reveal"},
		{"item", "list"}, {"item", "get", "item-a", "--vault", "vault-a", "--otp"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestOnepasswordServiceAccountRefusalsNameTheRealCause(t *testing.T) {
	newBackend, log := fakeOpServiceAccount(t)
	if _, err := newBackend().ResolvePassword("op:--help"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err %v", err)
	}
	if code, ok, err := newBackend().ResolveOTP("op:ghost"); err != nil || ok || code != "" {
		t.Fatalf("otp %q %v %v", code, ok, err)
	}
	for _, argv := range readOpLog(t, log) {
		if argvStarts(argv, "item", "get") {
			t.Fatalf("secret read for an unlisted handle: %v", argv)
		}
	}
	_, err := newBackend().ResolvePassword("op:item-c")
	var locked *UnlockRequired
	if err == nil || !strings.Contains(err.Error(), "DecodeSACredentials") || errors.As(err, &locked) {
		t.Fatalf("err %v", err)
	}
}

func TestSessionTokenReachesOnlyTheManagerCallThatUsesIt(t *testing.T) {
	ctx, env := vaultProfile(t)
	exe, dir := fakeManager(t, "fake-bw-env")
	backend := newBitwardenLoginBackend(ctx, map[string]any{"binary_path": exe})
	if err := backend.Unlock("dummy master password"); err != nil {
		t.Fatal(err)
	}
	if items, err := backend.ListItems(); err != nil || len(items) != 0 {
		t.Fatalf("items %v %v", items, err)
	}
	token := "dummy-bw-session-7f3a"
	calls := readFakeLog(t, filepath.Join(dir, "bw.log"))
	if len(calls) != 2 {
		t.Fatalf("calls %d", len(calls))
	}
	unlockEnv, listEnv := calls[0]["env"].(map[string]any), calls[1]["env"].(map[string]any)
	if _, ok := unlockEnv["BW_SESSION"]; ok {
		t.Fatal("the unlock call has no session yet")
	}
	if listEnv["BW_SESSION"] != token {
		t.Fatalf("list call session %v", listEnv["BW_SESSION"])
	}
	for _, kv := range os.Environ() {
		if _, v, _ := strings.Cut(kv, "="); v == token {
			t.Fatal("the token never enters Icebath's own environment")
		}
	}
	for _, scrub := range []bool{false, true} {
		child, err := tools.BuildSubprocessEnv(ctx, env, tools.SubprocessEnvOptions{ScrubSecrets: scrub})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range child {
			if v == token {
				t.Fatalf("subprocess env carries the token with scrub %v", scrub)
			}
		}
	}
}

func TestVaultStoreFilePermissions0600(t *testing.T) {
	_, _, store, dir := vaultStore(t)
	addLogin(t, store, "https://example.com", "s3cret-pw")
	for _, name := range []string{"vault.json.enc", "vault.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s has mode %v %v", name, info.Mode().Perm(), err)
		}
	}
}

func TestVaultHardeningVaultDirIsOwnerOnly(t *testing.T) {
	_, _, store, dir := vaultStore(t)
	old := syscall.Umask(0o022)
	addLogin(t, store, "https://example.com", "s3cret-pw")
	syscall.Umask(old)
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("vault dir mode %v %v", info.Mode().Perm(), err)
	}
}
