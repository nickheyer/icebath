package gateway

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Seams over the host facts the posture audit reads, replaced by tests the way hermes tests monkeypatch the module
var (
	auditIsRoot               = auditIsRootLive
	auditIterSshdConfigLines  = auditIterSshdConfigLinesLive
	auditInContainer          = auditInContainerLive
	auditRanMu                sync.Mutex
	auditRan                  bool
	auditPasswordAuthDirectRe = regexp.MustCompile(`(?i)^PasswordAuthentication\s+(\w+)`)
)

// Records of the posture audit carry its upstream logger name
func auditLog() *slog.Logger { return slog.Default().With("logger", "hermes.security_audit") }

// True when the process runs as POSIX uid 0, never on Windows
func auditIsRootLive() bool { return os.Geteuid() == 0 }

// Finding for a root process, empty when not root
func runningAsRoot() string {
	if !auditIsRoot() {
		return ""
	}
	return "Running as ROOT. The agent's terminal/file tools execute with full root privileges \u2014 a single " +
		"prompt-injection or exposed endpoint is a full host compromise. Run Hermes as an unprivileged user " +
		"(or in a sandboxed terminal backend / container with a non-root user)."
}

// Non comment lines of sshd_config and its drop in directory
func auditIterSshdConfigLinesLive() []string {
	paths := []string{"/etc/ssh/sshd_config"}
	if matches, err := pypath.Glob("/etc/ssh/sshd_config.d", "*.conf"); err == nil {
		slices.Sort(matches)
		paths = append(paths, matches...)
	}
	var lines []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text := strings.TrimPrefix(strings.ToValidUTF8(string(data), "\ufffd"), "\ufeff")
		for _, raw := range pystr.Splitlines(text, false) {
			s := strings.TrimSpace(raw)
			if s != "" && !strings.HasPrefix(s, "#") {
				lines = append(lines, s)
			}
		}
	}
	return lines
}

// Finding when sshd allows password authentication, empty without an sshd config or with it off
func sshPasswordAuthEnabled() string {
	lines := auditIterSshdConfigLines()
	if len(lines) == 0 {
		return ""
	}
	var directives []string
	for _, line := range lines {
		if m := auditPasswordAuthDirectRe.FindStringSubmatch(line); m != nil {
			directives = append(directives, strings.ToLower(m[1]))
		}
	}
	if len(directives) > 0 && directives[len(directives)-1] == "no" {
		return ""
	}
	qualifier := " (default \u2014 no explicit directive)"
	if len(directives) > 0 {
		qualifier = ""
	}
	return fmt.Sprintf("SSH password authentication is ENABLED%s. Password auth is brute-forceable and dangerous "+
		"on an internet-facing box. Set 'PasswordAuthentication no' in sshd_config and use key-based auth.", qualifier)
}

// Whether the process runs inside a Docker, Podman or other OCI container
func auditInContainerLive() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("ICEBATH_DESKTOP_CHILD_PID") != "" {
		return false
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	cgroup := string(data)
	for _, tok := range []string{"docker", "containerd", "kubepods", "libpod"} {
		if strings.Contains(cgroup, tok) {
			return true
		}
	}
	return false
}

// Whether path sits on a persistent mount, true when the mount table cannot tell
func pathIsMounted(path string) bool {
	target := path
	if resolved, err := pypath.Realpath(path, pypath.Lax); err == nil {
		target = resolved
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return true
	}
	type covering struct{ mountpoint, fstype string }
	var found []covering
	parents := map[string]bool{}
	for _, p := range pypath.PathParents(target) {
		parents[p] = true
	}
	for _, line := range pystr.Splitlines(strings.ToValidUTF8(string(data), "\ufffd"), false) {
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		mount := filepath.Clean(parts[1])
		if mount == target || parents[mount] {
			found = append(found, covering{mount, parts[2]})
		}
	}
	if len(found) == 0 {
		return true
	}
	best := found[0]
	for _, entry := range found[1:] {
		if cmp.Compare(len(entry.mountpoint), len(best.mountpoint)) > 0 {
			best = entry
		}
	}
	return best.fstype != "overlay" && best.fstype != "tmpfs" && best.fstype != "aufs"
}

// Finding for a container whose data home is not on a persistent volume
func containerNoVolumeMount(ctx context.Context, dataHome string) string {
	if !auditInContainer() {
		return ""
	}
	if dataHome == "" {
		dataHome = home.Dir(ctx)
	}
	if pathIsMounted(dataHome) {
		return ""
	}
	return fmt.Sprintf("Running in a container but the data dir (%s) is NOT on a persistent volume mount \u2014 "+
		"sessions, memory, skills, and API keys are ephemeral and lost on container restart. Mount a host "+
		"volume over the ICEBATH_HOME data directory.", dataHome)
}

// Finding for an enabled API server reachable off loopback with no API_SERVER_KEY
func networkListenerWithoutAuth(ctx context.Context, cfg map[string]any) []string {
	plats, _ := cfg["platforms"].(map[string]any)
	api, ok := plats["api_server"].(map[string]any)
	if !ok || !pyrepr.Truthy(api["enabled"]) {
		return nil
	}
	extra, _ := api["extra"].(map[string]any)
	host := any(envDefault("API_SERVER_HOST", "127.0.0.1"))
	if v := extra["host"]; pyrepr.Truthy(v) {
		host = v
	}
	key := any(envDefault("API_SERVER_KEY", ""))
	if v := extra["key"]; pyrepr.Truthy(v) {
		key = v
	}
	if !IsNetworkAccessible(ctx, pyrepr.Str(host)) || strings.TrimSpace(pyrepr.Str(key)) != "" {
		return nil
	}
	return []string{fmt.Sprintf("OpenAI-compatible API server is network-accessible (%s) with NO API_SERVER_KEY. It dispatches "+
		"terminal-capable agent work \u2014 an unauthenticated network endpoint is remote code execution. "+
		"Set a strong API_SERVER_KEY.", pyrepr.Str(host))}
}

// Environment value of name, fallback when unset
func envDefault(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

// Every posture finding as readable text, a check that panics contributing nothing
func RunSecurityAudit(ctx context.Context, dataHome string, cfg map[string]any) []string {
	findings := []string{}
	checks := []func() []string{
		func() []string { return auditFinding(runningAsRoot()) },
		func() []string { return auditFinding(sshPasswordAuthEnabled()) },
		func() []string { return auditFinding(containerNoVolumeMount(ctx, dataHome)) },
		func() []string { return networkListenerWithoutAuth(ctx, cfg) },
	}
	for _, check := range checks {
		func() {
			defer func() { _ = recover() }()
			findings = append(findings, check()...)
		}()
	}
	return findings
}

// One element slice of a non empty finding
func auditFinding(finding string) []string {
	if finding == "" {
		return nil
	}
	return []string{finding}
}

// Runs the audit once per process unless forced and logs each finding as a warning
func LogStartupSecurityWarnings(ctx context.Context, dataHome string, cfg map[string]any, force bool) []string {
	auditRanMu.Lock()
	if auditRan && !force {
		auditRanMu.Unlock()
		return []string{}
	}
	auditRan = true
	auditRanMu.Unlock()
	findings := RunSecurityAudit(ctx, dataHome, cfg)
	if len(findings) > 0 {
		auditLog().Warn(fmt.Sprintf("Security posture audit found %d issue(s) \u2014 review your deployment:", len(findings)))
		for i, f := range findings {
			auditLog().Warn(fmt.Sprintf("  [security %d/%d] %s", i+1, len(findings), f))
		}
	}
	return findings
}
