package cron

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool env of a cron run session with the optional auto deliver target of its creator
func cronContextEnv(s *Store, platform, chatID, threadID string) *tools.Env {
	env := &tools.Env{Home: s.home, Values: &sync.Map{}}
	vars := map[string]string{"ICEBATH_SESSION_PLATFORM": "", "ICEBATH_SESSION_CHAT_ID": "", "ICEBATH_SESSION_CHAT_NAME": "", "ICEBATH_CRON_SESSION": "1"}
	if platform != "" {
		vars["ICEBATH_CRON_AUTO_DELIVER_PLATFORM"] = platform
		vars["ICEBATH_CRON_AUTO_DELIVER_CHAT_ID"] = chatID
		if threadID != "" {
			vars["ICEBATH_CRON_AUTO_DELIVER_THREAD_ID"] = threadID
		}
	}
	tools.BindSessionContext(env, vars)
	return env
}

func createIn(t *testing.T, s *Store, env *tools.Env, deliver any) map[string]any {
	t.Helper()
	return runCronjob(t, s, &cronjobArgs{Action: "create", Schedule: "every 4h", Prompt: "check the thing", Deliver: deliver, env: env})
}

func TestCronContextDeliveryResolution(t *testing.T) {
	s, _ := newTestStore(t)
	tg := func() *tools.Env { return cronContextEnv(s, "telegram", "-100123456", "17") }
	for _, c := range []struct {
		env     *tools.Env
		deliver any
		want    string
	}{
		{tg(), nil, "telegram:-100123456:17"},
		{tg(), "origin", "telegram:-100123456:17"},
		{cronContextEnv(s, "slack", "C0ABC", ""), "origin", "slack:C0ABC"},
		{cronContextEnv(s, "", "", ""), "origin", "local"},
		{tg(), "origin,all", "telegram:-100123456:17,all"},
		{cronContextEnv(s, "telegram", "-100999", "3"), "discord:#engineering", "discord:#engineering"},
	} {
		got := createIn(t, s, c.env, c.deliver)
		if got["success"] != true || got["deliver"] != c.want {
			t.Errorf("deliver %v: %v", c.deliver, got)
		}
	}
}

func TestCronContextUpdatePath(t *testing.T) {
	s, _ := newTestStore(t)
	env := cronContextEnv(s, "telegram", "-100123456", "17")
	created := createIn(t, s, env, "local")
	res := runCronjob(t, s, &cronjobArgs{Action: "update", JobID: created["job_id"], Deliver: "origin", env: env})
	if res["success"] != true || deref(mustJob(t, s, created["job_id"].(string)).Deliver) != "telegram:-100123456:17" {
		t.Fatalf("update %v", res)
	}
	plain := createIn(t, s, nil, "local")
	res = runCronjob(t, s, &cronjobArgs{Action: "update", JobID: plain["job_id"], Deliver: "origin"})
	if res["success"] != true || deref(mustJob(t, s, plain["job_id"].(string)).Deliver) != "origin" {
		t.Fatalf("plain update %v", res)
	}
	if got := createIn(t, s, nil, "origin"); got["deliver"] != "origin" {
		t.Fatalf("chat session create %v", got)
	}
}

// Tool env of a chat session on the platform with the given async delivery support
func chatSessionEnv(s *Store, platform, chatID, key string, async bool) *tools.Env {
	env := &tools.Env{Home: s.home, Values: &sync.Map{}}
	tools.BindSessionContext(env, map[string]string{"ICEBATH_SESSION_PLATFORM": platform, "ICEBATH_SESSION_CHAT_ID": chatID, "ICEBATH_SESSION_KEY": key})
	env.Values.Store("session.async_delivery", async)
	return env
}

func TestNonPushSessionStampsNoOriginAndGetsCreationNotice(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	env := chatSessionEnv(s, "api_server", "desk-1", "desk-1", false)
	origin := s.originFromEnv(env, nil)
	if origin != nil {
		t.Fatalf("origin %v", origin)
	}
	if notice := s.localDeliveryNotice(context.Background(), env, &Job{ID: "j", Deliver: strPtr("origin")}, nil); notice == nil || *notice == "" {
		t.Fatal("no creation notice")
	}
	if o := s.originFromEnv(chatSessionEnv(s, "telegram", "777", "tg", true), nil); o == nil || o["platform"] != "telegram" {
		t.Fatalf("push origin %v", o)
	}
}

func TestExplicitEmptyToolsetsPersist(t *testing.T) {
	s, _ := newTestStore(t)
	job, err := s.createJob(CreateJobOptions{Prompt: strPtr("daily digest"), Schedule: "every 1h", EnabledToolsets: []any{}})
	if err != nil || job.EnabledToolsets == nil || len(job.EnabledToolsets) != 0 {
		t.Fatalf("created %+v %v", job, err)
	}
	reopened := openCronStore(s.home, nil)
	if got := mustJob(t, reopened, job.ID); got.EnabledToolsets == nil || len(got.EnabledToolsets) != 0 {
		t.Fatalf("reloaded %v", got.EnabledToolsets)
	}
	plain, _ := s.createJob(CreateJobOptions{Prompt: strPtr("daily digest"), Schedule: "every 1h"})
	if plain.EnabledToolsets != nil {
		t.Fatalf("absent toolsets %v", plain.EnabledToolsets)
	}
	updated, err := s.updateJob(plain.ID, map[string]any{"enabled_toolsets": []any{}})
	if err != nil || updated.EnabledToolsets == nil || len(updated.EnabledToolsets) != 0 {
		t.Fatalf("updated %v %v", updated, err)
	}
	if got, err := resolveCronEnabledToolsets(context.Background(), &Job{EnabledToolsets: []string{}}, config.Tree{}); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("explicit empty %v %v", got, err)
	}
	if got, _ := resolveCronEnabledToolsets(context.Background(), &Job{}, config.Tree{}); len(got) == 0 {
		t.Fatal("absent toolsets resolved to none")
	}
	res := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "daily digest", Schedule: "every 1h", EnabledToolsets: []any{}})
	if got := mustJob(t, s, res["job_id"].(string)); res["success"] != true || got.EnabledToolsets == nil || len(got.EnabledToolsets) != 0 {
		t.Fatalf("tool create %v %v", res, got.EnabledToolsets)
	}
}

func TestNoteFireForwardFailure(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mk := func() *Job {
		return mustCreate(t, s, CreateJobOptions{Prompt: strPtr("Daily invoice triage"), Schedule: "every 1h"})
	}
	job := mk()
	if ok, err := s.noteFireForwardFailure(job.ID, "gateway unreachable"); !ok || err != nil {
		t.Fatalf("stamp %v %v", ok, err)
	}
	stamp := mustJob(t, s, job.ID).LastFireError
	if stamp == nil || stamp.Detail != "gateway unreachable" {
		t.Fatalf("stamp %+v", stamp)
	}
	if _, err := ParseISO(stamp.At); err != nil {
		t.Fatalf("at %q: %v", stamp.At, err)
	}
	if ok, _ := s.noteFireForwardFailure("nope", "gateway unreachable"); ok {
		t.Fatal("unknown job stamped")
	}
	s.noteFireForwardFailure(job.ID, "first miss")
	s.noteFireForwardFailure(job.ID, "second miss")
	if d := mustJob(t, s, job.ID).LastFireError.Detail; d != "second miss" {
		t.Fatalf("latest %q", d)
	}
	cleared := mk()
	s.noteFireForwardFailure(cleared.ID, "gateway unreachable")
	if ok, _ := s.markJobRun(ctx, cleared.ID, true, MarkOptions{}); !ok || mustJob(t, s, cleared.ID).LastFireError != nil {
		t.Fatal("success kept the stamp")
	}
	kept := mk()
	s.noteFireForwardFailure(kept.ID, "gateway unreachable")
	if ok, _ := s.markJobRun(ctx, kept.ID, false, MarkOptions{Error: strPtr("boom")}); !ok {
		t.Fatal("mark failed run")
	}
	if st := mustJob(t, s, kept.ID).LastFireError; st == nil || st.Detail != "gateway unreachable" {
		t.Fatalf("failure dropped the stamp %+v", st)
	}
	formatted := s.formatJob(mustJob(t, s, kept.ID))
	fe, _ := formatted.Get("last_fire_error")
	obj, ok := fe.(*pyjson.Object)
	if !ok {
		t.Fatalf("formatted %v", fe)
	}
	if d, _ := obj.Get("detail"); d != "gateway unreachable" {
		t.Fatalf("formatted detail %v", d)
	}
	if !strings.Contains(stamp.At, "T") {
		t.Fatalf("at %q", stamp.At)
	}
}
