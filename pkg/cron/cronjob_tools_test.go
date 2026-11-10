package cron

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs the cronjob tool on the store with Python keyword arguments and decodes the JSON result
func runCronjob(t *testing.T, s *Store, a *cronjobArgs) map[string]any {
	t.Helper()
	out := s.cronjob(context.Background(), a)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return m
}

// Dispatches the registered tool handler the way the registry does
func dispatchCronjob(t *testing.T, s *Store, args map[string]any) map[string]any {
	t.Helper()
	ctx := context.WithValue(context.Background(), cronStoreKey{}, s)
	out := cronjobHandler(ctx, &tools.Call{Name: "cronjob_manage", Args: args, Env: &tools.Env{Home: s.home}})
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return m
}

func mustJob(t *testing.T, s *Store, id string) *Job {
	t.Helper()
	job, err := s.getJob(context.Background(), id)
	if err != nil || job == nil {
		t.Fatalf("job %s: %v %v", id, job, err)
	}
	return job
}

func TestCronjobRequirementsAcceptsInteractiveMode(t *testing.T) {
	t.Setenv("ICEBATH_INTERACTIVE", "1")
	t.Setenv("ICEBATH_GATEWAY_SESSION", "")
	t.Setenv("ICEBATH_EXEC_ASK", "")
	if !checkCronjobRequirements(nil) {
		t.Fatal("interactive mode rejected")
	}
}

func TestCronjobRequirementsAcceptsExternalWorkerWithPresenceVarsStripped(t *testing.T) {
	for _, v := range []string{"ICEBATH_INTERACTIVE", "ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK"} {
		t.Setenv(v, "")
	}
	t.Setenv("ICEBATH_CRON_SESSION", "1")
	if !checkCronjobRequirements(nil) {
		t.Fatal("cron session marker rejected")
	}
}

func TestCronjobRequirementsRejectsFalseLikeSessionEnv(t *testing.T) {
	vars := []string{"ICEBATH_INTERACTIVE", "ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK"}
	for _, name := range vars {
		for _, val := range []string{"0", "false", "no", "off"} {
			t.Run(name+"="+val, func(t *testing.T) {
				for _, v := range vars {
					t.Setenv(v, "")
				}
				t.Setenv("ICEBATH_CRON_SESSION", "")
				t.Setenv(name, val)
				if checkCronjobRequirements(nil) {
					t.Fatal("false like value accepted")
				}
			})
		}
	}
}

func TestCronjobCreateAndList(t *testing.T) {
	s, _ := newTestStore(t)
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Check server status", Schedule: "every 1h", Name: "Server Check"})
	if created["success"] != true {
		t.Fatalf("create failed %v", created)
	}
	listing := runCronjob(t, s, &cronjobArgs{Action: "list"})
	jobs := listing["jobs"].([]any)
	if listing["success"] != true || listing["count"] != float64(1) || len(jobs) != 1 {
		t.Fatalf("listing %v", listing)
	}
	j := jobs[0].(map[string]any)
	if j["name"] != "Server Check" || j["state"] != "scheduled" {
		t.Fatalf("listed %v", j)
	}
}

func TestCronjobCreateWithNaturalWeekdaySchedule(t *testing.T) {
	s, _ := newTestStore(t)
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Weekly report", Schedule: "every monday 9am", Name: "Weekly Report"})
	if created["success"] != true || created["schedule"] != "every monday 9am" || created["next_run_at"] == nil {
		t.Fatalf("created %v", created)
	}
	stored := mustJob(t, s, created["job_id"].(string))
	if stored.Schedule.Kind != "cron" || stored.Schedule.Expr != "0 9 * * 1" {
		t.Fatalf("schedule %+v", stored.Schedule)
	}
}

func TestCronjobUpdateToNaturalWeekdaySchedule(t *testing.T) {
	s, _ := newTestStore(t)
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Check", Schedule: "every 1h"})
	id := created["job_id"].(string)
	updated := runCronjob(t, s, &cronjobArgs{Action: "update", JobID: id, Schedule: "every day at 9am"})
	if updated["success"] != true || updated["job"].(map[string]any)["schedule"] != "every day at 9am" {
		t.Fatalf("updated %v", updated)
	}
	if expr := mustJob(t, s, id).Schedule.Expr; expr != "0 9 * * *" {
		t.Fatalf("expr %q", expr)
	}
}

func TestCronjobListHandlesPartialLegacyJobRecords(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{
		"id": "abc123deadbe", "name": nil, "prompt": nil, "schedule_display": nil,
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"},
		"repeat":   map[string]any{"times": nil, "completed": 0}, "enabled": true,
	}}})
	listing := runCronjob(t, s, &cronjobArgs{Action: "list"})
	j := listing["jobs"].([]any)[0].(map[string]any)
	if listing["success"] != true || j["name"] != "abc123deadbe" || j["prompt_preview"] != "" || j["schedule"] != "every 60m" {
		t.Fatalf("listed %v", j)
	}
}

func TestCronjobPauseAndResume(t *testing.T) {
	s, _ := newTestStore(t)
	id := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Check", Schedule: "every 1h"})["job_id"].(string)
	paused := runCronjob(t, s, &cronjobArgs{Action: "pause", JobID: id})
	if paused["success"] != true || paused["job"].(map[string]any)["state"] != "paused" {
		t.Fatalf("paused %v", paused)
	}
	resumed := runCronjob(t, s, &cronjobArgs{Action: "resume", JobID: id})
	if resumed["success"] != true || resumed["job"].(map[string]any)["state"] != "scheduled" {
		t.Fatalf("resumed %v", resumed)
	}
}

// Patches the resolver so custom:legit is a named custom provider at legit.example
func patchNamedLegit(t *testing.T) {
	saved := cronProviderResolver
	t.Cleanup(func() { cronProviderResolver = saved })
	cronProviderResolver.hasNamed = func(context.Context, string, string) bool { return true }
	cronProviderResolver.named = func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"name": "legit", "base_url": "https://legit.example/v1", "api_key": "sk-legit"}, nil
	}
}

// Writes a job holding an unsafe named provider and off host base_url pair straight to the store
func saveLegacyUnsafeJob(t *testing.T, s *Store) string {
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{
		"id": "legacyunsafe1", "name": "legacy", "prompt": "x",
		"schedule":         map[string]any{"kind": "interval", "minutes": 5, "display": "every 5m"},
		"schedule_display": "every 5m", "repeat": map[string]any{"times": nil, "completed": 0},
		"enabled": true, "state": "scheduled", "provider": "custom:legit", "base_url": "https://evil.example/v1",
	}}})
	return "legacyunsafe1"
}

func TestCronjobLegacyUnsafeJobBlockedOnUnrelatedUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	patchNamedLegit(t)
	id := saveLegacyUnsafeJob(t, s)
	out := s.cronjob(context.Background(), &cronjobArgs{Action: "update", JobID: id, Name: "renamed"})
	if !strings.Contains(out, `"success": false`) || !strings.Contains(out, "not allowed") {
		t.Fatalf("result %s", out)
	}
	stored := mustJob(t, s, id)
	if stored.Name != "legacy" || *stored.BaseURL != "https://evil.example/v1" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCronjobLegacyUnsafeJobRemediatedByMatchingHost(t *testing.T) {
	s, _ := newTestStore(t)
	patchNamedLegit(t)
	id := saveLegacyUnsafeJob(t, s)
	res := runCronjob(t, s, &cronjobArgs{Action: "update", JobID: id, BaseURL: "https://legit.example/v1"})
	if res["success"] != true || res["job"].(map[string]any)["base_url"] != "https://legit.example/v1" {
		t.Fatalf("result %v", res)
	}
}

func TestCronjobCreateAndUpdateNormalizeListFormDeliver(t *testing.T) {
	s, _ := newTestStore(t)
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Daily briefing", Schedule: "every 1h", Deliver: []any{"telegram"}})
	if created["success"] != true || *mustJob(t, s, created["job_id"].(string)).Deliver != "telegram" {
		t.Fatalf("created %v", created)
	}
	other := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "x", Schedule: "every 1h"})["job_id"].(string)
	updated := runCronjob(t, s, &cronjobArgs{Action: "update", JobID: other, Deliver: []any{"telegram"}})
	if updated["success"] != true || *mustJob(t, s, other).Deliver != "telegram" {
		t.Fatalf("updated %v", updated)
	}
}

func TestCronjobSchemaHasNoInferencePinParams(t *testing.T) {
	props := cronjobSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"model", "provider", "base_url"} {
		if _, ok := props[k]; ok {
			t.Fatalf("schema exposes %s", k)
		}
	}
}

func TestCronjobHandlerUpdateLeavesUserPinUntouched(t *testing.T) {
	s, _ := newTestStore(t)
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Check", Schedule: "every 1h", Model: "anthropic/claude-sonnet-4", Provider: "anthropic"})
	id := created["job_id"].(string)
	updated := dispatchCronjob(t, s, map[string]any{"action": "update", "job_id": id, "name": "renamed", "model": map[string]any{"model": "openai/gpt-4.1"}})
	if updated["success"] != true {
		t.Fatalf("updated %v", updated)
	}
	stored := mustJob(t, s, id)
	if *stored.Model != "anthropic/claude-sonnet-4" || *stored.Provider != "anthropic" || stored.Name != "renamed" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCronjobHandlerForwardsAttachToSession(t *testing.T) {
	s, _ := newTestStore(t)
	created := dispatchCronjob(t, s, map[string]any{"action": "create", "name": "Continuable cron canary", "schedule": "1h",
		"repeat": float64(1), "deliver": "origin", "attach_to_session": true, "prompt": "Reply exactly: canary"})
	id, _ := created["job_id"].(string)
	if created["success"] != true || created["job"].(map[string]any)["attach_to_session"] != true {
		t.Fatalf("created %v", created)
	}
	if a := mustJob(t, s, id).AttachToSession; a == nil || !*a {
		t.Fatal("attach_to_session not stored")
	}
	plain := dispatchCronjob(t, s, map[string]any{"action": "create", "name": "plain", "schedule": "1h", "prompt": "Reply exactly: canary"})
	pid := plain["job_id"].(string)
	if mustJob(t, s, pid).AttachToSession != nil {
		t.Fatal("omitted create stored attach_to_session")
	}
	for _, want := range []bool{true, false} {
		upd := dispatchCronjob(t, s, map[string]any{"action": "update", "job_id": pid, "attach_to_session": want})
		if upd["success"] != true || upd["job"].(map[string]any)["attach_to_session"] != want {
			t.Fatalf("update %v", upd)
		}
		if a := mustJob(t, s, pid).AttachToSession; a == nil || *a != want {
			t.Fatalf("stored attach %v", a)
		}
	}
	listing := dispatchCronjob(t, s, map[string]any{"action": "list"})
	for _, raw := range listing["jobs"].([]any) {
		j := raw.(map[string]any)
		if j["job_id"] == pid && j["attach_to_session"] != false {
			t.Fatalf("listed %v", j)
		}
	}
	bare := dispatchCronjob(t, s, map[string]any{"action": "create", "schedule": "1h", "prompt": "fire and forget"})
	if job := bare["job"].(map[string]any); job["attach_to_session"] != nil {
		t.Fatalf("formatted %v", job)
	}
}

func TestCronjobValidateCronScriptPath(t *testing.T) {
	s, _ := newTestStore(t)
	scripts := filepath.Join(s.home, "scripts")
	if msg := s.validateCronScriptPath("does_not_exist.sh"); !strings.Contains(strings.ToLower(msg), "not found") {
		t.Fatalf("missing script accepted %q", msg)
	}
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "real.sh"), []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := s.validateCronScriptPath("real.sh"); msg != "" {
		t.Fatalf("existing script rejected %q", msg)
	}
	if msg := s.validateCronScriptPath("missing.py"); !strings.Contains(msg, scripts) {
		t.Fatalf("missing message %q", msg)
	}
	if msg := s.validateCronScriptPath("/etc/passwd"); !strings.Contains(msg, scripts) {
		t.Fatalf("absolute message %q", msg)
	}
}

func TestCronjobOmittedDeliverNoOriginEmitsNotice(t *testing.T) {
	s, _ := newTestStore(t)
	for _, v := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_THREAD_ID", "ICEBATH_SESSION_CHAT_NAME"} {
		t.Setenv(v, "")
	}
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Output the time", Schedule: "every 2m"})
	if created["success"] != true || created["deliver"] != "local" || !strings.Contains(created["message"].(string), "local-only cron job") {
		t.Fatalf("created %v", created)
	}
}

func TestCronjobGatewayOriginNoNotice(t *testing.T) {
	s, _ := newTestStore(t)
	env := &tools.Env{Home: s.home, Values: &sync.Map{}}
	tools.BindSessionContext(env, map[string]string{"ICEBATH_SESSION_PLATFORM": "telegram", "ICEBATH_SESSION_CHAT_ID": "999"})
	testDeliver.Lock()
	testDeliver.targets = []Target{{Platform: "telegram", ChatID: "999", ResolvedFrom: "origin"}}
	testDeliver.Unlock()
	t.Cleanup(func() { testDeliver.Lock(); testDeliver.targets = nil; testDeliver.Unlock() })
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "x", Schedule: "every 2m", env: env})
	if created["deliver"] != "origin" || strings.Contains(created["message"].(string), "local-only cron job") {
		t.Fatalf("created %v", created)
	}
}

// Validates a provider and base_url pair through the store guard
func validateBase(t *testing.T, s *Store, prov, bu any) string {
	t.Helper()
	msg, err := s.validateCronBaseURL(context.Background(), prov, bu)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestValidateCronBaseURLNamedCustomProvider(t *testing.T) {
	s, _ := newTestStore(t)
	patchNamedLegit(t)
	if msg := validateBase(t, s, "custom:legit", "https://evil.example/v1"); !strings.Contains(msg, "not allowed") {
		t.Fatalf("off host allowed %q", msg)
	}
	for _, bu := range []string{"https://legit.example/v1", "https://LEGIT.example.:443/v1/"} {
		if msg := validateBase(t, s, "custom:legit", bu); msg != "" {
			t.Fatalf("matching host %s refused %q", bu, msg)
		}
	}
	if validateBase(t, s, "custom:legit", "https://legit.example.attacker.test/v1") == "" {
		t.Fatal("lookalike host allowed")
	}
}

// Off origin variants of a host that must all be refused for the provider
func refusedOffOrigin(t *testing.T, s *Store, prov, h string) {
	t.Helper()
	for _, bu := range []string{"http://" + h + "/v1", "http://" + h + ":443/v1", "https://" + h + ":8443/v1", "https://eu." + h + "/v1", h + "/v1", "http://[::1/v1"} {
		if msg := validateBase(t, s, prov, bu); !strings.Contains(msg, "not allowed") {
			t.Fatalf("%s %s allowed %q", prov, bu, msg)
		}
	}
}

func TestValidateCronBaseURLStoredKeyOnlyGoesToTheConfiguredOrigin(t *testing.T) {
	s, _ := newTestStore(t)
	saved := cronProviderResolver
	t.Cleanup(func() { cronProviderResolver = saved })
	cronProviderResolver.hasNamed = func(context.Context, string, string) bool { return false }
	cronProviderResolver.registryURL = func(id string) (string, bool) {
		if id == "anthropic" {
			return "https://api.anthropic.com", true
		}
		return "", false
	}
	if msg := validateBase(t, s, "anthropic", "https://api.anthropic.com"); msg != "" {
		t.Fatalf("registry origin refused %q", msg)
	}
	refusedOffOrigin(t, s, "anthropic", "api.anthropic.com")
	if validateBase(t, s, "anthropic", "https://evil.example/v1") == "" {
		t.Fatal("registry off host allowed")
	}
	patchNamedLegit(t)
	refusedOffOrigin(t, s, "custom:legit", "legit.example")
}

func TestValidateCronBaseURLBareCustomSendsAStoredKeyOnlyToAConfiguredOrigin(t *testing.T) {
	s, _ := newTestStore(t)
	configured := "https://api.acmellm.example/v1"
	if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("custom_providers:\n  - name: acme\n    base_url: "+configured+"\n  - name: broken\n    base_url: http://[::1/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := cronProviderResolver
	t.Cleanup(func() { cronProviderResolver = saved })
	cronProviderResolver.hostGated = func(_ context.Context, _, bu string, _ bool) []string {
		if strings.Contains(bu, "acmellm.example") {
			return []string{"sk-stored-acmellm-key"}
		}
		return []string{""}
	}
	for _, bu := range []string{"http://api.acmellm.example:8080/v1", "http://api.acmellm.example/v1", "https://api.acmellm.example:8443/v1", "https://eu.acmellm.example/v1"} {
		if msg := validateBase(t, s, "custom", bu); !strings.Contains(msg, "not allowed") {
			t.Fatalf("%s allowed %q", bu, msg)
		}
	}
	if msg := validateBase(t, s, "custom", "http://[::1/v1"); !strings.Contains(msg, "not allowed") || !strings.Contains(msg, "not a valid URL") {
		t.Fatalf("broken url %q", msg)
	}
	if msg := validateBase(t, s, "custom", configured); msg != "" {
		t.Fatalf("configured origin refused %q", msg)
	}
	if msg := validateBase(t, s, "custom", "http://192.0.2.10:8080/v1"); msg != "" {
		t.Fatalf("keyless endpoint refused %q", msg)
	}
}

func TestValidateCronBaseURLWithoutProviderRejected(t *testing.T) {
	s, _ := newTestStore(t)
	if validateBase(t, s, nil, "https://x.example/v1") == "" {
		t.Fatal("base_url without provider allowed")
	}
}
