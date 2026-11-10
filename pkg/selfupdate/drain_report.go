package selfupdate

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const drainReportIntervalS = 30.0

var (
	drainMonotonic  = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	drainReadStatus = gateway.ReadRuntimeStatus
)

// Installed by cmd/icebath, returns the id and name of every job in the cron store of a home in load order
var LoadCronJobsHook func(ctx context.Context, h string) ([][2]string, error)

func init() {
	gateway.DrainProgressReporterHook = func(ctx context.Context, h string, budgetS float64) func() {
		return drainProgressReporter(ctx, h, budgetS, drainReportIntervalS, func(s string) { fmt.Fprintln(os.Stdout, s) })
	}
}

// Elapsed seconds as XmYYs from one minute up, Ns below, a question mark when not a number
func fmtElapsed(seconds any) string {
	f, err := pyint.Float(seconds)
	if err != nil {
		if pe, ok := err.(*pyint.Error); ok && pe.Kind == "OverflowError" {
			panic(err)
		}
		return "?"
	}
	if math.IsNaN(f) {
		return "?"
	}
	if math.IsInf(f, 0) {
		panic(&pyint.Error{Kind: "OverflowError", Msg: "cannot convert float infinity to integer"})
	}
	total, _ := new(big.Float).SetFloat64(f).Int(nil)
	if total.Cmp(big.NewInt(60)) < 0 {
		return total.String() + "s"
	}
	q, r := new(big.Int).DivMod(total, big.NewInt(60), new(big.Int))
	return fmt.Sprintf("%sm%02ds", q.String(), r.Int64())
}

// Name of a cron job id in the jobs file of h or the ambient home, empty when unreadable or unnamed
func cronJobName(ctx context.Context, jobID, h string) string {
	if h == "" {
		h = home.Dir(ctx)
	}
	load := LoadCronJobsHook
	if load == nil {
		panic("selfupdate: cmd/icebath must install LoadCronJobsHook over the cron store")
	}
	jobs, err := load(ctx, h)
	if err != nil {
		return ""
	}
	for _, job := range jobs {
		if job[0] == jobID {
			return job[1]
		}
	}
	return ""
}

// Getter over one active work entry, an AttributeError when the entry is no mapping
func drainUnitGetter(unit any) (func(string) any, error) {
	switch u := unit.(type) {
	case *pyjson.Object:
		return func(k string) any { v, _ := u.Get(k); return v }, nil
	case map[string]any:
		return func(k string) any { return u[k] }, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(unit))
}

// Python value or its fallback when falsy, rendered with str
func drainOr(v any, fallback string) string {
	if pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return fallback
}

// One human line for an active work entry, unknown kinds reduced to their kind and pid
func describeActiveWorkUnit(ctx context.Context, unit any, h string) (string, error) {
	get, err := drainUnitGetter(unit)
	if err != nil {
		return "", err
	}
	kind := drainOr(get("kind"), "work")
	pidPart := ""
	if pid := get("pid"); pyrepr.Truthy(pid) {
		pidPart = " pid " + pyrepr.Str(pid)
	}
	elapsedPart := ""
	if elapsed := get("elapsed_s"); elapsed != nil {
		elapsedPart = ", running " + fmtElapsed(elapsed)
	}
	switch kind {
	case "cron":
		jobID := drainOr(get("job_id"), "?")
		label := "cron job " + jobID
		if name := cronJobName(ctx, jobID, h); name != "" {
			label += " (" + name + ")"
		}
		where := " in-process" + pidPart
		if pyrepr.Truthy(get("external")) {
			where = " in external worker" + pidPart
		}
		return label + where + elapsedPart, nil
	case "chat":
		session := drainOr(get("session"), "?")
		var parts []string
		if model := get("model"); pyrepr.Truthy(model) {
			parts = append(parts, "model "+pyrepr.Str(model))
		}
		if tool := get("current_tool"); pyrepr.Truthy(tool) {
			parts = append(parts, "tool "+pyrepr.Str(tool))
		}
		line := "chat turn " + session + pidPart + elapsedPart
		if len(parts) > 0 {
			line += " [" + strings.Join(parts, ", ") + "]"
		}
		return line, nil
	case "delegation":
		goal := pystr.Strip(drainOr(get("goal"), ""))
		line := "background delegation " + drainOr(get("delegation_id"), "?") + pidPart + elapsedPart
		if goal != "" {
			line += " [" + goal + "]"
		}
		return line, nil
	}
	return kind + " run" + pidPart + elapsedPart, nil
}

// Active work list the gateway of h or the ambient home last published, nil when absent or no list
func readActiveWork(ctx context.Context, h string) []any {
	path := ""
	if h != "" {
		path = filepath.Join(h, "gateway_state.json")
	} else if o := home.Override(ctx); o != "" {
		path = filepath.Join(home.Dir(ctx), "gateway_state.json")
	}
	record := drainReadStatus(path)
	if record == nil {
		return nil
	}
	work, _ := record.Get("active_work")
	list, ok := work.([]any)
	if !ok {
		return nil
	}
	return append([]any{}, list...)
}

// TUI command opening the profile of h, the bare command for the default profile or no profile
func drainTUICommand(h string) string {
	if name := home.ProfileNameFor(h); name != "" && name != "default" {
		return "icebath -p " + name + " --tui"
	}
	return "icebath --tui"
}

// Progress block naming what the gateway waits on and how to release the wait
func formatDrainReport(ctx context.Context, work []any, remainingS float64, h string) (string, error) {
	left := remainingS
	if 0 > left {
		left = 0
	}
	leftText, err := pyrepr.Format(left, ".0f")
	if err != nil {
		return "", err
	}
	lines := []string{"  ⏳ still draining — " + leftText + "s left before the forced restart"}
	switch {
	case work == nil:
		lines = append(lines, "     (gateway did not report what it is waiting on — pre-update gateway or unreadable state file)")
	case len(work) == 0:
		lines = append(lines, "     (no active work reported; the gateway should exit momentarily)")
	default:
		lines = append(lines, fmt.Sprintf("     waiting on %d active work unit(s):", len(work)))
		chat := false
		for _, u := range work {
			line, err := describeActiveWorkUnit(ctx, u, h)
			if err != nil {
				return "", err
			}
			lines = append(lines, "       • "+line)
		}
		for _, u := range work {
			if get, err := drainUnitGetter(u); err == nil && pyrepr.Equal(get("kind"), "chat") {
				chat = true
				break
			}
		}
		if chat {
			lines = append(lines, "     a chat turn waiting on an approval or clarify question can be answered now: "+
				"open it with `"+drainTUICommand(h)+"` (resume the session)")
		}
		lines = append(lines, "     finish or kill the work above to release the drain now; "+
			"agent.restart_after_turn_timeout in config.yaml caps this wait")
	}
	return strings.Join(lines, "\n"), nil
}

// Callback for the pid exit wait that emits the drain report once every interval while the wait runs
func drainProgressReporter(ctx context.Context, h string, budgetS, intervalS float64, emit func(string)) func() {
	started := drainMonotonic()
	last := started
	var mu sync.Mutex
	return func() {
		mu.Lock()
		defer mu.Unlock()
		now := drainMonotonic()
		if now-last < intervalS {
			return
		}
		last = now
		report, err := formatDrainReport(ctx, readActiveWork(ctx, h), budgetS-(now-started), h)
		if err != nil {
			panic(err)
		}
		emit(report)
	}
}
