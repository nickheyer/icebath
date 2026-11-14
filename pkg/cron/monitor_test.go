package cron

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMonitorScript(t *testing.T, s *Store, name, body string) string {
	t.Helper()
	dir := filepath.Join(s.home, "scripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return name
}

func wantErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatal(err, sub)
	}
}

func TestMonitorCreateAndUpdateInvariants(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strp("p"), Schedule: "every 5m", MonitorScript: "mon.sh", MonitorURL: "https://example.com/status"})
	wantErrContains(t, err, "monitor_script and monitor_url")
	writeMonitorScript(t, s, "w.sh", "echo hi\n")
	writeMonitorScript(t, s, "mon.sh", "echo stable\n")
	_, err = s.createJob(context.Background(), CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, MonitorScript: "w.sh"})
	wantErrContains(t, err, "no_agent")
	mon, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strp("React to the change"), Schedule: "every 5m", MonitorScript: "mon.sh", Deliver: strp("local")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.updateJob(context.Background(), mon.ID, map[string]any{"no_agent": true, "script": "w.sh"})
	wantErrContains(t, err, "no_agent")
	_, err = s.updateJob(context.Background(), mon.ID, map[string]any{"monitor_url": "https://example.com/status"})
	wantErrContains(t, err, "mutually exclusive")
	noAgent, err := s.createJob(context.Background(), CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: strp("local")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.updateJob(context.Background(), noAgent.ID, map[string]any{"monitor_script": "mon.sh"})
	wantErrContains(t, err, "no_agent")
	if _, err := s.updateJob(context.Background(), mon.ID, map[string]any{"monitor_script": "", "no_agent": true, "script": "w.sh"}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := s.getJob(context.Background(), mon.ID)
	if reloaded.MonitorScript != nil || !reloaded.NoAgent {
		t.Fatal(reloaded.MonitorScript, reloaded.NoAgent)
	}
}

func TestHashIsExactBytes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if hashMonitorOutput("a\nb") != hashMonitorOutput("a\nb") || hashMonitorOutput("a\nb") == hashMonitorOutput("a\nb ") {
		t.Fatal("hash must be exact bytes")
	}
}

func TestUnifiedDiffIsCapped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var oldL, newL []string
	for i := 0; i < 5000; i++ {
		oldL = append(oldL, fmt.Sprintf("line %d", i))
		newL = append(newL, fmt.Sprintf("LINE %d", i))
	}
	diff := buildMonitorDiff(strings.Join(oldL, "\n"), strings.Join(newL, "\n"))
	if len(diff) > monitorMaxDiffChars+200 || !strings.Contains(diff, "truncated") || !strings.HasPrefix(diff, "--- previous\n+++ current\n@@ ") {
		t.Fatal(len(diff), diff[:60])
	}
}

func TestCheckMonitorBaselineSuppressionDiffAndFailure(t *testing.T) {
	s, _ := newTestStore(t)
	writeMonitorScript(t, s, "mon.sh", "echo 'state A'\n")
	job, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strp("Summarize what changed"), Model: "test-model", Schedule: "every 5m", MonitorScript: "mon.sh", Deliver: strp("local")})
	if err != nil {
		t.Fatal(err)
	}
	if !jobHasMonitor(job) {
		t.Fatal("monitor job not recognised")
	}
	ctx := context.Background()
	out, err := s.checkMonitor(ctx, job)
	if err != nil || !out.OK || !out.Changed || !out.FirstRun || !strings.Contains(*out.ContextBlock, "## Monitor Baseline (first run)") || !strings.Contains(*out.ContextBlock, "state A") {
		t.Fatal(out, err)
	}
	reopened := openCronStore(s.home, nil)
	job, _ = reopened.getJob(ctx, job.ID)
	if job.MonitorState == nil || job.MonitorState.LastOutputHash == "" || job.MonitorState.LastChangedAt == "" {
		t.Fatal(job.MonitorState)
	}
	stored := job.MonitorState.LastOutputHash
	if out, err = reopened.checkMonitor(ctx, job); err != nil || !out.OK || out.Changed || out.ContextBlock != nil {
		t.Fatal("unchanged output must suppress", out, err)
	}
	writeMonitorScript(t, s, "mon.sh", "echo 'state B'\n")
	out, err = reopened.checkMonitor(ctx, job)
	if err != nil || !out.Changed || out.FirstRun {
		t.Fatal(out, err)
	}
	for _, want := range []string{"## MONITOR CHANGE DETECTED", "-state A", "+state B", "### Current output\n\n```\nstate B"} {
		if !strings.Contains(*out.ContextBlock, want) {
			t.Fatal(want, *out.ContextBlock)
		}
	}
	job, _ = reopened.getJob(ctx, job.ID)
	stored = job.MonitorState.LastOutputHash
	writeMonitorScript(t, s, "mon.sh", "echo boom >&2\nexit 3\n")
	if out, err = reopened.checkMonitor(ctx, job); err != nil || out.OK || out.Error == nil {
		t.Fatal("failure must be an error, not a change", out, err)
	}
	if job, _ = reopened.getJob(ctx, job.ID); job.MonitorState.LastOutputHash != stored {
		t.Fatal("failed source touched the stored hash")
	}
}

func TestFetchMonitorURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("User-Agent") != "icebath-cron-monitor" {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(strings.Repeat("x", monitorMaxURLBytes+10)))
	}))
	defer srv.Close()
	ctx := context.Background()
	if ok, body := fetchMonitorURL(ctx, srv.URL+"/ok"); !ok || len(body) != monitorMaxURLBytes {
		t.Fatal(ok, len(body))
	}
	if ok, body := fetchMonitorURL(ctx, srv.URL+"/missing"); ok || body != "monitor_url fetch failed: HTTP Error 404: Not Found" {
		t.Fatal(ok, body)
	}
	if ok, body := fetchMonitorURL(ctx, "ftp://example.com/x"); ok || body != "monitor_url must be http(s): 'ftp://example.com/x'" {
		t.Fatal(ok, body)
	}
	s, _ := newTestStore(t)
	ok, body, err := s.runMonitorSource(ctx, &Job{ID: "j"})
	if ok || err != nil || body != "monitor job has neither monitor_script nor monitor_url" {
		t.Fatal(ok, body, err)
	}
}
