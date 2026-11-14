package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Env over a fresh home with the real user config dirs out of reach
func cuCLIEnv(t *testing.T) *tools.Env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	return &tools.Env{Home: dir}
}

// Parses argv after computer-use through the leaf flags of its path and runs the dispatcher
func cuInvoke(t *testing.T, env *tools.Env, argv ...string) (int, string, []string) {
	t.Helper()
	path := []string{"computer-use"}
	rest := argv
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		path, rest = append(path, rest[0]), rest[1:]
	}
	fs := pflag.NewFlagSet(strings.Join(path, " "), pflag.ContinueOnError)
	switch strings.Join(path, " ") {
	case "computer-use install":
		fs.Bool("upgrade", false, "")
	case "computer-use doctor":
		fs.StringArray("include", nil, "")
		fs.StringArray("skip", nil, "")
		fs.Bool("json", false, "")
	case "computer-use permissions status", "computer-use screen status":
		fs.Bool("json", false, "")
	case "computer-use screen stop":
		fs.Bool("force", false, "")
	case "computer-use screen install":
		fs.BoolP("yes", "y", false, "")
	}
	if err := fs.Parse(rest); err != nil {
		t.Fatal(err)
	}
	ns, err := ParseComputerUseArgs(strings.Join(path, " "), fs)
	if err != nil {
		t.Fatal(err)
	}
	con, out := cuSetupConsole()
	var helped []string
	cli := &ComputerUseCLI{Env: env, Console: con, Stderr: out, PrintHelp: func(p string) error {
		helped = append(helped, p)
		return nil
	}}
	rc, err := BuildComputerUseParser()(context.Background(), cli, ns)
	if err != nil {
		t.Fatal(err)
	}
	return rc, out.String(), helped
}

// PM selection answering one binary, nil for none
func cuPMBinary(t *testing.T, binary string) {
	t.Helper()
	cuSwap(t, &pmInstalled, func(context.Context, string, bool) (*pm.InstalledPackage, error) {
		if binary == "" {
			return nil, nil
		}
		return &pm.InstalledPackage{Binary: binary}, nil
	})
}

func TestComputerUseStatusReportsPMWithoutPollingVendor(t *testing.T) {
	env := cuCLIEnv(t)
	t.Setenv(cuaDriverCmdEnv, "")
	cuPMBinary(t, filepath.Join(t.TempDir(), "cua-driver"))
	cuFixedContract(t, map[string]any{"ready": true, "version": "0.20.0"})
	rc, out, _ := cuInvoke(t, env, "status")
	if rc != 0 || !strings.Contains(out, "Icebath PM") || strings.Contains(out, "latest") {
		t.Fatalf("rc %d output %q", rc, out)
	}
}

func TestComputerUseStatusReturnsNonzeroWhenDriverIsMissing(t *testing.T) {
	env := cuCLIEnv(t)
	t.Setenv(cuaDriverCmdEnv, "")
	cuPMBinary(t, "")
	rc, out, _ := cuInvoke(t, env, "status")
	if rc != 1 || !strings.Contains(out, "cua-driver: not installed") {
		t.Fatalf("rc %d output %q", rc, out)
	}
}

func TestComputerUseStatusReportsUnusableDriver(t *testing.T) {
	for _, override := range []bool{false, true} {
		env := cuCLIEnv(t)
		driver := filepath.Join(t.TempDir(), "cua-driver")
		cuWriteDriver(t, driver)
		if override {
			t.Setenv(cuaDriverCmdEnv, driver)
		} else {
			t.Setenv(cuaDriverCmdEnv, "")
		}
		cuPMBinary(t, driver)
		cuFixedContract(t, map[string]any{"ready": false, "reason": "manifest is invalid"})
		rc, out, _ := cuInvoke(t, env, "status")
		if rc != 1 || !strings.Contains(out, "Repair required") {
			t.Fatalf("override %v rc %d output %q", override, rc, out)
		}
		if override && (!strings.Contains(out, "custom binary from ICEBATH_CUA_DRIVER_CMD") || !strings.Contains(out, "unset the override")) {
			t.Fatalf("override output %q", out)
		}
		if !override && !strings.Contains(out, "Run: icebath computer-use install") {
			t.Fatalf("pm output %q", out)
		}
	}
}

func TestComputerUseInstallPropagatesSetupResult(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, upgrade := range []bool{false, true} {
			env := cuCLIEnv(t)
			calls := 0
			cuSwap(t, &installCuaDriver, func(_ context.Context, _ *tools.Env, _ *cliui.Console, progress bool) (bool, error) {
				calls++
				if !progress {
					t.Fatal("the install command shows installer progress")
				}
				return ready, nil
			})
			argv := []string{"install"}
			if upgrade {
				argv = append(argv, "--upgrade")
			}
			want := 1
			if ready {
				want = 0
			}
			if rc, _, _ := cuInvoke(t, env, argv...); rc != want || calls != 1 {
				t.Fatalf("ready %v upgrade %v: rc %d calls %d", ready, upgrade, rc, calls)
			}
		}
	}
}

func TestPermissionsStatusNamesTheStaleTCCRowForTheMissingGrant(t *testing.T) {
	env := cuCLIEnv(t)
	st := pyjson.NewObject()
	for _, kv := range []struct {
		k string
		v any
	}{{"platform", "darwin"}, {"platform_supported", true}, {"installed", true}, {"version", "cua-driver 0.28.2"},
		{"ready", false}, {"can_grant", true}, {"checks", []any{}}, {"source", nil}, {"error", nil},
		{"accessibility", false}, {"screen_recording", true}, {"screen_recording_capturable", true}} {
		st.Set(kv.k, kv.v)
	}
	cuSwap(t, &cliComputerUseStatus, func(*tools.Env, *string) (*pyjson.Object, error) { return st, nil })
	rc, out, _ := cuInvoke(t, env, "permissions", "status")
	if rc != 1 || !strings.Contains(out, "tccutil reset Accessibility com.trycua.driver") || strings.Contains(out, "ScreenCapture") ||
		!strings.Contains(out, "icebath computer-use permissions grant") {
		t.Fatalf("rc %d output %q", rc, out)
	}
}

func TestComputerUseNoActionPrintsHelp(t *testing.T) {
	env := cuCLIEnv(t)
	for _, c := range []struct {
		argv []string
		help string
	}{{nil, "computer-use"}, {[]string{"permissions"}, "computer-use permissions"}, {[]string{"screen"}, "computer-use screen"}} {
		rc, _, helped := cuInvoke(t, env, c.argv...)
		if rc != 0 || !slices.Equal(helped, []string{c.help}) {
			t.Fatalf("%v: rc %d help %v", c.argv, rc, helped)
		}
	}
}

func TestScreenStopHandsBackEvenWhenTheDesktopHasAlreadyExited(t *testing.T) {
	env := cuCLIEnv(t)
	key := botdesktop.ProfileKey(env)
	if _, err := botdesktop.LeaseAcquire("disconnected-viewer", key, ""); err != nil {
		t.Fatal(err)
	}
	if rc, out, _ := cuInvoke(t, env, "screen", "stop", "--force"); rc != 0 {
		t.Fatalf("rc %d output %q", rc, out)
	}
	if h := botdesktop.LeaseGet(key).Holder; h != botdesktop.LeaseAgent {
		t.Fatalf("holder %s", h)
	}
}

func TestScreenStopRefusesWhileAHumanHoldsUnlessForced(t *testing.T) {
	env := cuCLIEnv(t)
	stopped := 0
	cuSwap(t, &screenStop, func(*tools.Env, func() (bool, error)) (bool, error) { stopped++; return true, nil })
	key := botdesktop.ProfileKey(env)
	if _, err := botdesktop.LeaseAcquire("human-at-keyboard", key, ""); err != nil {
		t.Fatal(err)
	}
	if rc, _, _ := cuInvoke(t, env, "screen", "stop"); rc != 1 || botdesktop.LeaseGet(key).Holder != botdesktop.LeaseHuman || stopped != 0 {
		t.Fatalf("unforced stop rc %d stopped %d", rc, stopped)
	}
	if rc, _, _ := cuInvoke(t, env, "screen", "stop", "--force"); rc != 0 || botdesktop.LeaseGet(key).Holder != botdesktop.LeaseAgent || stopped != 1 {
		t.Fatalf("forced stop rc %d stopped %d", rc, stopped)
	}
}

func TestStatusNeverPrintsTheHoldersRawViewerID(t *testing.T) {
	env := cuCLIEnv(t)
	pid, display, socket := 4242, ":20", "/x/rfb.sock"
	cuSwap(t, &screenStatus, func(*tools.Env, string) (botdesktop.DesktopStatus, error) {
		return botdesktop.DesktopStatus{Profile: "default", Supported: true, Installed: true, Missing: []string{}, Running: true,
			PID: &pid, Display: &display, Socket: &socket, Geometry: "1440x900"}, nil
	})
	secret := "secret-viewer-token-ABC123"
	key := botdesktop.ProfileKey(env)
	if _, err := botdesktop.LeaseAcquire(secret, key, "typing a password"); err != nil {
		t.Fatal(err)
	}
	rpcView, err := pyjson.Dumps(botdesktop.LeasePublicView(botdesktop.LeaseGet(key)), pyjson.SortKeys(true))
	if err != nil || strings.Contains(rpcView, secret) {
		t.Fatalf("rpc view %q err %v", rpcView, err)
	}
	rc, out, _ := cuInvoke(t, env, "screen", "status", "--json")
	if rc != 0 || strings.Contains(out, secret) {
		t.Fatalf("rc %d json %q", rc, out)
	}
	payload, err := pyjson.Loads(out)
	if err != nil {
		t.Fatal(err)
	}
	lease := cuGet(payload.(*pyjson.Object), "lease")
	got, _ := pyjson.Dumps(lease, pyjson.SortKeys(true))
	sum := sha256.Sum256([]byte(secret))
	if got != rpcView || cuGet(lease.(*pyjson.Object), "viewer_hash") != hex.EncodeToString(sum[:])[:12] {
		t.Fatalf("lease %s rpc %s", got, rpcView)
	}
	rc, human, _ := cuInvoke(t, env, "screen", "status")
	if rc != 0 || strings.Contains(human, secret) || !strings.Contains(human, "control: human") {
		t.Fatalf("rc %d human %q", rc, human)
	}
}

func TestCLIInstallGoesThroughTheSharedInstaller(t *testing.T) {
	env := cuCLIEnv(t)
	var calls [][2]bool
	cuSwap(t, &screenIsSupportedHost, func() bool { return true })
	cuSwap(t, &screenInstallCommand, func() string { return "sudo apt-get install -y tigervnc-standalone-server" })
	cuSwap(t, &screenMissingBinaries, func() []string {
		if len(calls) == 0 {
			return []string{"Xvnc"}
		}
		return []string{}
	})
	cuSwap(t, &runQuiet, func([]string, float64, runOpts) (*completedProcess, error) {
		t.Fatal("shell install spawned")
		return nil, nil
	})
	cuSwap(t, &BotDesktopInstallPackages, func(_ *tools.Env, ask func() (string, error), onLine func(string)) (int, error) {
		calls = append(calls, [2]bool{ask != nil, onLine != nil})
		return 0, nil
	})
	if rc, out, _ := cuInvoke(t, env, "screen", "install", "-y"); rc != 0 || !slices.Equal(calls, [][2]bool{{true, true}}) {
		t.Fatalf("rc %d calls %v output %q", rc, calls, out)
	}
}
