package gateway

import (
	"math"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestCappedDrainFitsInsideLaunchdBudgetMinusReserve(t *testing.T) {
	capped := 60.0 - LaunchdStopCleanupReserveS
	if ResolveLaunchdCappedDrain(180.0, 60.0, LaunchdStopCleanupReserveS) != capped ||
		ResolveLaunchdCappedDrain(20.0, 60.0, LaunchdStopCleanupReserveS) != 20.0 ||
		ResolveLaunchdCappedDrain(180.0, nil, LaunchdStopCleanupReserveS) != 180.0 {
		t.Fatal("capped drain")
	}
	direct := func(label string) map[string]string { return map[string]string{"XPC_SERVICE_NAME": label} }
	grandchild := func(label string) map[string]string {
		return map[string]string{"XPC_SERVICE_NAME": "0", LaunchdLabelEnv: label}
	}
	uid := 501
	fake := func(argv []string, timeout time.Duration) (string, int, error) {
		if argv[2] != "gui/501/ai.icebath.gateway" || timeout != 5*time.Second {
			t.Fatalf("launchctl %v %v", argv, timeout)
		}
		return "exit timeout = 60\n", 0, nil
	}
	for _, c := range []struct {
		platform string
		environ  map[string]string
		want     float64
	}{
		{"darwin", direct("ai.icebath.gateway"), capped}, {"darwin", grandchild("ai.icebath.gateway"), capped},
		{"darwin", direct("application.com.example.ide.123"), 180.0}, {"darwin", grandchild("application.com.example.ide.123"), 180.0},
		{"darwin", map[string]string{"XPC_SERVICE_NAME": "0"}, 180.0},
		{"linux", direct("ai.icebath.gateway"), 180.0}, {"linux", grandchild("ai.icebath.gateway"), 180.0},
	} {
		budget := ReadLaunchdExitTimeoutS(LaunchdExitTimeoutQuery{Environ: c.environ, UID: &uid, Run: fake, Platform: c.platform})
		var b any
		if budget != nil {
			b = *budget
		}
		if got := ResolveLaunchdCappedDrain(180.0, b, LaunchdStopCleanupReserveS); got != c.want {
			t.Fatalf("%s %v -> %v", c.platform, c.environ, got)
		}
	}
	gc := grandchild("ai.icebath.gateway")
	if LaunchdServiceLabel(gc, "darwin") != "ai.icebath.gateway" || !IsGatewaySupervisorProcess(gc) ||
		IsGatewaySupervisorProcess(map[string]string{"XPC_SERVICE_NAME": "0"}) || IsGatewaySupervisorProcess(grandchild("application.com.x.1")) {
		t.Fatal("forwarded label marks the grandchild supervised")
	}
}

func TestEffectiveDrainCappedOnlyForSignalStopsUnderLaunchd(t *testing.T) {
	signalStop := StopTiming{RestartDrainTimeout: f64(180), LaunchdExitTimeoutS: f64(60), StopRequestedBySignal: true}
	if EffectiveStopDrainTimeout(signalStop) != 50.0 ||
		EffectiveStopDrainTimeout(StopTiming{RestartDrainTimeout: f64(180), LaunchdExitTimeoutS: f64(60)}) != 180.0 ||
		EffectiveStopDrainTimeout(StopTiming{RestartDrainTimeout: f64(180), StopRequestedBySignal: true}) != 180.0 {
		t.Fatal("effective drain")
	}
	leash := ResolveShutdownWatchdogDelay(EffectiveStopDrainTimeout(signalStop), DefaultShutdownWatchdogGraceS)
	if !(EffectiveStopWatchdogDelay(signalStop, leash) < 60.0 && 60.0 < leash) {
		t.Fatalf("leash %v", leash)
	}
	if EffectiveStopDrainTimeout(StopTiming{}) != DefaultGatewayRestartDrainTimeout {
		t.Fatal("default drain")
	}
}

func TestRestartTimeoutParsersAndBudgets(t *testing.T) {
	if DefaultGatewayRestartDrainTimeout != 0 || DefaultGatewayCronDrainTimeout != 30 || DefaultGatewayRestartAfterTurnTimeout != 1800 || DefaultGatewaySignalInterruptGraceTimeout != 1 {
		t.Fatal("shipped defaults")
	}
	for raw, want := range map[any]float64{nil: 0, 0: 0, "": 0, "45": 45, -3: 0, "x": 0} {
		if got := ParseRestartDrainTimeout(raw); got != want {
			t.Fatalf("drain %v -> %v", raw, got)
		}
	}
	for raw, want := range map[any]float64{nil: 30, "  ": 30, 0: 0, "0": 0, 12.5: 12.5, "bad": 30, -1: 0} {
		if got := ParseCronDrainTimeout(raw); got != want {
			t.Fatalf("cron %v -> %v", raw, got)
		}
	}
	if ParseRestartAfterTurnTimeout(nil) != 1800 || ParseRestartAfterTurnTimeout(0) != 0 ||
		ParseSignalInterruptGraceTimeout("inf") != 1 || ParseSignalInterruptGraceTimeout("nan") != 1 || ParseSignalInterruptGraceTimeout(2) != 2 {
		t.Fatal("after turn and grace")
	}
	if ResolveCronDrainBudget(0, 30, 70, 0, CronDrainCleanupReserveS) != 30 || ResolveCronDrainBudget(0, 30, 30, 5, CronDrainCleanupReserveS) != 15 ||
		ResolveCronDrainBudget(20, 30, 25, 0, CronDrainCleanupReserveS) != 20 || ResolveCronDrainBudget(5, 0, 100, 0, CronDrainCleanupReserveS) != 5 {
		t.Fatal("cron drain budget")
	}
	for _, c := range []struct{ drain, cron, want float64 }{{0, 30, 70}, {0, 0, 60}, {100, 30, 130}, {0, 50, 90}} {
		got, err := DefaultSystemdTimeoutStopSec(c.drain)
		if c.cron != DefaultGatewayCronDrainTimeout {
			got, err = ResolveSystemdTimeoutStopSec(c.drain, c.cron, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
		}
		if err != nil || got.Int64() != int64(c.want) {
			t.Fatalf("%+v -> %v %v", c, got, err)
		}
	}
	if _, err := ResolveSystemdTimeoutStopSec(math.Inf(1), 30, 10, 30, 60); err == nil || err.(*PyError).Kind != "OverflowError" {
		t.Fatalf("infinite drain %v", err)
	}
	if b, err := ResolveRestartExitWaitBudget(math.Inf(1), 1800, 30, 15); err != nil || !math.IsInf(b, 1) {
		t.Fatalf("infinite wait %v %v", b, err)
	}
	if b, err := ResolveRestartExitWaitBudget(0, 1800, 30, 15); err != nil || b != 1800+70+15 {
		t.Fatalf("wait budget %v %v", b, err)
	}
}

func TestRestartSupervisorAndExitHelpers(t *testing.T) {
	if MapFatalConfigExitForLaunchd(78) != 0 || MapFatalConfigExitForLaunchd(75) != 75 || MapFatalConfigExitForLaunchd(1) != 1 {
		t.Fatal("launchd exit map")
	}
	if p := ParseLaunchdExitTimeout("state = running\n\texit timeout = 45 \n"); p == nil || *p != 45 {
		t.Fatalf("parse %v", p)
	}
	if ParseLaunchdExitTimeout(nil) != nil || ParseLaunchdExitTimeout("exit timeout = x") != nil {
		t.Fatal("parse absent")
	}
	for code, want := range map[string]bool{"telegram_lock": true, " LOCK_CONFLICT ": true, "": false, "rate_limited": false} {
		if IsGlobalStartupConflict(code) != want {
			t.Fatalf("conflict %q", code)
		}
	}
	for env, want := range map[string]bool{"INVOCATION_ID": true, "ICEBATH_S6_SUPERVISED_CHILD": true, ExternalGatewaySupervisorEnv: true, "ICEBATH_SUPERVISED_CHILD": false} {
		val := "x"
		if env == ExternalGatewaySupervisorEnv {
			val = " Yes "
		}
		if IsGatewaySupervisorProcess(map[string]string{env: val}) != want {
			t.Fatalf("supervisor %s", env)
		}
	}
	if !IsSupervisedGatewayLaunch(map[string]string{"ICEBATH_SUPERVISED_CHILD": "1"}) || IsSupervisedGatewayLaunch(map[string]string{}) {
		t.Fatal("supervised launch")
	}
	if ReadLaunchdExitTimeoutS(LaunchdExitTimeoutQuery{Label: "ai.icebath.gateway", UID: new(int), Run: func(argv []string, _ time.Duration) (string, int, error) {
		if argv[2] != "system/ai.icebath.gateway" {
			t.Fatalf("root domain %v", argv)
		}
		return "", 1, nil
	}}) != nil {
		t.Fatal("failed launchctl yields no budget")
	}
}

func TestCronDrainFloorParsersAndBudgets(t *testing.T) {
	for _, raw := range []any{nil, "", "   ", "soon"} {
		if ParseCronDrainTimeout(raw) != DefaultGatewayCronDrainTimeout {
			t.Fatalf("cron %q falls back", raw)
		}
	}
	if ParseCronDrainTimeout(0) != 0 || ParseCronDrainTimeout("0") != 0 || ParseCronDrainTimeout(-5) != 0 {
		t.Fatal("zero opt out and negative clamp")
	}
	r := CronDrainCleanupReserveS
	if ResolveCronDrainBudget(0.0, 30.0, 60.0, 1.0, r) != 30.0 ||
		math.Abs(ResolveCronDrainBudget(0.0, 300.0, 60.0, 5.0, r)-(60.0-5.0-r)) > 1e-6 ||
		ResolveCronDrainBudget(120.0, 30.0, 180.0, 0.0, r) != 120.0 ||
		ResolveCronDrainBudget(0.0, 30.0, 60.0, 59.0, r) != 0.0 ||
		ResolveCronDrainBudget(0.0, 0.0, 60.0, 0.0, r) != 0.0 ||
		ResolveCronDrainBudget(nil, "30", 60.0, nil, r) != 30.0 {
		t.Fatal("cron drain budget")
	}
	stop := func(drain, cron any) int64 {
		n, err := ResolveSystemdTimeoutStopSec(drain, cron, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
		if err != nil {
			t.Fatal(err)
		}
		return n.Int64()
	}
	if stop(60.0, 30.0) != 90 || stop(180.0, 30.0) != 210 || stop(0.0, 60.0) != 100 || stop(0.0, 0.0) != 60 ||
		stop(180.0, 30.0) != stop(180.0, 0.0) || stop("soon", nil) != 60 {
		t.Fatal("systemd TimeoutStopSec")
	}
}
