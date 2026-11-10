package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	localModelsPollInterval = 200 * time.Millisecond
	localModelsPauseWait    = 15 * time.Second
)

// Local Models handlers and prompts the terminal flow drives, the module attributes hermes tests patch
var localFlowDeps = struct {
	hardware       func(ctx context.Context) (map[string]any, error)
	status         func(ctx context.Context) (map[string]any, error)
	catalog        func(ctx context.Context) (map[string]any, error)
	quickstart     func(ctx context.Context, modelID, profile string) (map[string]any, error)
	runtimeInstall func(ctx context.Context, backend, profile string) (map[string]any, error)
	activate       func(ctx context.Context, modelID, profile string) (map[string]any, error)
	promptChoice   func(out io.Writer, choices []string, def int, title string) (int, bool)
	promptYesNo    func(question string, def bool, description string) bool
}{
	hardware: LocalModelsHardware, status: LocalModelsStatus, catalog: LocalModelsCatalog,
	quickstart: LocalModelsQuickstart, runtimeInstall: LocalModelsRuntimeInstall, activate: LocalModelsActivate,
	promptChoice: promptProviderChoice, promptYesNo: PromptYesNo,
}

// Whether PM pins a CPU engine for this machine at all, false when no backend can install
func localModelsAvailable() bool {
	reason, err := UnavailableReason("cpu", "")
	return err == nil && reason == ""
}

// Seconds as 3m05s or 42s
func fmtETA(seconds int) string {
	minutes, secs := seconds/60, seconds%60
	if seconds%60 < 0 {
		minutes, secs = minutes-1, secs+60
	}
	if minutes != 0 {
		return fmt.Sprintf("%dm%02ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// Bytes per second as GB/s above a gigabyte, whole MB/s above ten, else one decimal
func rateLabel(bytesPerSec float64) string {
	mb := bytesPerSec / float64(1<<20)
	switch {
	case mb >= 1024:
		return fmt.Sprintf("%.1f GB/s", mb/1024)
	case mb >= 10:
		return fmt.Sprintf("%.0f MB/s", mb)
	}
	return fmt.Sprintf("%.1f MB/s", mb)
}

// One progress line of a job view, the byte counter only during a download phase
func progressLine(view map[string]any) string {
	line := "  " + pyOrStr(view["detail"])
	if !pyTruthy(view["detail"]) {
		line = "  Starting"
	}
	if !localModelsDownloadPhases[anyString(view["phase"])] {
		return line
	}
	if total, ok := anyFloat(view["total_bytes"]); ok && total != 0 {
		done, _ := anyFloat(view["done_bytes"])
		percent := any(0)
		if p, present := view["percent"]; present {
			percent = p
		}
		line += fmt.Sprintf("  %s%%  %s / %s", pyStr(percent), LocalModelsHumanGB(done), LocalModelsHumanGB(total))
	}
	if rate, ok := anyFloat(view["bytes_per_sec"]); ok && rate != 0 {
		line += "  " + rateLabel(rate)
	}
	if eta, ok := anyFloat(view["eta_seconds"]); ok {
		line += fmt.Sprintf("  ~%s left", fmtETA(int(eta)))
	}
	return line
}

// Renders a job until it settles and returns its status, Ctrl-C pausing a pausable phase
func followJob(r *ModelFlowRun, jobID string) (string, error) {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	shownPhase, width, phaseShown := "", 0, false
	var view map[string]any
	for {
		var err error
		if view, err = LocalModelsJobView(jobID); err != nil {
			return "", err
		}
		if anyString(view["status"]) != "running" {
			break
		}
		if phase := anyString(view["phase"]); !phaseShown || phase != shownPhase {
			if phaseShown {
				fmt.Fprintln(r.Out)
			}
			shownPhase, width, phaseShown = phase, 0, true
		}
		line := progressLine(view)
		length := utf8.RuneCountInString(line)
		fmt.Fprintf(r.Out, "\r%s%s", line, strings.Repeat(" ", max(0, width-length)))
		width = length
		select {
		case <-interrupts:
			fmt.Fprintln(r.Out)
			current, err := LocalModelsJobView(jobID)
			if err != nil {
				return "", err
			}
			if !truthy(current["can_pause"]) || !LocalModelsPauseJob(jobID) {
				fmt.Fprintln(r.Out, "  Stopped following. Setup continues only while this process runs.")
				return "interrupted", nil
			}
			deadline := time.Now().Add(localModelsPauseWait)
			for time.Now().Before(deadline) {
				settled, err := LocalModelsJobView(jobID)
				if err != nil {
					return "", err
				}
				if anyString(settled["status"]) != "running" {
					break
				}
				select {
				case <-interrupts:
					return "", errFlowCancelled
				case <-time.After(localModelsPollInterval):
				}
			}
			fmt.Fprintln(r.Out, "  Paused. Run `icebath model` → Local models again to resume; finished bytes are kept.")
			return "paused", nil
		case <-time.After(localModelsPollInterval):
		}
	}
	if phaseShown {
		fmt.Fprintln(r.Out)
	}
	switch anyString(view["status"]) {
	case "error":
		msg := pyOrStr(view["error"])
		if msg == "" {
			msg = "Setup failed"
		}
		fmt.Fprintf(r.Out, "  ✗ %s\n", msg)
	case "done":
		fmt.Fprintf(r.Out, "  ✓ %s\n", pyStr(view["detail"]))
	}
	return anyString(view["status"]), nil
}

// Runs a route handler that starts a job, its HTTP refusal printed and an empty id returned
func startLocalModelsJob(r *ModelFlowRun, start func(ctx context.Context) (map[string]any, error)) (string, error) {
	result, err := start(r.ctx())
	if err != nil {
		var refusal *HTTPException
		if errors.As(err, &refusal) {
			fmt.Fprintf(r.Out, "  ✗ %s\n", refusal.Detail)
			return "", nil
		}
		return "", err
	}
	return anyString(result["job_id"]), nil
}

// Starts a job and follows it, true only when it finished
func runLocalModelsJob(r *ModelFlowRun, start func(ctx context.Context) (map[string]any, error)) (bool, error) {
	jobID, err := startLocalModelsJob(r, start)
	if err != nil || jobID == "" {
		return false, err
	}
	status, err := followJob(r, jobID)
	return status == "done", err
}

// Catalog row label with the recommended star, size, download state, fit and the active marker
func rowLabel(row map[string]any, activeID string) string {
	label := "  "
	if truthy(row["recommended"]) {
		label = "★ "
	}
	label += fmt.Sprintf("%s (%s", pyStr(row["display_name"]), pyStr(row["size_label"]))
	if truthy(row["downloaded"]) {
		label += ", downloaded)"
	} else {
		label += ")"
	}
	label += fmt.Sprintf(" — %s", pyStr(pyGetAny(row, "fit_summary", "")))
	if activeID != "" && (activeID == anyString(row["downloaded_model_id"]) || activeID == anyString(row["model_id"])) {
		label += "  ← currently active"
	}
	return label
}

func anyMaps(v any) []map[string]any {
	if typed, ok := v.([]map[string]any); ok {
		return typed
	}
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Local models wizard, a catalog model sized for this machine or one on disk made the default
func modelFlowLocal(r *ModelFlowRun) error {
	hw, err := localFlowDeps.hardware(r.ctx())
	if err != nil {
		return err
	}
	status, err := localFlowDeps.status(r.ctx())
	if err != nil {
		return err
	}
	gpu := ""
	if truthy(hw["gpu_name"]) {
		gpu = fmt.Sprintf("%s · ", pyStr(hw["gpu_name"]))
	}
	memoryKind := "GPU"
	if truthy(hw["uma"]) {
		memoryKind = "unified"
	}
	ramBytes, _ := anyFloat(hw["ram_total_bytes"])
	fmt.Fprintf(r.Out, "  Machine:  %s%s %s memory · %s RAM\n", gpu, pyStr(hw["vram_label"]), memoryKind, LocalModelsHumanGB(ramBytes))
	runtimeInstalled := truthy(status["runtime_installed"])
	if runtimeInstalled {
		update := ""
		if truthy(status["update_available"]) {
			update = "  (update available)"
		}
		fmt.Fprintf(r.Out, "  Engine:   llama.cpp %s (%s)%s\n", pyStr(status["tag"]), pyStr(status["runtime_backend"]), update)
	} else {
		fmt.Fprintf(r.Out, "  Engine:   not installed; llama.cpp %s installs with your first model\n", pyStr(status["configured_tag"]))
	}
	fmt.Fprintln(r.Out)
	catalog, err := localFlowDeps.catalog(r.ctx())
	if err != nil {
		return err
	}
	rows := anyMaps(catalog["models"])
	choices := []map[string]any{}
	catalogIDs := map[string]bool{}
	for _, row := range rows {
		if truthy(row["fits"]) && !truthy(row["needs_engine"]) {
			choices = append(choices, row)
		}
		catalogIDs[anyString(row["downloaded_model_id"])] = true
	}
	staged := []map[string]any{}
	for _, m := range anyMaps(status["models"]) {
		if !catalogIDs[anyString(m["id"])] {
			staged = append(staged, m)
		}
	}
	activeID := anyString(status["active_model_id"])
	labels := make([]string, 0, len(choices)+len(staged)+1)
	for _, row := range choices {
		labels = append(labels, rowLabel(row, activeID))
	}
	for _, m := range staged {
		label := fmt.Sprintf("  %s — %s · on disk", pyStr(m["id"]), pyStr(m["size_label"]))
		if anyString(m["id"]) == activeID {
			label += "  ← currently active"
		}
		labels = append(labels, label)
	}
	labels = append(labels, "Leave unchanged")
	if skipped := len(rows) - len(choices); skipped > 0 {
		fmt.Fprintf(r.Out, "  %d catalog model(s) need more memory or a newer engine than this machine has.\n", skipped)
	}
	def := -1
	for i, row := range choices {
		if activeID != "" && (activeID == anyString(row["downloaded_model_id"]) || activeID == anyString(row["model_id"])) {
			def = i
			break
		}
	}
	if def < 0 {
		def = 0
		for i, row := range choices {
			if truthy(row["recommended"]) {
				def = i
				break
			}
		}
	}
	idx, ok := localFlowDeps.promptChoice(r.Out, labels, def, "Select a local model (★ recommended for this machine):")
	if !ok || idx == len(labels)-1 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	var modelID, name string
	if idx < len(choices) {
		row := choices[idx]
		name = pyStr(row["display_name"])
		useDownloaded := truthy(row["downloaded"]) && runtimeInstalled
		if !useDownloaded {
			need := []string{}
			if !runtimeInstalled {
				need = append(need, fmt.Sprintf("the llama.cpp engine (%s)", pyStr(status["configured_tag"])))
			}
			if !truthy(row["downloaded"]) {
				need = append(need, fmt.Sprintf("%s (%s)", name, pyStr(row["size_label"])))
			}
			if len(need) > 0 && !localFlowDeps.promptYesNo(fmt.Sprintf("  Download %s?", strings.Join(need, " and ")), true, "") {
				fmt.Fprintln(r.Out, flowNoChange)
				return nil
			}
			rowID := anyString(row["id"])
			done, err := runLocalModelsJob(r, func(ctx context.Context) (map[string]any, error) {
				return localFlowDeps.quickstart(ctx, rowID, "")
			})
			if err != nil || !done {
				return err
			}
			fmt.Fprintf(r.Out, "Default model set to: %s (via Local models · llama.cpp)\n", name)
			return nil
		}
		modelID = anyString(row["downloaded_model_id"])
	} else {
		modelID = anyString(staged[idx-len(choices)]["id"])
		name = modelID
	}
	if !runtimeInstalled {
		if !localFlowDeps.promptYesNo(fmt.Sprintf("  Download the llama.cpp engine (%s)?", pyStr(status["configured_tag"])), true, "") {
			fmt.Fprintln(r.Out, flowNoChange)
			return nil
		}
		done, err := runLocalModelsJob(r, func(ctx context.Context) (map[string]any, error) {
			return localFlowDeps.runtimeInstall(ctx, "", "")
		})
		if err != nil || !done {
			return err
		}
	}
	done, err := runLocalModelsJob(r, func(ctx context.Context) (map[string]any, error) {
		return localFlowDeps.activate(ctx, modelID, "")
	})
	if err != nil {
		return err
	}
	if done {
		fmt.Fprintf(r.Out, "Default model set to: %s (via Local models · llama.cpp)\n", name)
	}
	return nil
}
