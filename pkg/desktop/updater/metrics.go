package updater

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	pendingUpdateRunFile = "update-metric-pending.json"
	pendingTTL           = 30 * 24 * time.Hour
)

// PendingUpdateRun is the update run recorded across a relaunch until the renderer reports it
type PendingUpdateRun struct {
	V              int      `json:"v"`
	Mechanism      string   `json:"mechanism"`
	StartedAt      float64  `json:"startedAt"`
	FromVersion    string   `json:"fromVersion"`
	FromCommitDate *float64 `json:"fromCommitDate,omitempty"`
	Outcome        string   `json:"outcome,omitempty"`
	FailedStage    string   `json:"failedStage,omitempty"`
	DurationMs     *float64 `json:"durationMs,omitempty"`
}

// UpdateRunReport is the metric the renderer sends for one finished update run
type UpdateRunReport struct {
	Outcome        string   `json:"outcome"`
	Mechanism      string   `json:"mechanism"`
	DurationMs     float64  `json:"duration_ms"`
	FailedStage    string   `json:"failed_stage,omitempty"`
	FromCommitDate *float64 `json:"from_commit_date,omitempty"`
}

// Records packaged update runs to a pending file the renderer takes and acknowledges once
type updateRunRecorder struct {
	dir        func() string
	appVersion func() string
	now        func() time.Time
	onRecorded func()

	mu        sync.Mutex
	inFlight  bool
	active    *PendingUpdateRun
	stage     string
	handedOff bool
}

func (r *updateRunRecorder) file() string {
	return filepath.Join(r.dir(), pendingUpdateRunFile)
}

func (r *updateRunRecorder) nowMS() float64 {
	return float64(r.now().UnixMilli())
}

func (r *updateRunRecorder) read() *PendingUpdateRun {
	raw, err := os.ReadFile(r.file())
	if err != nil {
		return nil
	}
	var probe map[string]any
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	_, mechanismOK := probe["mechanism"].(string)
	_, startedOK := probe["startedAt"].(float64)
	_, versionOK := probe["fromVersion"].(string)
	var run PendingUpdateRun
	if probe["v"] == float64(1) && mechanismOK && startedOK && versionOK && json.Unmarshal(raw, &run) == nil &&
		r.nowMS()-run.StartedAt <= float64(pendingTTL.Milliseconds()) {
		return &run
	}
	_ = os.RemoveAll(r.file())
	return nil
}

// Report of the pending run once per launch, nil while a run is active, handed off or already taken
func (r *updateRunRecorder) take() *UpdateRunReport {
	r.mu.Lock()
	if r.active != nil || r.handedOff || r.inFlight {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	run := r.read()
	if run == nil {
		return nil
	}
	r.mu.Lock()
	r.inFlight = true
	r.mu.Unlock()
	return r.report(*run)
}

// Settles a taken report, deleting the pending file once the report was sent
func (r *updateRunRecorder) ack(sent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.inFlight {
		return
	}
	if sent {
		_ = os.RemoveAll(r.file())
	}
	r.inFlight = false
}

func (r *updateRunRecorder) report(run PendingUpdateRun) *UpdateRunReport {
	outcome, failedStage := run.Outcome, run.FailedStage
	if outcome == "" {
		outcome = "failed"
		if r.appVersion() != run.FromVersion {
			outcome = "success"
		}
		failedStage = "apply"
	}
	duration := math.Max(0, r.nowMS()-run.StartedAt)
	if run.DurationMs != nil {
		duration = *run.DurationMs
	}
	report := &UpdateRunReport{Outcome: outcome, Mechanism: run.Mechanism, DurationMs: duration, FromCommitDate: run.FromCommitDate}
	if outcome == "failed" {
		report.FailedStage = failedStage
		if report.FailedStage == "" {
			report.FailedStage = "other"
		}
	}
	return report
}

// Report of the update run a previous launch recorded, nil when there is none to send
func (s *Service) UpdatesTakePendingRun() *UpdateRunReport {
	return s.metrics.take()
}

// Acknowledges the taken update run report, sent deleting it
func (s *Service) UpdatesAckPendingRun(sent any) {
	s.metrics.ack(sent == true)
}

var failedStageByProgress = map[string]string{"fetch": "download", "prepare": "verify", "restart": "apply"}

// Failure stage a progress stage maps to, other for the rest
func failedStageForProgress(stage string) string {
	if mapped, ok := failedStageByProgress[stage]; ok {
		return mapped
	}
	return "other"
}

func settledOutcome(result ApplyResult) string {
	if result.HandedOff {
		return ""
	}
	if !result.OK || result.Manual {
		return "refused"
	}
	return "noop"
}

// Applies through apply, recording the run only for a packaged mechanism
func (r *updateRunRecorder) track(mechanism string, fromCommitDate *float64, apply func() (ApplyResult, error)) (ApplyResult, error) {
	if mechanism == "" {
		return apply()
	}
	r.begin(mechanism, fromCommitDate)
	result, err := apply()
	if err != nil {
		r.finish("failed")
		return result, err
	}
	r.finish(settledOutcome(result))
	return result, nil
}

// Remembers the latest failure bearing progress stage of the active run
func (r *updateRunRecorder) noteProgress(stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := failedStageByProgress[stage]; r.active != nil && ok {
		r.stage = stage
	}
}

func (r *updateRunRecorder) begin(mechanism string, fromCommitDate *float64) {
	run := PendingUpdateRun{V: 1, Mechanism: mechanism, StartedAt: r.nowMS(), FromVersion: r.appVersion(), FromCommitDate: fromCommitDate}
	r.mu.Lock()
	r.active, r.stage = &run, ""
	r.mu.Unlock()
	r.write(run)
}

func (r *updateRunRecorder) finish(outcome string) {
	r.mu.Lock()
	active, stage := r.active, r.stage
	r.active = nil
	if active == nil {
		r.mu.Unlock()
		return
	}
	if outcome == "" {
		r.handedOff = true
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	done := *active
	duration := r.nowMS() - active.StartedAt
	done.Outcome, done.DurationMs = outcome, &duration
	if outcome == "failed" {
		done.FailedStage = failedStageForProgress(stage)
	}
	r.write(done)
	if r.onRecorded != nil {
		r.onRecorded()
	}
}

func (r *updateRunRecorder) write(run PendingUpdateRun) {
	body, err := json.Marshal(run)
	if err != nil {
		return
	}
	tmp := r.file() + ".tmp"
	if os.MkdirAll(filepath.Dir(tmp), 0o777) != nil || os.WriteFile(tmp, body, 0o666) != nil {
		return
	}
	_ = os.Rename(tmp, r.file())
}
