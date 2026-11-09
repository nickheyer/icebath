package provider

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

const (
	localModelsPollInterval = 200 * time.Millisecond
	localModelsPauseWait    = 15 * time.Second
)

// Job phases that carry a byte counter while the transfer runs
var localModelsDownloadPhases = map[string]bool{"starting": true, "installing-runtime": true, "downloading-runtime": true, "downloading": true}

// Whether PM pins a CPU engine for this machine at all, false when no backend can install
func localModelsAvailable() bool {
	reason, err := UnavailableReason("cpu", "")
	return err == nil && reason == ""
}

// Seconds as 3m05s or 42s
func fmtETA(seconds int) string {
	minutes, secs := seconds/60, seconds%60
	if minutes > 0 {
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
	detail := anyString(view["detail"])
	if detail == "" {
		detail = "Starting"
	}
	line := "  " + detail
	if !localModelsDownloadPhases[anyString(view["phase"])] {
		return line
	}
	if total, ok := anyFloat(view["total_bytes"]); ok && total > 0 {
		done, _ := anyFloat(view["done_bytes"])
		percent, _ := anyFloat(view["percent"])
		line += fmt.Sprintf("  %d%%  %s / %s", int(percent), LocalModelsHumanGB(done), LocalModelsHumanGB(total))
	}
	if rate, ok := anyFloat(view["bytes_per_sec"]); ok && rate > 0 {
		line += "  " + rateLabel(rate)
	}
	if eta, ok := anyFloat(view["eta_seconds"]); ok {
		line += fmt.Sprintf("  ~%s left", fmtETA(int(eta)))
	}
	return line
}

// Renders a job until it settles and returns its status, Ctrl-C pausing a pausable phase
func followJob(r *ModelFlowRun, jobID string) string {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	shownPhase, width, phaseShown := "", 0, false
	var view map[string]any
	for {
		view = LocalModelsJobView(jobID)
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
		fmt.Fprintf(r.Out, "\r%s%s", line, strings.Repeat(" ", max(0, width-len(line))))
		width = len(line)
		select {
		case <-interrupts:
			fmt.Fprintln(r.Out)
			if !truthy(view["can_pause"]) || !LocalModelsPauseJob(jobID) {
				fmt.Fprintln(r.Out, "  Stopped following. Setup continues only while this process runs.")
				return "interrupted"
			}
			deadline := time.Now().Add(localModelsPauseWait)
			for time.Now().Before(deadline) && anyString(LocalModelsJobView(jobID)["status"]) == "running" {
				time.Sleep(localModelsPollInterval)
			}
			fmt.Fprintln(r.Out, "  Paused. Run `icebath model` → Local models again to resume; finished bytes are kept.")
			return "paused"
		case <-time.After(localModelsPollInterval):
		}
	}
	if phaseShown {
		fmt.Fprintln(r.Out)
	}
	switch anyString(view["status"]) {
	case "error":
		msg := anyString(view["error"])
		if msg == "" {
			msg = "Setup failed"
		}
		fmt.Fprintf(r.Out, "  ✗ %s\n", msg)
	case "done":
		fmt.Fprintf(r.Out, "  ✓ %s\n", anyString(view["detail"]))
	}
	return anyString(view["status"])
}

// Runs a route handler that starts a job, its refusal printed and an empty id returned
func startLocalModelsJob(r *ModelFlowRun, start func(ctx context.Context) (map[string]any, error)) string {
	result, err := start(r.ctx())
	if err != nil {
		fmt.Fprintf(r.Out, "  ✗ %s\n", err.Error())
		return ""
	}
	return anyString(result["job_id"])
}

// Catalog row label with the recommended star, size, download state, fit and the active marker
func rowLabel(row map[string]any, activeID string) string {
	label := "  "
	if truthy(row["recommended"]) {
		label = "★ "
	}
	label += fmt.Sprintf("%s (%s", anyString(row["display_name"]), anyString(row["size_label"]))
	if truthy(row["downloaded"]) {
		label += ", downloaded)"
	} else {
		label += ")"
	}
	label += fmt.Sprintf(" — %s", anyString(row["fit_summary"]))
	if activeID != "" && (activeID == anyString(row["downloaded_model_id"]) || activeID == anyString(row["model_id"])) {
		label += "  ← currently active"
	}
	return label
}

func anyMaps(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	if typed, ok := v.([]map[string]any); ok {
		return typed
	}
	return out
}

// Local models wizard, a catalog model sized for this machine or one on disk made the default
func modelFlowLocal(r *ModelFlowRun) error {
	hw, err := LocalModelsHardware(r.ctx())
	if err != nil {
		return err
	}
	status, err := LocalModelsStatus(r.ctx())
	if err != nil {
		return err
	}
	gpu := ""
	if truthy(hw["gpu_name"]) {
		gpu = fmt.Sprintf("%s · ", anyString(hw["gpu_name"]))
	}
	memoryKind := "GPU"
	if truthy(hw["uma"]) {
		memoryKind = "unified"
	}
	ramBytes, _ := anyFloat(hw["ram_total_bytes"])
	fmt.Fprintf(r.Out, "  Machine:  %s%s %s memory · %s RAM\n", gpu, anyString(hw["vram_label"]), memoryKind, LocalModelsHumanGB(ramBytes))
	runtimeInstalled := truthy(status["runtime_installed"])
	if runtimeInstalled {
		update := ""
		if truthy(status["update_available"]) {
			update = "  (update available)"
		}
		fmt.Fprintf(r.Out, "  Engine:   llama.cpp %s (%s)%s\n", anyString(status["tag"]), anyString(status["runtime_backend"]), update)
	} else {
		fmt.Fprintf(r.Out, "  Engine:   not installed; llama.cpp %s installs with your first model\n", anyString(status["configured_tag"]))
	}
	fmt.Fprintln(r.Out)
	catalog, err := LocalModelsCatalog(r.ctx())
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
		label := fmt.Sprintf("  %s — %s · on disk", anyString(m["id"]), anyString(m["size_label"]))
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
	idx, ok := promptProviderChoice(r.Out, labels, def, "Select a local model (★ recommended for this machine):")
	if !ok || idx == len(labels)-1 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	var modelID, name string
	if idx < len(choices) {
		row := choices[idx]
		name = anyString(row["display_name"])
		useDownloaded := truthy(row["downloaded"]) && runtimeInstalled
		if !useDownloaded {
			need := []string{}
			if !runtimeInstalled {
				need = append(need, fmt.Sprintf("the llama.cpp engine (%s)", anyString(status["configured_tag"])))
			}
			if !truthy(row["downloaded"]) {
				need = append(need, fmt.Sprintf("%s (%s)", name, anyString(row["size_label"])))
			}
			if len(need) > 0 && !PromptYesNo(fmt.Sprintf("  Download %s?", strings.Join(need, " and ")), true, "") {
				fmt.Fprintln(r.Out, flowNoChange)
				return nil
			}
			rowID := anyString(row["id"])
			jobID := startLocalModelsJob(r, func(ctx context.Context) (map[string]any, error) { return LocalModelsQuickstart(ctx, rowID) })
			if jobID == "" || followJob(r, jobID) != "done" {
				return nil
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
		if !PromptYesNo(fmt.Sprintf("  Download the llama.cpp engine (%s)?", anyString(status["configured_tag"])), true, "") {
			fmt.Fprintln(r.Out, flowNoChange)
			return nil
		}
		jobID := startLocalModelsJob(r, LocalModelsRuntimeInstall)
		if jobID == "" || followJob(r, jobID) != "done" {
			return nil
		}
	}
	jobID := startLocalModelsJob(r, func(ctx context.Context) (map[string]any, error) { return LocalModelsActivate(ctx, modelID) })
	if jobID != "" && followJob(r, jobID) == "done" {
		fmt.Fprintf(r.Out, "Default model set to: %s (via Local models · llama.cpp)\n", name)
	}
	return nil
}
