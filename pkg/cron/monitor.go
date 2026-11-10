package cron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	monitorMaxDiffChars     = 4000
	monitorMaxOutputChars   = 8000
	monitorURLTimeout       = 30 * time.Second
	monitorMaxURLBytes      = 262_144
	monitorSnapshotFilename = "monitor_last_output.txt"
)

// MonitorOutcome is the result of one monitor source evaluation
type MonitorOutcome struct {
	OK           bool
	Changed      bool
	FirstRun     bool
	ContextBlock *string
	Error        *string
}

// Hex sha256 of the exact UTF-8 bytes of a monitor output
func hashMonitorOutput(output string) string {
	sum := sha256.Sum256([]byte(output))
	return hex.EncodeToString(sum[:])
}

// Unified diff of old and new output capped at the diff character budget
func buildMonitorDiff(oldText, newText string) string {
	lines := tools.UnifiedDiff(pystr.Splitlines(oldText, false), pystr.Splitlines(newText, false), "previous", "current", 3)
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\n")
	}
	diff := strings.Join(lines, "\n")
	if len([]rune(diff)) > monitorMaxDiffChars {
		diff = headRunes(diff, monitorMaxDiffChars) + "\n... [diff truncated]"
	}
	return diff
}

// Snapshot file holding the last agent triggering output of a job
func (s *Store) monitorSnapshotPath(jobID string) (string, error) {
	dir, err := s.jobOutputDir(jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, monitorSnapshotFilename), nil
}

// Last persisted monitor output, empty when missing or unreadable
func (s *Store) readLastMonitorOutput(jobID string) string {
	path, err := s.monitorSnapshotPath(jobID)
	if err == nil {
		var raw []byte
		if raw, err = os.ReadFile(path); err == nil {
			return strings.TrimPrefix(pystr.DecodeUTF8Replace(raw), "\ufeff")
		}
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
	}
	s.log.Warn(fmt.Sprintf("Monitor: failed to read last output for %s: %v", pyrepr.Repr(jobID), err))
	return ""
}

// Persists the monitor output snapshot, logging instead of failing
func (s *Store) writeLastMonitorOutput(jobID, output string) {
	path, err := s.monitorSnapshotPath(jobID)
	if err == nil {
		if err = ensureCronDir(filepath.Dir(path)); err == nil {
			err = os.WriteFile(path, []byte(output), 0o666)
		}
	}
	if err != nil {
		s.log.Warn(fmt.Sprintf("Monitor: failed to persist last output for %s: %v", pyrepr.Repr(jobID), err))
	}
}

// Client of monitor URL fetches, swapped by tests
var monitorHTTPClient = &http.Client{Timeout: monitorURLTimeout}

// Bounded GET of a monitor URL returning ok and the body or the error text
func fetchMonitorURL(ctx context.Context, url string) (bool, string) {
	lower := pystr.Lower(url)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false, "monitor_url must be http(s): " + pyrepr.Repr(url)
	}
	ctx, cancel := context.WithTimeout(ctx, monitorURLTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "monitor_url fetch failed: " + err.Error()
	}
	req.Header.Set("User-Agent", "icebath-cron-monitor")
	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		return false, "monitor_url fetch failed: <urlopen error " + err.Error() + ">"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, fmt.Sprintf("monitor_url fetch failed: HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, monitorMaxURLBytes+1))
	if err != nil {
		return false, "monitor_url fetch failed: " + err.Error()
	}
	if len(body) > monitorMaxURLBytes {
		body = body[:monitorMaxURLBytes]
	}
	return true, pystr.DecodeUTF8Replace(body)
}

// Stripped string value of an optional job field
func monitorField(v *string) string {
	if v == nil {
		return ""
	}
	return pystr.Strip(*v)
}

// Runs the job's monitor script or URL and returns ok and its output
func (s *Store) runMonitorSource(ctx context.Context, job *Job) (bool, string, error) {
	if script := monitorField(job.MonitorScript); script != "" {
		return s.runJobScript(ctx, script, monitorField(job.Workdir), nil, job.Interpreter)
	}
	if url := monitorField(job.MonitorURL); url != "" {
		ok, out := fetchMonitorURL(ctx, url)
		return ok, out, nil
	}
	return false, "monitor job has neither monitor_script nor monitor_url", nil
}

// True when the job carries a monitor script or URL
func jobHasMonitor(job *Job) bool {
	return monitorField(job.MonitorScript) != "" || monitorField(job.MonitorURL) != ""
}

// Runs the monitor source and decides whether the agent runs, persisting a change before it does
func (s *Store) checkMonitor(ctx context.Context, job *Job) (MonitorOutcome, error) {
	ok, output, err := s.runMonitorSource(ctx, job)
	if err != nil {
		return MonitorOutcome{}, err
	}
	if !ok {
		return MonitorOutcome{OK: false, Error: &output}, nil
	}
	newHash := hashMonitorOutput(output)
	lastHash := ""
	if job.MonitorState != nil {
		lastHash = job.MonitorState.LastOutputHash
	}
	if lastHash != "" && newHash == lastHash {
		return MonitorOutcome{OK: true}, nil
	}
	firstRun := lastHash == ""
	oldOutput := ""
	if !firstRun {
		oldOutput = s.readLastMonitorOutput(job.ID)
	}
	shown := output
	if len([]rune(shown)) > monitorMaxOutputChars {
		shown = headRunes(shown, monitorMaxOutputChars) + "\n... [output truncated]"
	}
	current := "### Current output\n\n```\n" + shown + "\n```"
	var block string
	if firstRun {
		block = "## Monitor Baseline (first run)\n\n" +
			"This is the first observation of the monitored source — there is " +
			"no previous output to diff against.\n\n" + current
	} else {
		block = "## MONITOR CHANGE DETECTED\n\n" +
			"The monitored source's output changed since the last run.\n\n" +
			"### Diff (previous → current)\n\n```diff\n" + buildMonitorDiff(oldOutput, output) + "\n```\n\n" + current
	}
	s.persistMonitorState(home.WithOverride(ctx, s.home), job.ID, newHash, output)
	return MonitorOutcome{OK: true, Changed: true, FirstRun: firstRun, ContextBlock: &block}, nil
}

// Writes the snapshot and stores the new hash with its change time on the job
func (s *Store) persistMonitorState(ctx context.Context, jobID, newHash, output string) {
	s.writeLastMonitorOutput(jobID, output)
	state := map[string]any{"last_output_hash": newHash, "last_changed_at": FormatISO(s.clk.now())}
	if _, err := s.updateJob(home.WithOverride(ctx, s.home), jobID, map[string]any{"monitor_state": state}); err != nil {
		s.log.Warn(fmt.Sprintf("Monitor: failed to persist state for %s: %v", pyrepr.Repr(jobID), err))
	}
}
