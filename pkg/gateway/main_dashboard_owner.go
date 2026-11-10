package gateway

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var (
	// Canonical holder subcommand of a command line, installed by pkg/selfupdate which owns the holder grammar
	HolderSubcommand func(cmdline string) (string, bool)

	dashboardGOOS = runtime.GOOS
	dashboardUID  = os.Getuid
	// Captured run of a short local probe such as systemctl or ps
	dashboardRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		return pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: timeout})
	}
	dashboardProcRoot           = "/proc"
	dashboardLaunchdPrintPID    = LaunchdPrintServicePID
	dashboardPortRE             = pyre.MustCompile(`(?:^|\s)--port(?:=|\s+)(\d+)`)
	dashboardHostRE             = pyre.MustCompile(`(?:^|\s)--host(?:=|\s+)("[^"]+"|'[^']+'|\S+)`)
	dashboardLaunchdPlistDirsFn = launchdPlistDirs
)

// Mode, host and port of a dashboard or serve command line, not ok for any other command
func parseDashboardRuntime(command string) (mode, host string, port int, ok bool) {
	mode, _ = HolderSubcommand(command)
	if mode != "dashboard" && mode != "serve" {
		return "", "", 0, false
	}
	port, host = 9119, "127.0.0.1"
	if m, _ := dashboardPortRE.FindStringMatch(command); m != nil {
		parsed, valid := pyint.Parse(m.GroupByNumber(1).String())
		if !valid || !parsed.IsInt64() {
			return "", "", 0, false
		}
		port = int(parsed.Int64())
	}
	if m, _ := dashboardHostRE.FindStringMatch(command); m != nil {
		if host = strings.Trim(m.GroupByNumber(1).String(), "\"'"); host == "" {
			host = "127.0.0.1"
		}
	}
	return mode, host, port, true
}

// Unified cgroup paths of a process from its proc cgroup file, none when unreadable
func pidUnifiedCgroupEntries(pid int) []string {
	path := filepath.Join(dashboardProcRoot, strconv.Itoa(pid), "cgroup")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []string
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8SigReplace(raw), false) {
		if _, entry, found := strings.Cut(pystr.Strip(line), "::"); found {
			entries = append(entries, entry)
		}
	}
	return entries
}

// First unified cgroup path of a process, not ok when it has none
func getPidCgroupPath(pid int) (string, bool) {
	if entries := pidUnifiedCgroupEntries(pid); len(entries) > 0 {
		return entries[0], true
	}
	return "", false
}

// Scope user or system a cgroup path names, empty when it names neither
func extractScopeFromCgroup(cgroupEntry string) string {
	if strings.Contains(cgroupEntry, "/system.slice/") {
		return "system"
	}
	if strings.Contains(cgroupEntry, "/user.slice/") {
		return "user"
	}
	return ""
}

// True when the live MainPID of the unit is pid, asked of systemctl show in the unit's scope
func unitMainPidIs(ctx context.Context, svcName, cgroupPath string, pid int) bool {
	scopes := [][]string{{}, {"--user"}}
	switch extractScopeFromCgroup(cgroupPath) {
	case "user":
		scopes = [][]string{{"--user"}}
	case "system":
		scopes = [][]string{{}}
	}
	for _, scopeArgs := range scopes {
		argv := append(append([]string{"systemctl"}, scopeArgs...), "show", svcName, "--property=MainPID", "--value")
		result, err := dashboardRunProbe(ctx, argv, 10*time.Second)
		if err != nil {
			continue
		}
		if result.Returncode == 0 && pystr.Strip(pystr.DecodeUTF8Replace(result.Stdout)) == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

// Systemd service unit whose live MainPID is pid, not ok when no unit provably owns the process
func getSystemdServiceForPid(ctx context.Context, pid int) (string, bool) {
	for _, cgPath := range pidUnifiedCgroupEntries(pid) {
		if strings.HasSuffix(cgPath, ".service") {
			svcName := cgPath[strings.LastIndex(cgPath, "/")+1:]
			if svcName != "" && unitMainPidIs(ctx, svcName, cgPath, pid) {
				return svcName, true
			}
		}
	}
	return "", false
}

// Unit and scope of the systemd unit whose MainPID is pid, the scope empty when the cgroup omits it
func SystemdOwnerForPid(ctx context.Context, pid int) (unit, scope string, ok bool) {
	unit, ok = getSystemdServiceForPid(ctx, pid)
	if !ok {
		return "", "", false
	}
	cgroup, _ := getPidCgroupPath(pid)
	return unit, extractScopeFromCgroup(cgroup), true
}

// Plist directory able to supervise a backend with the kind of launchctl domain its jobs load into
type launchdPlistDir struct{ kind, dir string }

// The three launchd plist directories, both LaunchAgents dirs sharing the agent kind
func launchdPlistDirs() []launchdPlistDir {
	userHome, _ := os.UserHomeDir()
	return []launchdPlistDir{{"agent", filepath.Join(userHome, "Library", "LaunchAgents")}, {"agent", "/Library/LaunchAgents"},
		{"daemon", "/Library/LaunchDaemons"}}
}

// Loaded launchd job whose ProgramArguments is a dashboard or serve backend
type LaunchdBackendJob struct {
	Domain  string
	Label   string
	Argv    []string
	LivePID *int
}

// Every loaded launchd job running a dashboard or serve backend, none off macOS
func LoadedLaunchdBackendJobs(ctx context.Context) []LaunchdBackendJob {
	return loadedLaunchdBackendJobs(ctx, dashboardLaunchdPlistDirsFn())
}

// Loaded backend jobs among the plists of the given directories, unreadable or malformed plists skipped
func loadedLaunchdBackendJobs(ctx context.Context, plistDirs []launchdPlistDir) []LaunchdBackendJob {
	jobs := []LaunchdBackendJob{}
	if dashboardGOOS != "darwin" {
		return jobs
	}
	uid := strconv.Itoa(dashboardUID())
	for _, entry := range plistDirs {
		plists, err := filepath.Glob(filepath.Join(globEscape(entry.dir), "*.plist"))
		if err != nil {
			continue
		}
		sort.Strings(plists)
		for _, plistPath := range plists {
			raw, err := os.ReadFile(plistPath)
			if err != nil {
				continue
			}
			doc, err := pyplist.Load(raw)
			if err != nil {
				continue
			}
			data, isDict := doc.(*pyplist.Dict)
			if !isDict || data == nil {
				continue
			}
			label := ""
			if v, _ := data.Get("Label"); pyrepr.Truthy(v) {
				label = pystr.Strip(pyrepr.Str(v))
			}
			rawArgs, _ := data.Get("ProgramArguments")
			args, isList := rawArgs.([]any)
			if label == "" || !isList || len(args) == 0 {
				continue
			}
			argv := make([]string, len(args))
			quoted := make([]string, len(args))
			for i, a := range args {
				argv[i] = pyrepr.Str(a)
				quoted[i] = pyshlex.Quote(argv[i])
			}
			if _, _, _, isBackend := parseDashboardRuntime(strings.Join(quoted, " ")); !isBackend {
				continue
			}
			domains := []string{"gui/" + uid, "user/" + uid}
			if entry.kind == "daemon" {
				domains = []string{"system"}
			}
			for _, domain := range domains {
				loaded, pid, hasPID, err := dashboardLaunchdPrintPID(ctx, domain, label)
				if err != nil || !loaded {
					continue
				}
				job := LaunchdBackendJob{Domain: domain, Label: label, Argv: argv}
				if hasPID {
					job.LivePID = &pid
				}
				jobs = append(jobs, job)
				break
			}
		}
	}
	return jobs
}

// Directory path with glob metacharacters escaped so only the file pattern matches
func globEscape(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if r == '*' || r == '?' || r == '[' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Argv without the flag a respawn adds
func withoutNoOpen(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if a != "--no-open" {
			out = append(out, a)
		}
	}
	return out
}

// Loaded job owning pid by live pid, ancestor or matching arguments, nil when none claims it
func LaunchdJobOwningBackend(pid int, cmdline []string, jobs []LaunchdBackendJob, ancestors []int) *LaunchdBackendJob {
	for i := range jobs {
		job := &jobs[i]
		if job.LivePID != nil {
			if *job.LivePID == pid {
				return job
			}
			for _, ancestor := range ancestors {
				if ancestor == *job.LivePID {
					return job
				}
			}
		}
		if cmdline != nil {
			have, want := withoutNoOpen(cmdline), withoutNoOpen(job.Argv)
			if len(have) == len(want) {
				same := true
				for k := range have {
					same = same && have[k] == want[k]
				}
				if same {
					return job
				}
			}
		}
	}
	return nil
}
