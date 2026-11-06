package gatewaycli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Isolates the home and enables the webhook platform for one test
func webhookHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	config.ResetCache()
	prev := webhookEnabled
	webhookEnabled = func(context.Context, string) bool { return true }
	t.Cleanup(func() { webhookEnabled = prev })
	return dir
}

func runWebhookCmd(t *testing.T, args WebhookArgs) (int, string, error) {
	var out bytes.Buffer
	if args.Deliver == "" {
		args.Deliver = "log"
	}
	code, err := WebhookCommand(context.Background(), &out, args)
	return code, out.String(), err
}

func stored(t *testing.T, dir string) *pyjson.Object { return webhooksubs.Load(dir) }

func routeField(t *testing.T, dir, name, key string) any {
	t.Helper()
	route, ok := stored(t, dir).Get(name)
	if !ok {
		t.Fatalf("route %s missing", name)
	}
	v, _ := route.(*pyjson.Object).Get(key)
	return v
}

func makeProfile(t *testing.T, dir, name string) string {
	p := filepath.Join(dir, "profiles", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func strp(s string) *string { return &s }

func TestWebhookSubscribeCustomAndAutoSecret(t *testing.T) {
	dir := webhookHome(t)
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "s", Secret: "my-secret"})
	if got := routeField(t, dir, "s", "secret"); got != "my-secret" {
		t.Fatalf("secret %v", got)
	}
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "auto"})
	if got, _ := routeField(t, dir, "auto", "secret").(string); len(got) <= 20 {
		t.Fatalf("auto secret %q", got)
	}
}

func TestWebhookProfileBindingAndSecretSurviveUpdate(t *testing.T) {
	dir := webhookHome(t)
	makeProfile(t, dir, "compta")
	_, out, _ := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", RouteProfile: strp("compta")})
	first := routeField(t, dir, "notifier", "secret")
	if routeField(t, dir, "notifier", "profile") != "compta" || !strings.Contains(out, "/p/compta/webhooks/notifier") {
		t.Fatalf("created route output %q", out)
	}
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Description: "updated"})
	if routeField(t, dir, "notifier", "profile") != "compta" || routeField(t, dir, "notifier", "secret") != first {
		t.Fatal("update dropped the profile binding or the secret")
	}
}

func TestWebhookRejectsUnknownProfileWithoutReplacingSubscription(t *testing.T) {
	dir := webhookHome(t)
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Secret: "original"})
	if code, _, _ := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", RouteProfile: strp("missing")}); code != 1 {
		t.Fatalf("unknown profile exit %d", code)
	}
	if routeField(t, dir, "notifier", "secret") != "original" {
		t.Fatal("refused update replaced the route")
	}
}

func TestWebhookStaleUpdateCannotRestoreRemovedOrDisabledRoute(t *testing.T) {
	dir := webhookHome(t)
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Secret: "original"})
	var mu sync.Mutex
	var armed bool
	loaded, allow := make(chan struct{}), make(chan struct{})
	prev := webhookSubscriptions
	webhookSubscriptions = func(h string) *pyjson.Object {
		subs := prev(h)
		mu.Lock()
		pause := armed
		armed = false
		mu.Unlock()
		if pause {
			loaded <- struct{}{}
			<-allow
		}
		return subs
	}
	t.Cleanup(func() { webhookSubscriptions = prev })
	stale := func() chan error {
		mu.Lock()
		armed = true
		mu.Unlock()
		done := make(chan error, 1)
		go func() {
			_, _, err := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Description: "stale update"})
			done <- err
		}()
		select {
		case <-loaded:
		case <-time.After(5 * time.Second):
			t.Fatal("stale update never loaded")
		}
		return done
	}
	done := stale()
	runWebhookCmd(t, WebhookArgs{Action: "remove", Name: "notifier"})
	allow <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := stored(t, dir).Get("notifier"); ok {
		t.Fatal("stale update restored a removed route")
	}
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Secret: "replacement"})
	done = stale()
	if err := webhooksubs.Mutate(dir, func(subs *pyjson.Object) error {
		route, _ := subs.Get("notifier")
		route.(*pyjson.Object).Set("enabled", false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	allow <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if routeField(t, dir, "notifier", "enabled") != false {
		t.Fatal("stale update re-enabled a disabled route")
	}
}

func TestWebhookProfileRebindRotatesSecret(t *testing.T) {
	dir := webhookHome(t)
	makeProfile(t, dir, "compta")
	prevNow, prevTok := webhookNow, webhookTokenURLSafe
	webhookNow = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	webhookTokenURLSafe = func(int) string { return "rotated-profile-secret" }
	t.Cleanup(func() { webhookNow, webhookTokenURLSafe = prevNow, prevTok })
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", Secret: "old-profile-secret-ab"})
	if code, _, _ := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", RouteProfile: strp("compta"), Secret: "old-profile-secret-ab"}); code != 1 {
		t.Fatal("rebind with the old secret was accepted")
	}
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "notifier", RouteProfile: strp("compta")})
	if routeField(t, dir, "notifier", "profile") != "compta" || routeField(t, dir, "notifier", "secret") != "rotated-profile-secret" ||
		routeField(t, dir, "notifier", "created_at") != "2026-01-01T00:00:00Z" {
		t.Fatalf("rebind record %v", stored(t, dir).Vals["notifier"])
	}
}

func TestWebhookCronJobSubscribe(t *testing.T) {
	dir := webhookHome(t)
	prev := webhookCronJobRef
	t.Cleanup(func() { webhookCronJobRef = prev })
	webhookCronJobRef = func(h, ref string) (*cron.Job, error) { return &cron.Job{ID: "job-abc123", Name: ref}, nil }
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "ev", CronJob: "sweeper"})
	if routeField(t, dir, "ev", "cron_job") != "job-abc123" {
		t.Fatal("job id not stored")
	}
	webhookCronJobRef = func(h, ref string) (*cron.Job, error) { return nil, nil }
	if code, _, _ := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "ev2", CronJob: "nope"}); code != 1 {
		t.Fatal("unknown job accepted")
	}
	if code, _, _ := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "ev3", CronJob: "sweeper", DeliverOnly: true, Deliver: "telegram"}); code != 1 {
		t.Fatal("cron job with deliver only accepted")
	}
	for _, n := range []string{"ev2", "ev3"} {
		if _, ok := stored(t, dir).Get(n); ok {
			t.Fatalf("%s written", n)
		}
	}
}

func TestWebhookSelectiveRemoveAndEnabledGate(t *testing.T) {
	dir := webhookHome(t)
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "keep"})
	runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "drop"})
	runWebhookCmd(t, WebhookArgs{Action: "remove", Name: "drop"})
	subs := stored(t, dir)
	if _, ok := subs.Get("keep"); !ok {
		t.Fatal("keep removed")
	}
	if _, ok := subs.Get("drop"); ok {
		t.Fatal("drop kept")
	}
	webhookEnabled = func(context.Context, string) bool { return false }
	code, out, _ := runWebhookCmd(t, WebhookArgs{Action: "list"})
	if code != 1 || !strings.Contains(strings.ToLower(out), "not enabled") {
		t.Fatalf("gate code=%d out=%q", code, out)
	}
}

// Writes default and coder profile cron stores each holding one daily job
func cronHomes(t *testing.T) (string, string) {
	root := webhookHome(t)
	coder := makeProfile(t, root, "coder")
	for _, hj := range [][2]string{{root, "default-job"}, {coder, "coder-job"}} {
		writeJobs(t, hj[0], `{"jobs": [{"id": "`+hj[1]+`", "name": "daily", "enabled": true}]}`)
	}
	return root, coder
}

func writeJobs(t *testing.T, h, body string) {
	if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cronSubscribe(t *testing.T, profile *string, cronJob string) (int, error) {
	code, _, err := runWebhookCmd(t, WebhookArgs{Action: "subscribe", Name: "route", Secret: "fixture-secret", RouteProfile: profile, CronJob: cronJob})
	return code, err
}

func TestWebhookSubscribeResolvesCronInRouteProfile(t *testing.T) {
	for _, ref := range []string{"coder-job", "daily"} {
		t.Run(ref, func(t *testing.T) {
			root, coder := cronHomes(t)
			cronSubscribe(t, strp("coder"), ref)
			if routeField(t, root, "route", "cron_job") != "coder-job" || routeField(t, root, "route", "profile") != "coder" {
				t.Fatalf("route %v", stored(t, root).Vals["route"])
			}
			if _, err := os.Stat(webhooksubs.Path(coder)); err == nil {
				t.Fatal("subscriptions written into the profile home")
			}
		})
	}
}

func TestWebhookUpdateKeepsProfileAndDefaultRouteResolvesDefaultJob(t *testing.T) {
	root, _ := cronHomes(t)
	cronSubscribe(t, strp("coder"), "")
	cronSubscribe(t, nil, "daily")
	if routeField(t, root, "route", "cron_job") != "coder-job" {
		t.Fatal("update lost the route profile for the cron lookup")
	}
	other, _ := cronHomes(t)
	cronSubscribe(t, nil, "daily")
	if routeField(t, other, "route", "cron_job") != "default-job" {
		t.Fatal("default route did not resolve the default job")
	}
}

func TestWebhookFailedTargetLookupPreservesRoute(t *testing.T) {
	for _, failure := range []string{"missing", "ambiguous", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			root, coder := cronHomes(t)
			cronSubscribe(t, strp("coder"), "")
			before, _ := os.ReadFile(webhooksubs.Path(root))
			switch failure {
			case "missing":
				writeJobs(t, coder, `{"jobs": []}`)
			case "ambiguous":
				writeJobs(t, coder, `{"jobs": [{"id": "one", "name": "daily"}, {"id": "two", "name": "daily"}]}`)
			default:
				writeJobs(t, coder, "not json")
			}
			code, err := cronSubscribe(t, nil, "daily")
			if failure == "corrupt" {
				if err == nil || !strings.Contains(err.Error(), "corrupted") {
					t.Fatalf("corrupt store err %v", err)
				}
			} else if code != 1 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			after, _ := os.ReadFile(webhooksubs.Path(root))
			if !bytes.Equal(before, after) {
				t.Fatal("failed lookup rewrote the subscriptions")
			}
			if job, err := cron.ResolveJobRef(root, "daily"); err != nil || job == nil || job.ID != "default-job" {
				t.Fatalf("default store job=%v err=%v", job, err)
			}
			if _, err := os.Stat(webhooksubs.Path(coder)); err == nil {
				t.Fatal("subscriptions written into the profile home")
			}
		})
	}
}

func TestWebhookBareRoutePreservesCustomCallerHome(t *testing.T) {
	root, _ := cronHomes(t)
	custom := filepath.Join(root, "standalone")
	writeJobs(t, custom, `{"jobs": [{"id": "custom-job", "name": "daily"}]}`)
	t.Setenv("ICEBATH_HOME", custom)
	cronSubscribe(t, nil, "daily")
	if routeField(t, custom, "route", "cron_job") != "custom-job" {
		t.Fatal("bare route left the caller home")
	}
	if _, err := os.Stat(webhooksubs.Path(root)); err == nil {
		t.Fatal("subscriptions written into the root home")
	}
}
