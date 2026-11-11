package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Config loader answering a fixed tree
func guiConfig(tree map[string]any) func(context.Context) (config.Tree, error) {
	return func(context.Context) (config.Tree, error) { return config.Tree(tree), nil }
}

// Launch options of a desktop config block
func guiOptions(t *testing.T, desktop map[string]any) desktopLaunchOptions {
	t.Helper()
	swapGUISteps(t)
	gui.loadConfig = guiConfig(map[string]any{"desktop": desktop})
	opts, err := readDesktopLaunchOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

// Clears every keychain variable the password store detection reads
func clearKeychainEnv(t *testing.T) {
	for _, v := range []string{"KDE_SESSION_VERSION", "KDE_FULL_SESSION", "GNOME_KEYRING_CONTROL", "ICEBATH_DESKTOP_PASSWORD_STORE"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}

// Checkout with a packaged app whose launch is recorded
func guiPackagedFixture(t *testing.T) (string, string, *[]guiRun) {
	t.Helper()
	root := guiCheckout(t, t.TempDir())
	exe := guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, e string) string { return e }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	runs := &[]guiRun{}
	gui.run = func(spec guiRun) (*guiResult, error) { *runs = append(*runs, spec); return &guiResult{}, nil }
	return root, exe, runs
}

func TestGuiRegistersLinuxDesktopEntryBeforeLaunch(t *testing.T) {
	root, _, _ := guiPackagedFixture(t)
	gui.platform = func() string { return "linux" }
	gui.registerEntry = registerLinuxDesktopEntry
	t.Setenv("DESKTOP_STARTUP_ID", "")
	var registered []string
	lde.installDesktopEntry = func(r string) (string, error) { registered = append(registered, r); return "/x/icebath.desktop", nil }
	runCmdGUI(t, guiNS(nil), "")
	if !slices.Equal(registered, []string{root}) {
		t.Fatalf("registered %v", registered)
	}
}

func TestGuiShellLaunchDefersDesktopEntryUntilWindowReveal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	root, exe, _ := guiPackagedFixture(t)
	gui.platform = func() string { return "linux" }
	gui.registerEntry = registerLinuxDesktopEntry
	t.Setenv("DESKTOP_STARTUP_ID", "gnome-shell/Icebath/1-0_TIME1")
	lde.sleep = func(d time.Duration) {}
	events := make(chan string, 4)
	lde.installDesktopEntry = func(r string) (string, error) { events <- "install:" + r; return "/x/icebath.desktop", nil }
	gui.run = func(spec guiRun) (*guiResult, error) {
		events <- "spawn"
		if spec.Env[readyFDEnvVar] != "3" || len(spec.ExtraFiles) != 1 {
			t.Errorf("ready fd %q files %d", spec.Env[readyFDEnvVar], len(spec.ExtraFiles))
		}
		_, _ = spec.ExtraFiles[0].Write([]byte("r"))
		time.Sleep(200 * time.Millisecond)
		return &guiResult{}, nil
	}
	if code, out := runCmdGUI(t, guiNS(nil), ""); code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
	close(events)
	var got []string
	for e := range events {
		got = append(got, e)
	}
	if !slices.Equal(got, []string{"spawn", "install:" + root}) {
		t.Fatalf("events %v", got)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatal(err)
	}
}

func TestGuiLaunchesEvenWhenDesktopEntryInstallFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	_, exe, runs := guiPackagedFixture(t)
	gui.platform = func() string { return "linux" }
	gui.registerEntry = registerLinuxDesktopEntry
	gui.needsDisableSetuid = desktopLinuxNeedsDisableSetuidSandbox
	t.Setenv("DESKTOP_STARTUP_ID", "")
	lde.installDesktopEntry = func(string) (string, error) { return "", errors.New("read-only /home") }
	if code, out := runCmdGUI(t, guiNS(nil), ""); code != 0 || len(*runs) != 1 || !slices.Equal((*runs)[0].Argv, []string{exe, "--disable-setuid-sandbox"}) {
		t.Fatalf("code %d runs %v out %q", code, *runs, out)
	}
}

func TestDesktopLaunchOptionsNormalizesPasswordStore(t *testing.T) {
	for raw, want := range map[any]string{"gnome-libsecret": "gnome-libsecret", "KWallet6": "kwallet6", "basic": "basic", "auto": "auto", "keychain-of-wonders": "auto", true: "auto"} {
		if got := guiOptions(t, map[string]any{"password_store": raw}).PasswordStore; got != want {
			t.Fatalf("%v got %q want %q", raw, got, want)
		}
	}
}

func TestDesktopLaunchOptionsNormalizesOzoneHint(t *testing.T) {
	for raw, want := range map[any]string{"x11": "x11", "WAYLAND": "wayland", "auto": "auto", "bogus": "auto", true: "auto"} {
		if got := guiOptions(t, map[string]any{"ozone_platform_hint": raw}).OzoneHint; got != want {
			t.Fatalf("%v got %q want %q", raw, got, want)
		}
	}
}

func TestDesktopLaunchOptionsNormalizesRendererAccessibility(t *testing.T) {
	cases := []struct {
		raw  any
		want bool
	}{{nil, true}, {true, true}, {false, false}, {"on", true}, {"1", true}, {"yes", true}, {"0", false}, {"false", false},
		{"OFF", false}, {"  no  ", false}, {"disabled", false}, {"Disabled", false}, {"enabled", true}, {0, false}, {1, true}, {0.0, false}, {"wibble", true}}
	for _, c := range cases {
		desktop := map[string]any{}
		if c.raw != nil {
			desktop["renderer_accessibility"] = c.raw
		}
		if got := guiOptions(t, desktop).RendererAccessibility; got != c.want {
			t.Fatalf("%#v got %v", c.raw, got)
		}
	}
}

// Launch environment of the default namespace
func guiEnv(t *testing.T) map[string]string {
	t.Helper()
	env, _, err := desktopLaunchEnv(context.Background(), guiNS(nil))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestDesktopEnvironmentBridgesOnlyTheAccessibilityOptOut(t *testing.T) {
	swapGUISteps(t)
	t.Setenv("ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY", "")
	os.Unsetenv("ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY")
	gui.passwordStore = func() string { return "" }
	gui.loadConfig = guiConfig(map[string]any{"desktop": map[string]any{}})
	if _, ok := guiEnv(t)["ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY"]; ok {
		t.Fatal("default bridged")
	}
	gui.loadConfig = guiConfig(map[string]any{"desktop": map[string]any{"renderer_accessibility": false}})
	if guiEnv(t)["ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY"] != "0" {
		t.Fatal("opt out not bridged")
	}
	t.Setenv("ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY", "0")
	gui.loadConfig = guiConfig(map[string]any{"desktop": map[string]any{}})
	if guiEnv(t)["ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY"] != "0" {
		t.Fatal("explicit variable lost")
	}
}

func TestDetectLinuxPasswordStorePrefersKdeSession(t *testing.T) {
	for version, want := range map[string]string{"6": "kwallet6", "5": "kwallet5", "4": "kwallet"} {
		clearKeychainEnv(t)
		t.Setenv("KDE_SESSION_VERSION", version)
		if got := detectLinuxPasswordStore(); got != want {
			t.Fatalf("%s got %q", version, got)
		}
	}
}

func TestDetectLinuxPasswordStoreKdeFullSession(t *testing.T) {
	clearKeychainEnv(t)
	t.Setenv("KDE_FULL_SESSION", "true")
	if got := detectLinuxPasswordStore(); got != "kwallet" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectLinuxPasswordStoreGnomeKeyring(t *testing.T) {
	clearKeychainEnv(t)
	t.Setenv("GNOME_KEYRING_CONTROL", "/run/user/1000/keyring")
	if got := detectLinuxPasswordStore(); got != "gnome-libsecret" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectLinuxPasswordStoreViaDbusSecretService(t *testing.T) {
	clearKeychainEnv(t)
	swapGUISteps(t)
	var argv []string
	gui.run = func(spec guiRun) (*guiResult, error) { argv = spec.Argv; return &guiResult{}, nil }
	if got := detectLinuxPasswordStore(); got != "gnome-libsecret" || !slices.Contains(argv, "--dest=org.freedesktop.secrets") {
		t.Fatalf("got %q argv %v", got, argv)
	}
}

func TestDetectLinuxPasswordStoreNoneWhenNoKeychain(t *testing.T) {
	clearKeychainEnv(t)
	swapGUISteps(t)
	gui.run = func(guiRun) (*guiResult, error) { return &guiResult{Returncode: 1}, nil }
	if got := detectLinuxPasswordStore(); got != "" {
		t.Fatalf("got %q", got)
	}
	gui.run = func(guiRun) (*guiResult, error) {
		return nil, &os.PathError{Op: "fork/exec", Path: "dbus-send", Err: os.ErrNotExist}
	}
	if got := detectLinuxPasswordStore(); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Packaged build fixture whose build step lays down the app and records the launch environment
func guiBuildingFixture(t *testing.T, sourceMode bool) (string, *[]guiRun) {
	root := guiCheckout(t, t.TempDir())
	guiLaunchFixture(t, root)
	gui.launchEnv = desktopLaunchEnv
	gui.loadConfig = guiConfig(map[string]any{})
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return true }
	gui.sourceBuildEnv = func(_ context.Context, env map[string]string, _ bool) (map[string]string, error) {
		return copyEnv(env), nil
	}
	gui.prepare = func(context.Context, string, []string, map[string]string, bool) error { return nil }
	gui.buildPrepared = func(context.Context, string, bool, string, map[string]string) (string, error) {
		if sourceMode {
			return "", nil
		}
		return guiPackagedExe(t, root), nil
	}
	gui.launchTarget = func(_ context.Context, _ string, e string) string { return e }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return false }
	runs := &[]guiRun{}
	gui.run = func(spec guiRun) (*guiResult, error) { *runs = append(*runs, spec); return &guiResult{}, nil }
	return root, runs
}

func TestGuiLinuxPackagedLaunchBridgesDetectedPasswordStore(t *testing.T) {
	clearKeychainEnv(t)
	_, runs := guiBuildingFixture(t, false)
	gui.platform = func() string { return "linux" }
	gui.passwordStore = func() string { return "gnome-libsecret" }
	runCmdGUI(t, guiNS(nil), "")
	if len(*runs) == 0 || (*runs)[len(*runs)-1].Env["ICEBATH_DESKTOP_PASSWORD_STORE"] != "gnome-libsecret" {
		t.Fatalf("runs %v", *runs)
	}
}

func TestDesktopEnvironmentPrecedence(t *testing.T) {
	cases := []struct{ explicit, configured, detected, expected string }{
		{"", "auto", "gnome-libsecret", "gnome-libsecret"}, {"", "kwallet6", "", "kwallet6"}, {"basic", "kwallet6", "", "basic"}}
	for _, c := range cases {
		clearKeychainEnv(t)
		swapGUISteps(t)
		gui.platform = func() string { return "linux" }
		t.Setenv("ELECTRON_OZONE_PLATFORM_HINT", "")
		os.Unsetenv("ELECTRON_OZONE_PLATFORM_HINT")
		gui.loadConfig = guiConfig(map[string]any{"desktop": map[string]any{"password_store": c.configured, "ozone_platform_hint": "x11"}})
		gui.passwordStore = func() string {
			if c.configured != "auto" || c.explicit != "" {
				t.Fatal("detection ran over an explicit or configured store")
			}
			return c.detected
		}
		if c.explicit != "" {
			t.Setenv("ICEBATH_DESKTOP_PASSWORD_STORE", c.explicit)
		}
		env := guiEnv(t)
		if env["ICEBATH_DESKTOP_PASSWORD_STORE"] != c.expected || env["ELECTRON_OZONE_PLATFORM_HINT"] != "x11" {
			t.Fatalf("%+v env store %q hint %q", c, env["ICEBATH_DESKTOP_PASSWORD_STORE"], env["ELECTRON_OZONE_PLATFORM_HINT"])
		}
		t.Setenv("ELECTRON_OZONE_PLATFORM_HINT", "wayland")
		if hint, present := guiEnv(t)["ELECTRON_OZONE_PLATFORM_HINT"]; present && hint != "wayland" {
			t.Fatalf("hint %q", hint)
		}
	}
}

func TestGuiLinuxSourceLaunchBridgesDetectedPasswordStore(t *testing.T) {
	clearKeychainEnv(t)
	root, runs := guiBuildingFixture(t, true)
	gui.platform = func() string { return "linux" }
	electron := filepath.Join(root, "node_modules", "electron")
	writeTestFile(t, filepath.Join(electron, "path.txt"), "electron", 0o644)
	writeTestFile(t, filepath.Join(electron, "dist", "electron"), "", 0o755)
	gui.passwordStore = func() string { return "kwallet6" }
	runCmdGUI(t, guiNS(map[string]any{"source": true}), "")
	last := (*runs)[len(*runs)-1]
	if !slices.Equal(last.Argv, []string{filepath.Join(electron, "dist", "electron"), "."}) || last.Env["ICEBATH_DESKTOP_PASSWORD_STORE"] != "kwallet6" {
		t.Fatalf("last %v", last)
	}
}

func TestGuiPasswordStoreBridgeIsLinuxOnly(t *testing.T) {
	clearKeychainEnv(t)
	_, runs := guiBuildingFixture(t, false)
	gui.platform = func() string { return "darwin" }
	gui.passwordStore = func() string { t.Fatal("detection ran off Linux"); return "" }
	runCmdGUI(t, guiNS(nil), "")
	if _, ok := (*runs)[len(*runs)-1].Env["ICEBATH_DESKTOP_PASSWORD_STORE"]; ok {
		t.Fatal("password store bridged off Linux")
	}
}

// Packaged fixture on a faked Windows host whose detached spawns are recorded and answered by errs in turn
func guiWin32Fixture(t *testing.T, errs ...error) (string, *[]guiRun) {
	_, exe, runs := guiPackagedFixture(t)
	gui.platform = func() string { return "win32" }
	gui.loadConfig = guiConfig(map[string]any{})
	popens := &[]guiRun{}
	gui.popen = func(spec guiRun) (int, error) {
		*popens = append(*popens, spec)
		if len(errs) > 0 {
			err := errs[0]
			errs = errs[1:]
			return 0, err
		}
		return 1, nil
	}
	gui.run = func(spec guiRun) (*guiResult, error) {
		*runs = append(*runs, spec)
		return &guiResult{}, nil
	}
	return exe, popens
}

func TestGuiWin32LaunchesDetachedAndReturns(t *testing.T) {
	exe, popens := guiWin32Fixture(t)
	code, _ := runCmdGUI(t, guiNS(map[string]any{"skip_build": true}), "")
	if code != 0 || len(*popens) != 1 {
		t.Fatalf("code %d popens %v", code, *popens)
	}
	spec := (*popens)[0]
	if spec.Argv[0] != exe || !reflect.DeepEqual(spec.Attr, creationFlagsAttr(windowsDetachFlags())) ||
		spec.Stdin != streamDevnull || spec.Stdout != streamDevnull || spec.Stderr != streamDevnull {
		t.Fatalf("spec %+v", spec)
	}
}

func TestGuiWin32DetachFallsBackWithoutBreakaway(t *testing.T) {
	_, popens := guiWin32Fixture(t, &os.SyscallError{Syscall: "CreateProcess", Err: syscall.Errno(5)})
	code, _ := runCmdGUI(t, guiNS(map[string]any{"skip_build": true}), "")
	if code != 0 || len(*popens) != 2 || !reflect.DeepEqual((*popens)[0].Attr, creationFlagsAttr(windowsDetachFlags())) ||
		!reflect.DeepEqual((*popens)[1].Attr, creationFlagsAttr(windowsDetachFlagsWithoutBreakaway())) {
		t.Fatalf("code %d popens %v", code, *popens)
	}
}

func TestGuiWin32DetachReraisesNonBreakawayOserror(t *testing.T) {
	_, popens := guiWin32Fixture(t, &os.SyscallError{Syscall: "CreateProcess", Err: syscall.Errno(2)})
	_, err := CmdGUI(context.Background(), guiNS(map[string]any{"skip_build": true}), "", io.Discard)
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != 2 || len(*popens) != 1 {
		t.Fatalf("err %v popens %d", err, len(*popens))
	}
}
