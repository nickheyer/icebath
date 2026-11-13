package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/logging"
)

var (
	linuxPasswordStores = []string{"gnome-libsecret", "kwallet", "kwallet5", "kwallet6", "basic"}
	gpuFlagWords        = map[string]string{"1": "1", "true": "1", "yes": "1", "on": "1", "0": "0", "false": "0", "no": "0", "off": "0"}
	wslDXGDevice        = "/dev/dxg"
	wslD3D12Drivers     = []string{
		"/usr/lib/x86_64-linux-gnu/dri/d3d12_dri.so",
		"/usr/lib/aarch64-linux-gnu/dri/d3d12_dri.so",
		"/usr/lib64/dri/d3d12_dri.so",
		"/usr/lib/dri/d3d12_dri.so",
	}
	mesaDriverOverrides = []string{"GALLIUM_DRIVER", "MESA_LOADER_DRIVER_OVERRIDE", "LIBGL_ALWAYS_SOFTWARE", "LIBGL_DRIVERS_PATH"}
)

// Launches the Wails desktop app, installing the release artifact first when this machine has none
func CmdGUI(ctx context.Context, ns map[string]any, explicitProfile string, w io.Writer) (int, error) {
	_, _ = logging.SetupLogging(ctx, logging.Options{Mode: "gui"})
	exe, err := resolveDesktopExecutable(ctx, w)
	if err != nil {
		fmt.Fprintf(w, "✗ Could not install Icebath Desktop: %s\n", err)
		return 1, nil
	}
	env, err := desktopLaunchEnv(ctx, ns)
	if err != nil {
		return 0, err
	}
	return runDesktop(w, exe, desktopLaunchArgs(ns, explicitProfile), env)
}

// Desktop executable beside this binary, in the install location, in a checkout build or on PATH, else freshly installed
func resolveDesktopExecutable(ctx context.Context, w io.Writer) (string, error) {
	name := install.DesktopExecutableName(runtime.GOOS)
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		dir := filepath.Dir(self)
		for _, candidate := range []string{filepath.Join(dir, name), install.DesktopExecutableIn(dir, runtime.GOOS)} {
			if isDesktopExecutable(candidate) {
				return candidate, nil
			}
		}
	}
	if exe := install.ResolveDesktopExe(install.InstallRootOf(install.Home())); exe != "" {
		return exe, nil
	}
	if root := install.SourceCheckoutRoot(); root != "" {
		if exe := install.CheckoutDesktopExe(root); exe != "" {
			return exe, nil
		}
	}
	if exe, err := exec.LookPath(name); err == nil {
		return exe, nil
	}
	tag := install.RunningReleaseTag()
	release := tag
	if release == "" {
		release = "the latest stable release"
	}
	fmt.Fprintf(w, "→ Icebath Desktop is not installed, installing it from %s\n", release)
	exe, err := install.InstallDesktop(ctx, tag, w)
	if err != nil {
		return "", err
	}
	if exe == "" {
		return "", fmt.Errorf("the install left no %s under %s", name, install.DesktopReleaseDir(install.InstallRootOf(install.Home())))
	}
	return exe, nil
}

// Reports a regular file this process may execute
func isDesktopExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && accessExecutable(p)
}

// Command line flags the desktop app reads from the launch
func desktopLaunchArgs(ns map[string]any, explicitProfile string) []string {
	var args []string
	if nsTruthy(ns, "local") {
		args = append(args, "--local")
	}
	if nsTruthy(ns, "close_preview") {
		args = append(args, "--close-preview")
	}
	if explicitProfile != "" {
		args = append(args, "--profile", explicitProfile)
	}
	return args
}

// Variables the desktop app reads, from the flags and the desktop config block, an explicit variable winning over config
func desktopLaunchEnv(ctx context.Context, ns map[string]any) ([]string, error) {
	var env []string
	set := func(key, value string) { env = append(env, key+"="+value) }
	if nsTruthy(ns, "fake_boot") {
		set("ICEBATH_DESKTOP_BOOT_FAKE", "1")
	}
	if nsTruthy(ns, "ignore_existing") {
		set("ICEBATH_DESKTOP_IGNORE_EXISTING", "1")
	}
	if root := nsText(ns, "icebath_root"); root != "" {
		set("ICEBATH_DESKTOP_ICEBATH_ROOT", expandResolve(root))
	}
	if cwd := nsText(ns, "cwd"); cwd != "" {
		set("ICEBATH_DESKTOP_CWD", expandResolve(cwd))
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		set("ICEBATH_DESKTOP_CWD", wd)
	}
	if runtime.GOOS == "linux" && preferWSLD3D12() {
		set("GALLIUM_DRIVER", "d3d12")
	}
	disableGPU, passwordStore := readDesktopLaunchOptions(ctx)
	if _, explicit := os.LookupEnv("ICEBATH_DESKTOP_DISABLE_GPU"); disableGPU != "auto" && !explicit {
		set("ICEBATH_DESKTOP_DISABLE_GPU", disableGPU)
	}
	if _, explicit := os.LookupEnv("ICEBATH_DESKTOP_PASSWORD_STORE"); runtime.GOOS == "linux" && !explicit {
		if passwordStore == "auto" {
			passwordStore = detectLinuxPasswordStore()
		}
		if passwordStore != "" {
			set("ICEBATH_DESKTOP_PASSWORD_STORE", passwordStore)
		}
	}
	return env, nil
}

// GPU switch and Linux password store of the desktop config block, auto for a missing or unknown value
func readDesktopLaunchOptions(ctx context.Context) (string, string) {
	disableGPU, passwordStore := "auto", "auto"
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return disableGPU, passwordStore
	}
	desktopCfg, _ := cfg["desktop"].(map[string]any)
	switch v := desktopCfg["disable_gpu"].(type) {
	case bool:
		disableGPU = "0"
		if v {
			disableGPU = "1"
		}
	case string:
		if word, ok := gpuFlagWords[strings.ToLower(strings.TrimSpace(v))]; ok {
			disableGPU = word
		}
	}
	if store, ok := desktopCfg["password_store"].(string); ok {
		if low := strings.ToLower(strings.TrimSpace(store)); slices.Contains(linuxPasswordStores, low) {
			passwordStore = low
		}
	}
	return disableGPU, passwordStore
}

// Password store backend of this Linux session from KDE, GNOME Keyring or a secrets D-Bus ping, empty when none answers
func detectLinuxPasswordStore() string {
	if kde := strings.TrimSpace(os.Getenv("KDE_SESSION_VERSION")); kde != "" {
		switch kde {
		case "6":
			return "kwallet6"
		case "5":
			return "kwallet5"
		}
		return "kwallet"
	}
	if os.Getenv("KDE_FULL_SESSION") != "" {
		return "kwallet"
	}
	if os.Getenv("GNOME_KEYRING_CONTROL") != "" {
		return "gnome-libsecret"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ping := exec.CommandContext(ctx, "dbus-send", "--session", "--print-reply", "--reply-timeout=2000",
		"--dest=org.freedesktop.secrets", "/org/freedesktop/secrets", "org.freedesktop.DBus.Peer.Ping")
	if ping.Run() == nil {
		return "gnome-libsecret"
	}
	return ""
}

// True under WSLg with the DXG device and a d3d12 Mesa driver present and no Mesa driver chosen
func preferWSLD3D12() bool {
	for _, key := range mesaDriverOverrides {
		if _, ok := os.LookupEnv(key); ok {
			return false
		}
	}
	if !hostinfo.IsWSL() {
		return false
	}
	if _, err := os.Stat(wslDXGDevice); err != nil {
		return false
	}
	for _, driver := range wslD3D12Drivers {
		if st, err := os.Stat(driver); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// Absolute form of a user supplied path with a leading tilde expanded and links resolved where it exists
func expandResolve(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if userHome, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(userHome, p[1:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
