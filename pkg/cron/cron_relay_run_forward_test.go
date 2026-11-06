package cron

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Fronts the given platforms and resolves every job to the given delivery targets
func frontRelay(t *testing.T, fronted []string, targets []Target) {
	t.Helper()
	prev := relayFrontedPlatformsSeam
	t.Cleanup(func() { relayFrontedPlatformsSeam = prev })
	set := map[string]bool{}
	for _, p := range fronted {
		set[p] = true
	}
	relayFrontedPlatformsSeam = func(context.Context) map[string]bool { return set }
	resetDeliver(t)
	testDeliver.Lock()
	testDeliver.targets = targets
	testDeliver.Unlock()
}

type forwardSeen struct {
	path, auth string
	body       map[string]any
}

// Serves the api_server run route on loopback with the given status and records the request
func serveRunRoute(t *testing.T, status int) *forwardSeen {
	t.Helper()
	seen := &forwardSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.auth = r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&seen.body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	t.Setenv("API_SERVER_PORT", port)
	t.Setenv("API_SERVER_HOST", "")
	return seen
}

func forwardOut(t *testing.T, s *Store, id string, extra *string) map[string]any {
	t.Helper()
	raw := s.forwardRelayFrontedRun(context.Background(), &Job{ID: id}, extra)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("forward %q: %v", raw, err)
	}
	return out
}

func TestRelayFrontedDeliveryPlatforms(t *testing.T) {
	s, _ := newTestStore(t)
	job := &Job{ID: "j1", Deliver: strPtr("discord")}
	frontRelay(t, []string{"discord"}, []Target{{Platform: "discord", ChatID: "123"}})
	if got := s.relayFrontedDeliveryPlatforms(context.Background(), job); len(got) != 1 || !got["discord"] {
		t.Fatalf("fronted %v", got)
	}
	frontRelay(t, []string{"telegram"}, []Target{{Platform: "discord", ChatID: "123"}})
	if got := s.relayFrontedDeliveryPlatforms(context.Background(), job); len(got) != 0 {
		t.Fatalf("not fronted %v", got)
	}
}

func TestForwardRelayFrontedRunNativeTopology(t *testing.T) {
	s, _ := newTestStore(t)
	frontRelay(t, nil, nil)
	if got := s.forwardRelayFrontedRun(context.Background(), &Job{ID: "j1"}, nil); got != "" {
		t.Fatalf("forwarded %q", got)
	}
}

func TestForwardRelayFrontedRunOutcomes(t *testing.T) {
	s, _ := newTestStore(t)
	frontRelay(t, []string{"discord"}, []Target{{Platform: "discord", ChatID: "123"}})
	serveRunRoute(t, 200)
	if out := forwardOut(t, s, "j1", nil); out["success"] != true || out["forwarded_to_gateway"] != true {
		t.Fatalf("success %v", out)
	}
	serveRunRoute(t, 401)
	if out := forwardOut(t, s, "j1", nil); out["success"] != false || !strings.Contains(out["error"].(string), "relay-fronted") {
		t.Fatalf("4xx %v", out)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	t.Setenv("API_SERVER_PORT", strconv.Itoa(port))
	if out := forwardOut(t, s, "j1", nil); out["success"] != false || !strings.Contains(out["error"].(string), "relay-fronted") {
		t.Fatalf("unreachable %v", out)
	}
}

func TestForwardRelayFrontedRunPostsToRunRouteWithBearer(t *testing.T) {
	s, _ := newTestStore(t)
	frontRelay(t, []string{"discord"}, []Target{{Platform: "discord", ChatID: "123"}})
	seen := serveRunRoute(t, 200)
	t.Setenv("API_SERVER_KEY", "secret-key-16chars")
	forwardOut(t, s, "abc123", nil)
	if seen.path != "/api/jobs/abc123/run" || seen.auth != "Bearer secret-key-16chars" {
		t.Fatalf("seen %+v", seen)
	}
	if !strings.HasPrefix(s.apiServerBaseURL(), "http://127.0.0.1:") {
		t.Fatalf("base %q", s.apiServerBaseURL())
	}
	forwardOut(t, s, "j1", strPtr("focus on EU numbers"))
	if len(seen.body) != 1 || seen.body["prompt"] != "focus on EU numbers" {
		t.Fatalf("body %v", seen.body)
	}
}

func TestAPIServerBaseURLHonorsHostEnv(t *testing.T) {
	s, _ := newTestStore(t)
	t.Setenv("API_SERVER_HOST", "10.9.8.7")
	if got := s.apiServerBaseURL(); !strings.HasPrefix(got, "http://10.9.8.7:") {
		t.Fatalf("base %q", got)
	}
	t.Setenv("API_SERVER_HOST", "0.0.0.0")
	if got := s.apiServerBaseURL(); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("wildcard base %q", got)
	}
}

// Runs run_one_job on the job with run_job replaced and returns the extra prompt it received
func consumedExtraPrompt(t *testing.T, s *Store, job *Job, explicit *string) *string {
	t.Helper()
	var got *string
	prev := runJobSeam
	t.Cleanup(func() { runJobSeam = prev })
	runJobSeam = func(ctx context.Context, s *Store, job *Job, o runJobOptions) (jobRunOutcome, error) {
		got = o.ExtraPrompt
		return jobRunOutcome{Success: true, FinalResponse: "ok"}, nil
	}
	resetDeliver(t)
	if _, err := runOneJob(context.Background(), s, job, runOneJobOptions{ExtraPrompt: explicit}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestManualRunPromptConsumption(t *testing.T) {
	s, _ := newTestStore(t)
	mk := func(at *string, prompt string) *Job {
		job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("report"), Schedule: "every 1h", Deliver: strPtr("local")})
		job.ManualRunAt, job.ManualRunPrompt = at, strPtr(prompt)
		return job
	}
	stamp := strPtr("2026-08-28T00:00:00+00:00")
	if got := consumedExtraPrompt(t, s, mk(stamp, "focus on EU numbers"), nil); got == nil || *got != "focus on EU numbers" {
		t.Fatalf("stamped %v", got)
	}
	if got := consumedExtraPrompt(t, s, mk(stamp, "stale stamp"), strPtr("direct context")); got == nil || *got != "direct context" {
		t.Fatalf("explicit %v", got)
	}
	if got := consumedExtraPrompt(t, s, mk(nil, "orphaned"), nil); got != nil {
		t.Fatalf("orphaned %v", *got)
	}
}

func TestTriggerJobPromptStamp(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("daily report"), Schedule: "0 9 * * *", Name: strPtr("stamp-test")})
	triggered, err := s.triggerJob(job.ID, "just EU today")
	if err != nil || deref(triggered.ManualRunPrompt) != "just EU today" || deref(triggered.ManualRunAt) != deref(triggered.NextRunAt) {
		t.Fatalf("triggered %+v %v", triggered, err)
	}
	s.markJobRun(ctx, job.ID, true, MarkOptions{})
	if after := mustJob(t, s, job.ID); after.ManualRunPrompt != nil || after.ManualRunAt != nil {
		t.Fatalf("stamp kept %+v", after)
	}
	other := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("daily report"), Schedule: "0 9 * * *", Name: strPtr("stamp-test-2")})
	s.triggerJob(other.ID, "first context")
	if re, _ := s.triggerJob(other.ID, ""); re.ManualRunPrompt != nil {
		t.Fatalf("stale stamp %v", *re.ManualRunPrompt)
	}
}
