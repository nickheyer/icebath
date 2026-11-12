package startupfast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Text of a small utf-8-sig file with universal newlines, false when it is missing, unreadable or undecodable
func readText(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text, err := pycodecs.Decode(data, "utf-8-sig")
	if err != nil {
		return "", false
	}
	return pystr.TranslateNewlines(text), true
}

// Reports whether the path exists the way os.path.exists does, any stat failure counting as absent
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Inside a container already by the docker or podman env files or the init cgroup, where the fast path is safe
func isContainerStartupEnvironment() bool {
	if pathExists("/.dockerenv") || pathExists("/run/.containerenv") {
		return true
	}
	cgroup, _ := readText("/proc/1/cgroup")
	return strings.Contains(cgroup, "docker") || strings.Contains(cgroup, "podman") || strings.Contains(cgroup, "/lxc/")
}

// Cheap probe for an active non default profile redirecting the home
func activeProfileMayOverrideHome(root string) bool {
	text, _ := readText(pypath.Join(root, "active_profile"))
	active := pystr.Strip(text)
	return active != "" && active != "default"
}

// Conservative probe for container mode routing, any profile ambiguity meaning it may be active
func ContainerModeMayBeActive() bool {
	if os.Getenv("ICEBATH_DEV") == "1" || isContainerStartupEnvironment() {
		return false
	}
	if h := pystr.Strip(os.Getenv(home.EnvHome)); h != "" {
		if pathExists(pypath.Join(h, ".container-mode")) {
			return true
		}
		parent := pypath.Basename(pypath.Dirname(pypath.Normpath(h)))
		return parent != home.ProfilesDir && activeProfileMayOverrideHome(h)
	}
	def := home.PlatformDefault()
	return activeProfileMayOverrideHome(def) || pathExists(pypath.Join(def, ".container-mode"))
}

// The canonical icebath --version output, also behind the /version command, the update status line when asked
func PrintFastVersionInfo(ctx context.Context, w io.Writer, checkUpdates bool) {
	label, err := app.BannerVersionLabel(ctx)
	if err != nil {
		info, verr := updaterecord.GetVersionInfo()
		if verr != nil {
			panic(verr)
		}
		label = fmt.Sprintf("Icebath Agent v%s (%s)", info.DerivedVersion, app.ReleaseDate)
	}
	fmt.Fprintln(w, label)
	root := pm.RepoRoot()
	fmt.Fprintln(w, "Install directory: "+root)
	method, err := config.DetectInstallMethod(ctx, home.Dir(ctx), root)
	if err != nil {
		method = readInstallMethod()
	}
	if method != "" {
		fmt.Fprintln(w, "Install method: "+method)
	}
	fmt.Fprintln(w, "Go: "+hostinfo.GoVersion())
	if checkUpdates {
		printUpdateStatus(ctx, w, root)
	}
}

// Passive update check line printed when the status names one
func printUpdateStatus(ctx context.Context, w io.Writer, root string) {
	status, err := selfupdate.CheckForUpdates(ctx, selfupdate.SourceCheckOptions{InstallRoot: root, Home: home.Dir(ctx), Passive: true})
	if err != nil || status == nil {
		return
	}
	command := func() (string, error) { return config.RecommendedUpdateCommand(ctx, home.Dir(ctx)) }
	if line, ok := updateStatusLine(status, command); ok {
		fmt.Fprintln(w, line)
	}
}

// Update status line, rollback before the behind count, none when no branch matches or the command lookup fails
func updateStatusLine(status *pyjson.Object, command func() (string, error)) (string, bool) {
	behind, _ := status.Get("behind")
	rollback, _ := status.Get("rollback")
	switch {
	case pyrepr.Truthy(rollback):
		sourceVersion, _ := status.Get("sourceVersion")
		cmd, err := command()
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("Newer than stable release v%s — '%s' moves back to it", pyrepr.Str(sourceVersion), cmd), true
	case pyrepr.Equal(behind, selfupdate.UpdateAvailableNoCount):
		cmd, err := command()
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("Update available — run '%s'", cmd), true
	case pyrepr.Truthy(behind):
		positive, comparable := pyPositive(behind)
		if !comparable || !positive {
			return "", false
		}
		word := "commits"
		if pyrepr.Equal(behind, 1) {
			word = "commit"
		}
		cmd, err := command()
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("Update available: %s %s behind — run '%s'", pyrepr.Str(behind), word, cmd), true
	case pyrepr.Equal(behind, 0):
		return "Up to date", true
	}
	return "", false
}

// Python value greater than zero for a bool, int or float, comparable false where the comparison raises TypeError
func pyPositive(v any) (positive, comparable bool) {
	switch n := v.(type) {
	case bool:
		return n, true
	case float64:
		return n > 0, true
	case float32:
		return n > 0, true
	case json.Number:
		if f, err := n.Float64(); err == nil && strings.ContainsAny(string(n), ".eE") {
			return f > 0, true
		}
	}
	if i, ok := pyrepr.Int(v); ok {
		return i.Sign() > 0, true
	}
	return false, false
}

// The installer's .install_method stamp in the resolved home, lower cased and empty when absent
func readInstallMethod() string {
	h := pystr.Strip(os.Getenv(home.EnvHome))
	if h == "" {
		h = home.PlatformDefault()
	}
	text, _ := readText(pypath.Join(h, ".install_method"))
	return pystr.Lower(pystr.Strip(text))
}
