package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Profile routes parsed from route mappings
func routesOf(t *testing.T, routes ...map[string]any) []*gateway.ProfileRoute {
	t.Helper()
	raw := make([]any, len(routes))
	for i, r := range routes {
		raw[i] = r
	}
	parsed, err := gateway.ParseProfileRoutes(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// Multiplexing runner with a Discord primary, profiles yuki and other and a yuki route on guild parent
func setupRoutedRunner(t *testing.T) (*GatewayRunner, string, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	h := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(h, "kanban.db"))
	for _, name := range []string{"yuki", "other"} {
		dir := filepath.Join(h, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformDiscord, newKanbanRecordingAdapterOn(gateway.PlatformDiscord))
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileAdapters.Set("yuki", adapterDict())
	r.primaryProfileName = "default"
	r.kanbanNotifierProfile = "default"
	f, err := os.CreateTemp(tmp, "lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r.kanbanDispatcherLockHandle = f
	r.config = gateway.DefaultGatewayConfig(context.Background())
	on := true
	r.config.MultiplexProfiles = &on
	r.config.ProfileRoutes = routesOf(t, map[string]any{"platform": "discord", "guild_id": "guild", "chat_id": "parent", "profile": "yuki"})
	return r, tmp, h
}

// Options of one routed completion
type routedCompletion struct {
	profile  any
	metadata map[string]any
	chat     string
	thread   string
	user     any
	mode     string
}

// Completed task with a Discord thread subscription built from the options with the Python defaults
func routedComplete(t *testing.T, o routedCompletion) string {
	t.Helper()
	if o.profile == nil {
		o.profile = "yuki"
	}
	if o.chat == "" {
		o.chat = "post"
	}
	if o.thread == "" {
		o.thread = "post"
	}
	if o.user == nil {
		o.user = "creator"
	}
	if o.mode == "" {
		o.mode = "notify+wake"
	}
	if o.metadata == nil {
		o.metadata = map[string]any{"guild_id": "guild", "scope_id": "guild", "parent_chat_id": "parent"}
	}
	conn, err := kanban.Connect(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tid := kanbanCreate(t, conn, "route completion", "worker", nil)
	sub := kanban.NotifySubOptions{TaskID: tid, Platform: "discord", ChatID: o.chat, ChatType: kstr("thread"),
		DeliveryMode: kstr(o.mode), DeliveryMetadata: o.metadata}
	if o.thread != "-" {
		sub.ThreadID = kstr(o.thread)
	} else {
		sub.ThreadID = kstr("")
	}
	if s, ok := o.profile.(string); ok && s != "-" {
		sub.NotifierProfile = kstr(s)
	}
	if s, ok := o.user.(string); ok && s != "-" {
		sub.UserID = kstr(s)
	}
	kanbanSubscribe(t, conn, sub)
	if _, err := kanban.CompleteTask(context.Background(), conn, tid, kanban.CompleteTaskOptions{Result: kstr("finished")}); err != nil {
		t.Fatal(err)
	}
	return tid
}

// Claimed deliveries of one collection pass for the default notifier profile
func routedCollect(t *testing.T, r *GatewayRunner) []*kanbanDelivery {
	t.Helper()
	rows, err := notifierCollect(context.Background(), r, "default", false, 30)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Delivers each claimed row
func routedDeliver(t *testing.T, ctx context.Context, r *GatewayRunner, rows []*kanbanDelivery) {
	t.Helper()
	for _, row := range rows {
		if err := newKanbanNotification(ctx, r, row, map[kanbanSubKey]int{}).deliver(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// Unseen completed events of the post thread subscription
func routedUnseen(t *testing.T, tid string) int {
	t.Helper()
	conn := kanbanTestConn(t)
	_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, "discord", "post", kstr("post"), []string{"completed"})
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

// Task ids of claimed rows
func rowTaskIDs(rows []*kanbanDelivery) string {
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.task.ID)
	}
	return strings.Join(ids, ",")
}

// Warning level log lines captured until the returned stop runs
func captureKanbanWarnings(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	stop := func() { slog.SetDefault(prev) }
	t.Cleanup(stop)
	return &buf, stop
}

func TestExactRoutedProfileDeliversOnceOnItsAuthorizedTransport(t *testing.T) {
	r, _, _ := setupRoutedRunner(t)
	primaryAny, _ := r.adapters.Get(gateway.PlatformDiscord)
	primary := primaryAny.(*kanbanRecordingAdapter)
	task := routedComplete(t, routedCompletion{metadata: map[string]any{"scope_id": "guild", "guild_id": "stale-alias", "parent_chat_id": "parent"}})
	rows := routedCollect(t, r)
	if rowTaskIDs(rows) != task {
		t.Fatalf("rows %s", rowTaskIDs(rows))
	}
	ctx := context.Background()
	routedDeliver(t, ctx, r, rows)
	if len(primary.sent) != 1 || len(primary.handled) != 1 {
		t.Fatalf("sent %d handled %d", len(primary.sent), len(primary.handled))
	}
	src := primary.handled[0].Source
	if deref(src.Profile) != "yuki" || deref(src.ScopeID) != "guild" || deref(src.ParentChatID) != "parent" {
		t.Fatalf("source %+v", src)
	}
	if a, err := r.deliveryAdapterFor(ctx, src); err != nil || a != gateway.PlatformAdapter(primary) {
		t.Fatalf("delivery adapter %v %v", a, err)
	}
	if len(routedCollect(t, r)) != 0 {
		t.Fatal("claimed twice")
	}
	secondary := &scopedRecordingAdapter{kanbanRecordingAdapter: newKanbanRecordingAdapterOn(gateway.PlatformDiscord), scope: "stale-cache"}
	secondary.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformDiscord, secondary)
	r.profileAdapters.Set("yuki", adapterDict(gateway.PlatformDiscord, secondary))
	task = routedComplete(t, routedCompletion{metadata: map[string]any{"guild_id": "guild", "parent_chat_id": "parent"}})
	routedDeliver(t, ctx, r, routedCollect(t, r))
	if len(primary.sent) != 1 || len(secondary.sent) != 1 || len(secondary.handled) != 1 {
		t.Fatalf("primary %d secondary %d/%d", len(primary.sent), len(secondary.sent), len(secondary.handled))
	}
	if deref(secondary.handled[0].Source.ScopeID) != "guild" {
		t.Fatalf("scope %v", secondary.handled[0].Source.ScopeID)
	}
	if a, err := r.deliveryAdapterFor(ctx, secondary.handled[0].Source); err != nil || a != gateway.PlatformAdapter(secondary) {
		t.Fatalf("delivery adapter %v %v", a, err)
	}
	if routedUnseen(t, task) != 0 {
		t.Fatal("unseen after delivery")
	}
}

// Recording adapter answering one workspace scope for every chat
type scopedRecordingAdapter struct {
	*kanbanRecordingAdapter
	scope string
}

func (a *scopedRecordingAdapter) ScopeIDForChat(string) *string { s := a.scope; return &s }

func TestUserRoutedSubscriptionUsesOnlyItsAuthorizedProfile(t *testing.T) {
	r, _, _ := setupRoutedRunner(t)
	primaryAny, _ := r.adapters.Get(gateway.PlatformDiscord)
	primary := primaryAny.(*kanbanRecordingAdapter)
	r.config.ProfileRoutes = routesOf(t, map[string]any{"platform": "discord", "user_id": "creator", "profile": "yuki"})
	routed := routedComplete(t, routedCompletion{chat: "shared", thread: "-"})
	rows := routedCollect(t, r)
	if rowTaskIDs(rows) != routed {
		t.Fatalf("rows %s", rowTaskIDs(rows))
	}
	routedDeliver(t, context.Background(), r, rows)
	if len(primary.sent) != 1 || len(primary.handled) != 1 || deref(primary.handled[0].Source.UserID) != "creator" {
		t.Fatalf("sent %d handled %d", len(primary.sent), len(primary.handled))
	}
	routedComplete(t, routedCompletion{profile: "default", chat: "shared", thread: "-"})
	routedComplete(t, routedCompletion{profile: "default", chat: "shared", thread: "-", user: "-"})
	if len(routedCollect(t, r)) != 0 {
		t.Fatal("default profile rows claimed")
	}
}

func TestRouteDenialsLeaveEventsRetryableAtClaimAndSend(t *testing.T) {
	r, tmp, h := setupRoutedRunner(t)
	primaryAny, _ := r.adapters.Get(gateway.PlatformDiscord)
	primary := primaryAny.(*kanbanRecordingAdapter)
	var tasks []string
	for _, owner := range []string{"other", "default", "-"} {
		tasks = append(tasks, routedComplete(t, routedCompletion{profile: owner}))
	}
	for _, meta := range []map[string]any{{"parent_chat_id": "parent"}, {"guild_id": "guild"}, {"guild_id": "wrong", "parent_chat_id": "parent"}} {
		tasks = append(tasks, routedComplete(t, routedCompletion{metadata: meta}))
	}
	if len(routedCollect(t, r)) != 0 {
		t.Fatal("denied rows claimed")
	}
	for _, task := range tasks {
		if routedUnseen(t, task) == 0 {
			t.Fatalf("task %s lost its event", task)
		}
	}
	good := routedComplete(t, routedCompletion{})
	yukiHome := filepath.Join(h, "profiles", "yuki")
	if err := home.MarkProfileDeleted(yukiHome); err != nil {
		t.Fatal(err)
	}
	if len(routedCollect(t, r)) != 0 {
		t.Fatal("deleted profile row claimed")
	}
	if err := home.ClearProfileDeleted(yukiHome); err != nil {
		t.Fatal(err)
	}
	rows := routedCollect(t, r)
	if rowTaskIDs(rows) != good {
		t.Fatalf("rows %s", rowTaskIDs(rows))
	}
	r.config.ProfileRoutes = routesOf(t, map[string]any{"platform": "discord", "guild_id": "guild", "chat_id": "parent", "profile": "other"})
	routedDeliver(t, context.Background(), r, rows)
	if len(primary.sent) != 0 || len(primary.handled) != 0 || routedUnseen(t, good) == 0 {
		t.Fatalf("sent %d handled %d", len(primary.sent), len(primary.handled))
	}

	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "tied-routes.db"))
	r.config.ProfileRoutes = routesOf(t,
		map[string]any{"platform": "discord", "guild_id": "guild", "chat_id": "parent", "profile": "other"},
		map[string]any{"platform": "discord", "guild_id": "guild", "chat_id": "post", "profile": "yuki"})
	ambiguous := routedComplete(t, routedCompletion{metadata: map[string]any{"scope_id": "guild"}})
	exact := routedComplete(t, routedCompletion{metadata: map[string]any{"scope_id": "guild", "parent_chat_id": "different-parent"}})
	rows = routedCollect(t, r)
	if rowTaskIDs(rows) != exact {
		t.Fatalf("rows %s", rowTaskIDs(rows))
	}
	routedDeliver(t, context.Background(), r, rows)
	if len(primary.sent) != 1 || len(primary.handled) != 1 || routedUnseen(t, ambiguous) == 0 {
		t.Fatalf("sent %d handled %d", len(primary.sent), len(primary.handled))
	}
}

// Recording adapter noting the profile, secret and home each wake turn runs under
type observingAdapter struct {
	*kanbanRecordingAdapter
	mu       *sync.Mutex
	observed *[][3]string
}

func (a *observingAdapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	secret, _, _ := provider.GetSecret(ctx, "KANBAN_TEST_SECRET")
	a.mu.Lock()
	*a.observed = append(*a.observed, [3]string{deref(event.Source.Profile), secret, home.Dir(ctx)})
	a.mu.Unlock()
	return a.kanbanRecordingAdapter.HandleMessage(ctx, event)
}

func TestKanbanWakesInstallTheDestinationRuntimeScope(t *testing.T) {
	r, _, h := setupRoutedRunner(t)
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("KANBAN_TEST_SECRET=primary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var observed [][3]string
	for _, name := range []string{"yuki", "other"} {
		if err := os.WriteFile(filepath.Join(h, "profiles", name, ".env"), []byte("KANBAN_TEST_SECRET="+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := &observingAdapter{kanbanRecordingAdapter: newKanbanRecordingAdapterOn(gateway.PlatformDiscord), mu: &mu, observed: &observed}
		a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformDiscord, a)
		r.profileAdapters.Set(name, adapterDict(gateway.PlatformDiscord, a))
		routedComplete(t, routedCompletion{profile: name})
	}
	rows := routedCollect(t, r)
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	ctx := gateway.ProfileRuntimeScope(context.Background(), h, true)
	var wg sync.WaitGroup
	errs := make([]error, len(rows))
	for i, row := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = newKanbanNotification(ctx, r, row, map[kanbanSubKey]int{}).deliver(ctx)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if secret, _, _ := provider.GetSecret(ctx, "KANBAN_TEST_SECRET"); secret != "primary" {
		t.Fatalf("outer secret %q", secret)
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i][0] < observed[j][0] })
	want := [][3]string{{"other", "other", filepath.Join(h, "profiles", "other")}, {"yuki", "yuki", filepath.Join(h, "profiles", "yuki")}}
	if len(observed) != 2 || observed[0] != want[0] || observed[1] != want[1] {
		t.Fatalf("observed %v", observed)
	}
}

func TestRemovedProfileNeverWakesUnderThePrimaryRuntime(t *testing.T) {
	r, _, h := setupRoutedRunner(t)
	secondary := newKanbanRecordingAdapterOn(gateway.PlatformDiscord)
	r.profileAdapters.Set("yuki", adapterDict(gateway.PlatformDiscord, secondary))
	task := routedComplete(t, routedCompletion{mode: "wake"})
	rows := routedCollect(t, r)
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	if err := os.RemoveAll(filepath.Join(h, "profiles", "yuki")); err != nil {
		t.Fatal(err)
	}
	routedDeliver(t, context.Background(), r, rows)
	if len(secondary.handled) != 0 || routedUnseen(t, task) == 0 {
		t.Fatalf("handled %d", len(secondary.handled))
	}
}

func TestAnchorlessThreadSubscriptionWarnsOnceInsteadOfSilentSkip(t *testing.T) {
	r, _, _ := setupRoutedRunner(t)
	kanbanWarnedMu.Lock()
	kanbanAnchorlessWarned = map[kanbanSubKey]bool{}
	kanbanWarnedMu.Unlock()
	task := routedComplete(t, routedCompletion{metadata: map[string]any{"chat_type": "thread"}})
	logs, stop := captureKanbanWarnings(t)
	if len(routedCollect(t, r)) != 0 || len(routedCollect(t, r)) != 0 {
		t.Fatal("anchorless row claimed")
	}
	stop()
	var warnings []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "parent_chat_id") && strings.Contains(line, task) {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "level=WARN") || !strings.Contains(warnings[0], "--parent-chat-id") {
		t.Fatalf("warnings %v", warnings)
	}
	if routedUnseen(t, task) == 0 {
		t.Fatal("event lost")
	}
}

func TestPinnedProfileWithoutThisPlatformDeliversViaPrimary(t *testing.T) {
	r, tmp, _ := setupRoutedRunner(t)
	primaryAny, _ := r.adapters.Get(gateway.PlatformDiscord)
	primary := primaryAny.(*kanbanRecordingAdapter)
	kanbanWarnedMu.Lock()
	kanbanUnroutableWarned = map[kanbanSubKey]bool{}
	kanbanWarnedMu.Unlock()
	r.profileAdapters.Set("yuki", adapterDict(gateway.PlatformTelegram, newKanbanRecordingAdapter()))
	task := routedComplete(t, routedCompletion{})
	logs, stop := captureKanbanWarnings(t)
	rows := routedCollect(t, r)
	if rowTaskIDs(rows) != task {
		t.Fatalf("rows %s", rowTaskIDs(rows))
	}
	routedDeliver(t, context.Background(), r, rows)
	stop()
	if len(primary.sent) != 1 || len(primary.handled) != 1 || deref(primary.handled[0].Source.Profile) != "yuki" {
		t.Fatalf("sent %d handled %d", len(primary.sent), len(primary.handled))
	}
	if routedUnseen(t, task) != 0 || strings.TrimSpace(logs.String()) != "" {
		t.Fatalf("warnings %q", logs.String())
	}

	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "stamped-owner.db"))
	stamped := routedComplete(t, routedCompletion{profile: "default"})
	logs, stop = captureKanbanWarnings(t)
	if len(routedCollect(t, r)) != 0 {
		t.Fatal("stamped row claimed")
	}
	stop()
	var warnings []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "pins that chat to profile yuki") && strings.Contains(line, stamped) {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "level=WARN") || !strings.Contains(warnings[0], "--notifier-profile yuki") {
		t.Fatalf("warnings %v", warnings)
	}
	if routedUnseen(t, stamped) == 0 {
		t.Fatal("event lost")
	}
}
