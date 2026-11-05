package cron

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Two profile homes under one root, the second a named profile, with the ticked set reset afterwards
func ownershipHomes(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	a, b := root, filepath.Join(root, "profiles", "beta")
	for _, h := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", a)
	prevProbes, prevDiscarded := gatewayProbes, testHooks.discarded
	testHooks.discarded = nil
	t.Cleanup(func() {
		registerTickedHomes(nil)
		gatewayProbes, testHooks.discarded = prevProbes, prevDiscarded
	})
	return a, b
}

// Probes describing a live foreign gateway holding the runtime lock with the given status record
func foreignGateway(record map[string]any) {
	gatewayProbes.ownsRuntimeLock = func() bool { return false }
	gatewayProbes.runtimeLockActive = func(string) (bool, error) { return true, nil }
	gatewayProbes.runningPid = func(string, bool) (int, bool, error) { return 4321, true, nil }
	gatewayProbes.runtimeStatusIsStale = func(*pyjson.Object, int) bool { return false }
	gatewayProbes.readRuntimeStatus = func(string) *pyjson.Object {
		if record == nil {
			return nil
		}
		obj := pyjson.NewObject()
		for _, k := range []string{"pid", "code_sha", "hermes_home", "served_profiles"} {
			if v, ok := record[k]; ok {
				obj.Set(k, v)
			}
		}
		return obj
	}
}

func TestPoolOfAnUntickedHomeIsReaped(t *testing.T) {
	a, b := ownershipHomes(t)
	registerTickedHomes([]string{a, b})
	if got := tickedHomes(); len(got) != 2 || got[home.Key(a)] != a || got[home.Key(b)] != b {
		t.Fatalf("ticked homes %v", got)
	}
	if !servesProfile(a) || !servesProfile(b) || !servesProfile("") {
		t.Fatal("registered homes are not served")
	}
	cronB := filepath.Join(b, "cron")
	noteUnwritableAt(slog.New(slog.DiscardHandler), cronB, &os.PathError{Op: "open", Path: cronB, Err: syscall.EROFS}, "jobs not saved", "test")
	if DegradedRecord(cronB) == nil {
		t.Fatal("degraded record not stored")
	}
	registerTickedHomes([]string{a})
	if !reflect.DeepEqual(testHooks.discarded, []string{home.Key(b)}) {
		t.Fatalf("departed pools %v", testHooks.discarded)
	}
	if servesProfile(b) || !servesProfile(a) {
		t.Fatal("served set did not follow the republished homes")
	}
	if DegradedRecord(cronB) != nil {
		t.Fatal("departed home kept its store health state")
	}
}

func TestSharedStoreOfADepartedHomeKeepsItsState(t *testing.T) {
	a, b := ownershipHomes(t)
	if err := os.RemoveAll(filepath.Join(b, "cron")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a, "cron"), filepath.Join(b, "cron")); err != nil {
		t.Fatal(err)
	}
	registerTickedHomes([]string{a, b})
	cronA := filepath.Join(a, "cron")
	noteUnwritableAt(slog.New(slog.DiscardHandler), cronA, &os.PathError{Op: "open", Path: cronA, Err: syscall.EROFS}, "jobs not saved", "test")
	t.Cleanup(func() { ForgetStores([]string{StoreKey(a)}) })
	registerTickedHomes([]string{a})
	if DegradedRecord(cronA) == nil {
		t.Fatal("store still reached by a ticked home was forgotten")
	}
	if !reflect.DeepEqual(testHooks.discarded, []string{home.Key(b)}) {
		t.Fatalf("departed pools %v", testHooks.discarded)
	}
}

func TestHostMultiplexerOwnsOnlyTheHomesItTicks(t *testing.T) {
	a, b := ownershipHomes(t)
	gatewayProbes.ownsRuntimeLock = func() bool { return true }
	registerTickedHomes([]string{a})
	if !ownsCronTickFor(a) || ownsCronTickFor(b) {
		t.Fatal("runtime lock excused a home this process does not tick")
	}
	gatewayProbes.ownsRuntimeLock = func() bool { return false }
	if ownsCronTickFor(a) {
		t.Fatal("ticked home owned without the runtime lock")
	}
	gatewayProbes.ownsRuntimeLock = func() bool { panic("status unreadable") }
	if ownsCronTickFor(a) {
		t.Fatal("probe failure answered yes")
	}
	if liveGatewayTicking(a) != nil {
		t.Fatal("probe failure proved a foreign ticker")
	}
}

func TestLiveGatewayTickingIsAnsweredPerServedProfile(t *testing.T) {
	a, b := ownershipHomes(t)
	disk := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	foreignGateway(map[string]any{"pid": int64(4321), "code_sha": disk, "hermes_home": a})
	registerTickedHomes([]string{a, b})
	if rec := liveGatewayTicking(a); rec == nil {
		t.Fatal("fresher gateway serving A was not found")
	} else if sha, _ := rec.Get("code_sha"); sha != disk {
		t.Fatalf("record %v", sha)
	}
	if liveGatewayTicking(b) != nil {
		t.Fatal("profile B has no fresher ticker")
	}
	foreignGateway(map[string]any{"pid": 4321.0, "hermes_home": a, "served_profiles": []any{"gamma", "beta"}})
	if liveGatewayTicking(b) == nil {
		t.Fatal("served set naming beta did not cover profile B")
	}
	foreignGateway(map[string]any{"pid": int64(99), "hermes_home": a})
	if liveGatewayTicking(a) != nil {
		t.Fatal("record of a different pid was trusted")
	}
	foreignGateway(map[string]any{"pid": int64(4321), "hermes_home": a})
	gatewayProbes.runtimeStatusIsStale = func(*pyjson.Object, int) bool { return true }
	if liveGatewayTicking(a) != nil {
		t.Fatal("stale heartbeat was trusted")
	}
	foreignGateway(map[string]any{"pid": int64(4321), "hermes_home": a})
	gatewayProbes.runtimeLockActive = func(string) (bool, error) { return false, nil }
	if liveGatewayTicking(a) != nil {
		t.Fatal("no lock held yet a ticker was proven")
	}
	gatewayProbes.runtimeLockActive = func(string) (bool, error) { return false, errors.New("lock probe failed") }
	if liveGatewayTicking(a) != nil {
		t.Fatal("lock probe failure proved a ticker")
	}
	foreignGateway(map[string]any{"pid": int64(4321), "hermes_home": a})
	gatewayProbes.runningPid = func(string, bool) (int, bool, error) { return 0, false, nil }
	if liveGatewayTicking(a) != nil {
		t.Fatal("missing pid file proved a ticker")
	}
	foreignGateway(nil)
	if liveGatewayTicking(a) != nil {
		t.Fatal("missing status record proved a ticker")
	}
	foreignGateway(map[string]any{"pid": int64(4321), "hermes_home": a})
	gatewayProbes.ownsRuntimeLock = func() bool { return true }
	if liveGatewayTicking(a) != nil {
		t.Fatal("this process holding the lock is never a foreign ticker")
	}
}

func TestRecordServesProfileFallsBackToTheProcessHome(t *testing.T) {
	a, b := ownershipHomes(t)
	if recordServesProfile(nil, a) {
		t.Fatal("nil record proved a home")
	}
	rec := pyjson.NewObject()
	rec.Set("hermes_home", "   ")
	if !recordServesProfile(rec, a) {
		t.Fatal("blank hermes_home did not fall back to the process home")
	}
	if recordServesProfile(rec, b) {
		t.Fatal("empty served set covered another profile")
	}
	rec.Set("served_profiles", "beta")
	if recordServesProfile(rec, b) {
		t.Fatal("non list served set was trusted")
	}
	gatewayProbes.sameHermesHome = func(string, string) (bool, error) { return false, errors.New("realpath failed") }
	rec.Set("served_profiles", []any{"beta"})
	if recordServesProfile(rec, b) {
		t.Fatal("home comparison failure answered yes")
	}
}

func TestClaimOwnerIsDeadOnlyForAProvenLocalDeath(t *testing.T) {
	ownershipHomes(t)
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	var probed []int
	gatewayProbes.pidExists = func(pid int) bool { probed = append(probed, pid); return pid == 7 }
	cases := []struct {
		by   string
		dead bool
	}{
		{"", false},
		{host, false},
		{host + ":abc", false},
		{host + ":", false},
		{"other-host:123", false},
		{host + ":7", false},
		{host + ":123", true},
		{host + ":123:token", true},
		{host + ":١٢٣", true},
		{host + ":99999999999999999999999", false},
	}
	for _, tc := range cases {
		if got := claimOwnerIsDead(&Claim{By: tc.by}); got != tc.dead {
			t.Errorf("claim by %q dead=%v want %v", tc.by, got, tc.dead)
		}
	}
	if fmt.Sprint(probed) != "[7 123 123 123]" {
		t.Fatalf("pid probes %v", probed)
	}
	if claimOwnerIsDead(nil) {
		t.Fatal("nil claim proved dead")
	}
	gatewayProbes.hostname = func() (string, error) { return "", errors.New("no hostname") }
	if claimOwnerIsDead(&Claim{By: host + ":123"}) {
		t.Fatal("hostname failure proved a death")
	}
}
