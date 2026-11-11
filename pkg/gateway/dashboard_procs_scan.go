package gateway

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Host facts of the dashboard process scan, swapped by tests
var (
	dashboardScanGetpid    = os.Getpid
	dashboardLedgerEntries = func() []map[string]any { return tools.LedgerEntries(tools.LedgerOptions{}) }
	dashboardProcPPID      = proc.PPID
	dashboardProcCmdline   = proc.Cmdline
	dashboardHomeForPID    = func(ctx context.Context, pid int) (string, bool, error) { return HermesHomeForPID(ctx, pid, false) }
)

// Executables that only carry an icebath command line and never serve traffic themselves
var wrapperHeadCommands = map[string]bool{
	"ash": true, "bash": true, "csh": true, "dash": true, "fish": true, "ksh": true, "sh": true, "tcsh": true, "zsh": true,
	"env": true, "nohup": true, "nice": true, "stdbuf": true, "timeout": true, "watch": true, "xargs": true,
	"screen": true, "tmux": true, "sudo": true,
}

// Pid and command line of every process from wmic on Windows or ps, an error when the ps scan cannot run
func iterDashboardProcessTable(ctx context.Context) ([]pidCommand, error) {
	rows := []pidCommand{}
	if dashboardGOOS == "windows" {
		result, err := dashboardRunProbe(ctx, []string{"wmic", "process", "get", "ProcessId,CommandLine", "/FORMAT:LIST"}, 10*time.Second)
		if err != nil || result == nil || result.Returncode != 0 {
			return rows, nil
		}
		current := ""
		for _, line := range strings.Split(strings.ToValidUTF8(string(result.Stdout), ""), "\n") {
			line = pystr.Strip(line)
			if strings.HasPrefix(line, "CommandLine=") {
				current = line[len("CommandLine="):]
			} else if strings.HasPrefix(line, "ProcessId=") {
				if pid, ok := pyIntText(line[len("ProcessId="):]); ok {
					rows = append(rows, pidCommand{pid, current})
				}
			}
		}
		return rows, nil
	}
	result, err := dashboardRunProbe(ctx, []string{"ps", "-A", "-o", "pid=,command="}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if result.Returncode == 0 {
		for _, line := range strings.Split(pystr.DecodeUTF8Replace(result.Stdout), "\n") {
			parts := pystr.SplitWhitespace(pystr.Strip(line), 1)
			if len(parts) == 2 && !strings.Contains(line, "grep") {
				if pid, ok := pyIntText(parts[0]); ok {
					rows = append(rows, pidCommand{pid, parts[1]})
				}
			}
		}
	}
	return rows, nil
}

// Running dashboard and serve processes minus this one and the excluded pids, empty on a scan error
func scanDashboardProcesses(ctx context.Context, excludePIDs map[int]bool) []pidCommand {
	skip := map[int]bool{dashboardScanGetpid(): true}
	for pid := range excludePIDs {
		skip[pid] = true
	}
	table, err := iterDashboardProcessTable(ctx)
	if err != nil {
		return []pidCommand{}
	}
	found := []pidCommand{}
	for _, row := range table {
		if skip[row.pid] {
			continue
		}
		if mode, _ := HolderSubcommand(row.command); mode == "dashboard" || mode == "serve" {
			found = append(found, row)
		}
	}
	func() {
		defer func() { _ = recover() }()
		seen := map[int]bool{}
		for pid := range skip {
			seen[pid] = true
		}
		for _, row := range found {
			seen[row.pid] = true
		}
		for _, entry := range dashboardLedgerEntries() {
			pid, ok := ledgerInt(entry["pid"])
			if purpose := entry["purpose"]; (purpose == "serve" || purpose == "dashboard") && ok && !seen[pid] {
				argv := ""
				if raw := entry["argv"]; pyTruthy(raw) {
					argv = pyrepr.Str(raw)
				}
				found = append(found, pidCommand{pid, argv})
			}
		}
	}()
	return found
}

// Python int of a ledger value, bools excluded the way isinstance(x, int) is used on JSON rows
func ledgerInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

// Host and port the spawn ledger records for each live serve or dashboard pid
func ledgerServeBinds() map[int]hostPort {
	binds := map[int]hostPort{}
	func() {
		defer func() { _ = recover() }()
		for _, entry := range dashboardLedgerEntries() {
			pid, okPID := ledgerInt(entry["pid"])
			port, okPort := ledgerInt(entry["port"])
			if purpose := entry["purpose"]; (purpose == "serve" || purpose == "dashboard") && okPID && okPort && port > 0 {
				host := ""
				if raw := entry["host"]; pyTruthy(raw) {
					host = pyrepr.Str(raw)
				}
				binds[pid] = hostPort{host, port}
			}
		}
	}()
	return binds
}

type hostPort struct {
	host string
	port int
}

// Only the pids whose resolved home is the given home, an unreadable owner never matching
func pidsOwnedByHermesHome(ctx context.Context, pids []int, h string) []int {
	target := NormalizedHomeForCompare(h)
	out := []int{}
	for _, pid := range pids {
		pidHome, ok, err := dashboardHomeForPID(ctx, pid)
		if err == nil && ok && pidHome != "" && NormalizedHomeForCompare(pidHome) == target {
			out = append(out, pid)
		}
	}
	return out
}

// Pids of the ancestors of this process, itself excluded, empty when the parent chain is unreadable
func callerAncestorPIDs() map[int]bool {
	ancestors := map[int]bool{}
	cur := dashboardScanGetpid()
	for range 2048 {
		parent, ok := dashboardProcPPID(cur)
		if !ok || parent <= 0 || ancestors[parent] {
			return ancestors
		}
		ancestors[parent] = true
		cur = parent
	}
	return ancestors
}

// Basename of the first argv token of a pid, empty when unreadable
func argvHeadCommand(pid int) string {
	argv, err := dashboardProcCmdline(pid)
	if err != nil || len(argv) == 0 {
		return ""
	}
	head := argv[0]
	return head[strings.LastIndex(head, "/")+1:]
}

// True when pid is an ancestor of the caller headed by a wrapper executable such as a shell
func isCallerWrapperShell(pid int, ancestors map[int]bool) bool {
	if !ancestors[pid] {
		return false
	}
	return wrapperHeadCommands[argvHeadCommand(pid)]
}
