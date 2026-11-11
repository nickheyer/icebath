package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Isolated default home with one named profile, both with a cron dir and a config
func cronIsolatedProfiles(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	worker := filepath.Join(root, "profiles", "worker_alpha")
	for _, h := range []string{root, worker} {
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model: test-model\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	return map[string]string{"default": root, "worker_alpha": worker}
}

// Creates a job in a profile store the way the dashboard create path does
func cronCreateIn(t *testing.T, profile string, pairs ...any) *pyjson.Object {
	t.Helper()
	spec := pyjson.ObjectOf(pairs...)
	job, err := callCronForProfile(context.Background(), profile, func(ctx context.Context, h string) (any, error) {
		return cron.CreateJobWithSchedulerRegistration(ctx, h, spec)
	})
	if err != nil {
		t.Fatal(err)
	}
	return job.(*pyjson.Object)
}

// Test client over a fresh app with only the cron router
func cronClient(t *testing.T) *testClient {
	app := NewFastAPI("t", "1", nil, nil)
	app.IncludeRouter(cronRouter(), "")
	return newTestClient(t, app)
}

// Sends a JSON body to the cron router
func cronSend(c *testClient, method, target string, body any) *testResponse {
	raw, _ := json.Marshal(body)
	return c.request(method, target, raw, "content-type", "application/json")
}

// Decoded object of a response, failing on a non object body
func cronObj(t *testing.T, r *testResponse) *pyjson.Object {
	t.Helper()
	o, ok := r.JSON(t).(*pyjson.Object)
	if !ok {
		t.Fatalf("not an object: %s", r.Body)
	}
	return o
}

// Ids of the jobs a list response holds
func cronIDs(t *testing.T, r *testResponse) []any {
	t.Helper()
	list, ok := r.JSON(t).([]any)
	if !ok {
		t.Fatalf("not a list: %d %s", r.Status, r.Body)
	}
	out := []any{}
	for _, j := range list {
		out = append(out, j.(*pyjson.Object).GetOr("id", nil))
	}
	return out
}

func TestStandaloneJobsRemainDiscoverableForDashboardManagement(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	if err := os.WriteFile(filepath.Join(homes["worker_alpha"], "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := cronCreateIn(t, "worker_alpha", "prompt", "local job", "schedule", "every 1h")
	ctx := context.Background()
	found := false
	for _, p := range cronProfileDicts(ctx) {
		if p.GetOr("name", nil) == "worker_alpha" {
			found = true
		}
	}
	if !found {
		t.Fatal("worker_alpha not listed")
	}
	owner, err := findCronJobProfile(ctx, job.GetOr("id", "").(string))
	if err != nil || owner != "worker_alpha" {
		t.Fatalf("owner %q %v", owner, err)
	}
}

func TestCronMutationWithoutProfileFindsNamedProfileJob(t *testing.T) {
	cronIsolatedProfiles(t)
	job := cronCreateIn(t, "worker_alpha", "prompt", "managed by named profile", "schedule", "every 1h", "name", "named-profile-job")
	c := cronClient(t)
	paused := cronObj(t, c.request("POST", "/api/cron/jobs/"+job.GetOr("id", "").(string)+"/pause", nil))
	if paused.GetOr("profile", nil) != "worker_alpha" || paused.GetOr("enabled", nil) != false {
		t.Fatalf("paused %v", paused)
	}
	if ids := cronIDs(t, c.get("/api/cron/jobs?profile=default")); len(ids) != 0 {
		t.Fatalf("default jobs %v", ids)
	}
	workerList := c.get("/api/cron/jobs?profile=worker_alpha").JSON(t).([]any)
	if len(workerList) != 1 || workerList[0].(*pyjson.Object).GetOr("id", nil) != job.GetOr("id", nil) ||
		workerList[0].(*pyjson.Object).GetOr("enabled", nil) != false {
		t.Fatalf("worker jobs %v", workerList)
	}
}

func TestUpdateCronJobNormalizesDashboardCoreFields(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	scripts := filepath.Join(homes["worker_alpha"], "scripts")
	if err := os.Mkdir(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "collect.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := cronCreateIn(t, "worker_alpha", "prompt", "managed by named profile", "schedule", "every 1h", "name", "normalizes-dashboard-fields")
	c := cronClient(t)
	updated := cronObj(t, cronSend(c, "PUT", "/api/cron/jobs/"+job.GetOr("id", "").(string)+"?profile=worker_alpha", map[string]any{
		"updates": map[string]any{"base_url": "https://example.invalid/v1/", "script": filepath.Join(scripts, "collect.py"), "context_from": "", "no_agent": true},
	}))
	if updated.GetOr("base_url", nil) != "https://example.invalid/v1" || updated.GetOr("script", nil) != "collect.py" ||
		updated.GetOr("context_from", "x") != nil || updated.GetOr("no_agent", nil) != true {
		t.Fatalf("updated %v", updated)
	}
}

func TestCreateCronJobRejectsScriptOutsideProfileScripts(t *testing.T) {
	cronIsolatedProfiles(t)
	outside := filepath.Join(t.TempDir(), "outside.py")
	if err := os.WriteFile(outside, []byte("print('nope')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := cronSend(cronClient(t), "POST", "/api/cron/jobs?profile=worker_alpha", map[string]any{"schedule": "every 1h", "script": outside, "no_agent": true})
	if r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func TestCreateCronJobRejectsEmptyAgentJob(t *testing.T) {
	cronIsolatedProfiles(t)
	r := cronSend(cronClient(t), "POST", "/api/cron/jobs?profile=worker_alpha", map[string]any{"schedule": "every 1h"})
	if r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func TestUpdateCronJobNoAgentReusesExistingScript(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	scripts := filepath.Join(homes["worker_alpha"], "scripts")
	if err := os.Mkdir(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "collect.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := cronClient(t)
	job := cronObj(t, cronSend(c, "POST", "/api/cron/jobs?profile=worker_alpha", map[string]any{"schedule": "every 1h", "script": filepath.Join(scripts, "collect.py")}))
	updated := cronObj(t, cronSend(c, "PUT", "/api/cron/jobs/"+job.GetOr("id", "").(string)+"?profile=worker_alpha", map[string]any{"updates": map[string]any{"no_agent": true}}))
	if updated.GetOr("no_agent", nil) != true || updated.GetOr("script", nil) != "collect.py" {
		t.Fatalf("updated %v", updated)
	}
}

func TestDashboardCronRejectsMissingContextFrom(t *testing.T) {
	cronIsolatedProfiles(t)
	c := cronClient(t)
	r := cronSend(c, "POST", "/api/cron/jobs?profile=worker_alpha", map[string]any{"prompt": "process missing upstream", "schedule": "every 1h", "context_from": []string{"missing-job-id"}})
	if r.Status != 400 || !strings.Contains(string(r.Body), "missing-job-id") {
		t.Fatalf("create %d %s", r.Status, r.Body)
	}
	job := cronCreateIn(t, "worker_alpha", "prompt", "managed by named profile", "schedule", "every 1h", "name", "context-update-target")
	r = cronSend(c, "PUT", "/api/cron/jobs/"+job.GetOr("id", "").(string)+"?profile=worker_alpha", map[string]any{"updates": map[string]any{"context_from": []string{"missing-job-id"}}})
	if r.Status != 400 || !strings.Contains(string(r.Body), "missing-job-id") {
		t.Fatalf("update %d %s", r.Status, r.Body)
	}
}

func TestUpdateCronJobRejectsIdMutation(t *testing.T) {
	cronIsolatedProfiles(t)
	job := cronCreateIn(t, "worker_alpha", "prompt", "managed by named profile", "schedule", "every 1h", "name", "immutable-id-job")
	c := cronClient(t)
	r := cronSend(c, "PUT", "/api/cron/jobs/"+job.GetOr("id", "").(string)+"?profile=worker_alpha", map[string]any{"updates": map[string]any{"id": "../escape"}})
	if r.Status != 400 || !strings.Contains(string(r.Body), "id") {
		t.Fatalf("update %d %s", r.Status, r.Body)
	}
	if ids := cronIDs(t, c.get("/api/cron/jobs?profile=worker_alpha")); len(ids) != 1 || ids[0] != job.GetOr("id", nil) {
		t.Fatalf("ids %v", ids)
	}
}

func TestCronDeleteWithProfileDeletesOnlyTargetProfile(t *testing.T) {
	cronIsolatedProfiles(t)
	defaultJob := cronCreateIn(t, "default", "prompt", "same-ish default", "schedule", "every 1h", "name", "shared-name")
	workerJob := cronCreateIn(t, "worker_alpha", "prompt", "same-ish worker", "schedule", "every 1h", "name", "shared-name-worker")
	c := cronClient(t)
	c.request("DELETE", "/api/cron/jobs/"+workerJob.GetOr("id", "").(string)+"?profile=worker_alpha", nil).expect(t, 200, `{"ok":true}`)
	if ids := cronIDs(t, c.get("/api/cron/jobs?profile=default")); len(ids) != 1 || ids[0] != defaultJob.GetOr("id", nil) {
		t.Fatalf("default %v", ids)
	}
	if ids := cronIDs(t, c.get("/api/cron/jobs?profile=worker_alpha")); len(ids) != 0 {
		t.Fatalf("worker %v", ids)
	}
}

func TestCronProfileValidationErrors(t *testing.T) {
	cronIsolatedProfiles(t)
	c := cronClient(t)
	if r := c.get("/api/cron/jobs?profile=../bad"); r.Status != 400 {
		t.Fatalf("bad name %d %s", r.Status, r.Body)
	}
	if r := c.get("/api/cron/jobs?profile=missing_profile"); r.Status != 404 {
		t.Fatalf("missing %d %s", r.Status, r.Body)
	}
}

func TestCreateCronJobWithoutProfileUsesBackendOwnProfile(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	t.Setenv("ICEBATH_HOME", homes["worker_alpha"])
	job := cronObj(t, cronSend(cronClient(t), "POST", "/api/cron/jobs", map[string]any{"prompt": "runs in my own profile", "schedule": "every 1h", "name": "own-profile-job"}))
	if job.GetOr("profile", nil) != "worker_alpha" {
		t.Fatalf("profile %v", job.GetOr("profile", nil))
	}
	if _, err := os.Stat(filepath.Join(homes["worker_alpha"], "cron", "jobs.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homes["default"], "cron", "jobs.json")); err == nil {
		t.Fatal("default store written")
	}
}

func TestCreateCronJobWithoutProfileDefaultsWhenUnscoped(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	job := cronObj(t, cronSend(cronClient(t), "POST", "/api/cron/jobs", map[string]any{"prompt": "runs in default", "schedule": "every 1h", "name": "default-job"}))
	if job.GetOr("profile", nil) != "default" {
		t.Fatalf("profile %v", job.GetOr("profile", nil))
	}
	if _, err := os.Stat(filepath.Join(homes["default"], "cron", "jobs.json")); err != nil {
		t.Fatal(err)
	}
}

func TestListCronJobsCarriesEachProfilesTickerHeartbeatAge(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	for name := range homes {
		cronCreateIn(t, name, "prompt", name+" hourly", "schedule", "every 1h")
	}
	stamp := fmt.Sprintf("%f", float64(time.Now().Unix())-25*3600)
	if err := os.WriteFile(filepath.Join(homes["worker_alpha"], "cron", "ticker_heartbeat"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs, err := listCronJobsSync(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	ages := map[any]any{}
	for _, j := range jobs {
		ages[j.GetOr("profile", nil)] = j.GetOr("scheduler_heartbeat_age_s", nil)
	}
	if ages["default"] != nil {
		t.Fatalf("default age %v", ages["default"])
	}
	age, ok := ages["worker_alpha"].(float64)
	if !ok || age < 25*3600 || age >= 25*3600+60 {
		t.Fatalf("worker age %v", ages["worker_alpha"])
	}
}

// Writes a raw jobs.json list into a profile home
func cronWriteJobsFile(t *testing.T, h string, jobs []map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(jobs)
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// One interval job record named and prompted as given
func cronIntervalJob(id, name, prompt string) map[string]any {
	j := map[string]any{"name": name, "prompt": prompt, "enabled": true, "schedule": map[string]any{"type": "interval", "every": "1h"}}
	if id != "" {
		j["id"] = id
	}
	return j
}

// Jobs of the all profiles listing carrying the id
func cronAllWithID(t *testing.T, id string) []*pyjson.Object {
	t.Helper()
	result, err := listCronJobsSync(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	var out []*pyjson.Object
	for _, j := range result {
		if j.GetOr("id", nil) == id {
			out = append(out, j)
		}
	}
	return out
}

func TestCronJobsCrossProfileDedupSharedIdDefaultProfileCopyWins(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	cronWriteJobsFile(t, homes["default"], []map[string]any{cronIntervalJob("shared-job-1", "shared", "DEFAULT COPY")})
	cronWriteJobsFile(t, homes["worker_alpha"], []map[string]any{cronIntervalJob("shared-job-1", "shared", "WORKER COPY")})
	matches := cronAllWithID(t, "shared-job-1")
	if len(matches) != 1 || matches[0].GetOr("prompt", nil) != "DEFAULT COPY" || matches[0].GetOr("is_default_profile", nil) != true {
		t.Fatalf("matches %v", matches)
	}
}

func TestCronJobsCrossProfileDedupWorkerFirstSharedIdStillResolvesToDefaultCopy(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	cronWriteJobsFile(t, homes["default"], []map[string]any{cronIntervalJob("shared-job-2", "shared", "DEFAULT COPY")})
	cronWriteJobsFile(t, homes["worker_alpha"], []map[string]any{cronIntervalJob("shared-job-2", "shared", "WORKER COPY")})
	workerFirst, err := cronListJobs(context.Background(), "worker_alpha")
	if err != nil || workerFirst[0].GetOr("prompt", nil) != "WORKER COPY" {
		t.Fatalf("worker first %v %v", workerFirst, err)
	}
	matches := cronAllWithID(t, "shared-job-2")
	if len(matches) != 1 || matches[0].GetOr("prompt", nil) != "DEFAULT COPY" {
		t.Fatalf("matches %v", matches)
	}
}

func TestCronJobsCrossProfileDedupUniqueIdsAllKept(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	cronWriteJobsFile(t, homes["default"], []map[string]any{cronIntervalJob("job-a", "a", "a")})
	cronWriteJobsFile(t, homes["worker_alpha"], []map[string]any{cronIntervalJob("job-b", "b", "b")})
	result, err := listCronJobsSync(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[any]bool{}
	for _, j := range result {
		ids[j.GetOr("id", nil)] = true
	}
	if len(ids) != 2 || !ids["job-a"] || !ids["job-b"] {
		t.Fatalf("ids %v", ids)
	}
}

func TestCronJobsCrossProfileDedupIdlessRecordsFromDifferentProfilesBothPreserved(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	cronWriteJobsFile(t, homes["default"], []map[string]any{cronIntervalJob("", "legacy-default", "legacy job in default")})
	cronWriteJobsFile(t, homes["worker_alpha"], []map[string]any{cronIntervalJob("", "legacy-worker", "legacy job in worker")})
	result, err := listCronJobsSync(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	prompts := map[any]bool{}
	for _, j := range result {
		prompts[j.GetOr("prompt", nil)] = true
	}
	if len(prompts) != 2 || !prompts["legacy job in default"] || !prompts["legacy job in worker"] {
		t.Fatalf("prompts %v", prompts)
	}
}

func TestCronRunHistoryFallbackFallsBackToOutputDocsWhenNoSessionRunsExist(t *testing.T) {
	homes := cronIsolatedProfiles(t)
	cronWriteJobsFile(t, homes["default"], []map[string]any{{"id": "job-script-only", "name": "job-script-only", "no_agent": true,
		"script": "sync.sh", "prompt": "", "last_status": "ok", "enabled": true, "schedule": map[string]any{"type": "interval", "every": "1h"}}})
	out := filepath.Join(homes["default"], "cron", "output", "job-script-only")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"2026-07-08_09-00-00.md": "older output\n", "2026-07-08_09-05-00.md": "latest output\n"} {
		if err := os.WriteFile(filepath.Join(out, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openSessionDBForProfile(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	result, err := listCronJobRunsSync(context.Background(), "job-script-only", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	runs := result.GetOr("runs", nil).([]any)
	if result.GetOr("limit", nil) != int64(2) || len(runs) != 2 {
		t.Fatalf("result %v", result)
	}
	first, second := runs[0].(*pyjson.Object), runs[1].(*pyjson.Object)
	if first.GetOr("id", nil) != "cron_output:job-script-only:2026-07-08_09-05-00" || second.GetOr("id", nil) != "cron_output:job-script-only:2026-07-08_09-00-00" {
		t.Fatalf("ids %v %v", first.GetOr("id", nil), second.GetOr("id", nil))
	}
	if first.GetOr("source", nil) != "cron_output" || first.GetOr("title", nil) != "latest output" || second.GetOr("title", nil) != "older output" {
		t.Fatalf("rows %v %v", first, second)
	}
	if first.GetOr("is_active", nil) != false || second.GetOr("is_active", nil) != false {
		t.Fatal("active row")
	}
}

// Test application over an isolated default home sending the session token on every request
type cronAppClient struct{ *testClient }

func (c cronAppClient) send(method, target string, body any) *testResponse {
	raw, _ := json.Marshal(body)
	return c.request(method, target, raw, "content-type", "application/json", sessionHeaderName, SessionToken())
}

func (c cronAppClient) get(target string) *testResponse {
	return c.request("GET", target, nil, sessionHeaderName, SessionToken())
}

// Isolated default home for the repeat cases served through the dashboard application
func cronRepeatClient(t *testing.T) cronAppClient {
	app := dashboardTestApp(t)
	h := os.Getenv("ICEBATH_HOME")
	if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model: test-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cronAppClient{newTestClient(t, app)}
}

// Repeat object of a created job response
func cronRepeatOf(t *testing.T, r *testResponse) string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	raw, _ := pyjson.Dumps(cronObj(t, r).GetOr("repeat", nil))
	return raw
}

func TestCreatePreservesFiniteRepeatCount(t *testing.T) {
	c := cronRepeatClient(t)
	got := cronRepeatOf(t, c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "run the check", "schedule": "every 1h", "name": "two checks", "repeat": 2}))
	if got != `{"times": 2, "completed": 0}` {
		t.Fatalf("repeat %s", got)
	}
}

func TestCreateWithoutRepeatRemainsUnlimited(t *testing.T) {
	c := cronRepeatClient(t)
	got := cronRepeatOf(t, c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "run forever", "schedule": "every 1h"}))
	if got != `{"times": null, "completed": 0}` {
		t.Fatalf("repeat %s", got)
	}
}

func TestCreateRejectsInvalidRepeatCount(t *testing.T) {
	for _, repeat := range []any{0, -1, 1.5, "not-a-count"} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			c := cronRepeatClient(t)
			r := c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "must not run forever", "schedule": "every 1h", "repeat": repeat})
			if r.Status != 400 && r.Status != 422 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			if !strings.Contains(strings.ToLower(string(r.Body)), "repeat") {
				t.Fatalf("detail %s", r.Body)
			}
			if ids := cronIDs(t, c.get("/api/cron/jobs?profile=default")); len(ids) != 0 {
				t.Fatalf("jobs %v", ids)
			}
		})
	}
}

func TestCreateAcceptsCliStringRepeatForms(t *testing.T) {
	for _, tc := range []struct{ repeat, want string }{{"2", `{"times": 2, "completed": 0}`}, {"once", `{"times": 1, "completed": 0}`}, {"forever", `{"times": null, "completed": 0}`}} {
		t.Run(tc.repeat, func(t *testing.T) {
			c := cronRepeatClient(t)
			got := cronRepeatOf(t, c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "string repeat form", "schedule": "every 1h", "repeat": tc.repeat}))
			if got != tc.want {
				t.Fatalf("repeat %s", got)
			}
		})
	}
}

func TestCreateStoresTheMissedRunPolicyOnlyWhenGiven(t *testing.T) {
	c := cronRepeatClient(t)
	chosen := c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "nightly", "schedule": "0 3 * * *", "name": "skip missed", "catch_up": false})
	plain := c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "nightly", "schedule": "0 3 * * *", "name": "default"})
	if chosen.Status != 200 || cronObj(t, chosen).GetOr("catch_up", nil) != false {
		t.Fatalf("chosen %d %s", chosen.Status, chosen.Body)
	}
	if _, has := cronObj(t, plain).Get("catch_up"); plain.Status != 200 || has {
		t.Fatalf("plain %d %s", plain.Status, plain.Body)
	}
}
