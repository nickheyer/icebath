package observability

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// gatewayRow is one captured decision mark with the profile home that owned it
type gatewayRow struct {
	home string
	data map[string]string
}

// gatewayCapture collects every recorded mark under collection the test switches
type gatewayCapture struct {
	mu   sync.Mutex
	on   bool
	rows []struct {
		mark string
		row  gatewayRow
	}
}

// Captures gateway marks with collection on and every worker side table cleared
func captureGatewayRows(t *testing.T) *gatewayCapture {
	t.Helper()
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "home"))
	c := &gatewayCapture{on: true}
	savedEnabled, savedRecord, savedToday := relayEnabled, recordProcessMark, gatewayToday
	t.Cleanup(func() { relayEnabled, recordProcessMark, gatewayToday = savedEnabled, savedRecord, savedToday })
	relayEnabled = func(context.Context) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.on
	}
	recordProcessMark = func(ctx context.Context, mark string, data map[string]string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.rows = append(c.rows, struct {
			mark string
			row  gatewayRow
		}{mark, gatewayRow{home.Dir(ctx), data}})
	}
	replyMu.Lock()
	replyClocks.clear()
	chatHomes.clear()
	replyMu.Unlock()
	failingMu.Lock()
	failingConnects = map[[2]string]string{}
	failingMu.Unlock()
	cronMu.Lock()
	cronDelivery.clear()
	cronSkipped = map[string]bool{}
	cronMu.Unlock()
	return c
}

// Collection switch the capture's enabled policy reads
func (c *gatewayCapture) set(on bool) {
	c.mu.Lock()
	c.on = on
	c.mu.Unlock()
}

// Drained rows of one metric, each checked against the contract
func (c *gatewayCapture) read(t *testing.T, metric string) []gatewayRow {
	t.Helper()
	if err := Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []gatewayRow
	for _, r := range c.rows {
		if decisionMarkMetrics[r.mark] != metric {
			continue
		}
		dims := map[string]any{}
		for k, v := range r.row.data {
			dims[k] = v
		}
		if !CounterDimensionsAreValid(metric, dims) {
			t.Fatalf("%s row %v is not contract valid", metric, r.row.data)
		}
		out = append(out, r.row)
	}
	return out
}

// Data maps of one metric's rows
func (c *gatewayCapture) data(t *testing.T, metric string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, r := range c.read(t, metric) {
		out = append(out, r.data)
	}
	return out
}

// fakeAdapter is a gateway adapter with the platform, fatal error and fronting surface the recorders read
type fakeAdapter struct {
	platform  any
	fatal     any
	retryable bool
	fronted   func(chatID string) (any, error)
}

func (a *fakeAdapter) MetricsPlatform() any      { return a.platform }
func (a *fakeAdapter) FatalErrorCode() any       { return a.fatal }
func (a *fakeAdapter) FatalErrorRetryable() bool { return a.retryable }
func (a *fakeAdapter) MetricsPlatformFor(chatID string) (any, error) {
	if a.fronted == nil {
		return nil, nil
	}
	return a.fronted(chatID)
}

// fakeSource is an inbound message source
type fakeSource struct{ platform, chatID any }

func (s fakeSource) MetricsPlatform() any { return s.platform }
func (s fakeSource) MetricsChatID() any   { return s.chatID }

// fakeConsumer is a stream consumer whose transport sends visible text
type fakeConsumer struct {
	adapter, chatID any
	last            any
}

func (c *fakeConsumer) LastSentText() any   { return c.last }
func (c *fakeConsumer) MetricsAdapter() any { return c.adapter }
func (c *fakeConsumer) MetricsChatID() any  { return c.chatID }

// Runs one stream send or edit that delivers text
func (c *fakeConsumer) sendOrEdit(ctx context.Context, text string) bool {
	ok, _ := StopsReplyClock(ctx, c, func() (any, error) {
		c.last = text
		return true, nil
	})
	return ok == true
}

// Connects an adapter through the runner seam, the adapter's own error coming back unchanged
func connectFake(ctx context.Context, a *fakeAdapter, isReconnect bool, outcome any) any {
	switch o := outcome.(type) {
	case error:
		RecordPlatformConnect(ctx, a, a.platform, isReconnect, false, o)
		return o
	case bool:
		if !o {
			a.fatal, a.retryable = "telegram_missing_dependency", false
		}
		RecordPlatformConnect(ctx, a, a.platform, isReconnect, o, nil)
		return o
	}
	panic("connect outcome")
}

func connReset() error { return os.NewSyscallError("read", syscall.ECONNRESET) }

func TestEveryGatewayRowIsContractValidAndDisabledConfigRecordsNothing(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	adapter := &fakeAdapter{platform: "telegram", fatal: "telegram_auth_error", retryable: true}
	RecordPlatformConnect(ctx, adapter, "telegram", false, false, context.DeadlineExceeded)
	RecordPlatformDisconnect(ctx, adapter, "")
	RecordCronMissed(ctx, map[string]any{"deliver": "telegram:-100123"})
	want := []map[string]string{
		{"error_class": "network", "event": "connect_failed", "platform": "telegram"},
		{"error_class": "auth", "event": "disconnect", "platform": "telegram"},
	}
	if got := c.data(t, PlatformHealthMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("health rows %v", got)
	}
	cron := []map[string]string{{"delivery_kind": "platform", "duration_bucket": "lt_1s", "outcome": "missed"}}
	if got := c.data(t, CronRunMetric); !reflect.DeepEqual(got, cron) {
		t.Fatalf("cron rows %v", got)
	}
	c.set(false)
	RecordPlatformDisconnect(ctx, adapter, "")
	if got := c.data(t, PlatformHealthMetric); len(got) != 2 {
		t.Fatalf("disabled collection recorded %v", got)
	}
}

func TestPlatformNamesArePublishedOrProvenByTheInstallerRecord(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	savedOwner := CatalogPlatformOwnerHook
	t.Cleanup(func() {
		CatalogPlatformOwnerHook = savedOwner
		SetCatalogSource("bundled_platform_names", nil)
		SetCatalogSource("catalog_platform_names", nil)
		ResetCatalogCaches()
	})
	SetCatalogSource("bundled_platform_names", func() ([]string, error) { return []string{"irc"}, nil })
	SetCatalogSource("catalog_platform_names", func() ([]string, error) { return []string{"sendblue", "vk-platform"}, nil })
	CatalogPlatformOwnerHook = func(homeDir, platform string) (any, bool) {
		if platform == "vk" {
			return "vk-platform", true
		}
		return nil, false
	}
	ResetCatalogCaches()
	ctx := context.Background()
	for raw, want := range map[string]string{"telegram": "telegram", "irc": "irc", "vk": "vk-platform", "sneaky": "plugin", "my-private-bridge": "plugin"} {
		if got := AdapterPlatform(ctx, raw); got != want {
			t.Fatalf("adapter platform %q is %q, want %q", raw, got, want)
		}
	}
	if got := GatewayPlatform(ctx, map[string]any{"platform": "vk", "surface": "gateway"}, "gateway"); got != "vk-platform" {
		t.Fatalf("gateway platform %q", got)
	}
	if !GatewayPlatforms.Has("vk-platform") {
		t.Fatal("vk-platform missing from the gateway platforms")
	}
}

func TestEveryConnectAttemptRecordsOneHealthRowWithAClosedErrorClass(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	connectFake(ctx, &fakeAdapter{platform: "telegram", retryable: true}, false, true)
	connectFake(ctx, &fakeAdapter{platform: "telegram", retryable: true}, true, true)
	connectFake(ctx, &fakeAdapter{platform: "telegram", retryable: true}, false, false)
	connectFake(ctx, &fakeAdapter{platform: "telegram", retryable: true}, false, connReset())
	want := []map[string]string{
		{"error_class": "none", "event": "connect_ok", "platform": "telegram"},
		{"error_class": "none", "event": "reconnect", "platform": "telegram"},
		{"error_class": "config", "event": "connect_failed", "platform": "telegram"},
		{"error_class": "network", "event": "connect_failed", "platform": "telegram"},
	}
	if got := c.data(t, PlatformHealthMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("health rows %v", got)
	}
}

func TestAReconnectBackoffLoopIsOneFailedConnectUntilItRecovers(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	fake := func(p string) *fakeAdapter { return &fakeAdapter{platform: p, retryable: true} }
	c.set(false)
	connectFake(ctx, fake("telegram"), false, connReset())
	if err := Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	c.set(true)
	connectFake(ctx, fake("telegram"), false, connReset())
	for i := 0; i < 5; i++ {
		connectFake(ctx, fake("telegram"), true, connReset())
	}
	connectFake(ctx, fake("discord"), false, connReset())
	connectFake(ctx, fake("telegram"), true, true)
	connectFake(ctx, fake("telegram"), true, connReset())
	if err := Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	failingMu.Lock()
	for k := range failingConnects {
		failingConnects[k] = "2000-01-01"
	}
	failingMu.Unlock()
	connectFake(ctx, fake("telegram"), true, connReset())
	for _, step := range []struct {
		name    string
		outcome any
	}{{"alpha", connReset()}, {"beta", connReset()}, {"beta", true}, {"alpha", connReset()}} {
		connectFake(ctx, fake(step.name), true, step.outcome)
	}
	var got [][2]string
	for _, r := range c.data(t, PlatformHealthMetric) {
		got = append(got, [2]string{r["event"], r["platform"]})
	}
	want := [][2]string{
		{"connect_failed", "telegram"}, {"connect_failed", "discord"},
		{"reconnect", "telegram"}, {"connect_failed", "telegram"}, {"connect_failed", "telegram"},
		{"connect_failed", "plugin"}, {"connect_failed", "plugin"}, {"reconnect", "plugin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("health rows %v", got)
	}
}

func TestOneLogicalDeliveryIsOneRowWhateverTheRetries(t *testing.T) {
	c := captureGatewayRows(t)
	savedHook := ClassifySendErrorHook
	t.Cleanup(func() { ClassifySendErrorHook = savedHook })
	ClassifySendErrorHook = func(string) string { return "" }
	ctx := context.Background()
	adapter := &fakeAdapter{platform: "telegram", retryable: true}
	retries := func(results ...*SendResult) func() (any, error) {
		return func() (any, error) {
			var last *SendResult
			for _, r := range results {
				if last = r; r.Success == true {
					break
				}
			}
			return last, nil
		}
	}
	flaky := retries(&SendResult{Success: false, Error: "ConnectionError: reset", Retryable: true}, &SendResult{Success: true})
	if res, _ := RecordsDelivery(ctx, adapter, "42", flaky); res.(*SendResult).Success != true {
		t.Fatal("flaky send did not recover")
	}
	blocked := &SendResult{Success: false, Error: "Forbidden: bot was blocked", ErrorKind: "forbidden"}
	if res, _ := RecordsDelivery(ctx, adapter, "42", retries(blocked, blocked, blocked)); res.(*SendResult).Success != false {
		t.Fatal("blocked send succeeded")
	}
	want := []map[string]string{
		{"failure_class": "none", "outcome": "sent", "platform": "telegram"},
		{"failure_class": "forbidden", "outcome": "failed", "platform": "telegram"},
	}
	if got := c.data(t, PlatformDeliveryMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery rows %v", got)
	}
}

func TestFirstReplyLatencyIsOneRowPerTurnStoppedOnlyByReplyText(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	adapter := &fakeAdapter{platform: "telegram", retryable: true}
	sent := func() (any, error) { return &SendResult{Success: true}, nil }
	StartReplyClock(ctx, fakeSource{"telegram", "42"}, false)
	if _, err := RecordsDelivery(ctx, adapter, "42", sent); err != nil {
		t.Fatal(err)
	}
	if got := c.data(t, ReplyLatencyMetric); len(got) != 0 {
		t.Fatalf("busy ack stopped the clock %v", got)
	}
	consumer := &fakeConsumer{adapter: adapter, chatID: "42"}
	if !consumer.sendOrEdit(ctx, "first streamed chunk") {
		t.Fatal("stream send failed")
	}
	consumer.sendOrEdit(ctx, "first streamed chunk, more")
	StartReplyClock(ctx, fakeSource{"telegram", "8"}, false)
	StopReplyClock(ctx, adapter, "8", &SendResult{Success: true})
	StartReplyClock(ctx, fakeSource{"telegram", "7"}, true)
	StopReplyClock(ctx, adapter, "8", &SendResult{Success: true})
	row := map[string]string{"first_response_bucket": "lt_2s", "platform": "telegram"}
	if got := c.data(t, ReplyLatencyMetric); !reflect.DeepEqual(got, []map[string]string{row, row}) {
		t.Fatalf("reply rows %v", got)
	}
}

func TestARelayDeliveredTurnStopsTheClockItsInboundStarted(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	relay := &fakeAdapter{platform: "relay", retryable: true}
	StartReplyClock(ctx, fakeSource{"discord", "c1"}, false)
	if !(&fakeConsumer{adapter: relay, chatID: "c1"}).sendOrEdit(ctx, "first streamed chunk") {
		t.Fatal("stream send failed")
	}
	StartReplyClock(ctx, fakeSource{"slack", "C2"}, false)
	StopReplyClock(ctx, relay, "C2", &SendResult{Success: true})
	want := []map[string]string{
		{"first_response_bucket": "lt_2s", "platform": "discord"}, {"first_response_bucket": "lt_2s", "platform": "slack"},
	}
	if got := c.data(t, ReplyLatencyMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("reply rows %v", got)
	}
	replyMu.Lock()
	defer replyMu.Unlock()
	if len(replyClocks.keys) != 0 {
		t.Fatalf("clocks left %v", replyClocks.keys)
	}
}

func TestADeliverySentAfterTheRoutedScopeResetsLandsInTheOwningProfile(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	homeB := filepath.Join(t.TempDir(), "profiles", "b")
	StartReplyClock(home.WithOverride(ctx, homeB), fakeSource{"slack", "C9"}, false)
	sent := func() (any, error) { return &SendResult{Success: true}, nil }
	RecordsDelivery(ctx, &fakeAdapter{platform: "slack"}, "C9", sent)
	RecordsDelivery(ctx, &fakeAdapter{platform: "telegram"}, "unseen", sent)
	want := []gatewayRow{
		{homeB, map[string]string{"failure_class": "none", "outcome": "sent", "platform": "slack"}},
		{home.Dir(ctx), map[string]string{"failure_class": "none", "outcome": "sent", "platform": "telegram"}},
	}
	if got := c.read(t, PlatformDeliveryMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery rows %v", got)
	}
}

func TestFatalHandlersCountLostConnectionsInTheOwningProfileOnly(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	homeB := filepath.Join(t.TempDir(), "profiles", "b")
	RecordPlatformDisconnect(ctx, &fakeAdapter{platform: "telegram", fatal: "telegram_network_error"}, homeB)
	for _, code := range []string{"relay_disabled", "telegram_auth_error"} {
		RecordPlatformDisconnect(ctx, &fakeAdapter{platform: "relay", fatal: code}, "")
	}
	want := []gatewayRow{
		{homeB, map[string]string{"error_class": "network", "event": "disconnect", "platform": "telegram"}},
		{home.Dir(ctx), map[string]string{"error_class": "auth", "event": "disconnect", "platform": "relay"}},
	}
	if got := c.read(t, PlatformHealthMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("health rows %v", got)
	}
}

// closedWithCode is a websockets style closure carrying a close code outside the HTTP range
type closedWithCode struct{}

func (closedWithCode) Error() string { return "" }
func (closedWithCode) Code() int     { return 1011 }
func (closedWithCode) PyExceptionMRO() []string {
	return []string{"_ClosedWithDeprecatedCode", "ConnectionError", "OSError", "Exception", "BaseException"}
}

func TestClassifyingAConnectFailureNeverReplacesTheAdaptersException(t *testing.T) {
	c := captureGatewayRows(t)
	raised := closedWithCode{}
	seen := connectFake(context.Background(), &fakeAdapter{platform: "telegram", retryable: true}, false, raised)
	if seen != error(raised) {
		t.Fatalf("connect returned %v", seen)
	}
	want := []map[string]string{{"error_class": "network", "event": "connect_failed", "platform": "telegram"}}
	if got := c.data(t, PlatformHealthMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("health rows %v", got)
	}
}

func TestEachTerminalExecutionIsOneRowClassifiedWithoutJobDetails(t *testing.T) {
	c := captureGatewayRows(t)
	ctx := context.Background()
	delivered, suppressed := "delivered", "suppressed"
	start, end := "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00.5+00:00"
	note := func(id, deliver string) { NoteCronExecution(map[string]any{"execution_id": id, "deliver": deliver}) }
	note("e1", "telegram:-100555")
	RecordCronFinish(ctx, map[string]any{"id": "e1", "status": "completed", "started_at": start, "finished_at": end}, &delivered)
	note("e2", "local")
	RecordCronFinish(ctx, map[string]any{"id": "e2", "status": "failed", "started_at": start, "finished_at": end}, nil)
	note("e3", "webhook")
	RecordCronFinish(ctx, map[string]any{"id": "e3", "status": "failed", "started_at": nil, "finished_at": end}, nil)
	note("e4", "origin")
	NoteCronSkipped(map[string]any{"execution_id": "e4"})
	RecordCronFinish(ctx, map[string]any{"id": "e4", "status": "completed", "started_at": start, "finished_at": end}, &suppressed)
	var got [][2]string
	for _, r := range c.data(t, CronRunMetric) {
		got = append(got, [2]string{r["outcome"], r["delivery_kind"]})
	}
	want := [][2]string{{"success", "platform"}, {"failed", "local"}, {"skipped", "webhook"}, {"skipped", "platform"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cron rows %v", got)
	}
}

func TestAnOccurrenceDroppedAfterDowntimeIsOneMissedRow(t *testing.T) {
	c := captureGatewayRows(t)
	RecordCronMissed(context.Background(), map[string]any{"deliver": "local"})
	want := []map[string]string{{"delivery_kind": "local", "duration_bucket": "lt_1s", "outcome": "missed"}}
	if got := c.data(t, CronRunMetric); !reflect.DeepEqual(got, want) {
		t.Fatalf("cron rows %v", got)
	}
}

func TestCronDeliveryKindAndRunFields(t *testing.T) {
	for _, tc := range []struct {
		job  any
		want string
	}{
		{"local", "other"}, {map[string]any{}, "local"}, {map[string]any{"deliver": "local,"}, "local"},
		{map[string]any{"deliver": []any{"none", "telegram:1"}}, "none"}, {map[string]any{"deliver": "webhook:x"}, "webhook"},
		{map[string]any{"deliver": "webhook,slack:C1"}, "platform"}, {map[string]any{"deliver": " OFF "}, "none"},
	} {
		if got := CronDeliveryKind(tc.job); got != tc.want {
			t.Fatalf("delivery kind of %v is %q, want %q", tc.job, got, tc.want)
		}
	}
	if got := CronRunFields("bogus", "carrier", 3); !reflect.DeepEqual(got, map[string]string{"delivery_kind": "other", "duration_bucket": "1s_to_5s", "outcome": "failed"}) {
		t.Fatalf("cron fields %v", got)
	}
	if isoSeconds("2026-01-01T00:00:00", "2026-01-01T00:00:05+00:00") != 0 || isoSeconds(nil, "x") != 0 || isoSeconds("2026-01-01T00:00:05", "2026-01-01T00:00:00") != 0 {
		t.Fatal("unparseable or mixed timestamps must read as zero seconds")
	}
}

func TestSetRelayRecorderSwapsAndRestoresTheRelayHooks(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "home"))
	prevEnabled, prevRecord := reflect.ValueOf(relayEnabled).Pointer(), reflect.ValueOf(recordProcessMark).Pointer()
	var mu sync.Mutex
	var got []string
	restore := SetRelayRecorder(func(context.Context) bool { return true }, func(_ context.Context, mark string, data map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, mark+":"+data["outcome"])
	})
	RecordCronMissed(context.Background(), map[string]any{"deliver": "local"})
	if err := Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	restore()
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(got, []string{CronRunMark + ":missed"}) {
		t.Fatalf("captured %v", got)
	}
	if reflect.ValueOf(relayEnabled).Pointer() != prevEnabled || reflect.ValueOf(recordProcessMark).Pointer() != prevRecord {
		t.Fatal("restore left the swapped hooks installed")
	}
}
