package config

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Fresh home with no managed env marker, the shape the managed install tests start from
func installHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED", "")
	os.Unsetenv("ICEBATH_MANAGED")
	return h
}

func TestOldUpdaterShimsDoNoWork(t *testing.T) {
	if IsUVToolInstall() || IsUnsupportedInstallMethod("pip") || FormatUnsupportedInstallWarning("pip") != "" {
		t.Fatal("retired probes must report nothing")
	}
}

func TestRecommendedUpdateCommandDefaultsToHermesUpdate(t *testing.T) {
	h := installHome(t)
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".install_method"), []byte("git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_INSTALL_ROOT", tree)
	if got, err := RecommendedUpdateCommand(context.Background(), h); err != nil || got != "icebath update" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestGetManagedSystemFalseValues(t *testing.T) {
	for _, v := range []string{"false", "0", "no", "off", "FALSE"} {
		h := installHome(t)
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, ".install_method"), []byte("git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_INSTALL_ROOT", tree)
		t.Setenv("ICEBATH_MANAGED", v)
		if got := ManagedSystem(context.Background(), h); got != "" {
			t.Fatalf("%s names the managing system %q", v, got)
		}
		if got, err := RecommendedUpdateCommand(context.Background(), h); err != nil || got != "icebath update" {
			t.Fatalf("%s: got %q %v", v, got, err)
		}
	}
}

func TestManagedInstallShapes(t *testing.T) {
	ctx := context.Background()
	t.Run("test_env_var_names_the_managing_system", func(t *testing.T) {
		for _, tc := range []struct {
			env  string
			set  bool
			want string
		}{
			{"nixos", true, "nixos"}, {"home-manager", true, "home-manager"}, {"HOME-MANAGER", true, "home-manager"},
			{"true", true, "nixos"}, {"1", true, "nixos"}, {"brew", true, ""}, {"homebrew", true, ""}, {"", false, ""},
		} {
			h := installHome(t)
			if tc.set {
				t.Setenv("ICEBATH_MANAGED", tc.env)
			} else {
				os.Unsetenv("ICEBATH_MANAGED")
			}
			if got := ManagedSystem(ctx, h); got != tc.want {
				t.Fatalf("env %q: got %q want %q", tc.env, got, tc.want)
			}
		}
	})
	t.Run("test_marker_file_names_the_managing_system", func(t *testing.T) {
		for _, tc := range [][2]string{{"home-manager", "home-manager"}, {"nixos", "nixos"}, {"", "nixos"}} {
			h := installHome(t)
			if err := os.WriteFile(filepath.Join(h, ".managed"), []byte(tc[0]), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Unsetenv("ICEBATH_MANAGED")
			if got := ManagedSystem(ctx, h); got != tc[1] {
				t.Fatalf("marker %q: got %q want %q", tc[0], got, tc[1])
			}
		}
	})
	t.Run("test_env_var_wins_over_the_marker", func(t *testing.T) {
		h := installHome(t)
		if err := os.WriteFile(filepath.Join(h, ".managed"), []byte("nixos"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_MANAGED", "home-manager")
		if got := ManagedSystem(ctx, h); got != "home-manager" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_managed_install_names_its_system_and_offers_an_update", func(t *testing.T) {
		for _, managed := range []string{"nixos", "home-manager"} {
			h := installHome(t)
			t.Setenv("ICEBATH_MANAGED", managed)
			tree := t.TempDir()
			msg := FormatManagedMessage(ctx, h, "set model")
			if !strings.Contains(msg, managed) || !strings.Contains(msg, "set model") {
				t.Fatalf("%s: message %q", managed, msg)
			}
			if GetManagedUpdateCommand(ctx, h) == "" {
				t.Fatalf("%s: no update command", managed)
			}
			if got, err := DetectInstallMethod(ctx, h, tree); err != nil || got != managed {
				t.Fatalf("%s: detect %q %v", managed, got, err)
			}
			if got, err := RecommendedUpdateCommand(ctx, h); err != nil || got == "icebath update" {
				t.Fatalf("%s: recommended %q %v", managed, got, err)
			}
		}
	})
	t.Run("test_a_stamp_can_name_every_managed_system", func(t *testing.T) {
		for _, managed := range []string{"nixos", "home-manager"} {
			h := installHome(t)
			os.Unsetenv("ICEBATH_MANAGED")
			tree := t.TempDir()
			if err := os.WriteFile(filepath.Join(tree, ".install_method"), []byte(managed+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := DetectInstallMethod(ctx, h, tree); err != nil || got != managed {
				t.Fatalf("stamp %s: got %q %v", managed, got, err)
			}
		}
	})
	t.Run("test_unmanaged_install_offers_no_update_command", func(t *testing.T) {
		h := installHome(t)
		os.Unsetenv("ICEBATH_MANAGED")
		if got := GetManagedUpdateCommand(ctx, h); got != "" {
			t.Fatalf("update command %q", got)
		}
	})
	t.Run("test_unreadable_marker_still_reports_managed", func(t *testing.T) {
		h := installHome(t)
		marker := filepath.Join(h, ".managed")
		if err := os.WriteFile(marker, []byte("home-manager"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(marker, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(marker, 0o600)
		os.Unsetenv("ICEBATH_MANAGED")
		if f, err := os.Open(marker); err == nil {
			f.Close()
			t.Skip("running as a user that ignores file modes (for example root)")
		}
		if ManagedSystem(ctx, h) == "" {
			t.Fatal("an unreadable marker no longer reports managed")
		}
	})
}

func TestDetectInstallMethodOrder(t *testing.T) {
	h := installHome(t)
	code := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(code, ".install_method"), "\xef\xbb\xbfGIT\n")
	write(filepath.Join(h, ".install_method"), "docker\n")
	if got, _ := DetectInstallMethod(context.Background(), h, code); got != "git" {
		t.Fatalf("code stamp must win, got %q", got)
	}
	bare := t.TempDir()
	t.Cleanup(hostinfo.PinContainer(false))
	if got, _ := DetectInstallMethod(context.Background(), h, bare); got != "unknown" {
		t.Fatalf("a home docker stamp outside a container is ignored, got %q", got)
	}
	t.Cleanup(hostinfo.PinContainer(true))
	if got, _ := DetectInstallMethod(context.Background(), h, bare); got != "docker" {
		t.Fatalf("a home docker stamp inside a container counts, got %q", got)
	}
	os.Remove(filepath.Join(h, ".install_method"))
	if err := os.Mkdir(filepath.Join(bare, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := DetectInstallMethod(context.Background(), h, bare); got != "git" {
		t.Fatalf("a container without a stamp is not docker, got %q", got)
	}
	worktree := t.TempDir()
	write(filepath.Join(worktree, ".git"), "gitdir: /src/.git/worktrees/x\n")
	if got, _ := DetectInstallMethod(context.Background(), h, worktree); got != "git" {
		t.Fatalf("a gitdir pointer file is a checkout, got %q", got)
	}
	write(filepath.Join(worktree, ".install_method"), "uv-tool\n")
	if got, _ := DetectInstallMethod(context.Background(), h, worktree); got != "git" {
		t.Fatalf("an unsupported stamp is ignored, got %q", got)
	}
	store := t.TempDir()
	prevStore := nixStore
	t.Cleanup(func() { nixStore = prevStore })
	nixStore, _ = filepath.EvalSymlinks(store)
	pkg := filepath.Join(store, "abc-icebath", "bin")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := DetectInstallMethod(context.Background(), h, pkg); got != "nix" {
		t.Fatalf("a tree under the nix store is nix, got %q", got)
	}
	if got, _ := DetectInstallMethod(context.Background(), h, store); got != "unknown" {
		t.Fatalf("the store root itself is not an install, got %q", got)
	}
	bad := t.TempDir()
	write(filepath.Join(bad, ".install_method"), "\xffgit")
	if _, err := DetectInstallMethod(context.Background(), h, bad); err == nil || !strings.Contains(err.Error(), "invalid start byte") {
		t.Fatalf("undecodable stamp must raise, got %v", err)
	}
}

func TestUpdateCommandsByMethod(t *testing.T) {
	for method, want := range map[string]string{
		"docker": "docker pull ghcr.io/nickheyer/icebath:latest", "apt": "pkg upgrade icebath-agent",
		"nix": nixUpdateMsg, "nixos": nixUpdateMsg, "home-manager": nixUpdateMsg, "git": "icebath update", "unknown": "icebath update",
		"script": "icebath update", "setup-app": "icebath update", "chocolatey": "choco upgrade icebath", "pip": "pipx upgrade icebath-cli",
		"homebrew": "brew upgrade --cask icebath", "scoop": "scoop update icebath", "winget": "winget upgrade --id nickheyer.icebath",
		"dpkg": "sudo apt update && sudo apt install --only-upgrade icebath", "rpm": "sudo dnf upgrade icebath",
		"pacman": "paru -Syu icebath-bin", "apk": "apk upgrade icebath",
	} {
		if got := RecommendedUpdateCommandForMethod(method); got != want {
			t.Fatalf("%s: got %q", method, got)
		}
	}
	if !IsNixInstallMethod("home-manager") || IsNixInstallMethod("docker") {
		t.Fatal("nix ownership")
	}
	msg := FormatDockerUpdateMessage()
	for _, want := range []string{"``icebath update`` doesn't apply inside the Docker container", "docker pull ghcr.io/nickheyer/icebath:latest",
		"docker compose up -d --force-recreate gateway dashboard", "$ICEBATH_HOME", "ICEBATH_BOT_DESKTOP=1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("docker message lacks %q", want)
		}
	}
}

func TestValidCustomProviderFieldsAcceptExtraBody(t *testing.T) {
	want := []string{"api_key", "api_mode", "base_url", "catalog_provider", "context_length", "extra_body", "key_env", "model",
		"models", "name", "rate_limit_delay", "ssl_ca_cert", "ssl_verify"}
	if got := slices.Sorted(maps.Keys(ValidCustomProviderFields)); !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

// Platform plugin manifest written under dir
func writeManifest(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// User platform plugin declaring one password secret
func userPlatformPlugin(t *testing.T, h, name, secret string) {
	writeManifest(t, filepath.Join(h, "plugins", "platforms", name),
		"name: "+name+"\nkind: platform\nrequires_env:\n  - name: "+secret+"\n    password: true\n")
}

// Registers core declared names for the platform secret readers and restores the env table afterwards
func platformTestHooks(t *testing.T, core ...string) {
	t.Helper()
	prevHooks, prevVars, prevBundled := hooks(), OptionalEnvVars.Snapshot(), bundledPlatformSecretEnvs
	names := StaticCoreEnvNames()
	for _, n := range core {
		names[n] = true
	}
	withCore := prevHooks
	withCore.CoreDeclaredEnvNames = func() map[string]bool { return names }
	RegisterConfigFacade(withCore)
	t.Cleanup(func() {
		RegisterConfigFacade(prevHooks)
		OptionalEnvVars.Restore(prevVars)
		bundledPlatformSecretEnvs = prevBundled
	})
}

func TestUserPlatformPluginsInjectEnvMetadataButNonPlatformsDoNot(t *testing.T) {
	platformTestHooks(t)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	h := t.TempDir()
	writeManifest(t, filepath.Join(h, "plugins", "demo-platform"), "name: demo-platform\nkind: platform\nlabel: Demo Platform\n"+
		"requires_env:\n  - name: DEMO_PLATFORM_TOKEN\n    description: Token for the demo platform\n"+
		"    prompt: Demo token\n    url: https://example.invalid/demo\n"+
		"optional_env:\n  - name: DEMO_PLATFORM_ROOM\n    password: false\n")
	writeManifest(t, filepath.Join(h, "plugins", "platforms", "nested-platform"), "name: nested-platform\nrequires_env:\n  - NESTED_PLATFORM_SECRET\n")
	writeManifest(t, filepath.Join(h, "plugins", "ignore-me"), "name: ignore-me\nkind: backend\nrequires_env:\n  - IGNORE_ME_TOKEN\n")
	if _, err := injectPlatformPluginEnvVars(h); err != nil {
		t.Fatal(err)
	}
	want := EnvVar{Description: "Token for the demo platform", Prompt: "Demo token", URL: "https://example.invalid/demo", Password: true, Category: "messaging"}
	if got := OptionalEnvVars.Entry("DEMO_PLATFORM_TOKEN"); got.Description != want.Description || got.Prompt != want.Prompt ||
		got.URL != want.URL || !got.Password || got.Category != want.Category {
		t.Fatalf("got %+v", got)
	}
	if OptionalEnvVars.Entry("DEMO_PLATFORM_ROOM").Password || !OptionalEnvVars.Entry("NESTED_PLATFORM_SECRET").Password {
		t.Fatal("password flags")
	}
	if OptionalEnvVars.Entry("NESTED_PLATFORM_SECRET").Description != "nested-platform configuration" {
		t.Fatalf("label falls back to the manifest name, got %q", OptionalEnvVars.Entry("NESTED_PLATFORM_SECRET").Description)
	}
	if OptionalEnvVars.Has("IGNORE_ME_TOKEN") {
		t.Fatal("a non platform flat plugin declares nothing")
	}
	names := OptionalEnvVars.Names()
	if i := slices.Index(names, "DEMO_PLATFORM_TOKEN"); i < 0 || names[i+1] != "DEMO_PLATFORM_ROOM" {
		t.Fatal("injected names keep manifest order")
	}
}

func TestUnreadablePlatformManifestFailsClosed(t *testing.T) {
	platformTestHooks(t)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	h := t.TempDir()
	userPlatformPlugin(t, h, "chatx", "CHATX_SIGNING_SECRET")
	writeManifest(t, filepath.Join(h, "plugins", "platforms", "broken"), "name: [unclosed\n")
	got, err := PlatformManifestSecretEnvs(h, PlatformSourceUser, true)
	if err != nil || !maps.Equal(got, map[string]bool{"CHATX_SIGNING_SECRET": true}) {
		t.Fatalf("got %v %v", got, err)
	}
	manifest := filepath.Join(h, "plugins", "platforms", "chatx", "plugin.yaml")
	os.Chmod(manifest, 0)
	if f, err := os.Open(manifest); err == nil {
		f.Close()
		t.Skip("running as a user that ignores file modes")
	}
	if _, err := PlatformManifestSecretEnvs(h, PlatformSourceUser, true); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("an unreadable platform manifest must raise, got %v", err)
	}
	os.Chmod(manifest, 0o644)
	locked, memx := filepath.Join(h, "plugins", "lockedx"), filepath.Join(h, "plugins", "memx")
	os.Mkdir(locked, 0o755)
	writeManifest(t, memx, "kind: memory\n")
	os.Chmod(locked, 0)
	os.Chmod(filepath.Join(memx, "plugin.yaml"), 0)
	got, err = PlatformManifestSecretEnvs(h, PlatformSourceUser, true)
	os.Chmod(locked, 0o755)
	os.Chmod(filepath.Join(memx, "plugin.yaml"), 0o644)
	if err != nil || !maps.Equal(got, map[string]bool{"CHATX_SIGNING_SECRET": true}) {
		t.Fatalf("flat plugins are not provably platforms, got %v %v", got, err)
	}
	other := t.TempDir()
	userPlatformPlugin(t, other, "chaty", "CHATY_SIGNING_SECRET")
	platforms := filepath.Join(other, "plugins", "platforms")
	os.Chmod(platforms, 0o300)
	got, err = PlatformManifestSecretEnvs(other, PlatformSourceUser, true)
	os.Chmod(platforms, 0o755)
	if err != nil || len(got) != 0 {
		t.Fatalf("an unlistable platforms root is skipped, got %v %v", got, err)
	}
	if _, complete, _ := PlatformManifestSecretScan(h); complete {
		t.Fatal("a manifest that does not parse makes the scan partial")
	}
	os.RemoveAll(filepath.Join(h, "plugins", "platforms", "broken"))
	names, complete, err := PlatformManifestSecretScan(h)
	if err != nil || !complete || !names["CHATX_SIGNING_SECRET"] {
		t.Fatalf("scan %v %v %v", names, complete, err)
	}
	os.Chmod(platforms, 0o300)
	_, complete, err = PlatformManifestSecretScan(other)
	os.Chmod(platforms, 0o755)
	if err != nil || complete {
		t.Fatalf("a skipped root makes the scan partial, got %v %v", complete, err)
	}
}

func TestManifestScanFollowsPluginDiscoveryAndSeesMtimePreservingEdits(t *testing.T) {
	platformTestHooks(t)
	h := t.TempDir()
	userPlatformPlugin(t, h, "__cache__", "CACHED_SIGNING_SECRET")
	userPlatformPlugin(t, h, "chatx", "CHATX_SIGNING_SECRET")
	got, err := PlatformManifestSecretEnvs(h, PlatformSourceUser, false)
	if err != nil || !maps.Equal(got, map[string]bool{"CHATX_SIGNING_SECRET": true}) {
		t.Fatalf("got %v %v", got, err)
	}
	manifest := filepath.Join(h, "plugins", "platforms", "chatx", "plugin.yaml")
	before := PlatformManifestStamp(h)
	st, _ := os.Stat(manifest)
	data, _ := os.ReadFile(manifest)
	os.WriteFile(manifest, []byte(strings.ReplaceAll(string(data), "CHATX_SIGNING", "CHATX_SIGNINGX")), 0o644)
	os.Chtimes(manifest, st.ModTime(), st.ModTime())
	if PlatformManifestStamp(h) == before {
		t.Fatal("an mtime preserving edit must change the stamp")
	}
}

func TestUnreadableBundledManifestFailsThePolicyInsteadOfDroppingIt(t *testing.T) {
	platformTestHooks(t, "OPENAI_API_KEY")
	bundled := t.TempDir()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	h := t.TempDir()
	writeManifest(t, filepath.Join(bundled, "platforms", "teams"), "name: teams\nrequires_env:\n  - TEAMS_CLIENT_SECRET\n  - OPENAI_API_KEY\n"+
		"optional_env:\n  - name: TEAMS_WEBHOOK_KEY\n    category: tool\n  - name: TEAMS_TENANT\n")
	secrets, err := injectPlatformPluginEnvVars(h)
	if err != nil || !maps.Equal(secrets, map[string]bool{"TEAMS_CLIENT_SECRET": true, "OPENAI_API_KEY": true}) {
		t.Fatalf("the startup read subtracts the static names only, got %v %v", secrets, err)
	}
	bundledPlatformSecretEnvs = secrets
	if got, err := PlatformManifestSecretEnvs(h, PlatformSourceBundled, true); err != nil || !maps.Equal(got, map[string]bool{"TEAMS_CLIENT_SECRET": true}) {
		t.Fatalf("got %v %v", got, err)
	}
	manifests, err := BundledPlatformManifests(h)
	if err != nil || len(manifests) != 1 || manifests[0].Name != "teams" || manifests[0].ManifestName != "teams" ||
		!slices.Equal(manifests[0].RequiredEnv, []string{"TEAMS_CLIENT_SECRET", "OPENAI_API_KEY"}) {
		t.Fatalf("manifests %+v %v", manifests, err)
	}
	target := filepath.Join(bundled, "platforms", "teams", "plugin.yaml")
	os.Chmod(target, 0)
	defer os.Chmod(target, 0o644)
	if f, err := os.Open(target); err == nil {
		f.Close()
		t.Skip("running as a user that ignores file modes")
	}
	if got, err := injectPlatformPluginEnvVars(h); err != nil || got != nil {
		t.Fatalf("an unreadable bundled manifest returns no set, got %v %v", got, err)
	}
	bundledPlatformSecretEnvs = nil
	if _, err := PlatformManifestSecretEnvs(h, PlatformSourceBundled, true); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("the strict re-read must raise, got %v", err)
	}
}

func TestPlatformManifestEnvEntriesFollowPythonUnpacking(t *testing.T) {
	entries, err := platformManifestEnvEntries(map[string]any{"requires_env": "AB", "optional_env": []any{
		map[string]any{"name": "X_TOKEN", "password": false}, map[string]any{"name": "Y_ID", "secret": true}, 7, ""}}, true)
	if err != nil || len(entries) != 4 || entries[0].name != "A" || entries[2].isSecret || !entries[3].isSecret {
		t.Fatalf("got %+v %v", entries, err)
	}
	if _, err := platformManifestEnvEntries(map[string]any{"requires_env": 5}, true); err == nil ||
		err.Error() != "Value after * must be an iterable, not int" {
		t.Fatalf("a scalar requires_env raises like Python, got %v", err)
	}
	prev := hooks()
	RegisterConfigFacade(FacadeHooks{})
	defer RegisterConfigFacade(prev)
	if _, err := manifestSecretEnvs(nil, nil); !errors.Is(err, ErrCoreEnvNamesUnregistered) {
		t.Fatalf("got %v", err)
	}
	if pyUpper("straße_key") != "STRASSE_KEY" {
		t.Fatal("full case mapping")
	}
}

func TestPlatformRootUnderAFileIsSkipped(t *testing.T) {
	platformTestHooks(t)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "plugins"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, complete, err := PlatformManifestSecretScan(h)
	if err != nil || !complete || len(names) != 0 {
		t.Fatalf("a plugins path that is a file is no plugin root, got %v %v %v", names, complete, err)
	}
}

func TestEveryPackagingStampIsAnInstallMethod(t *testing.T) {
	h := installHome(t)
	for _, method := range []string{"script", "setup-app", "chocolatey", "pip", "homebrew", "scoop", "winget", "dpkg", "rpm", "pacman", "apk", "docker", "nix", "git"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".install_method"), []byte(method+"\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := DetectInstallMethod(context.Background(), h, root); err != nil || got != method {
			t.Fatalf("%s stamp detected as %q %v", method, got, err)
		}
	}
	managed := map[string]bool{"chocolatey": true, "pip": true, "homebrew": true, "scoop": true, "winget": true, "dpkg": true, "rpm": true, "pacman": true, "apk": true, "apt": true}
	for _, method := range []string{"chocolatey", "pip", "homebrew", "scoop", "winget", "dpkg", "rpm", "pacman", "apk", "apt", "docker", "nix", "git", "script", "setup-app", "unknown"} {
		if IsPackageManagerInstallMethod(method) != managed[method] {
			t.Fatalf("%s package manager ownership", method)
		}
		if IsReleaseBinaryInstallMethod(method) != (method == "script" || method == "setup-app" || method == "unknown") {
			t.Fatalf("%s release binary ownership", method)
		}
	}
}

func TestUnstampedPackageManagerLayoutsNameTheirManager(t *testing.T) {
	h := installHome(t)
	t.Setenv("SCOOP", "")
	t.Setenv("SCOOP_GLOBAL", "")
	t.Setenv("ChocolateyInstall", "")
	base := t.TempDir()
	for rel, want := range map[string]string{
		"opt/homebrew/Caskroom/icebath/1.0":                         "homebrew",
		"AppData/Local/Microsoft/WinGet/Packages/nickheyer.icebath": "winget",
		"scoop/apps/icebath/current":                                "scoop",
		"ProgramData/chocolatey/lib/icebath/tools":                  "chocolatey",
		"home/me/.local/share/pipx/venvs/icebath-cli/bin":           "pip",
		"home/me/.local/bin":                                        "unknown",
	} {
		root := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if got, err := DetectInstallMethod(context.Background(), h, root); err != nil || got != want {
			t.Fatalf("%s detected as %q %v", rel, got, err)
		}
	}
}
