package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

const (
	handoffResultMaxAge   = 30 * time.Minute
	handoffResultFileName = ".icebath-update-result.json"
	consentFileName       = ".close-holders-consent.json"
	updatesGuide          = "https://hermes-agent.nousresearch.com/docs/getting-started/updating"
)

var stopClassToken = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// ReleaseHolder is a program outside Icebath that holds the desktop app folder
type ReleaseHolder struct {
	PID       int     `json:"pid"`
	StartTime float64 `json:"startTime"`
	Name      string  `json:"name"`
	Exe       string  `json:"exe"`
}

// HandoffResult is what a detached update hand-off reported on exit
type HandoffResult struct {
	OK             bool
	ExitCode       float64
	Manual         bool
	Message        string
	Branch         string
	RunID          *string
	Warnings       []string
	ReleaseHolders []ReleaseHolder
}

func toReleaseHolders(raw any) []ReleaseHolder {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []ReleaseHolder
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, exe := "", ""
		if v, ok := obj["name"]; ok && v != nil {
			name = jsString(v)
		}
		if v, ok := obj["exe"]; ok && v != nil {
			exe = jsString(v)
		}
		pid, start := jsToNumber(obj["pid"]), jsToNumber(obj["start_time"])
		if _, ok := obj["pid"]; !ok {
			pid = jsToNumber(undefinedValue{})
		}
		if _, ok := obj["start_time"]; !ok {
			start = jsToNumber(undefinedValue{})
		}
		if jsSafeInteger(pid) && pid > 0 && jsFinite(start) && name != "" {
			out = append(out, ReleaseHolder{PID: int(pid), StartTime: start, Name: name, Exe: exe})
		}
	}
	return out
}

// Stand in for a missing JSON key, which JavaScript Number reads as NaN
type undefinedValue struct{}

func handoffResultPath(home string) string {
	return filepath.Join(home, handoffResultFileName)
}

// Reads and deletes the result file, a corrupt one kept aside
func consumeHandoffResultFile(file string, log func(string)) (map[string]any, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		log(fmt.Sprintf("[updates] hand-off result is not valid JSON (%s); kept as %s.corrupt", jsonErrorText(err), filepath.Base(file)))
		_ = os.Rename(file, file+".corrupt")
		return nil, false
	}
	_ = os.Remove(file)
	obj, _ := parsed.(map[string]any)
	return obj, true
}

// Parse error text in the shape V8 JSON.parse words it
func jsonErrorText(err error) string {
	if syntax, ok := err.(*json.SyntaxError); ok {
		return fmt.Sprintf("%s in JSON at position %d", syntax.Error(), syntax.Offset)
	}
	return err.Error()
}

func numberField(obj map[string]any, key string) float64 {
	v, ok := obj[key]
	if !ok {
		return jsToNumber(undefinedValue{})
	}
	return jsToNumber(v)
}

func lineTwoWithinRun(lineTwo float64, parsed map[string]any) bool {
	startedAt, finishedAt := numberField(parsed, "started_at"), numberField(parsed, "finished_at")
	return lineTwo == startedAt || (lineTwo > startedAt && jsFinite(finishedAt) && lineTwo <= finishedAt)
}

func handoffRunMismatch(parsed map[string]any, expectedRunID *string, expectedStartedAt *float64) string {
	runID, hasRun := parsed["run_id"]
	startedAt := numberField(parsed, "started_at")
	if hasRun {
		text, ok := runID.(string)
		if !ok || !runIDRE.MatchString(text) {
			return "[updates] hand-off result has an invalid run_id; discarded"
		}
	}
	if expectedRunID != nil && hasRun {
		if runID != *expectedRunID {
			return fmt.Sprintf("[updates] hand-off result is for run %s, not %s; discarded", runID, *expectedRunID)
		}
		return ""
	}
	if expectedStartedAt != nil && jsFinite(startedAt) && !lineTwoWithinRun(*expectedStartedAt, parsed) {
		return fmt.Sprintf("[updates] hand-off result is for the run started at %s, not %s; discarded", jsNumberText(startedAt), jsNumberText(*expectedStartedAt))
	}
	return ""
}

func toHandoffResult(parsed map[string]any, manual bool) HandoffResult {
	result := HandoffResult{OK: jsTruthy(parsed["ok"]), ExitCode: 1, Manual: manual, ReleaseHolders: toReleaseHolders(parsed["release_holders"])}
	if code := numberField(parsed, "exit_code"); jsFinite(code) {
		result.ExitCode = code
	}
	result.Message, _ = parsed["message"].(string)
	result.Branch, _ = parsed["branch"].(string)
	if run, ok := parsed["run_id"].(string); ok {
		result.RunID = &run
	}
	if warnings, ok := parsed["warnings"].([]any); ok {
		for _, w := range warnings {
			if text := jsStringOfArrayItem(w); text != "" {
				result.Warnings = append(result.Warnings, text)
			}
		}
	}
	return result
}

// String of an array element as Array.map(String) converts it
func jsStringOfArrayItem(v any) string {
	if v == nil {
		return "null"
	}
	return jsString(v)
}

// Consumes the hand-off result once, discarding a stale, foreign or malformed one
func readAndConsumeHandoffResult(home string, expectedStartedAt *float64, expectedRunID *string, log func(string), now func() time.Time) *HandoffResult {
	parsed, ok := consumeHandoffResultFile(handoffResultPath(home), log)
	if !ok {
		return nil
	}
	if parsed == nil {
		parsed = map[string]any{}
	}
	manual := jsTruthy(parsed["manual"])
	finishedAt := numberField(parsed, "finished_at")
	if !jsFinite(finishedAt) {
		log("[updates] hand-off result has no finished_at; discarded")
		return nil
	}
	if mismatch := handoffRunMismatch(parsed, expectedRunID, expectedStartedAt); mismatch != "" {
		log(mismatch)
		return nil
	}
	if !manual && float64(now().UnixMilli())-finishedAt*1000 > float64(handoffResultMaxAge.Milliseconds()) {
		return nil
	}
	result := toHandoffResult(parsed, manual)
	return &result
}

// UpdateStop is the stop a failed or refused update receipt recorded
type UpdateStop struct {
	StopClass string
	Outcome   string
}

// Receipt directory of a home, the root home for a profile home
func receiptDirectory(home string) string {
	abs, err := filepath.Abs(home)
	if err != nil {
		abs = filepath.Clean(home)
	}
	root := abs
	if filepath.Base(filepath.Dir(abs)) == "profiles" {
		root = filepath.Dir(filepath.Dir(abs))
	}
	return filepath.Join(root, "logs", "update_receipts")
}

// Stop of the run whose receipt carries correlationID, nil when the receipt is not that run's finished stop
func readStoppedRun(home string, correlationID *string) *UpdateStop {
	if correlationID == nil || *correlationID == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(receiptDirectory(home), "latest.json"))
	if err != nil {
		return nil
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil || parsed == nil || parsed["correlation_id"] != *correlationID {
		return nil
	}
	if finished, ok := parsed["finished_at"].(string); !ok || finished == "" {
		return nil
	}
	outcome, _ := parsed["outcome"].(string)
	if outcome != "failed" && outcome != "refused" {
		return nil
	}
	stopClass, _ := parsed["stop_class"].(string)
	if !stopClassToken.MatchString(stopClass) {
		return nil
	}
	return &UpdateStop{StopClass: stopClass, Outcome: outcome}
}

type stopExplanation struct {
	message string
	detail  string
}

var stopExplanations = map[string]stopExplanation{
	"downgrade_refused": {
		message: "Icebath kept your install: the offered release is older than it",
		detail: "The updater refused to move this install back to the release it offered, because that release predates the " +
			"gateway runtime your session stores already use. Downgrading is unsupported, and nothing was changed: you are " +
			"still on the version you had.\n\n" +
			"To follow this checkout instead of the release channel, run `icebath update --set-channel main`, or pick a " +
			"channel in About ▸ Updates.\n\n" + updatesGuide,
	},
}

// Explanation of a known stop class, nil for an unknown one
func explainUpdateStop(stopClass *string) *stopExplanation {
	if stopClass == nil {
		return nil
	}
	if explained, ok := stopExplanations[*stopClass]; ok {
		return &explained
	}
	return nil
}

// Programs the user can close from here, those whose executable path is readable
func closableReleaseHolders(holders []ReleaseHolder) []ReleaseHolder {
	var out []ReleaseHolder
	for _, holder := range holders {
		if holder.Exe != "" {
			out = append(out, holder)
		}
	}
	return out
}

func holderNames(holders []ReleaseHolder) string {
	names := make([]string, len(holders))
	for i, holder := range holders {
		names[i] = holder.Name
	}
	return strings.Join(names, ", ")
}

// Consent file the next promotion swap reads, inside the update root's desktop release dir
func releaseHolderConsentPath(updateRoot string) string {
	return filepath.Join(updateRoot, "apps", "desktop", "release", consentFileName)
}

// Reports a detached update's result once, where boot passes the update gate
func (s *Service) reportHandoffResult(run ParkedRun, closeHoldersAndRetry func([]ReleaseHolder)) {
	result := readAndConsumeHandoffResult(s.opts.IcebathHome, run.StartedAt, run.RunID, s.log, time.Now)
	switch {
	case result == nil:
	case result.OK && len(result.Warnings) > 0 && !result.Manual:
		s.log("[updates] detached update finished with warnings: " + strings.Join(result.Warnings, " | "))
		go s.host.MessageBox(MessageBox{Type: "info", Title: "Icebath update", Message: "Icebath updated, but some follow-up steps need another try",
			Detail: strings.Join(result.Warnings, "\n") + "\n\nHermes retries them on the next launch or the next update."})
	case result.OK && result.Manual && len(result.ReleaseHolders) > 0 && closeHoldersAndRetry != nil:
		s.reportReleaseHolders(result.ReleaseHolders, result.Message, closeHoldersAndRetry)
	case result.OK && result.Manual:
		s.log(fmt.Sprintf("[updates] detached update finished with manual action (branch %s): %s", result.Branch, result.Message))
		go s.host.MessageBox(MessageBox{Type: "warning", Title: "Icebath update", Message: "The update finished, but needs one more step", Detail: result.Message})
	case result.OK:
		s.log(fmt.Sprintf("[updates] detached update finished OK (branch %s)", result.Branch))
	default:
		s.reportFailedHandoff(*result)
	}
}

// A failed run names the stop its receipt recorded and offers that stop's way out
func (s *Service) reportFailedHandoff(result HandoffResult) {
	stop := readStoppedRun(s.opts.IcebathHome, result.RunID)
	var stopClass *string
	if stop != nil {
		stopClass = &stop.StopClass
	}
	explained := explainUpdateStop(stopClass)
	handoffLog := filepath.Join(s.opts.IcebathHome, "logs", "desktop-update-handoff.log")
	suffix := ""
	if stop != nil {
		suffix = fmt.Sprintf(" (stop_class %s)", stop.StopClass)
	}
	s.log(fmt.Sprintf("[updates] detached update FAILED (exit %s): %s%s", jsNumberText(result.ExitCode), result.Message, suffix))
	box := MessageBox{Type: "error", Title: "Icebath update", Message: "Icebath couldn't finish updating", DefaultID: 0, CancelID: 2, NoLink: true,
		Detail:  "You're still on the previous version and can keep using it. Try the update again, or open the update log to report the problem.\n\nDetails: " + result.Message,
		Buttons: []string{"Try again", "Open log", "Close"}}
	if explained != nil {
		box.Message, box.Detail = explained.message, explained.detail+"\n\nDetails: "+result.Message
		box.Buttons = []string{"Update settings", "Open log", "Close"}
	}
	go func() {
		switch s.host.MessageBox(box) {
		case 0:
			s.host.OpenUpdates()
		case 1:
			s.host.ShowItemInFolder(handoffLog)
		}
	}()
}

// Names the programs blocking the app swap and offers to close the closable ones on an explicit click
func (s *Service) reportReleaseHolders(holders []ReleaseHolder, message string, closeHoldersAndRetry func([]ReleaseHolder)) {
	names := holderNames(holders)
	closable := closableReleaseHolders(holders)
	closableNames := holderNames(closable)
	listed := make([]string, len(holders))
	for i, holder := range holders {
		where := holder.Exe
		if where == "" {
			where = "Close this one yourself (Task Manager)."
		}
		listed[i] = fmt.Sprintf("• %s (pid %d)\n   %s", holder.Name, holder.PID, where)
	}
	action := "Icebath cannot close it. Close it yourself (Task Manager), then run the update again."
	buttons, defaultID := []string{"OK"}, 0
	if len(closable) > 0 {
		action = "Icebath did not close it. Close it and retry the update now, or keep using the current app and update later."
		buttons, defaultID = []string{"Close " + closableNames + " and retry", "Not now"}, 1
	}
	s.log("[updates] Desktop app swap blocked by programs outside Icebath: " + names)
	box := MessageBox{Type: "warning", Title: "Icebath update", Message: names + " is blocking the Desktop app update",
		Detail: "Icebath updated, but Windows would not replace the Desktop app because this program, which is not part of Icebath, is using its folder:\n\n" +
			strings.Join(listed, "\n") + "\n\n" + action + "\n\nDetails: " + message,
		Buttons: buttons, DefaultID: defaultID, CancelID: defaultID, NoLink: true}
	go func() {
		if response := s.host.MessageBox(box); len(closable) > 0 && response == 0 {
			s.log(fmt.Sprintf("[updates] user consented to closing %s; retrying the update", closableNames))
			closeHoldersAndRetry(closable)
		}
	}()
}

// Message of a failed file call in Node's code and syscall wording
func nodeErrorMessage(err error) string {
	return hardening.NodeErr(err).Error()
}
