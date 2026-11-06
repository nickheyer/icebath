package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Plugin platform entries carrying cron home env vars, registered for one test
func registerHomePlatforms(t *testing.T, names ...string) {
	t.Helper()
	ctx := context.Background()
	saved := cronDeliveryPluginScan
	cronDeliveryPluginScan = func(context.Context) {}
	t.Cleanup(func() { cronDeliveryPluginScan = saved })
	for _, name := range names {
		envVar := strings.ToUpper(name) + "_HOME_CHANNEL"
		if err := gateway.Registry.Register(ctx, &gateway.PlatformEntry{Name: name, Label: name, Source: "plugin", CronDeliverEnvVar: envVar}, ""); err != nil {
			t.Fatal(err)
		}
		n := name
		t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, n, "") })
	}
}

func TestStampedAPIServerOriginFallsBackToHomeChannel(t *testing.T) {
	s, _ := newTestStore(t)
	useRealDelivery(t)
	registerHomePlatforms(t, "telegram", "discord")
	t.Setenv("TELEGRAM_HOME_CHANNEL", "12345")
	ctx := context.Background()
	job := &Job{ID: "old", Deliver: strPtr("origin"), Origin: map[string]any{"platform": "api_server", "chat_id": "desk-1"}}
	got := resolveDeliveryTargets(ctx, s, job, false)
	if len(got) != 1 || got[0] != (Target{Platform: "telegram", ChatID: "12345", ResolvedFrom: "origin_fallback"}) {
		t.Fatalf("targets %+v", got)
	}
	job.Origin = map[string]any{"platform": "discord", "chat_id": "42"}
	if got := resolveDeliveryTargets(ctx, s, job, false); len(got) == 0 || got[0].Platform != "discord" {
		t.Fatalf("push origin %+v", got)
	}
}

func TestNonPushSessionCreationNoticeNamesHomeChannelFallback(t *testing.T) {
	s, _ := newTestStore(t)
	useRealDelivery(t)
	registerHomePlatforms(t, "telegram")
	t.Setenv("TELEGRAM_HOME_CHANNEL", "12345")
	env := chatSessionEnv(s, "api_server", "desk-1", "desk-1", false)
	job := &Job{ID: "j", Deliver: strPtr("origin"), Origin: s.originFromEnv(env, nil)}
	notice := s.localDeliveryNotice(context.Background(), env, job, nil)
	if notice == nil || !strings.Contains(*notice, "telegram:12345") {
		t.Fatalf("notice %v", notice)
	}
}

// Store with the Slack home channel the mirror eligibility cases share
func mirrorEligibilityStore(t *testing.T) *Store {
	s, _ := newTestStore(t)
	useRealDelivery(t)
	registerHomePlatforms(t, "slack", "telegram", "discord")
	t.Setenv("SLACK_HOME_CHANNEL", "D0HOME")
	t.Setenv("TELEGRAM_HOME_CHANNEL", "")
	t.Setenv("DISCORD_HOME_CHANNEL", "")
	return s
}

func slackTargets(targets []Target) []Target {
	var out []Target
	for _, t := range targets {
		if strings.ToLower(t.Platform) == "slack" {
			out = append(out, t)
		}
	}
	return out
}

func TestMirrorEligibilityResolution(t *testing.T) {
	ctx := context.Background()
	attach := func(job *Job, v bool) *Job { c := *job; c.AttachToSession = &v; return &c }
	dm := map[string]any{"platform": "slack", "chat_id": "D0AAA", "chat_type": "dm"}
	t.Run("origin target is eligible", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("origin"), Origin: dm}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) != 1 || !targetMirrorEligible(job, got[0], true, nil) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("origin fallback target is eligible", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("origin")}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) != 1 || got[0].ChatID != "D0HOME" || !targetMirrorEligible(job, got[0], true, nil) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("all expansion is never eligible", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("all")}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) == 0 {
			t.Fatal("home channel should expand from 'all'")
		}
		for _, tg := range got {
			if targetMirrorEligible(job, tg, true, nil) || targetMirrorEligible(attach(job, true), tg, true, nil) {
				t.Fatalf("eligible %+v", tg)
			}
		}
	})
	t.Run("bare platform target is eligible as home", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("slack")}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) != 1 || !targetMirrorEligible(job, got[0], true, nil) || targetMirrorEligible(job, got[0], false, nil) ||
			!targetMirrorEligible(attach(job, true), got[0], false, nil) || targetMirrorEligible(attach(job, false), got[0], true, nil) {
			t.Fatalf("%+v", got)
		}
	})
	for _, deliver := range []string{"slack,all", "all,slack", " ALL , slack ", "origin,all", "all,origin"} {
		t.Run("dedup keeps eligibility "+deliver, func(t *testing.T) {
			s := mirrorEligibilityStore(t)
			job := &Job{Deliver: strPtr(deliver)}
			got := slackTargets(resolveDeliveryTargets(ctx, s, job, false))
			if len(got) != 1 || !targetMirrorEligible(job, got[0], true, nil) {
				t.Fatalf("%+v", got)
			}
		})
	}
	t.Run("explicit target not eligible under global flag", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("slack:D0EXPL")}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) != 1 || targetMirrorEligible(job, got[0], true, nil) || !targetMirrorEligible(attach(job, true), got[0], false, nil) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("explicit other chat with origin not eligible", func(t *testing.T) {
		s := mirrorEligibilityStore(t)
		job := &Job{Deliver: strPtr("slack:D0OTHER"), Origin: dm}
		got := resolveDeliveryTargets(ctx, s, job, false)
		if len(got) != 1 || targetMirrorEligible(job, got[0], true, nil) {
			t.Fatalf("%+v", got)
		}
	})
}

func TestBotChatTargetWithoutLiveOwnerIsParkedAndAdmittedLater(t *testing.T) {
	s, _ := newTestStore(t)
	useRealDelivery(t)
	t.Setenv(externalWorkerEnv, "")
	if err := os.WriteFile(filepath.Join(s.home, "state.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	savedFind, savedDeliver := findCanonicalLiveOwner, deliverToLiveOwner
	t.Cleanup(func() { findCanonicalLiveOwner, deliverToLiveOwner = savedFind, savedDeliver })
	findCanonicalLiveOwner = func(context.Context, string) (map[string]any, error) {
		return nil, &gateway.PyError{Kind: "ValueError", Msg: "profile authority is not ready"}
	}
	ctx := context.Background()
	job := &Job{ID: "digest", Name: "Digest", ExecutionID: strPtr("run-1"), Deliver: strPtr("bot-chat")}
	if out, err := s.deliverResult(ctx, job, "the report", nil, nil, false); out != nil || err != nil {
		t.Fatalf("deliver %v %v", out, err)
	}
	if err := s.drainLegacyPendingRecords(ctx); err != nil {
		t.Fatal(err)
	}
	type admission struct {
		key    string
		report bool
	}
	var admitted []admission
	findCanonicalLiveOwner = func(context.Context, string) (map[string]any, error) {
		return map[string]any{"session_id": "bot"}, nil
	}
	deliverToLiveOwner = func(_ context.Context, _ string, _ map[string]any, message, deliveryID, _ string) (map[string]any, error) {
		admitted = append(admitted, admission{deliveryID, strings.Contains(message, "the report")})
		return map[string]any{"status": "queued", "message": message, "delivery_id": deliveryID}, nil
	}
	for i := 0; i < 2; i++ {
		if err := s.drainLegacyPendingRecords(ctx); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := job.run.botChatDeliveryReceipts.Get("bot-chat:(own)")
	key, _ := v.(interface{ Get(string) (any, bool) }).Get("delivery_id")
	if len(admitted) != 1 || admitted[0] != (admission{key.(string), true}) {
		t.Fatalf("admitted %+v key %v", admitted, key)
	}
}

func TestCorruptLegacyRecordIsIsolatedAndDueJobStillFires(t *testing.T) {
	s, _ := newTestStore(t)
	useRealTickSeams(t)
	reconcilePending = func(context.Context, *Store, bool) error { return nil }
	t.Cleanup(func() { discardParallelPools([]string{home.Key(s.Home())}) })
	pending := filepath.Join(s.home, "cron", "bot_chat_pending")
	scripts := filepath.Join(s.home, "scripts")
	for _, dir := range []string{pending, scripts} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := filepath.Join(pending, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"id": "legacy-1", "status": "queued"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "fire.sh"), []byte("#!/bin/sh\necho fired\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 1h", Name: prompt("script"), Script: "fire.sh", NoAgent: true, Deliver: prompt("local")})
	makeTickDue(t, s, job.ID)
	ran, err := schedulerTick(t.Context(), s, tickOptions{Sync: true})
	if err != nil || ran != 1 {
		t.Fatal(ran, err)
	}
	got, err := s.getJob(job.ID)
	if err != nil || got == nil || got.LastStatus == nil || *got.LastStatus != "ok" {
		t.Fatalf("job %+v %v", got, err)
	}
	var record map[string]any
	raw, _ := os.ReadFile(corrupt)
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["status"] != "ambiguous" || record["error"] == "" || record["error"] == nil {
		t.Fatalf("record %v", record)
	}
}

func TestDeliverResultRecordsUnresolvedOutcomes(t *testing.T) {
	s, _ := newTestStore(t)
	useRealDelivery(t)
	registerHomePlatforms(t)
	ctx := context.Background()
	if out, err := s.deliverResult(ctx, &Job{ID: "l", Deliver: strPtr("local")}, "x", nil, nil, false); out != nil || err != nil {
		t.Fatalf("local %v %v", out, err)
	}
	if out, err := s.deliverResult(ctx, &Job{ID: "o", Deliver: strPtr("origin")}, "x", nil, nil, false); out != nil || err != nil {
		t.Fatalf("origin %v %v", out, err)
	}
	out, err := s.deliverResult(ctx, &Job{ID: "u", Deliver: strPtr("nosuchplatform")}, "x", nil, nil, false)
	if err != nil || out == nil || *out != "no delivery target resolved for deliver=nosuchplatform" {
		t.Fatalf("unknown %v %v", out, err)
	}
}

func TestConfirmAdapterDeliveryEvidence(t *testing.T) {
	s, _ := newTestStore(t)
	type sendResult struct {
		Success     bool
		MessageID   any
		RawResponse map[string]any
	}
	var gap []bool
	cases := []struct {
		result any
		want   bool
		gap    int
	}{
		{nil, false, 0},
		{map[string]any{"error": "x"}, false, 0},
		{map[string]any{"success": true, "delivered": false, "message_id": "1"}, false, 0},
		{map[string]any{"success": true, "message_id": "1"}, true, 0},
		{&sendResult{Success: true}, true, 1},
		{&sendResult{Success: true, MessageID: "m"}, true, 0},
	}
	for i, c := range cases {
		gap = nil
		if got := s.confirmAdapterDelivery(c.result, "j", &gap); got != c.want || len(gap) != c.gap {
			t.Fatalf("case %d got %v gap %d", i, got, len(gap))
		}
	}
	if !errors.Is(errFutureTimeout, errFutureTimeout) || pyExceptionName(errFutureTimeout) != "TimeoutError" {
		t.Fatal("timeout name")
	}
}

func TestStampOriginDiscriminatorsNeverOverrides(t *testing.T) {
	td := &targetDelivery{origin: map[string]any{"scope_id": "T1"}, originTarget: true, originUserID: strPtr("U1")}
	route, media := map[string]any{"scope_id": "router"}, map[string]any{}
	stampOriginDiscriminators(td, route, media)
	if route["scope_id"] != "router" || media["scope_id"] != "T1" || route["user_id"] != nil {
		t.Fatalf("route %v media %v", route, media)
	}
}

func TestCronJobOriginLogSuffix(t *testing.T) {
	job := &Job{Origin: map[string]any{"platform": "slack", "chat_id": "C1\nX", "thread_id": nil, "remote": "  "}}
	if got := cronJobOriginLogSuffix(job); got != " origin_platform='slack' origin_chat_id='C1 X'" {
		t.Fatalf("%q", got)
	}
	if cronJobOriginLogSuffix(&Job{}) != "" {
		t.Fatal("no origin")
	}
}
