package yuanbao

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestPipelineSkipsGuardedMiddlewareAndRunsAsOnion(t *testing.T) {
	var order []string
	record := func(name string, after bool) MiddlewareFunc {
		return func(_ context.Context, _ *InboundContext, next func() error) error {
			order = append(order, name)
			err := next()
			if after {
				order = append(order, name+"-after")
			}
			return err
		}
	}
	pipeline := NewInboundPipeline().Use("outer", record("outer", true), nil).
		Use("skipped", record("skipped", false), func(*InboundContext) bool { return false }).
		Use("inner", record("inner", false), nil)
	if err := pipeline.Execute(context.Background(), NewInboundContext(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"outer", "inner", "outer-after"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v", order)
	}
}

func TestPipelineInsertsRelativeToTargetAndTakesMiddlewareInstances(t *testing.T) {
	noop := func(_ context.Context, _ *InboundContext, next func() error) error { return next() }
	pipeline := NewInboundPipeline().Use("a", noop, nil).Use("c", noop, nil)
	pipeline.UseBefore("c", "b", noop, nil).UseAfter("c", newDedupMiddleware(), nil, nil).Remove("a")
	if want := []string{"b", "c", "dedup"}; !reflect.DeepEqual(pipeline.MiddlewareNames(), want) {
		t.Fatalf("names %v", pipeline.MiddlewareNames())
	}
	names := BuildInboundPipeline().MiddlewareNames()
	if len(names) != 20 || names[0] != "decode" || names[17] != "media-resolve" || names[18] != "patch-anchors" || names[19] != "dispatch" {
		t.Fatalf("default pipeline %v", names)
	}
}

func TestDecodeMiddlewareParsesJSONPushAndStopsOnNoFrames(t *testing.T) {
	adapter := makeAdapter(t, nil)
	ic := NewInboundContext(adapter, [][]byte{makeJSONPush(t, "alice", "", "hi", "msg-001")})
	if calls := runMiddleware(t, newDecodeMiddleware(), ic); calls != 1 || ic.DecodedVia != "json" || ic.Push.GetOr("from_account", nil) != "alice" {
		t.Fatalf("decode %d %q %v", calls, ic.DecodedVia, ic.Push)
	}
	empty := NewInboundContext(adapter, nil)
	if calls := runMiddleware(t, newDecodeMiddleware(), empty); calls != 0 || empty.Push != nil {
		t.Fatalf("empty frames continued %d %v", calls, empty.Push)
	}
}

func TestFieldDedupSelfAndRoutingMiddlewares(t *testing.T) {
	adapter := makeAdapter(t, nil)
	ic := NewInboundContext(adapter, nil)
	ic.Push = pyjson.ObjectOf("from_account", "alice", "group_code", "grp-1", "group_name", "Test Group", "sender_nickname", "Alice",
		"msg_body", []any{textElem("hi")}, "msg_id", "msg-001", "cloud_custom_data", `{"key": "val"}`)
	if calls := runMiddleware(t, newExtractFieldsMiddleware(), ic); calls != 1 || ic.FromAccount != "alice" || ic.GroupCode != "grp-1" ||
		ic.SenderNickname != "Alice" || len(ic.MsgBody) != 1 || ic.MsgID != "msg-001" || ic.CloudCustomData != `{"key": "val"}` {
		t.Fatalf("fields %+v", ic)
	}
	if runMiddleware(t, newDedupMiddleware(), ic) != 1 || runMiddleware(t, newDedupMiddleware(), ic) != 0 {
		t.Fatal("a repeated message id must stop the chain")
	}
	if calls := runMiddleware(t, newChatRoutingMiddleware(), ic); calls != 1 || ic.ChatID != "group:grp-1" || ic.ChatType != "group" || ic.ChatName != "Test Group" {
		t.Fatalf("group routing %q %q %v", ic.ChatID, ic.ChatType, ic.ChatName)
	}
	dm := NewInboundContext(adapter, nil)
	dm.FromAccount, dm.SenderNickname = "alice", "Alice"
	if calls := runMiddleware(t, newChatRoutingMiddleware(), dm); calls != 1 || dm.ChatID != "direct:alice" || dm.ChatType != "dm" || dm.ChatName != "Alice" {
		t.Fatalf("dm routing %q %q %v", dm.ChatID, dm.ChatType, dm.ChatName)
	}
	self := NewInboundContext(adapter, nil)
	self.FromAccount = "bot_123"
	if runMiddleware(t, newSkipSelfMiddleware(), self) != 0 || runMiddleware(t, newSkipSelfMiddleware(), dm) != 1 {
		t.Fatal("only the bot's own message stops the chain")
	}
}

func TestAccessGuardMiddleware(t *testing.T) {
	cases := []struct {
		name                   string
		optIn                  bool
		dmPolicy, groupPolicy  string
		dmAllowFrom            []string
		chatType               string
		fromAccount, groupCode any
		wantContinued          int
	}{
		{"open dm with opt in", true, "open", "open", nil, "dm", "alice", "", 1},
		{"open dm without opt in", false, "open", "open", nil, "dm", "alice", "", 0},
		{"allowlist dm outside the list", false, "allowlist", "open", []string{"bob"}, "dm", "alice", "", 0},
		{"allowlist dm inside the list", false, "allowlist", "open", []string{"alice"}, "dm", "alice", "", 1},
		{"open group without opt in", false, "pairing", "open", nil, "group", "alice", "grp-1", 0},
		{"open group with opt in", true, "pairing", "open", nil, "group", "alice", "grp-1", 1},
		{"unknown group policy", false, "pairing", "typo", nil, "group", "alice", "grp-1", 0},
		{"pairing dm", false, "pairing", "pairing", nil, "dm", "alice", "", 1},
		{"pairing dm empty sender", false, "pairing", "pairing", nil, "dm", "", "", 0},
		{"pairing dm blank sender", false, "pairing", "pairing", nil, "dm", "   ", "", 0},
		{"pairing dm missing sender", false, "pairing", "pairing", nil, "dm", nil, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			adapter := makeAdapter(t, nil)
			if c.optIn {
				t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
			}
			adapter.accessPolicy = makePolicy(t, c.dmPolicy, c.dmAllowFrom, c.groupPolicy, nil)
			ic := NewInboundContext(adapter, nil)
			ic.ChatType, ic.FromAccount, ic.GroupCode = c.chatType, c.fromAccount, c.groupCode
			if calls := runMiddleware(t, newAccessGuardMiddleware(), ic); calls != c.wantContinued {
				t.Fatalf("continued %d times", calls)
			}
		})
	}
}

func TestAccessPolicyGroupRulesAndBlankPrincipals(t *testing.T) {
	isolatedHome(t)
	ctx := context.Background()
	for _, groupPolicy := range []string{"open", "typo"} {
		if allowed, err := makePolicy(t, "pairing", nil, groupPolicy, nil).IsGroupAllowed(ctx, "unknown-group"); err != nil || allowed {
			t.Fatalf("%s group policy admitted an unknown group %v", groupPolicy, err)
		}
	}
	if allowed, err := makePolicy(t, "pairing", nil, "allowlist", []string{"grp-1"}).IsGroupAllowed(ctx, " grp-1 "); err != nil || !allowed {
		t.Fatalf("allowlisted group refused %v", err)
	}
	for _, blank := range []string{"", "   "} {
		if allowed, err := makePolicy(t, "pairing", nil, "pairing", nil).IsDMIntakeAllowed(ctx, blank); err != nil || allowed {
			t.Fatalf("blank principal %q admitted %v", blank, err)
		}
	}
}

func TestAccessPolicyReadsTheScopedAllowAllOptIn(t *testing.T) {
	isolatedHome(t)
	policy := makePolicy(t, "open", nil, "pairing", nil)
	scoped := withMultiplexScope(t, map[string]string{"YUANBAO_ALLOW_ALL_USERS": "true"})
	if opted, err := policy.OpenDMOptedIn(scoped); err != nil || !opted {
		t.Fatalf("scoped opt in ignored %v", err)
	}
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	other := withMultiplexScope(t, map[string]string{"SOMETHING_ELSE": "x"})
	if opted, err := policy.OpenDMOptedIn(other); err != nil || opted {
		t.Fatalf("the default profile opt in leaked into a scoped miss %v", err)
	}
}

func TestSenderMayDesignateHomeFollowsPairingApproval(t *testing.T) {
	adapter := makeAdapter(t, nil)
	adapter.accessPolicy = makePolicy(t, "pairing", nil, "pairing", nil)
	ic := NewInboundContext(adapter, nil)
	ic.ChatType, ic.FromAccount = "dm", "sender-1"
	for _, approved := range []bool{false, true} {
		swap(t, &pairingApprovedFn, func(_ context.Context, sender string) (bool, error) { return approved && sender == "sender-1", nil })
		if may, err := adapter.senderMayDesignateHome(context.Background(), ic); err != nil || may != approved {
			t.Fatalf("approved=%v may=%v err=%v", approved, may, err)
		}
	}
}

func TestAutoSetHomeOnlyForApprovedSenders(t *testing.T) {
	for _, approved := range []bool{false, true} {
		adapter := makeAdapter(t, nil)
		t.Setenv("YUANBAO_HOME_CHANNEL", "")
		adapter.setAutoSethomeDone(false)
		adapter.accessPolicy = makePolicy(t, "pairing", nil, "pairing", nil)
		swap(t, &pairingApprovedFn, func(context.Context, string) (bool, error) { return approved, nil })
		ic := NewInboundContext(adapter, nil)
		ic.ChatType, ic.ChatID, ic.FromAccount, ic.ChatName = "dm", "direct:sender-1", "sender-1", "Approved"
		if calls := runMiddleware(t, newAutoSetHomeMiddleware(), ic); calls != 1 {
			t.Fatalf("approved=%v continued %d times", approved, calls)
		}
		_, statErr := os.Stat(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"))
		switch {
		case approved && (os.Getenv("YUANBAO_HOME_CHANNEL") != "direct:sender-1" || statErr != nil || adapter.Config.HomeChannel == nil ||
			adapter.Config.HomeChannel.ChatID != "direct:sender-1" || !adapter.autoSethomeDoneValue()):
			t.Fatalf("approved sender did not claim the home channel %q %v", os.Getenv("YUANBAO_HOME_CHANNEL"), statErr)
		case !approved && (os.Getenv("YUANBAO_HOME_CHANNEL") != "" || statErr == nil || adapter.Config.HomeChannel != nil):
			t.Fatalf("intake only sender claimed the home channel %q", os.Getenv("YUANBAO_HOME_CHANNEL"))
		}
	}
}

func TestContentPlaceholderAndGroupGuardMiddlewares(t *testing.T) {
	adapter := makeAdapter(t, nil)
	ic := NewInboundContext(adapter, nil)
	ic.MsgBody = []any{textElem("Hello!"), pyjson.ObjectOf("msg_type", "TIMImageElem", "msg_content",
		pyjson.ObjectOf("image_info_array", []any{pyjson.ObjectOf("url", "https://img.example.com/1.jpg")}))}
	if calls := runMiddleware(t, newExtractContentMiddleware(), ic); calls != 1 || ic.RawText != "Hello! [image]" || len(ic.MediaRefs) != 1 ||
		ic.MediaRefs[0].Kind != "image" {
		t.Fatalf("content %q %+v", ic.RawText, ic.MediaRefs)
	}
	placeholder := NewInboundContext(adapter, nil)
	placeholder.RawText = "[image]"
	if runMiddleware(t, newPlaceholderFilterMiddleware(), placeholder) != 0 || runMiddleware(t, newPlaceholderFilterMiddleware(), ic) != 1 {
		t.Fatal("only a bare placeholder without media stops the chain")
	}
	dm := NewInboundContext(adapter, nil)
	dm.ChatType = "dm"
	group := NewInboundContext(adapter, nil)
	group.ChatType, group.ChatID, group.MsgBody, group.FromAccount, group.RawText = "group", "group:grp-1", []any{textElem("hi")}, "alice", "hi"
	owner := NewInboundContext(adapter, nil)
	command := "/new"
	owner.ChatType, owner.OwnerCommand = "group", &command
	mention, err := pyjson.Dumps(pyjson.ObjectOf("elem_type", int64(1002), "text", "@Bot", "user_id", "bot_123"))
	if err != nil {
		t.Fatal(err)
	}
	addressed := NewInboundContext(adapter, nil)
	addressed.ChatType = "group"
	addressed.MsgBody = []any{pyjson.ObjectOf("msg_type", "TIMCustomElem", "msg_content", pyjson.ObjectOf("data", mention)), textElem("hi")}
	guard := newGroupAtGuardMiddleware()
	if runMiddleware(t, guard, dm) != 1 || runMiddleware(t, guard, group) != 0 || runMiddleware(t, guard, owner) != 1 || runMiddleware(t, guard, addressed) != 1 {
		t.Fatal("group guard lets through only DMs, owner commands and bot mentions")
	}
}

func TestQuoteContextMiddlewareReadsTheQuote(t *testing.T) {
	adapter := makeAdapter(t, nil)
	ic := NewInboundContext(adapter, nil)
	ic.CloudCustomData = `{"quote": {"id": "quoted-msg-004", "desc": "Check this image", "sender_nickname": "Dave"}}`
	if calls := runMiddleware(t, newQuoteContextMiddleware(), ic); calls != 1 || ic.ReplyToMessageID == nil || *ic.ReplyToMessageID != "quoted-msg-004" ||
		ic.ReplyToText == nil || *ic.ReplyToText != "Dave: Check this image" || len(ic.QuoteMediaRefs) != 0 {
		t.Fatalf("quote %v %v %v", ic.ReplyToMessageID, ic.ReplyToText, ic.QuoteMediaRefs)
	}
}

func TestFullPipelineDispatchesADirectMessage(t *testing.T) {
	adapter := makeAdapter(t, nil)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	adapter.accessPolicy = makePolicy(t, "open", nil, "open", nil)
	events := captureEvents(t)
	ic := NewInboundContext(adapter, [][]byte{makeJSONPush(t, "alice", "", "Hello bot!", "msg-e2e-001")})
	if err := adapter.inboundPipeline.Execute(context.Background(), ic); err != nil {
		t.Fatal(err)
	}
	if ic.DecodedVia != "json" || ic.FromAccount != "alice" || ic.ChatType != "dm" || ic.ChatID != "direct:alice" || ic.RawText != "Hello bot!" || ic.Source == nil {
		t.Fatalf("context %+v", ic)
	}
	eventually(t, "the dispatched event", func() bool { return len(events()) == 1 })
	event := events()[0]
	if event.Text != "Hello bot!" || event.MessageType != gateway.MessageTypeText || event.MessageID == nil || *event.MessageID != "msg-e2e-001" ||
		event.Source.ChatID != "direct:alice" || event.RawMessage != ic.Push {
		t.Fatalf("event %+v", event)
	}
	sessionKey, err := adapter.SourceSessionKey(context.Background(), ic.Source)
	if err != nil {
		t.Fatal(err)
	}
	if pairs := adapter.processingSnapshot(); len(pairs) != 1 || pairs[0] != [2]string{sessionKey, "msg-e2e-001"} ||
		adapter.processingText(sessionKey) != "Hello bot!" || adapter.cachedMsgContent("msg-e2e-001") != "Hello bot!" {
		t.Fatalf("recall tracking %v", pairs)
	}
}

func TestFullPipelineStopsBlankSendersAndUnaddressedGroupTraffic(t *testing.T) {
	adapter := makeAdapter(t, nil)
	adapter.accessPolicy = makePolicy(t, "pairing", nil, "pairing", nil)
	events := captureEvents(t)
	blank := NewInboundContext(adapter, [][]byte{makeJSONPush(t, "   ", "", "Hello bot!", "msg-blank-001")})
	if err := adapter.inboundPipeline.Execute(context.Background(), blank); err != nil {
		t.Fatal(err)
	}
	if blank.FromAccount != "   " || blank.ChatID != "direct:   " || blank.Source != nil {
		t.Fatalf("blank sender passed the access guard %+v", blank)
	}
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	t.Setenv("YUANBAO_HOME_CHANNEL", "")
	adapter.setAutoSethomeDone(false)
	adapter.accessPolicy = makePolicy(t, "pairing", nil, "open", nil)
	group := NewInboundContext(adapter, [][]byte{makeJSONPush(t, "alice", "grp-1", "hello group", "msg-group-001")})
	if err := adapter.inboundPipeline.Execute(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml")); err == nil || os.Getenv("YUANBAO_HOME_CHANNEL") != "" {
		t.Fatal("unaddressed group traffic claimed the home channel")
	}
	time.Sleep(20 * time.Millisecond)
	if len(events()) != 0 {
		t.Fatalf("dispatched %d events", len(events()))
	}
}

func TestGroupDispatchesRunOneAtATimePerSession(t *testing.T) {
	adapter := makeAdapter(t, nil)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	adapter.accessPolicy = makePolicy(t, "pairing", nil, "open", nil)
	t.Cleanup(func() { _ = adapter.Disconnect(context.Background()) })
	var mu sync.Mutex
	var texts []string
	release := make(chan struct{})
	swap(t, &handleMessageFn, func(ctx context.Context, _ *YuanbaoAdapter, event *gateway.MessageEvent) error {
		mu.Lock()
		texts = append(texts, event.Text)
		first := len(texts) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return nil
	})
	mention, err := pyjson.Dumps(pyjson.ObjectOf("elem_type", int64(1002), "text", "@Bot", "user_id", "bot_123"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		push := pyjson.ObjectOf("From_Account", "alice", "GroupId", "grp-1", "MsgKey", "msg-"+text, "MsgBody", []any{
			pyjson.ObjectOf("MsgType", "TIMCustomElem", "MsgContent", pyjson.ObjectOf("data", mention)),
			pyjson.ObjectOf("MsgType", "TIMTextElem", "MsgContent", pyjson.ObjectOf("text", text))})
		encoded, err := pyjson.Dumps(push)
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.inboundPipeline.Execute(context.Background(), NewInboundContext(adapter, [][]byte{[]byte(encoded)})); err != nil {
			t.Fatal(err)
		}
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, texts...)
	}
	eventually(t, "the first group dispatch", func() bool { return len(seen()) == 1 })
	time.Sleep(30 * time.Millisecond)
	if got := seen(); len(got) != 1 {
		t.Fatalf("the second dispatch did not wait for the first %v", got)
	}
	close(release)
	eventually(t, "the second group dispatch", func() bool { return len(seen()) == 2 })
	if got := seen(); got[0] != "[alice|alice]\n@Bot first" || got[1] != "[alice|alice]\n@Bot second" {
		t.Fatalf("dispatch order %q", got)
	}
}

func TestResolveYbresRefsKeepsInputOrderAndDropsFailures(t *testing.T) {
	adapter := makeAdapter(t, map[string]any{"media_resolve_concurrency": int64(4)})
	resourceCache.entries.Clear()
	delays := map[string]time.Duration{"rid-A": 60 * time.Millisecond, "rid-B": 20 * time.Millisecond}
	var fetched atomic.Int32
	swap(t, &fetchResourceURLFn, func(_ context.Context, _ *YuanbaoAdapter, rid string) (string, error) {
		fetched.Add(1)
		return "https://fresh/" + rid, nil
	})
	var names sync.Map
	swap(t, &downloadAndCacheFn, func(_ context.Context, _ *YuanbaoAdapter, fetchURL, kind, fileName, logTag, rid string) (string, string, bool, error) {
		names.Store(rid, fileName+"|"+kind+"|"+fetchURL+"|"+logTag)
		time.Sleep(delays[rid])
		if rid == "rid-miss" {
			return "", "", false, nil
		}
		return "/cache/" + rid, "image/jpeg", true, nil
	})
	refs := []ybresRef{{"rid-A", "image", "a.jpg"}, {"rid-miss", "image", ""}, {"rid-B", "file", "doc.pdf"}, {"rid-C", "image", ""}, {"rid-voice", "voice", ""}}
	paths, mimes, err := resolveYbresRefs(context.Background(), adapter, refs, "test")
	if err != nil || !reflect.DeepEqual(paths, []string{"/cache/rid-A", "/cache/rid-B", "/cache/rid-C"}) || len(mimes) != 3 || fetched.Load() != 4 {
		t.Fatalf("paths %v mimes %v fetched %d err %v", paths, mimes, fetched.Load(), err)
	}
	if got, _ := names.Load("rid-A"); got != "a.jpg|image|https://fresh/rid-A|test rid=rid-A" {
		t.Fatalf("download arguments %v", got)
	}
}

func TestResolveServesCachedResourcesWithoutAFetch(t *testing.T) {
	adapter := makeAdapter(t, nil)
	cached := filepath.Join(t.TempDir(), "rid-cached.jpg")
	if err := os.WriteFile(cached, []byte("cached-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	resourceCache.entries.Clear()
	t.Cleanup(resourceCache.entries.Clear)
	putCachedResource("rid-cached", cached, "image/jpeg")
	refuse := func(context.Context, *YuanbaoAdapter, string) (string, error) {
		t.Error("a cache hit must not look up a download URL")
		return "", nil
	}
	swap(t, &fetchResourceURLFn, refuse)
	swap(t, &resolveDownloadURLFn, refuse)
	paths, mimes, err := resolveYbresRefs(context.Background(), adapter, []ybresRef{{"rid-cached", "image", ""}}, "test")
	if err != nil || !reflect.DeepEqual(paths, []string{cached}) || !reflect.DeepEqual(mimes, []string{"image/jpeg"}) {
		t.Fatalf("ybres hit %v %v %v", paths, mimes, err)
	}
	refs := []InboundMediaRef{{Kind: "image", URL: "https://hunyuan.tencent.com/api/resource/download?resourceId=rid-cached"}}
	paths, mimes, err = resolveMediaURLs(context.Background(), adapter, refs)
	if err != nil || !reflect.DeepEqual(paths, []string{cached}) || !reflect.DeepEqual(mimes, []string{"image/jpeg"}) {
		t.Fatalf("media hit %v %v %v", paths, mimes, err)
	}
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	if _, _, hit := getCachedResource("rid-cached"); hit || resourceCache.entries.Len() != 0 {
		t.Fatal("an entry whose file is gone must be dropped")
	}
}

func TestResolveConcurrencyIsBoundedAndClamped(t *testing.T) {
	for _, limit := range []int64{1, 2} {
		adapter := makeAdapter(t, map[string]any{"media_resolve_concurrency": limit})
		resourceCache.entries.Clear()
		var inFlight, maxInFlight atomic.Int64
		swap(t, &fetchResourceURLFn, func(_ context.Context, _ *YuanbaoAdapter, rid string) (string, error) {
			return "https://fresh/" + rid, nil
		})
		swap(t, &downloadAndCacheFn, func(_ context.Context, _ *YuanbaoAdapter, _, _, _, _, rid string) (string, string, bool, error) {
			now := inFlight.Add(1)
			for seen := maxInFlight.Load(); now > seen && !maxInFlight.CompareAndSwap(seen, now); seen = maxInFlight.Load() {
			}
			time.Sleep(10 * time.Millisecond)
			inFlight.Add(-1)
			return "/cache/" + rid, "image/jpeg", true, nil
		})
		var refs []ybresRef
		var want []string
		for _, rid := range []string{"rid-0", "rid-1", "rid-2", "rid-3", "rid-4", "rid-5"} {
			refs = append(refs, ybresRef{rid, "image", ""})
			want = append(want, "/cache/"+rid)
		}
		paths, _, err := resolveYbresRefs(context.Background(), adapter, refs, "test")
		if err != nil || !reflect.DeepEqual(paths, want) || maxInFlight.Load() != limit {
			t.Fatalf("limit %d max in flight %d paths %v err %v", limit, maxInFlight.Load(), paths, err)
		}
	}
	for raw, want := range map[any]int{int64(-3): minResolveConcurrency, int64(9999): maxResolveConcurrency, "fast": defaultResolveConcurrency,
		"3": 3, nil: defaultResolveConcurrency} {
		if got := makeAdapter(t, map[string]any{"media_resolve_concurrency": raw}).MediaResolveConcurrency; got != want {
			t.Fatalf("concurrency of %v is %d", raw, got)
		}
	}
}

func TestMediaResolveMiddlewareRoutesQuoteAndObservedMedia(t *testing.T) {
	adapter := makeAdapter(t, nil)
	var calls []string
	source := func(name string, paths ...string) ([]string, []string) {
		calls = append(calls, name)
		return paths, make([]string, len(paths))
	}
	swap(t, &resolveMediaURLsFn, func(context.Context, *YuanbaoAdapter, []InboundMediaRef) ([]string, []string, error) {
		paths, mimes := source("own")
		return paths, mimes, nil
	})
	swap(t, &resolveQuoteMediaFn, func(context.Context, *YuanbaoAdapter, []ybresRef) ([]string, []string, error) {
		paths, mimes := source("quote", "/cache/q.jpg")
		return paths, mimes, nil
	})
	swap(t, &collectObservedMediaFn, func(context.Context, *YuanbaoAdapter, *gateway.SessionSource) ([]string, []string, error) {
		paths, mimes := source("observed", "/cache/o1.jpg", "/cache/o1.jpg")
		return paths, mimes, nil
	})
	quoted := "quoted-004"
	cases := []struct {
		chatType, rawText   string
		replyTo             *string
		quoteRefs           []ybresRef
		wantCalls, wantURLs []string
		wantContinued       int
	}{
		{"dm", "hello", nil, nil, []string{"own"}, []string{}, 1},
		{"group", "hello", nil, nil, []string{"own", "observed"}, []string{"/cache/o1.jpg"}, 1},
		{"group", "hello", &quoted, []ybresRef{{"rid", "image", ""}}, []string{"own", "quote"}, []string{"/cache/q.jpg"}, 1},
		{"dm", "[image]", &quoted, []ybresRef{{"rid", "image", ""}}, []string{"own", "quote"}, []string{"/cache/q.jpg"}, 0},
	}
	for _, c := range cases {
		calls = nil
		ic := NewInboundContext(adapter, nil)
		ic.ChatType, ic.RawText, ic.ReplyToMessageID, ic.QuoteMediaRefs = c.chatType, c.rawText, c.replyTo, c.quoteRefs
		if continued := runMiddleware(t, newMediaResolveMiddleware(), ic); continued != c.wantContinued || !reflect.DeepEqual(calls, c.wantCalls) ||
			!reflect.DeepEqual(ic.MediaURLs, c.wantURLs) {
			t.Fatalf("%+v: continued %d calls %v urls %v", c, continued, calls, ic.MediaURLs)
		}
	}
}

func TestQuotedDirectMessageRecoversItsLocalMediaFromTheMessageCache(t *testing.T) {
	adapter := makeAdapter(t, nil)
	local := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(local, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter.recordProcessing("sk", "quoted-1", "see [file: report.pdf → "+local+"] and [image: /missing/a.png]")
	quoted := "quoted-1"
	ic := NewInboundContext(adapter, nil)
	ic.ChatType, ic.RawText, ic.ReplyToMessageID = "dm", "what is this", &quoted
	if continued := runMiddleware(t, newMediaResolveMiddleware(), ic); continued != 1 || !reflect.DeepEqual(ic.MediaURLs, []string{local}) ||
		!reflect.DeepEqual(ic.MediaTypes, []string{"application/pdf"}) {
		t.Fatalf("continued %d urls %v types %v", continued, ic.MediaURLs, ic.MediaTypes)
	}
}

func TestPatchAnchorsRewritesOnlyResolvedFittingMedia(t *testing.T) {
	cases := []struct {
		text        string
		urls, types []string
		want        string
	}{
		{"", nil, nil, ""},
		{"hello", nil, nil, "hello"},
		{"see [file:doc.pdf|ybres:rid-1]", []string{"/cache/doc.pdf"}, []string{"application/pdf"}, "see [file: doc.pdf → /cache/doc.pdf]"},
		{"[file|ybres:rid-1]", []string{"/cache/x.bin"}, []string{"application/octet-stream"}, "[file: x.bin → /cache/x.bin]"},
		{"[image|ybres:rid]", []string{"/cache/odd.bin"}, []string{"application/octet-stream"}, "[image|ybres:rid]"},
		{"[image|ybres:a] 你好 [video|ybres:b]", []string{"/c/a.png", "/c/b.mp4"}, []string{"image/png", "video/mp4"}, "[image: /c/a.png] 你好 [video: /c/b.mp4]"},
		{"[image|ybres:a]", []string{"https://remote/a.png"}, []string{"image/png"}, "[image|ybres:a]"},
	}
	for _, c := range cases {
		if got := patchAnchors(c.text, c.urls, c.types); got != c.want {
			t.Fatalf("patch %q gave %q", c.text, got)
		}
	}
}
