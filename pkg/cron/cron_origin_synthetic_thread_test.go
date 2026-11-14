package cron

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool env of a push capable chat session bound to the given session variables
func sessionVarsEnv(s *Store, vars map[string]string) *tools.Env {
	env := &tools.Env{Home: s.home, Values: &sync.Map{}}
	tools.BindSessionContext(env, vars)
	return env
}

var topLevelSlack = map[string]string{"ICEBATH_SESSION_PLATFORM": "slack", "ICEBATH_SESSION_CHAT_ID": "C0AGENERAL", "ICEBATH_SESSION_THREAD_ID": "1755043010.123456", "ICEBATH_SESSION_MESSAGE_ID": "1755043010.123456"}

func TestSlackSyntheticThreadCapture(t *testing.T) {
	s, _ := newTestStore(t)
	for _, c := range []struct {
		vars       map[string]string
		wantThread any
	}{
		{map[string]string{"ICEBATH_SESSION_PLATFORM": "slack", "ICEBATH_SESSION_CHAT_ID": "D0BJTDCSR7C", "ICEBATH_SESSION_THREAD_ID": "1755043010.123456", "ICEBATH_SESSION_MESSAGE_ID": "1755043010.123456"}, nil},
		{map[string]string{"ICEBATH_SESSION_PLATFORM": "slack", "ICEBATH_SESSION_CHAT_ID": "C0AGENERAL", "ICEBATH_SESSION_THREAD_ID": "1755040000.000100", "ICEBATH_SESSION_MESSAGE_ID": "1755043010.123456"}, "1755040000.000100"},
		{map[string]string{"ICEBATH_SESSION_PLATFORM": "telegram", "ICEBATH_SESSION_CHAT_ID": "-1003941067111", "ICEBATH_SESSION_THREAD_ID": "2203", "ICEBATH_SESSION_MESSAGE_ID": "2203"}, "2203"},
		{map[string]string{"ICEBATH_SESSION_PLATFORM": "slack", "ICEBATH_SESSION_CHAT_ID": "D0BJTDCSR7C", "ICEBATH_SESSION_THREAD_ID": "1755040000.000100"}, "1755040000.000100"},
	} {
		origin := s.originFromEnv(sessionVarsEnv(s, c.vars), nil)
		if origin == nil || origin["platform"] != c.vars["ICEBATH_SESSION_PLATFORM"] || origin["chat_id"] != c.vars["ICEBATH_SESSION_CHAT_ID"] || origin["thread_id"] != c.wantThread {
			t.Errorf("%v origin %v", c.vars, origin)
		}
	}
}

func runAtIn(s *Store, minutes int) string {
	return FormatISO(s.clk.now().Add(time.Duration(minutes) * time.Minute))
}

func TestNearHorizonSlackThreadKept(t *testing.T) {
	s, _ := newTestStore(t)
	env := sessionVarsEnv(s, topLevelSlack)
	threadFor := func(schedule any) any {
		origin := s.originFromEnv(env, schedule)
		if origin == nil {
			t.Fatalf("no origin for %v", schedule)
		}
		return origin["thread_id"]
	}
	once := func(runAt string) map[string]any { return map[string]any{"kind": "once", "run_at": runAt} }
	if got := threadFor(once(runAtIn(s, 60))); got != "1755043010.123456" {
		t.Errorf("boundary %v", got)
	}
	for _, sched := range []any{once(runAtIn(s, 61)), once(runAtIn(s, -10)), map[string]any{"kind": "interval", "minutes": 1}, once("not-a-timestamp"), nil} {
		if got := threadFor(sched); got != nil {
			t.Errorf("%v kept %v", sched, got)
		}
	}
	frozen := s.clk.now().Truncate(time.Microsecond)
	setNow(s, frozen)
	if got := threadFor(once(FormatISO(frozen))); got != "1755043010.123456" {
		t.Errorf("firing now %v", got)
	}
	s.clk.nowFn = time.Now
	ctx := context.WithValue(context.Background(), cronStoreKey{}, s)
	raw := cronjobHandler(ctx, &tools.Call{Name: "cronjob_manage", Args: map[string]any{"action": "create", "schedule": runAtIn(s, 1), "prompt": "remind me"}, Env: env})
	var res map[string]any
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res["success"] != true {
		t.Fatalf("create %s %v", raw, err)
	}
	stored := mustJob(t, s, res["job_id"].(string))
	if stored.Origin == nil || stored.Origin["thread_id"] != "1755043010.123456" {
		t.Fatalf("stored origin %+v", stored.Origin)
	}
}
