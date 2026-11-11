package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Event kinds the notifier claims for a subscription
var kanbanTerminalKinds = []string{"completed", "blocked", "gave_up", "crashed", "timed_out", "status", "archived", "unblocked", "block_loop_detected", "review_requested", "changes_requested"}

// Event kinds that hand a decision back to the origin, in wake text order
var kanbanWakeKinds = []string{"completed", "gave_up", "crashed", "timed_out", "blocked", "review_requested", "changes_requested", "block_loop_detected"}

// Notify subscription reads of the collector, the kanban package functions
var (
	kanbanClaimUnseenEventsForSub = kanban.ClaimUnseenEventsForSub
	kanbanListNotifySubs          = kanban.ListNotifySubs
)

// Consecutive send failures before a subscription is dropped as a dead chat
const kanbanMaxSendFailures = 12

var kanbanLocalPathRe = pyre.MustCompile(`(?<![\w:/])(?:/(?:Users|home|private|tmp|var|etc|workspace)/[^\s,;]+|[A-Za-z]:\\[^\s,;]+)`)

// Value of key in an event payload mapping, nil when the payload is no mapping or lacks it
func kanbanPayloadGet(payload any, key string) any {
	switch p := payload.(type) {
	case *pyjson.Object:
		return objValue(p, key)
	case map[string]any:
		return p[key]
	}
	return nil
}

// Whether the event is infrastructure attention rather than an explicit owner decision
func kanbanDiagnosticEvent(ev *kanban.Event) bool {
	switch ev.Kind {
	case "crashed", "timed_out", "gave_up":
		return true
	case "blocked", "block_loop_detected":
		return !pyrepr.Equal(kanbanPayloadGet(ev.Payload, "kind"), "needs_input")
	case "status":
		st := kanbanPayloadGet(ev.Payload, "status")
		return pyrepr.Equal(st, "blocked") || pyrepr.Equal(st, "triage")
	}
	return false
}

// Redacted single spaced review reason with local paths masked and clipped to limit characters
func safeReviewReason(ctx context.Context, value any, limit int) string {
	text := ""
	if value != nil {
		text = pyrepr.Str(value)
	}
	reason := tools.RedactSensitiveTextWith(&tools.Env{Home: home.Dir(ctx)}, text, tools.RedactOptions{Force: true, URLCredentials: true})
	if masked, err := kanbanLocalPathRe.Replace(reason, "[local path]", -1, -1); err == nil {
		reason = masked
	}
	reason = strings.Join(pystr.SplitWhitespace(reason, -1), " ")
	if len(pystr.Runes(reason)) > limit {
		reason = pystr.Rstrip(pystr.Slice(reason, 0, limit-1)) + "…"
	}
	return reason
}

// Tenant scope a subscription wake keys to, persisted delivery metadata first, nil for unscoped
func wakeScopeID(adapter gateway.PlatformAdapter, sub *pyjson.Object) *string {
	if meta, ok := objValue(sub, "delivery_metadata").(*pyjson.Object); ok {
		for _, key := range []string{"scope_id", "guild_id", "slack_team_id", "team_id"} {
			if v := objValue(meta, key); pyrepr.Truthy(v) {
				s := pyrepr.Str(v)
				return &s
			}
		}
	}
	resolver, ok := adapter.(scopeIDForChatAdapter)
	if !ok {
		return nil
	}
	chat := ""
	if v := objValue(sub, "chat_id"); pyrepr.Truthy(v) {
		chat = pyrepr.Str(v)
	}
	resolved, failure := func() (res *string, failure any) {
		defer func() { failure = recover() }()
		return resolver.ScopeIDForChat(chat), nil
	}()
	if failure != nil {
		runLog().Debug(fmt.Sprintf("kanban notifier: scope lookup failed for chat %s: %s", pyrepr.Str(objValue(sub, "chat_id")), pyrepr.Str(failure)))
		return nil
	}
	if resolved == nil || *resolved == "" {
		return nil
	}
	s := *resolved
	return &s
}

// Identity of one subscription row
type kanbanSubKey struct {
	taskID, platform, chatID, threadID string
}

var (
	kanbanWarnedMu         sync.Mutex
	kanbanAnchorlessWarned = map[kanbanSubKey]bool{}
	kanbanUnroutableWarned = map[kanbanSubKey]bool{}
)

// Subscription value as text, empty for a missing or None value
func subText(sub *pyjson.Object, key string) string {
	v := objValue(sub, key)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Truthy subscription value as text, empty otherwise
func subTruthyText(sub *pyjson.Object, key string) string {
	if v := objValue(sub, key); pyrepr.Truthy(v) {
		return subText(sub, key)
	}
	return ""
}

// Delivery metadata of a subscription, empty when absent
func subMetadata(sub *pyjson.Object) *pyjson.Object {
	if meta, ok := objValue(sub, "delivery_metadata").(*pyjson.Object); ok && meta.Len() > 0 {
		return meta
	}
	return pyjson.NewObject()
}

// Identity key of a subscription with an empty thread for none
func subKeyOf(sub *pyjson.Object, platform string) kanbanSubKey {
	return kanbanSubKey{subText(sub, "task_id"), platform, subText(sub, "chat_id"), subTruthyText(sub, "thread_id")}
}

// Whether a subscription is shaped like a thread
func subThreadLike(sub *pyjson.Object, meta *pyjson.Object) bool {
	if pyrepr.Truthy(objValue(sub, "thread_id")) {
		return true
	}
	chatType := objValue(sub, "chat_type")
	if !pyrepr.Truthy(chatType) {
		chatType = objValue(meta, "chat_type")
	}
	s, ok := chatType.(string)
	if !ok {
		return false
	}
	switch s {
	case "thread", "forum", "forum_post", "forum-post", "topic":
		return true
	}
	return false
}

// Warns once per row about a thread subscription with no parent chat anchor
func warnAnchorlessThreadSubOnce(sub *pyjson.Object, platform string) {
	meta := subMetadata(sub)
	if !subThreadLike(sub, meta) || pyrepr.Truthy(objValue(meta, "parent_chat_id")) {
		return
	}
	key := subKeyOf(sub, platform)
	kanbanWarnedMu.Lock()
	seen := kanbanAnchorlessWarned[key]
	kanbanAnchorlessWarned[key] = true
	kanbanWarnedMu.Unlock()
	if seen {
		return
	}
	runLog().Warn(fmt.Sprintf("kanban notifier: subscription for %s on %s thread %s has no parent_chat_id anchor and matched no "+
		"profile route; it will not be delivered. Re-subscribe with `icebath kanban notify-subscribe ... "+
		"--parent-chat-id <channel id> [--guild-id <guild id>]`.",
		pyrepr.Str(objValue(sub, "task_id")), platform, pyrepr.Str(objValue(sub, "chat_id"))))
}

// Warns once per row about a routed subscription the credential gate fails closed
func warnUnroutableSubOnce(sub *pyjson.Object, platform gateway.Platform, stamped, pinned string) {
	key := subKeyOf(sub, string(platform))
	kanbanWarnedMu.Lock()
	seen := kanbanUnroutableWarned[key]
	kanbanUnroutableWarned[key] = true
	kanbanWarnedMu.Unlock()
	if seen {
		return
	}
	runLog().Warn(fmt.Sprintf("kanban notifier: subscription for %s on %s chat %s is stamped with profile %s but a "+
		"profile_routes entry pins that chat to profile %s; it will not be delivered. "+
		"Re-subscribe with `icebath kanban notify-subscribe ... --notifier-profile %s`.",
		pyrepr.Str(objValue(sub, "task_id")), string(platform), pyrepr.Str(objValue(sub, "chat_id")), stamped, pinned, pinned))
}

// Lower cased platform names of an adapters mapping
func kanbanPlatformNames(m *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]) map[string]bool {
	out := map[string]bool{}
	for _, p := range m.Keys() {
		out[pystr.Lower(string(p))] = true
	}
	return out
}

// Optional route discriminator from a metadata value, nil for None
func routeArg(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	if str, ok := v.(string); ok {
		s = str
	}
	return &s
}

// First truthy of two values, else the second as Python or does
func pyOr(a, b any) any {
	if pyrepr.Truthy(a) {
		return a
	}
	return b
}

// Adapter that may deliver a durable subscription, never letting a missing secondary bot borrow primary authority
func adapterForSubscription(ctx context.Context, r *GatewayRunner, platform gateway.Platform, sub *pyjson.Object, ownerProfile string) (gateway.PlatformAdapter, error) {
	var owner *string
	if ownerProfile != "" {
		owner = &ownerProfile
	}
	adapter := r.authorizationAdapter(ctx, platform, owner)
	if !r.multiplexing() {
		return adapter, nil
	}
	primary, _ := r.adapters.Get(platform)
	if adapter != nil && adapter != primary {
		return adapter, nil
	}
	profile := ownerProfile
	if profile == "" {
		profile = r.kanbanNotifierProfile
	}
	primaryProfile := r.primaryProfileName
	if primaryProfile == "" {
		primaryProfile = r.activeProfileName(ctx)
	}
	if profile == "" {
		profile = primaryProfile
	}
	own, _ := r.profileAdapters.Get(profile)
	if kanbanPlatformNames(own)[pystr.Lower(string(platform))] {
		return nil, nil
	}
	meta := subMetadata(sub)
	guild := routeArg(pyOr(objValue(meta, "scope_id"), objValue(meta, "guild_id")))
	parent := routeArg(objValue(meta, "parent_chat_id"))
	chat := routeArg(objValue(sub, "chat_id"))
	thread := routeArg(pyOr(objValue(sub, "thread_id"), nil))
	userID := routeArg(pyOr(objValue(sub, "user_id"), nil))
	threadLike := pyrepr.Truthy(objValue(sub, "thread_id")) || subThreadLike(sub, meta)
	for _, route := range r.config.ProfileRoutes {
		hit, err := route.Matches(ctx, gateway.RouteQuery{Platform: string(platform), GuildID: guild, ChatID: chat,
			ThreadID: thread, ParentChatID: parent, UserID: userID})
		if err != nil {
			return nil, err
		}
		if hit {
			if route.Profile != profile {
				warnUnroutableSubOnce(sub, platform, profile, route.Profile)
				return nil, nil
			}
			served, err := gateway.MultiplexProfileHomes(ctx, r.config)
			if err != nil {
				return nil, err
			}
			for _, sp := range served {
				if sp.Name == profile {
					return primary, nil
				}
			}
			return nil, nil
		}
		widenedGuild, widenedParent, widenedUser := guild, parent, userID
		if !nonEmptyPtr(widenedGuild) {
			widenedGuild = route.GuildID
		}
		if !nonEmptyPtr(widenedParent) {
			widenedParent = nil
			if threadLike {
				widenedParent = route.ChatID
			}
		}
		if !nonEmptyPtr(widenedUser) {
			widenedUser = route.UserID
		}
		hit, err = route.Matches(ctx, gateway.RouteQuery{Platform: string(platform), GuildID: widenedGuild, ChatID: chat,
			ThreadID: thread, ParentChatID: widenedParent, UserID: widenedUser})
		if err != nil {
			return nil, err
		}
		if hit {
			return nil, nil
		}
	}
	if profile != primaryProfile && platform == "api_server" && gateway.SessionOwnedByProfile(ctx, r.config, profile, deref(chat)) {
		return primary, nil
	}
	if profile == primaryProfile {
		return primary, nil
	}
	return nil, nil
}

// Whether an optional string is set and non empty
func nonEmptyPtr(p *string) bool { return p != nil && *p != "" }

// Value of an optional string, empty for nil
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// One subscription's claimed events ready for delivery
type kanbanDelivery struct {
	sub       *pyjson.Object
	oldCursor int64
	cursor    int64
	events    []*kanban.Event
	task      *kanban.Task
	board     string
}

// One tick's claim state, the served profiles and platforms and the stale subscription sweep gate
type kanbanCollector struct {
	runner           *GatewayRunner
	notifierProfile  string
	gcDue            bool
	gcRetentionDays  int64
	deliveries       []*kanbanDelivery
	includeUnowned   bool
	notifierProfiles []string
	activePlatforms  map[string]bool
}

// Collector of the profiles and platforms this gateway serves
func newKanbanCollector(r *GatewayRunner, notifierProfile string, gcDue bool, gcRetentionDays int64) *kanbanCollector {
	c := &kanbanCollector{runner: r, notifierProfile: notifierProfile, gcDue: gcDue, gcRetentionDays: gcRetentionDays,
		includeUnowned: r.ownsKanbanDispatcherLock()}
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			c.notifierProfiles = append(c.notifierProfiles, p)
		}
	}
	add(notifierProfile)
	for _, p := range r.profileAdapters.Keys() {
		if s := pystr.Strip(p); s != "" {
			add(s)
		}
	}
	if r.multiplexing() {
		names := kanbanPlatformNames(r.adapters)
		for _, route := range r.config.ProfileRoutes {
			if pyrepr.Truthy(route.Enabled) && names[routePlatformName(route.Platform)] {
				add(route.Profile)
			}
		}
	}
	c.activePlatforms = kanbanPlatformNames(r.adapters)
	for _, m := range r.profileAdapters.Values() {
		for name := range kanbanPlatformNames(m) {
			c.activePlatforms[name] = true
		}
	}
	return c
}

// Platform of a route as the string its config carries
func routePlatformName(v any) string {
	switch p := v.(type) {
	case string:
		return p
	case gateway.Platform:
		return string(p)
	}
	return "\x00"
}

// Owner scope of the subscriptions this collector reads
func (c *kanbanCollector) ownerScope() kanban.NotifyOwnerScope {
	return kanban.NotifyOwnerScope{Profiles: c.notifierProfiles, Scoped: true, IncludeUnowned: c.includeUnowned}
}

// Context resolving board paths env pin first, so enumerated slugs map to a pinned database
func pinFirst(ctx context.Context) context.Context {
	return kanban.PinFirstBoardResolution(ctx)
}

// Claims unseen events on every board once per resolved database path
func (c *kanbanCollector) collect(ctx context.Context) ([]*kanbanDelivery, error) {
	if len(c.activePlatforms) == 0 {
		runLog().Debug("kanban notifier: no connected adapters; skipping tick")
		return c.deliveries, nil
	}
	pinned := pinFirst(ctx)
	boards, err := kanbanListBoards()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, meta := range boards {
		slug := boardMetaSlug(meta)
		resolved := ""
		if dbPath := objValue(meta, "db_path"); pyrepr.Truthy(dbPath) {
			p, err := pypath.Resolve(pypath.Expanduser(pyrepr.Str(dbPath)))
			resolved = p
			if err != nil {
				resolved = "slug:" + slug
			}
		} else {
			p, err := kanban.KanbanDBPath(pinned, &slug)
			if err == nil {
				p, err = pypath.Resolve(p)
			}
			resolved = p
			if err != nil {
				resolved = "slug:" + slug
			}
		}
		if seen[resolved] {
			runLog().Debug(fmt.Sprintf("kanban notifier: skipping duplicate board slug %s for DB %s", slug, resolved))
			continue
		}
		seen[resolved] = true
		if err := c.collectBoard(pinned, slug); err != nil {
			return nil, err
		}
	}
	return c.deliveries, nil
}

// Read only probe for owned subscriptions on a board, true when the probe fails
func (c *kanbanCollector) boardHasSubs(ctx context.Context, slug string) bool {
	count, err := kanban.CountNotifySubs(ctx, "", &slug, c.ownerScope(), nil, nil, nil)
	if err != nil {
		runLog().Debug(fmt.Sprintf("kanban notifier: read-only subscription probe failed "+
			"for board %s (%s); falling back to writable open", slug, pyrepr.Str(err)))
		return true
	}
	if count == 0 {
		sorted := append([]string(nil), c.notifierProfiles...)
		sort.Strings(sorted)
		items := make([]any, len(sorted))
		for i, p := range sorted {
			items[i] = p
		}
		runLog().Debug(fmt.Sprintf("kanban notifier: board %s has no subscriptions owned by %s; skipping open", slug, pyrepr.Repr(items)))
	}
	return count != 0
}

// Sweeps stale done subscriptions, a failed sweep logged and retried at the next hourly gate
func (c *kanbanCollector) gcStaleSubs(ctx context.Context, conn state.SQLiteConn, slug string) {
	purged, err := kanban.PurgeStaleDoneNotifySubs(ctx, conn, c.gcRetentionDays)
	if err != nil {
		runLog().Debug(fmt.Sprintf("kanban notifier: stale-sub GC failed for board %s: %s", slug, pyrepr.Str(err)))
		return
	}
	if purged > 0 {
		runLog().Info(fmt.Sprintf("kanban notifier: purged %d stale done/blocked-task subscription(s) on board %s (retention %dd)",
			purged, slug, c.gcRetentionDays))
	}
}

// Claims one subscription's unseen events, nil when skipped or nothing is new
func (c *kanbanCollector) claimForSub(ctx context.Context, conn state.SQLiteConn, slug string, sub *pyjson.Object) (*kanbanDelivery, error) {
	ownerProfile := subTruthyText(sub, "notifier_profile")
	platform := pystr.Lower(subTruthyText(sub, "platform"))
	if !c.activePlatforms[platform] {
		shown := platform
		if shown == "" {
			shown = "<missing>"
		}
		runLog().Debug(fmt.Sprintf("kanban notifier: subscription for %s on %s skipped; adapter not connected",
			pyrepr.Str(objValue(sub, "task_id")), shown))
		return nil, nil
	}
	plat, err := gateway.ParsePlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	if ownerProfile == "" {
		ownerProfile = c.notifierProfile
	}
	adapter, err := adapterForSubscription(ctx, c.runner, plat, sub, ownerProfile)
	if err != nil {
		return nil, err
	}
	if adapter == nil {
		warnAnchorlessThreadSubOnce(sub, platform)
		return nil, nil
	}
	taskID := subText(sub, "task_id")
	thread := subTruthyText(sub, "thread_id")
	oldCursor, cursor, events, err := kanbanClaimUnseenEventsForSub(ctx, conn, taskID, subText(sub, "platform"),
		subText(sub, "chat_id"), &thread, kanbanTerminalKinds)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	task, err := kanban.GetTask(ctx, conn, taskID)
	if err != nil {
		return nil, err
	}
	runLog().Debug(fmt.Sprintf("kanban notifier: claimed %d event(s) for %s on board %s cursor %d→%d",
		len(events), taskID, slug, oldCursor, cursor))
	return &kanbanDelivery{sub: sub, oldCursor: oldCursor, cursor: cursor, events: events, task: task, board: slug}, nil
}

// Claims events on one board, appending each delivery
func (c *kanbanCollector) collectBoard(ctx context.Context, slug string) error {
	if !c.boardHasSubs(ctx, slug) {
		return nil
	}
	conn, err := kanbanConnect(ctx, "", &slug)
	if err != nil {
		runLog().Debug(fmt.Sprintf("kanban notifier: cannot open board %s: %s", slug, pyrepr.Str(err)))
		return nil
	}
	defer conn.Close()
	if c.gcDue {
		c.gcStaleSubs(ctx, conn, slug)
	}
	subs, err := kanbanListNotifySubs(ctx, conn, nil, c.ownerScope())
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		runLog().Debug(fmt.Sprintf("kanban notifier: board %s has no subscriptions", slug))
	}
	for _, sub := range subs {
		claimed, err := c.claimForSub(ctx, conn, slug, sub)
		if err != nil {
			runLog().Warn(fmt.Sprintf("kanban notifier: subscription for %s on board %s failed: %s",
				pyrepr.Str(objValue(sub, "task_id")), slug, pyrepr.Str(err)))
			continue
		}
		if claimed != nil {
			c.deliveries = append(c.deliveries, claimed)
		}
	}
	return nil
}

// Claims unseen terminal events for every owned subscription on every board
func notifierCollect(ctx context.Context, r *GatewayRunner, notifierProfile string, gcDue bool, gcRetentionDays int64) ([]*kanbanDelivery, error) {
	return newKanbanCollector(r, notifierProfile, gcDue, gcRetentionDays).collect(ctx)
}

// Truthy payload value of the event, nil otherwise
func kanbanPayload(ev *kanban.Event, key string) any {
	if !pyrepr.Truthy(ev.Payload) {
		return nil
	}
	if v := kanbanPayloadGet(ev.Payload, key); pyrepr.Truthy(v) {
		return v
	}
	return nil
}

// Translated catalog line of the active language
func kt(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, key, "", kwargs)
}

// Catalog message rendered with the clipped payload value, empty when absent
func kanbanClip(ctx context.Context, ev *kanban.Event, key, msgKey string, limit int) string {
	value := kanbanPayload(ev, key)
	if value == nil {
		return ""
	}
	return kt(ctx, msgKey, map[string]any{"value": pystr.Slice(pyrepr.Str(value), 0, limit)})
}

// First line of the stripped text clipped to limit, the clipped text when it has no line
func kanbanFirstLine(text string, limit int) string {
	lines := pystr.Splitlines(pystr.Strip(text), false)
	if len(lines) > 0 {
		return pystr.Slice(lines[0], 0, limit)
	}
	return pystr.Slice(text, 0, limit)
}

// Rendered ping of one event with the wake handoff and review detail it sets, nil leaving them untouched
type kanbanFormatted struct {
	msg          string
	handoff      *string
	reviewDetail *string
}

// Formatter of one event kind
type kanbanFormatter func(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error)

// Completed ping carrying the run summary or legacy result as handoff
func fmtCompleted(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	var wakeHandoff *string
	if summary := kanbanPayload(ev, "summary"); summary != nil {
		s := kanbanFirstLine(pyrepr.Str(summary), 200)
		wakeHandoff = &s
	} else if n.task != nil && n.task.Result != nil && *n.task.Result != "" {
		s := kanbanFirstLine(*n.task.Result, 160)
		wakeHandoff = &s
	}
	handoff := ""
	if wakeHandoff != nil {
		handoff = "\n" + *wakeHandoff
	}
	msg := kt(ctx, "gateway.kanban.ping.completed", map[string]any{"head": n.head, "title": n.title, "handoff": handoff})
	return kanbanFormatted{msg: msg, handoff: wakeHandoff}, nil
}

// Review requested ping carrying the summary into the wake
func fmtReviewRequested(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	handoff := ""
	var wakeHandoff *string
	if summary := kanbanPayload(ev, "summary"); summary != nil {
		s := pyrepr.Str(summary)
		handoff = "\n" + pystr.Slice(s, 0, 200)
		first := kanbanFirstLine(s, 200)
		wakeHandoff = &first
	}
	msg := kt(ctx, "gateway.kanban.ping.review_requested", map[string]any{"head": n.head, "title": n.title, "handoff": handoff})
	return kanbanFormatted{msg: msg, handoff: wakeHandoff}, nil
}

// Changes requested ping with a redacted reason and reviewer provenance
func fmtChangesRequested(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	reason := safeReviewReason(ctx, kanbanPayloadGet(ev.Payload, "reason"), 160)
	reviewer := safeReviewReason(ctx, kanbanPayloadGet(ev.Payload, "reviewer"), 48)
	implementer := safeReviewReason(ctx, kanbanPayloadGet(ev.Payload, "implementer"), 48)
	reasonText := reason
	if reasonText == "" {
		reasonText = kt(ctx, "gateway.kanban.ping.changes_default_reason", nil)
	}
	provenance := ""
	if reviewer != "" {
		provenance = kt(ctx, "gateway.kanban.ping.reviewer_suffix", map[string]any{"reviewer": reviewer})
	}
	if implementer != "" {
		provenance += kt(ctx, "gateway.kanban.ping.implementer_suffix", map[string]any{"implementer": implementer})
	}
	msg := kt(ctx, "gateway.kanban.ping.changes_requested", map[string]any{"board_tag": n.boardTag, "task_id": n.taskID,
		"reason": reasonText, "provenance": provenance})
	return kanbanFormatted{msg: msg, reviewDetail: &reasonText}, nil
}

// Triage ping for a task re blocked past the limit, decision wording only for an owner input block
func fmtBlockLoopDetected(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	whyKey := "gateway.kanban.ping.triage_attention"
	if pyrepr.Equal(kanbanPayload(ev, "kind"), "needs_input") {
		whyKey = "gateway.kanban.ping.triage_decision"
	}
	msg := kt(ctx, "gateway.kanban.ping.triage", map[string]any{"head": n.head, "why": kt(ctx, whyKey, nil),
		"recurrences": kanbanClip(ctx, ev, "recurrences", "gateway.kanban.ping.triage_recurrences", 200),
		"reason":      kanbanClip(ctx, ev, "reason", "gateway.kanban.ping.reason_suffix", 160)})
	return kanbanFormatted{msg: msg}, nil
}

// Gave up ping with the failure count and last error
func fmtGaveUp(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	count := kt(ctx, "gateway.kanban.ping.kept_failing", nil)
	if failures := kanbanPayload(ev, "failures"); failures != nil {
		v, err := pyint.Int64(failures)
		if err != nil {
			return kanbanFormatted{}, err
		}
		count = kt(ctx, "gateway.kanban.ping.failed_n_times", map[string]any{"count": v})
	}
	last := kanbanClip(ctx, ev, "error", "gateway.kanban.ping.last_error", 160)
	msg := kt(ctx, "gateway.kanban.ping.gave_up", map[string]any{"head": n.head, "count": count, "last": last, "task_id": n.taskID})
	return kanbanFormatted{msg: msg}, nil
}

// Timed out ping naming the limit in minutes when known
func fmtTimedOut(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
	raw := kanbanPayload(ev, "limit_seconds")
	if raw == nil {
		raw = int64(0)
	}
	limit, err := pyint.Int64(raw)
	if err != nil {
		return kanbanFormatted{}, err
	}
	minutes := int64(0)
	if limit != 0 {
		minutes = max(1, int64(math.RoundToEven(float64(limit)/60)))
	}
	span := kt(ctx, "gateway.kanban.ping.limit_generic", nil)
	if minutes != 0 {
		span = kt(ctx, "gateway.kanban.ping.limit_minutes", map[string]any{"minutes": minutes})
	}
	return kanbanFormatted{msg: kt(ctx, "gateway.kanban.ping.timed_out", map[string]any{"head": n.head, "span": span})}, nil
}

// Formatter per event kind, archived and unblocked claimed but silent
var kanbanEventFormatters = map[string]kanbanFormatter{
	"completed": fmtCompleted,
	"blocked": func(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
		return kanbanFormatted{msg: kt(ctx, "gateway.kanban.ping.blocked", map[string]any{"head": n.head,
			"reason": kanbanClip(ctx, ev, "reason", "gateway.kanban.ping.reason_suffix", 160)})}, nil
	},
	"gave_up": fmtGaveUp,
	"crashed": func(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
		return kanbanFormatted{msg: kt(ctx, "gateway.kanban.ping.crashed", map[string]any{"head": n.head})}, nil
	},
	"timed_out": fmtTimedOut,
	"status": func(ctx context.Context, ev *kanban.Event, n *kanbanNotification) (kanbanFormatted, error) {
		status := kanbanPayload(ev, "status")
		if status == nil {
			status = ""
		}
		return kanbanFormatted{msg: kt(ctx, "gateway.kanban.ping.status", map[string]any{"head": n.head, "status": status})}, nil
	},
	"review_requested":    fmtReviewRequested,
	"changes_requested":   fmtChangesRequested,
	"block_loop_detected": fmtBlockLoopDetected,
}

// One wake turn to deliver, its text, category and the kinds it covers
type kanbanWakePayload struct {
	synth      string
	diagnostic bool
	kinds      map[string]bool
}

// Delivery of one subscription's claimed events, settling the cursor only after notify and wake both succeed
type kanbanNotification struct {
	runner           *GatewayRunner
	d                *kanbanDelivery
	subFailCounts    map[kanbanSubKey]int
	sub              *pyjson.Object
	task             *kanban.Task
	boardSlug        string
	platformStr      string
	taskID           string
	subProfile       string
	title            string
	boardTag         string
	head             string
	subKey           kanbanSubKey
	wakeAgent        bool
	sendPassive      bool
	wakeHandoff      string
	wakeReviewDetail string
	sessionKey       string
	synth            string
	plat             gateway.Platform
	adapter          gateway.PlatformAdapter
	isPushAdapter    bool
	wakeKinds        map[string]bool
	wakeDiagnostic   bool
}

// Notification of one claimed batch with its display head and delivery mode
func newKanbanNotification(ctx context.Context, r *GatewayRunner, d *kanbanDelivery, subFailCounts map[kanbanSubKey]int) *kanbanNotification {
	sub, task := d.sub, d.task
	n := &kanbanNotification{runner: r, d: d, subFailCounts: subFailCounts, sub: sub, task: task, boardSlug: d.board,
		platformStr: pystr.Lower(subTruthyText(sub, "platform")), taskID: subText(sub, "task_id"),
		subProfile: subTruthyText(sub, "notifier_profile"), isPushAdapter: true, wakeKinds: map[string]bool{}}
	title := n.taskID
	if task != nil {
		title = task.Title
	}
	n.title = pystr.Slice(title, 0, 120)
	if n.boardSlug != "" {
		n.boardTag = kt(ctx, "gateway.kanban.ping.board_tag", map[string]any{"board": n.boardSlug})
	}
	tag := ""
	if task != nil && task.Assignee != nil && *task.Assignee != "" {
		tag = kt(ctx, "gateway.kanban.ping.assignee_tag", map[string]any{"assignee": *task.Assignee})
	}
	n.head = kt(ctx, "gateway.kanban.ping.head", map[string]any{"board_tag": n.boardTag, "assignee_tag": tag, "task_id": n.taskID})
	n.subKey = kanbanSubKey{n.taskID, subText(sub, "platform"), subText(sub, "chat_id"), subTruthyText(sub, "thread_id")}
	mode := subTruthyText(sub, "delivery_mode")
	if mode == "" {
		mode = "notify"
	}
	n.wakeAgent = mode == "notify+wake" || mode == "wake"
	n.sendPassive = mode != "wake"
	return n
}

// Undoes the claimed cursor so the next tick retries
func (n *kanbanNotification) rewind(ctx context.Context) error {
	_, err := toThreadProcessService(ctx, func(fresh context.Context) (struct{}, error) {
		return struct{}{}, n.runner.kanbanRewind(fresh, n.sub, n.d.cursor, n.d.oldCursor, n.boardSlug)
	})
	return err
}

// Settles the cursor past the delivered events
func (n *kanbanNotification) advance(ctx context.Context) error {
	_, err := toThreadProcessService(ctx, func(fresh context.Context) (struct{}, error) {
		return struct{}{}, n.runner.kanbanAdvance(fresh, n.sub, n.d.cursor, n.boardSlug)
	})
	return err
}

// Removes the subscription
func (n *kanbanNotification) unsub(ctx context.Context) error {
	_, err := toThreadProcessService(ctx, func(fresh context.Context) (struct{}, error) {
		return struct{}{}, n.runner.kanbanUnsub(fresh, n.sub, n.boardSlug)
	})
	return err
}

// Forgets the consecutive failure count of the subscription
func (n *kanbanNotification) clearFailures() { delete(n.subFailCounts, n.subKey) }

// Counts a failed delivery, dropping the subscription past the limit and rewinding the claim otherwise
func (n *kanbanNotification) deliveryFailed(ctx context.Context, msg func(fails int) string, dropFmt string, exc error, excInfo bool) error {
	fails := n.subFailCounts[n.subKey] + 1
	n.subFailCounts[n.subKey] = fails
	if excInfo {
		runLog().Warn(msg(fails), "exc_info", exc)
	} else {
		runLog().Warn(msg(fails))
	}
	if fails >= kanbanMaxSendFailures {
		runLog().Warn(fmt.Sprintf(dropFmt, n.taskID, n.platformStr, fails))
		if err := n.unsub(ctx); err != nil {
			return err
		}
		n.clearFailures()
		return nil
	}
	return n.rewind(ctx)
}

// Counts a failed wake under the wake drop message
func (n *kanbanNotification) wakeFailed(ctx context.Context, format string, exc error) error {
	dropFmt := "kanban notifier: dropping subscription %s on %s after %d consecutive wake failures"
	return n.deliveryFailed(ctx, func(fails int) string {
		return fmt.Sprintf(format, n.taskID, fails, kanbanMaxSendFailures, pyrepr.Str(exc))
	}, dropFmt, exc, true)
}

// Rendered ping of one event accumulating its wake handoff and review detail, nil for a silent kind
func (n *kanbanNotification) formatEvent(ctx context.Context, ev *kanban.Event) (*string, error) {
	formatter, ok := kanbanEventFormatters[ev.Kind]
	if !ok {
		return nil, nil
	}
	out, err := formatter(ctx, ev, n)
	if err != nil {
		return nil, err
	}
	if out.handoff != nil {
		n.wakeHandoff = *out.handoff
	}
	if out.reviewDetail != nil {
		n.wakeReviewDetail = *out.reviewDetail
	}
	return &out.msg, nil
}

// Wake kinds, session key and synthetic wake text for the claimed events
func (n *kanbanNotification) buildWakeText(ctx context.Context) {
	task, sub := n.task, n.sub
	n.wakeKinds = map[string]bool{}
	if n.wakeAgent {
		for _, ev := range n.d.events {
			if containsStr(kanbanWakeKinds, ev.Kind) {
				n.wakeKinds[ev.Kind] = true
			}
		}
	}
	n.wakeDiagnostic = true
	for _, ev := range n.d.events {
		if n.wakeKinds[ev.Kind] && !kanbanDiagnosticEvent(ev) {
			n.wakeDiagnostic = false
		}
	}
	if len(n.wakeKinds) == 0 {
		return
	}
	taskSession := ""
	if task != nil && task.SessionID != nil {
		taskSession = *task.SessionID
	}
	if n.isPushAdapter {
		n.sessionKey = taskSession
	} else {
		n.sessionKey = subTruthyText(sub, "chat_id")
		if n.sessionKey == "" {
			n.sessionKey = taskSession
		}
	}
	var parts []string
	for _, k := range kanbanWakeKinds {
		if n.wakeKinds[k] {
			parts = append(parts, kt(ctx, "gateway.kanban.wake."+k, nil))
		}
	}
	status := strings.Join(parts, kt(ctx, "gateway.kanban.wake.status_joiner", nil))
	if status == "" {
		status = kt(ctx, "gateway.kanban.wake.status_default", nil)
	}
	var assignee any = ""
	if task != nil {
		assignee = nil
		if task.Assignee != nil {
			assignee = *task.Assignee
		}
	}
	var board any
	if n.boardSlug != "" {
		board = n.boardSlug
	}
	synth := kt(ctx, "gateway.kanban.wake.message", map[string]any{"task_id": objValue(sub, "task_id"), "status": status,
		"title": n.title, "assignee": assignee, "board": board})
	if n.wakeHandoff != "" {
		synth += "\n" + kt(ctx, "gateway.kanban.wake.handoff", map[string]any{"summary": n.wakeHandoff})
	}
	if n.wakeReviewDetail != "" {
		synth += "\n" + kt(ctx, "gateway.kanban.wake.review_detail", map[string]any{"reason": n.wakeReviewDetail})
	}
	n.synth = synth + "\n\n" + kt(ctx, "gateway.kanban.wake.guidance", nil)
}

// Whether a list holds a string
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Python set repr of the wake kinds in wake order
func wakeKindsRepr(kinds map[string]bool) string {
	if len(kinds) == 0 {
		return "set()"
	}
	var parts []string
	for _, k := range kanbanWakeKinds {
		if kinds[k] {
			parts = append(parts, pyrepr.StrRepr(k))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Logs the woken session
func (n *kanbanNotification) logWoke() {
	profile := n.subProfile
	if profile == "" {
		profile = "default"
	}
	runLog().Info(fmt.Sprintf("kanban notifier: woke agent for %s on %s/%s profile=%s events=%s",
		n.taskID, n.platformStr, subText(n.sub, "chat_id"), profile, wakeKindsRepr(n.wakeKinds)))
}

// Subscription profile when this gateway multiplexes it, empty otherwise
func (n *kanbanNotification) servedWakeProfile() string {
	if n.subProfile == "" || !n.runner.multiplexing() {
		return ""
	}
	return n.subProfile
}

// Context in the subscription profile's runtime scope under multiplex, ctx unchanged otherwise
func (n *kanbanNotification) ownerScope(ctx context.Context) (context.Context, error) {
	served := n.servedWakeProfile()
	if served == "" {
		return ctx, nil
	}
	source := &gateway.SessionSource{Platform: n.plat, ChatID: subText(n.sub, "chat_id"), Profile: &served}
	h, err := n.runner.resolveProfileHomeForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	return gateway.ProfileRuntimeScope(ctx, h, true), nil
}

// Notification category of the wake
func (n *kanbanNotification) wakeCategory() string {
	if n.wakeDiagnostic {
		return "diagnostic"
	}
	return "result"
}

// Wakes the creator session, push adapters with a rebuilt source and non push adapters with a raw session
func (n *kanbanNotification) wake(ctx context.Context) error {
	sub := n.sub
	if !n.isPushAdapter {
		scoped, err := n.ownerScope(ctx)
		if err != nil {
			return err
		}
		if err := gateway.DeliverWake(scoped, n.adapter, gateway.WakeOptions{Text: n.synth, SessionID: n.sessionKey,
			Profile: n.servedWakeProfile(), NotificationCategory: n.wakeCategory()}); err != nil {
			return err
		}
		n.logWoke()
		return nil
	}
	meta := subMetadata(sub)
	chatType := pystr.Strip(pyrepr.Str(pyOr(pyOr(objValue(sub, "chat_type"), objValue(meta, "chat_type")), "")))
	if chatType == "" {
		chatType = "group"
	}
	source := &gateway.SessionSource{Platform: n.plat, ChatID: subText(sub, "chat_id"), ChatType: chatType,
		ThreadID: subOptText(sub, "thread_id"), UserID: routeArg(objValue(sub, "user_id")), UserIDAlt: routeArg(objValue(sub, "user_id_alt")),
		ScopeID: wakeScopeID(n.adapter, sub), ParentChatID: routeArg(objValue(meta, "parent_chat_id"))}
	if n.subProfile != "" {
		p := n.subProfile
		source.Profile = &p
	}
	source.SetTransportAdapterRef(n.adapter)
	if n.subProfile != "" && n.runner.multiplexing() && !profiles.ProfileExists(n.subProfile) {
		return fmt.Errorf("Kanban wake profile %s no longer exists", pyrepr.StrRepr(n.subProfile))
	}
	h, err := n.runner.resolveProfileHomeForSource(ctx, source)
	if err != nil {
		return err
	}
	scoped := gateway.ProfileRuntimeScope(ctx, h, true)
	if err := gateway.DeliverWake(scoped, n.adapter, gateway.WakeOptions{Text: n.synth, SessionID: n.sessionKey, Source: source,
		NotificationCategory: n.wakeCategory()}); err != nil {
		return err
	}
	n.logWoke()
	return nil
}

// Truthy subscription value as an optional string, nil otherwise
func subOptText(sub *pyjson.Object, key string) *string {
	if s := subTruthyText(sub, key); s != "" {
		return &s
	}
	return nil
}

// Sends one text ping and uploads its artifacts, false when presentation policy hid it
func (n *kanbanNotification) sendEvent(ctx context.Context, ev *kanban.Event, msg string) (bool, error) {
	sub, adapter := n.sub, n.adapter
	metadata := map[string]any{}
	if meta, ok := objValue(sub, "delivery_metadata").(*pyjson.Object); ok {
		for _, k := range meta.Keys {
			metadata[k], _ = meta.Get(k)
		}
	}
	if thread := subTruthyText(sub, "thread_id"); thread != "" && !pyrepr.Truthy(metadata["thread_id"]) {
		metadata["thread_id"] = objValue(sub, "thread_id")
	}
	var sendRes *gateway.SendResult
	chatID := subText(sub, "chat_id")
	presented, err := config.PresentNotification(ctx, func(c context.Context) error {
		res, err := adapter.Send(c, chatID, msg, "", metadata)
		if err != nil {
			return err
		}
		sendRes = &res
		return nil
	}, n.platformStr, kanbanDiagnosticEvent(ev), nil)
	if err != nil {
		return false, err
	}
	if !presented {
		return false, nil
	}
	if sendRes != nil && !sendRes.Success {
		detail := "unknown error"
		if pyrepr.Truthy(sendRes.Error) {
			detail = pyrepr.Str(sendRes.Error)
		}
		return false, fmt.Errorf("adapter send() reported failure: %s", detail)
	}
	runLog().Debug(fmt.Sprintf("kanban notifier: delivered %s event for %s to %s/%s on board %s",
		ev.Kind, n.taskID, n.platformStr, chatID, boardDisplay(n.boardSlug)))
	if ev.Kind == "completed" || ev.Kind == "review_requested" {
		if err := n.runner.deliverKanbanArtifacts(ctx, adapter, chatID, metadata, ev.Payload, n.task); err != nil {
			runLog().Debug(fmt.Sprintf("kanban notifier: artifact delivery for %s failed: %s", n.taskID, pyrepr.Str(err)))
		}
	}
	return true, nil
}

// Board slug as Python formats an optional value
func boardDisplay(slug string) string {
	if slug == "" {
		return "None"
	}
	return slug
}

// Sends every text ping, false when a send failed and the claim was already rewound or dropped
func (n *kanbanNotification) sendPings(ctx context.Context) (bool, error) {
	for _, ev := range n.d.events {
		msg, err := n.formatEvent(ctx, ev)
		if err != nil {
			return false, err
		}
		if msg == nil {
			continue
		}
		if !n.isPushAdapter && n.wakeAgent {
			runLog().Debug(fmt.Sprintf("kanban notifier: adapter %s has no push channel; skipping text ping for %s, relying "+
				"on wake self-post instead", n.platformStr, n.taskID))
			continue
		}
		if !n.sendPassive {
			continue
		}
		lastPing := int64(0)
		if v, ok := n.sub.Get("last_ping_event_id"); ok {
			parsed, err := pyint.Int64(v)
			if err != nil {
				return false, err
			}
			lastPing = parsed
		}
		if ev.ID <= lastPing {
			continue
		}
		err = func() error {
			sent, err := n.sendEvent(ctx, ev, *msg)
			if err != nil || !sent {
				return err
			}
			if _, err := toThreadProcessService(ctx, func(fresh context.Context) (struct{}, error) {
				return struct{}{}, n.runner.kanbanSubOp(fresh, n.boardSlug, "record_notify_ping", n.sub, map[string]int64{"event_id": ev.ID})
			}); err != nil {
				return err
			}
			n.clearFailures()
			return nil
		}()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			exc := err
			if err := n.deliveryFailed(ctx, func(fails int) string {
				return fmt.Sprintf("kanban notifier: send failed for %s on %s (attempt %d/%d): %s",
					n.taskID, n.platformStr, fails, kanbanMaxSendFailures, pyrepr.Str(exc))
			}, "kanban notifier: dropping subscription %s on %s after %d consecutive send failures", exc, false); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	return true, nil
}

// Delivers the pings and wakes of the batch, then advances the cursor and unsubscribes an archived task
func (n *kanbanNotification) deliver(ctx context.Context) error {
	plat, err := gateway.ParsePlatform(ctx, n.platformStr)
	if err != nil {
		return n.advance(ctx)
	}
	n.plat = plat
	adapter, err := awaitThread(ctx, func() (gateway.PlatformAdapter, error) {
		return adapterForSubscription(ctx, n.runner, n.plat, n.sub, n.subProfile)
	})
	if err != nil {
		return err
	}
	if adapter == nil {
		runLog().Debug(fmt.Sprintf("kanban notifier: adapter %s disconnected before delivery for %s; rewinding claim",
			n.platformStr, n.taskID))
		return n.rewind(ctx)
	}
	n.adapter = adapter
	n.isPushAdapter = gateway.AdapterSupportsPush(adapter)

	scoped, err := n.ownerScope(ctx)
	if err != nil {
		return err
	}
	ok, err := n.sendPings(scoped)
	if err != nil || !ok {
		return err
	}
	originalEvents := n.d.events
	split := !config.WarningNotificationsEnabled(scoped, n.platformStr, nil)
	groups := [][]*kanban.Event{originalEvents}
	if split {
		var diag, plain []*kanban.Event
		for _, ev := range originalEvents {
			if kanbanDiagnosticEvent(ev) {
				diag = append(diag, ev)
			} else {
				plain = append(plain, ev)
			}
		}
		groups = [][]*kanban.Event{diag, plain}
	}
	var wakePayloads []kanbanWakePayload
	for _, events := range groups {
		if len(events) == 0 {
			continue
		}
		cp := *n.d
		cp.events = events
		n.d = &cp
		n.wakeHandoff, n.wakeReviewDetail = "", ""
		for _, ev := range events {
			if _, err := n.formatEvent(scoped, ev); err != nil {
				return err
			}
		}
		n.buildWakeText(scoped)
		if len(n.wakeKinds) > 0 {
			wakePayloads = append(wakePayloads, kanbanWakePayload{n.synth, n.wakeDiagnostic, n.wakeKinds})
		}
	}
	restored := *n.d
	restored.events = originalEvents
	n.d = &restored
	isPush := n.isPushAdapter

	if len(wakePayloads) > 0 {
		err := func() error {
			for _, p := range wakePayloads {
				n.synth, n.wakeDiagnostic, n.wakeKinds = p.synth, p.diagnostic, p.kinds
				if err := n.wake(ctx); err != nil {
					return err
				}
			}
			n.clearFailures()
			return nil
		}()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var notAccepted *gateway.WakeNotAccepted
			if errors.As(err, &notAccepted) {
				return n.rewind(ctx)
			}
			format := "kanban notifier: wake self-post failed for %s (attempt %d/%d): %s"
			if isPush {
				format = "kanban notifier: wake-only delivery failed for %s (attempt %d/%d): %s"
			}
			return n.wakeFailed(ctx, format, err)
		}
	}

	if err := n.advance(ctx); err != nil {
		return err
	}
	if !isPush {
		n.clearFailures()
	}
	if n.task != nil && n.task.Status == "archived" {
		return n.unsub(ctx)
	}
	return nil
}
