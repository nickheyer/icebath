package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const eventsPublicURL = "https://hooks.example.test"

var eventsArgs = map[string]any{"feed_id": "11111111-1111-4111-8111-111111111111"}

// eventsTransport answers each request with the next scripted reply and records what was sent
type eventsTransport struct {
	mu      sync.Mutex
	sent    []map[string]any
	replies []any
	in      chan *Message
	onSend  func(method string)
	once    sync.Once
}

func (f *eventsTransport) Send(ctx context.Context, msg *Message) error {
	if !msg.IsRequest() {
		return nil
	}
	var params map[string]any
	_ = json.Unmarshal(msg.Params, &params)
	f.mu.Lock()
	f.sent = append(f.sent, map[string]any{"method": msg.Method, "params": params})
	reply := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()
	var resp *Message
	if e, ok := reply.(*RPCError); ok {
		resp = NewErrorResponse(msg.ID, e.Code, e.Message)
		resp.Error.Data = e.Data
	} else {
		resp, _ = NewResponse(msg.ID, reply)
	}
	if f.onSend != nil {
		f.onSend(msg.Method)
	}
	f.in <- resp
	return nil
}

func (f *eventsTransport) Receive() <-chan *Message { return f.in }
func (f *eventsTransport) Err() error               { return nil }
func (f *eventsTransport) Label() string            { return "events" }
func (f *eventsTransport) Close() error {
	f.once.Do(func() { close(f.in) })
	return nil
}

// Requests the fake server received
func (f *eventsTransport) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

// Isolated home whose webhook platform has an HTTPS public URL
func eventsHomeDir(t *testing.T) string {
	t.Helper()
	dir := isolateHome(t)
	writeStartupConfig(t, dir, "platforms:\n  webhook:\n    enabled: true\n    extra:\n      public_url: "+eventsPublicURL+"/\n")
	return dir
}

// Events enabled server task over a scripted session
func eventsServer(t *testing.T, dir string, replies ...any) (*serverTask, *eventsTransport) {
	t.Helper()
	tr := &eventsTransport{replies: replies, in: make(chan *Message, 16)}
	sess := NewSession(tr, SessionOptions{Name: "ambush"})
	t.Cleanup(func() { sess.Close() })
	srv := newServerTask("ambush", &tools.Env{Home: dir})
	srv.mu.Lock()
	srv.session = sess
	srv.config = map[string]any{"events": true}
	srv.mu.Unlock()
	return srv, tr
}

// Subscribe result for a grant expiring after seconds
func eventsGrant(seconds float64) map[string]any {
	expires := time.Now().UTC().Add(time.Duration(seconds * float64(time.Second)))
	return map[string]any{"id": "sub_1", "refreshBefore": expires.Format("2006-01-02T15:04:05.999999Z"), "cursor": nil, "truncated": false}
}

// JSON file of the home decoded as an object, empty when absent
func readHomeJSON(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Writes an object as a JSON file of the home
func writeHomeJSON(t *testing.T, dir, name string, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Rendered result of an events utility decoded back into a map
func decodedEventResult(t *testing.T, o orderedJSON, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	text, err := renderEventResult(o)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Runs subscribe_event for the feed event with extra arguments
func eventsSubscribe(t *testing.T, srv *serverTask, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"name": "feed.emission.created", "arguments": eventsArgs, "deliver": "telegram"}
	for k, v := range extra {
		args[k] = v
	}
	o, err := subscribeEvent(context.Background(), srv, args)
	return decodedEventResult(t, o, err)
}

// Single route of the webhook store
func onlyRoute(t *testing.T, dir string) (string, map[string]any) {
	t.Helper()
	routes := readHomeJSON(t, dir, "webhook_subscriptions.json")
	if len(routes) != 1 {
		t.Fatalf("routes %v", routes)
	}
	for k, v := range routes {
		return k, v.(map[string]any)
	}
	return "", nil
}

func TestEventsListingStopsAtThePageCap(t *testing.T) {
	dir := eventsHomeDir(t)
	var replies []any
	for i := 1; i <= 25; i++ {
		replies = append(replies, map[string]any{"events": []any{map[string]any{"name": "e"}}, "nextCursor": "c"})
	}
	srv, tr := eventsServer(t, dir, replies...)
	o, err := listEventTypes(context.Background(), srv, nil)
	result := decodedEventResult(t, o, err)
	if n := len(result["events"].([]any)); n != maxEventListPages || len(tr.requests()) != maxEventListPages {
		t.Fatalf("events %d requests %d", n, len(tr.requests()))
	}
	if _, ok := result["truncated"]; !ok {
		t.Fatal("no truncated notice")
	}
}

func TestEventsSubscribeRegistersASignedRouteThenTheSubscription(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400))
	result := eventsSubscribe(t, srv, map[string]any{"instructions": "Explain the deal."})
	name, route := onlyRoute(t, dir)
	sent := tr.requests()[0]
	params := sent["params"].(map[string]any)
	if sent["method"] != "events/subscribe" {
		t.Fatalf("method %v", sent["method"])
	}
	delivery := params["delivery"].(map[string]any)
	if delivery["mode"] != "webhook" || delivery["url"] != eventsPublicURL+"/webhooks/"+name || delivery["secret"] != route["secret"] || len(delivery) != 3 {
		t.Fatalf("delivery %v", delivery)
	}
	if _, ok := params["cursor"]; ok || params["arguments"].(map[string]any)["feed_id"] != eventsArgs["feed_id"] {
		t.Fatalf("params %v", params)
	}
	secret := route["secret"].(string)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if !strings.HasPrefix(secret, "whsec_") || err != nil || len(raw) != 32 {
		t.Fatalf("secret %q", secret)
	}
	if route["deliver"] != "telegram" || !strings.Contains(route["prompt"].(string), "Explain the deal.") {
		t.Fatalf("route %v", route)
	}
	state := readHomeJSON(t, dir, eventStateFilename)[name].(map[string]any)
	if state["url"] != delivery["url"] || state["subscription_id"] != "sub_1" {
		t.Fatalf("state %v", state)
	}
	if result["subscribed"].(map[string]any)["refreshBefore"] != state["refresh_before"] {
		t.Fatalf("result %v", result)
	}
	if event := route["mcp_event"].(map[string]any); len(event) != 3 || event["server"] != "ambush" {
		t.Fatalf("mcp_event %v", event)
	}
}

func TestEventsResubscribingKeepsTheRouteSecret(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, _ := eventsServer(t, dir, eventsGrant(86400), eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	name, first := onlyRoute(t, dir)
	first["created_at"] = "2026-01-01T00:00:00Z"
	writeHomeJSON(t, dir, "webhook_subscriptions.json", map[string]any{name: first})
	eventsSubscribe(t, srv, nil)
	_, again := onlyRoute(t, dir)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatalf("route changed\n%s\n%s", a, b)
	}
}

func TestEventsFreshSubscriptionIgnoresStateLeftByARemovedRoute(t *testing.T) {
	dir := eventsHomeDir(t)
	name := eventRouteName("ambush", "feed.emission.created", eventsArgs)
	writeHomeJSON(t, dir, eventStateFilename, map[string]any{name: map[string]any{"cursor": "stale"}})
	srv, tr := eventsServer(t, dir, eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	if _, ok := tr.requests()[0]["params"].(map[string]any)["cursor"]; ok {
		t.Fatal("stale cursor sent")
	}
}

func TestEventsRejectedSubscriptionLeavesNoRoute(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, _ := eventsServer(t, dir, &RPCError{Code: -32027, Message: "CallbackEndpointError", Data: json.RawMessage(`{"reason":"challenge_failed"}`)})
	result := eventsSubscribe(t, srv, nil)
	got, _ := json.Marshal(result)
	if string(got) != `{"error":{"code":-32027,"data":{"reason":"challenge_failed"},"message":"CallbackEndpointError"}}` {
		t.Fatalf("result %s", got)
	}
	if len(readHomeJSON(t, dir, "webhook_subscriptions.json")) != 0 {
		t.Fatal("route left behind")
	}
}

func TestEventsSubscribeNeedsAnHTTPSPublicURL(t *testing.T) {
	dir := eventsHomeDir(t)
	writeStartupConfig(t, dir, "platforms:\n  webhook:\n    extra:\n      public_url: http://x\n")
	srv, tr := eventsServer(t, dir)
	if msg, _ := eventsSubscribe(t, srv, nil)["error"].(string); !strings.Contains(msg, "public_url") {
		t.Fatalf("error %q", msg)
	}
	if len(tr.requests()) != 0 || len(readHomeJSON(t, dir, "webhook_subscriptions.json")) != 0 {
		t.Fatal("subscribed without a public url")
	}
}

func TestEventsApprovalShowsTheInstructionsAndTheDestination(t *testing.T) {
	var shown string
	var opts tools.ElicitationOptions
	prev := requestEventConsent
	requestEventConsent = func(ctx context.Context, env *tools.Env, message, description string, o tools.ElicitationOptions) string {
		shown, opts = message+"\n"+description, o
		return "accept"
	}
	t.Cleanup(func() { requestEventConsent = prev })
	args := map[string]any{"name": "feed.emission.created", "arguments": eventsArgs, "instructions": "Post a two-line summary.",
		"deliver": "telegram", "deliver_chat_id": "-100123"}
	if denied := consentError(context.Background(), nil, "ambush", args); denied != "" {
		t.Fatalf("denied %s", denied)
	}
	for _, want := range []string{"Post a two-line summary.", "telegram", "-100123", eventsArgs["feed_id"].(string)} {
		if !strings.Contains(shown, want) {
			t.Fatalf("prompt lacks %q: %s", want, shown)
		}
	}
	if opts.Surface != "mcp-events/ambush" || opts.Title != "MCP server 'ambush' event subscription" {
		t.Fatalf("options %+v", opts)
	}
	requestEventConsent = func(context.Context, *tools.Env, string, string, tools.ElicitationOptions) string { return "decline" }
	if denied := consentError(context.Background(), nil, "ambush", args); !strings.Contains(denied, "did not approve") {
		t.Fatalf("decline returned %q", denied)
	}
}

func TestEventsOverlongInstructionsAreRefusedBeforeAsking(t *testing.T) {
	if e := subscribeArgsError(map[string]any{"name": "x", "instructions": strings.Repeat("a", maxInstructionsChars)}); e != "" {
		t.Fatal(e)
	}
	if subscribeArgsError(map[string]any{"name": "x", "instructions": strings.Repeat("a", maxInstructionsChars+1)}) == "" {
		t.Fatal("overlong instructions accepted")
	}
	if subscribeArgsError(map[string]any{"name": "x", "deliver_chat_id": 123}) == "" {
		t.Fatal("numeric chat id accepted")
	}
	if subscribeArgsError(map[string]any{"name": "x", "instructions": 5}) != "'instructions' must be a string" {
		t.Fatal("non string instructions accepted")
	}
}

func TestEventsDisabledSubscriptionIsNotTurnedBackOn(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	name, route := onlyRoute(t, dir)
	route["enabled"] = false
	writeHomeJSON(t, dir, "webhook_subscriptions.json", map[string]any{name: route})
	if _, ok := eventsSubscribe(t, srv, nil)["error"]; !ok {
		t.Fatal("disabled route resubscribed")
	}
	if len(tr.requests()) != 1 || readHomeJSON(t, dir, "webhook_subscriptions.json")[name].(map[string]any)["enabled"] != false {
		t.Fatal("disabled route turned back on")
	}
}

func TestEventsUnsubscribeSendsTheGrantedURLAndDropsTheRoute(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400), map[string]any{})
	eventsSubscribe(t, srv, nil)
	name, _ := onlyRoute(t, dir)
	url := readHomeJSON(t, dir, eventStateFilename)[name].(map[string]any)["url"]
	writeStartupConfig(t, dir, "{}\n")
	o, err := unsubscribeEvent(context.Background(), srv, map[string]any{"name": "feed.emission.created", "arguments": eventsArgs})
	result := decodedEventResult(t, o, err)
	params := tr.requests()[1]["params"].(map[string]any)
	if params["delivery"].(map[string]any)["url"] != url || params["name"] != "feed.emission.created" {
		t.Fatalf("params %v", params)
	}
	if _, ok := result["unsubscribed"]; !ok || len(readHomeJSON(t, dir, "webhook_subscriptions.json")) != 0 || len(readHomeJSON(t, dir, eventStateFilename)) != 0 {
		t.Fatalf("result %v", result)
	}
	o, err = unsubscribeEvent(context.Background(), srv, map[string]any{"name": "feed.emission.created", "arguments": eventsArgs})
	if msg, _ := decodedEventResult(t, o, err)["error"].(string); !strings.Contains(msg, "No subscription") {
		t.Fatalf("second unsubscribe %q", msg)
	}
}

func TestEventsDueTimeFollowsTheGrant(t *testing.T) {
	now := epochSeconds(time.Now())
	expires := time.Unix(0, int64((now+1000)*1e9)).UTC().Format("2006-01-02T15:04:05.999999+00:00")
	if refreshDueAt(map[string]any{}) != 0 {
		t.Fatal("no grant is not due now")
	}
	if !math.IsInf(refreshDueAt(map[string]any{"granted_at": now, "refresh_before": nil}), 1) {
		t.Fatal("no expiry is not infinite")
	}
	if due := refreshDueAt(map[string]any{"granted_at": now, "refresh_before": expires}); math.Abs(due-(now+800)) > 0.01 {
		t.Fatalf("due %v want %v", due, now+800)
	}
	if refreshDueAt(map[string]any{"granted_at": now, "refresh_before": "soon"}) != 0 {
		t.Fatal("unparseable expiry is not due now")
	}
	for in, want := range map[string]float64{"2025-01-02": 0, "2025-01-02T03:04:05Z": 1735787045, "20250102T030405+0100": 1735783445,
		"2025-W01-4T00:00:00Z": 1735776000, "2025-01-02T03:04:05.123456789Z": 1735787045.123456} {
		got, err := pyISOTimestamp(in)
		if err != nil || (want != 0 && math.Abs(got-want) > 1e-5) {
			t.Fatalf("%s -> %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"2025-02-30", "2025-13-01", "2025-01-02T25:00", "x"} {
		if _, err := pyISOTimestamp(bad); err == nil {
			t.Fatalf("%s parsed", bad)
		}
	}
}

func TestEventsFreshConnectionResubscribesEveryRoute(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400), eventsGrant(3600))
	eventsSubscribe(t, srv, nil)
	if _, err := refreshServerSubscriptions(context.Background(), srv, true, nil); err != nil {
		t.Fatal(err)
	}
	if sent := tr.requests(); len(sent) != 2 || sent[1]["method"] != "events/subscribe" {
		t.Fatalf("sent %v", sent)
	}
}

func TestEventsRoutinePassSkipsRoutesThatAreNotDue(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	next, err := refreshServerSubscriptions(context.Background(), srv, false, nil)
	if err != nil || len(tr.requests()) != 1 || next <= epochSeconds(time.Now()) {
		t.Fatalf("next %v err %v sent %d", next, err, len(tr.requests()))
	}
}

func TestEventsRefusalThatCannotChangeEndsTheSubscription(t *testing.T) {
	for _, code := range []int{-32024, -32023, -32602, -32026} {
		dir := eventsHomeDir(t)
		srv, _ := eventsServer(t, dir, eventsGrant(86400), &RPCError{Code: code, Message: "refused"})
		eventsSubscribe(t, srv, nil)
		if _, err := refreshServerSubscriptions(context.Background(), srv, true, nil); err != nil {
			t.Fatal(err)
		}
		if len(readHomeJSON(t, dir, "webhook_subscriptions.json")) != 0 {
			t.Fatalf("code %d left the route", code)
		}
	}
}

func TestEventsFailingRefreshBacksOff(t *testing.T) {
	dir := eventsHomeDir(t)
	replies := []any{eventsGrant(86400)}
	for i := 0; i < 3; i++ {
		replies = append(replies, &RPCError{Code: -32027, Message: "CallbackEndpointError", Data: json.RawMessage(`{"reason":"timeout"}`)})
	}
	srv, tr := eventsServer(t, dir, replies...)
	eventsSubscribe(t, srv, nil)
	failures := map[string]refreshFailure{}
	var delays []int
	for i := 0; i < 3; i++ {
		next, err := refreshServerSubscriptions(context.Background(), srv, true, failures)
		if err != nil {
			t.Fatal(err)
		}
		delays = append(delays, int(math.Round((next-epochSeconds(time.Now()))/refreshMinSleep)))
	}
	if delays[0] != 1 || delays[1] != 2 || delays[2] != 4 {
		t.Fatalf("delays %v", delays)
	}
	state := readHomeJSON(t, dir, eventStateFilename)
	for _, v := range state {
		v.(map[string]any)["granted_at"] = epochSeconds(time.Now()) - 7200
		v.(map[string]any)["refresh_before"] = eventsGrant(-3600)["refreshBefore"]
	}
	writeHomeJSON(t, dir, eventStateFilename, state)
	next, err := refreshServerSubscriptions(context.Background(), srv, false, failures)
	if left := next - epochSeconds(time.Now()); err != nil || len(tr.requests()) != 4 || left <= 100 || left > 4*refreshMinSleep {
		t.Fatalf("next in %v err %v sent %d", left, err, len(tr.requests()))
	}
}

func TestEventsDisabledRouteIsLeftToLapse(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	name, route := onlyRoute(t, dir)
	route["enabled"] = false
	writeHomeJSON(t, dir, "webhook_subscriptions.json", map[string]any{name: route})
	if _, err := refreshServerSubscriptions(context.Background(), srv, true, nil); err != nil || len(tr.requests()) != 1 {
		t.Fatalf("err %v sent %d", err, len(tr.requests()))
	}
}

func TestEventsRouteRemovedDuringTheRefreshIsReleased(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400), eventsGrant(86400), map[string]any{})
	eventsSubscribe(t, srv, nil)
	tr.onSend = func(method string) {
		if method == "events/subscribe" {
			writeHomeJSON(t, dir, "webhook_subscriptions.json", map[string]any{})
		}
	}
	if _, err := refreshServerSubscriptions(context.Background(), srv, true, nil); err != nil {
		t.Fatal(err)
	}
	sent := tr.requests()[1:]
	if len(sent) != 2 || sent[0]["method"] != "events/subscribe" || sent[1]["method"] != "events/unsubscribe" {
		t.Fatalf("sent %v", sent)
	}
	granted := sent[0]["params"].(map[string]any)["delivery"].(map[string]any)["url"]
	if d := sent[1]["params"].(map[string]any)["delivery"].(map[string]any); len(d) != 1 || d["url"] != granted {
		t.Fatalf("release delivery %v", d)
	}
}

func TestEventsNewSubscriptionWakesTheRefreshLoop(t *testing.T) {
	dir := eventsHomeDir(t)
	var mu sync.Mutex
	var passes []bool
	prev := refreshPass
	refreshPass = func(ctx context.Context, srv *serverTask, force bool, failures map[string]refreshFailure) (float64, error) {
		mu.Lock()
		passes = append(passes, force)
		mu.Unlock()
		return math.Inf(1), nil
	}
	t.Cleanup(func() { refreshPass = prev })
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(passes)
	}
	srv, _ := eventsServer(t, dir, eventsGrant(600))
	stop := startEventsRefreshTask(context.Background(), srv)
	waitUntil(t, "the first pass", func() bool { return count() == 1 })
	eventsSubscribe(t, srv, nil)
	waitUntil(t, "a pass for the new subscription", func() bool { return count() == 2 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(passes) != 2 || !passes[0] || passes[1] {
		t.Fatalf("passes %v", passes)
	}
	refreshWakersMu.Lock()
	defer refreshWakersMu.Unlock()
	if len(refreshWakers["ambush"]) != 0 {
		t.Fatal("waker left registered")
	}
}

func TestEventsRefreshTaskNeedsTheOptIn(t *testing.T) {
	srv := newServerTask("plain", &tools.Env{Home: t.TempDir()})
	srv.config = map[string]any{"events": "no"}
	if startEventsRefreshTask(context.Background(), srv) != nil {
		t.Fatal("refresh task started without the opt in")
	}
	if !eventsEnabled(map[string]any{"events": "yes"}) || eventsEnabled(nil) {
		t.Fatal("events opt in misread")
	}
}

func TestEventsUnsubscribeWhileWaitingForTheSessionIsNotUndone(t *testing.T) {
	dir := eventsHomeDir(t)
	srv, tr := eventsServer(t, dir, eventsGrant(86400), eventsGrant(86400))
	eventsSubscribe(t, srv, nil)
	if err := srv.rpc.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := refreshServerSubscriptions(context.Background(), srv, true, nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	writeHomeJSON(t, dir, "webhook_subscriptions.json", map[string]any{})
	srv.rpc.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(tr.requests()) != 1 || len(readHomeJSON(t, dir, "webhook_subscriptions.json")) != 0 {
		t.Fatalf("sent %d", len(tr.requests()))
	}
}

func TestEventsHandlersCheckConnectionAndArguments(t *testing.T) {
	isolateHome(t)
	h := makeSubscribeEventHandler("absent", 5)
	if out := h(context.Background(), &tools.Call{Args: map[string]any{"name": "x"}}); !strings.Contains(out, "MCP server 'absent' is not connected") {
		t.Fatalf("handler returned %s", out)
	}
}
