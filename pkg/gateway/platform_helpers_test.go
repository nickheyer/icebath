package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/provider"
)

func fixedDedupClock(t *testing.T, now *float64) {
	prev := dedupNow
	dedupNow = func() float64 { return *now }
	t.Cleanup(func() { dedupNow = prev })
}

func TestMessageDeduplicatorTTL(t *testing.T) {
	t.Run("duplicate_within_ttl", func(t *testing.T) {
		d := NewMessageDeduplicator(DefaultDedupMaxSize, 60)
		if d.IsDuplicate("msg-1") || !d.IsDuplicate("msg-1") {
			t.Fatal("second sighting inside the window must be a duplicate")
		}
	})
	t.Run("not_duplicate_after_ttl_expires", func(t *testing.T) {
		d := NewMessageDeduplicator(DefaultDedupMaxSize, 5)
		d.IsDuplicate("msg-1")
		d.seen.Set("msg-1", dedupNow()-10)
		if d.IsDuplicate("msg-1") {
			t.Fatal("expired entry treated as duplicate")
		}
	})
	t.Run("contains_expires_stale_message_without_refreshing_it", func(t *testing.T) {
		d := NewMessageDeduplicator(DefaultDedupMaxSize, 5)
		d.seen.Set("msg-1", dedupNow()-10)
		if d.Contains("msg-1") || d.seen.Has("msg-1") {
			t.Fatal("stale id must read absent and be dropped")
		}
	})
	t.Run("max_size_eviction_prunes_expired", func(t *testing.T) {
		d := NewMessageDeduplicator(5, 60)
		now := dedupNow()
		for _, k := range []string{"old-0", "old-1", "old-2"} {
			d.seen.Set(k, now-120)
		}
		for _, k := range []string{"new-0", "new-1", "new-2", "trigger"} {
			d.IsDuplicate(k)
		}
		if d.seen.Len() != 4 || d.seen.Has("old-0") || !d.seen.Has("new-0") {
			t.Fatalf("seen %v", d.seen.Keys())
		}
	})
	t.Run("all_fresh_overflow_keeps_the_newest", func(t *testing.T) {
		now := 1000.0
		fixedDedupClock(t, &now)
		d := NewMessageDeduplicator(2, 60)
		for _, k := range []string{"a", "b", "c"} {
			now++
			d.IsDuplicate(k)
		}
		if got := d.seen.Keys(); !slices.Equal(got, []string{"b", "c"}) {
			t.Fatalf("seen %v", got)
		}
	})
}

func TestDedupSitesKeepTheirOwnWindow(t *testing.T) {
	now := 1000.0
	fixedDedupClock(t, &now)
	for _, w := range []struct {
		cap int
		ttl float64
	}{{1000, 300}, {2000, 300}, {1000, math.Inf(1)}} {
		d := NewMessageDeduplicator(w.cap, w.ttl)
		if d.IsDuplicate("m") || !d.IsDuplicate("m") {
			t.Fatalf("window %v", w)
		}
		if math.IsInf(w.ttl, 1) {
			now += 1e9
		} else {
			now += w.ttl
		}
		if d.IsDuplicate("m") != math.IsInf(w.ttl, 1) {
			t.Fatalf("after the window %v", w)
		}
	}
}

type dedupAdapter struct {
	*BasePlatformAdapter
	inbound  *MessageDeduplicator
	Reaction *MessageDeduplicator
	held     []any
}

func (a *dedupAdapter) AdoptHeldInbound(source any) { a.held = append(a.held, source) }

func TestCarryInboundDedupAndHeldInboundHandover(t *testing.T) {
	old := &dedupAdapter{inbound: NewMessageDeduplicator(10, 300), Reaction: NewMessageDeduplicator(10, 300)}
	old.inbound.IsDuplicate("evt-1")
	old.Reaction.IsDuplicate("r-1")
	old.inbound.seen.Set("stale", dedupNow()-1000)
	fresh := &dedupAdapter{inbound: NewMessageDeduplicator(10, 300), Reaction: NewMessageDeduplicator(10, 300)}
	CarryInboundDedup(old, fresh)
	CarryInboundDedup(nil, fresh)
	if !fresh.inbound.IsDuplicate("evt-1") || !fresh.Reaction.Contains("r-1") || fresh.inbound.Contains("stale") {
		t.Fatal("replayed ids must stay suppressed on the rebuilt adapter and stale ones must not carry")
	}
	HandOverHeldInbound(old, fresh)
	HandOverHeldInbound(nil, fresh)
	HandOverHeldInbound(old, struct{}{})
	if len(fresh.held) != 1 || fresh.held[0] != any(old) {
		t.Fatalf("held %v", fresh.held)
	}
}

func TestCancelTaskUnwindsAndIsSelfCancelSafe(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	task := SpawnAdapterTask(ctx, func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Second):
			return nil
		}
	})
	<-started
	CancelTask(ctx, task)
	if !task.Done() || !errors.Is(task.err, context.Canceled) {
		t.Fatalf("done %v err %v", task.Done(), task.err)
	}
	CancelTask(ctx, nil)
	CancelTask(ctx, task)
	var selfErr error
	me := SpawnAdapterTask(ctx, func(ctx context.Context) error {
		if CurrentAdapterTask(ctx) == nil {
			return errors.New("no current task")
		}
		CancelTask(ctx, CurrentAdapterTask(ctx))
		selfErr = ctx.Err()
		return nil
	})
	<-me.done
	if !errors.Is(selfErr, context.Canceled) || CurrentAdapterTask(ctx) != nil {
		t.Fatalf("a self cancel must only request cancellation, got %v", selfErr)
	}
}

func TestBoundedPutRefreshesAndCaps(t *testing.T) {
	store := pydict.New[string, any]()
	for i, k := range []string{"k0", "k1", "k2", "k3", "k4"} {
		BoundedPut(store, k, any(i), 3)
	}
	if !slices.Equal(store.Keys(), []string{"k2", "k3", "k4"}) {
		t.Fatalf("keys %v", store.Keys())
	}
	BoundedPut(store, "k2", any("again"), 3)
	if !slices.Equal(store.Keys(), []string{"k3", "k4", "k2"}) {
		t.Fatalf("keys %v", store.Keys())
	}
}

func TestSetTransportAdapterRefStampsTheReceivingAdapter(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	src := &SessionSource{}
	if src.TransportAdapterRef() != nil || (*SessionSource)(nil).TransportAdapterRef() != nil {
		t.Fatal("unstamped source names an adapter")
	}
	src.SetTransportAdapterRef(a)
	if src.transportAdapterRef != PlatformAdapter(a) || src.TransportAdapterRef() != PlatformAdapter(a) {
		t.Fatal("transport ref not stamped")
	}
}

func TestStripMarkdownKeepLinkTargetsKeepsHTTPURLsOnly(t *testing.T) {
	text := "see [Open it](https://example.com/i?x=1) or [mail](mailto:a@example.com)"
	if got := StripMarkdown(text, false); got != "see Open it or mail" {
		t.Fatalf("got %q", got)
	}
	if got := StripMarkdown(text, true); got != "see Open it\nhttps://example.com/i?x=1 or mail" {
		t.Fatalf("got %q", got)
	}
	if got := StripMarkdown("# Title\n\n**bold** _it_ snake_case_name `code`\n\n\n\nend", false); got != "Title\n\nbold it snake_case_name code\n\nend" {
		t.Fatalf("got %q", got)
	}
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

var fenceSamples = func() []string {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("console.log(" + string(rune('0'+i%10)) + "); // padding padding")
	}
	var table strings.Builder
	for i := 0; i < 40; i++ {
		if i > 0 {
			table.WriteString("\n")
		}
		table.WriteString("| value" + string(rune('0'+i%10)) + " | data" + string(rune('0'+i%10)) + " |")
	}
	return []string{
		strings.Repeat(strings.Repeat("This is a sentence that goes on for a while. ", 8)+"\n\n", 6),
		"Header line\n\n```js\n" + b.String() + "\n```\n\nTail paragraph after the code block ends here.",
		"Some text before.\n\n```bash\necho one\n" + strings.Repeat("echo more stuff here to pad the line out\n", 10),
		"Intro paragraph.\n\n| col1 | col2 |\n|------|------|\n" + table.String() + "\n\nDone.",
		strings.Repeat("这是一个中文段落，包含了很多字符。用于测试宽字符处理。这句话结束了。\n\n", 10),
		"Start.\n\n```sql\nSELECT * FROM t;\n```\n\n| x | y |\n|---|---|\n| 1 | 2 |\n\nEnd paragraph with some extra words to pad things out.",
		strings.Repeat("x", 500),
	}
}()

func TestFenceChunker(t *testing.T) {
	n := utf8.RuneCountInString
	t.Run("paragraph_mode_chunks_within_limit_unless_atomic", func(t *testing.T) {
		for _, text := range fenceSamples {
			for _, limit := range []int{80, 200, 400} {
				oversize := slices.ContainsFunc(SplitMarkdownAtoms(text), func(a string) bool { return n(a) > limit })
				for _, chunk := range SplitTextFenceAware(text, limit, nil, true, false) {
					if n(chunk) > limit && !oversize {
						t.Fatalf("chunk of %d > %d without an oversize atom", n(chunk), limit)
					}
				}
			}
		}
	})
	t.Run("newline_mode_balanced_fences_every_chunk", func(t *testing.T) {
		for _, text := range fenceSamples {
			for _, chunk := range SplitTextFenceAware(text, 100, nil, false, true) {
				if TextHasUnclosedFence(chunk) {
					t.Fatalf("unbalanced chunk %q", chunk)
				}
			}
		}
	})
	t.Run("split_at_paragraph_boundary_head_plus_tail", func(t *testing.T) {
		for _, text := range fenceSamples {
			head, tail := SplitAtParagraphBoundary(text, 100, nil)
			rs := []rune(text)
			if head+tail != text || (n(head) > 100 && strings.Contains(string(rs[:min(100, len(rs))]), "\n")) {
				t.Fatalf("head %d", n(head))
			}
		}
	})
	t.Run("atoms_fence_kept_whole", func(t *testing.T) {
		var fences []string
		for _, a := range SplitMarkdownAtoms(fenceSamples[1]) {
			if strings.HasPrefix(strings.TrimLeft(a, " \t\n"), "```") {
				fences = append(fences, a)
			}
		}
		if len(fences) != 1 || !strings.HasSuffix(strings.TrimRight(fences[0], " \n"), "```") {
			t.Fatalf("fences %q", fences)
		}
	})
	t.Run("merge_streaming_fences_rejoins_split_fence", func(t *testing.T) {
		merged := MergeStreamingFences([]string{"intro\n```py\ncode line", "more code\n```\ntail"})
		if len(merged) != 1 || TextHasUnclosedFence(merged[0]) {
			t.Fatalf("merged %q", merged)
		}
	})
	t.Run("infer_block_separator_rules", func(t *testing.T) {
		if InferBlockSeparator("text\n```", "next") != "\n" || InferBlockSeparator("text", "```py\nx") != "\n" ||
			InferBlockSeparator("| a | b |", "| c | d |") != "\n" || InferBlockSeparator("plain", "plain") != "\n\n" {
			t.Fatal("separator rules")
		}
	})
	t.Run("balance_closes_and_reopens", func(t *testing.T) {
		out := BalanceFencesAcrossChunks([]string{"a\n```go\nx", "y\n```\nb"})
		if !strings.HasSuffix(out[0], "\n```") || !strings.HasPrefix(out[1], "```go\n") || TextHasUnclosedFence(out[0]) || TextHasUnclosedFence(out[1]) {
			t.Fatalf("out %q", out)
		}
	})
	t.Run("greedy_pack_overflow_callback", func(t *testing.T) {
		var calls []string
		packed := GreedyPackBlocks([]string{strings.Repeat("x", 120)}, 60, nil, "\n\n", func(block string) []string {
			calls = append(calls, block)
			return []string{block[:50], block[50:]}
		})
		if len(calls) == 0 || !slices.Equal(packed, []string{strings.Repeat("x", 50), strings.Repeat("x", 70)}) {
			t.Fatalf("packed %v", packed)
		}
	})
	t.Run("emoji_heavy_split_respects_utf16_limit", func(t *testing.T) {
		text := strings.Repeat("🚀", 3000)
		chunks := SplitTextFenceAware(text, 3996, utf16Len, false, true)
		if len(chunks) <= 1 || strings.Join(chunks, "") != text || slices.ContainsFunc(chunks, func(c string) bool { return utf16Len(c) > 3996 }) {
			t.Fatalf("%d chunks", len(chunks))
		}
	})
	t.Run("mixed_emoji_newline_text_utf16_limit", func(t *testing.T) {
		chunks := SplitTextFenceAware(strings.Repeat("emoji 🎉🎊 line with some more words attached\n", 400), 500, utf16Len, false, true)
		if slices.ContainsFunc(chunks, func(c string) bool { return utf16Len(c) > 500 || c == "" }) {
			t.Fatal("chunk over the utf16 limit or empty")
		}
	})
	t.Run("empty_fence_block_atoms_match_main", func(t *testing.T) {
		for in, want := range map[string][]string{
			"```\n```\ntext":            {"```\n```", "text"},
			"```\n```\ncode\n```\ntail": {"```\n```", "code", "```\ntail"},
			"```py\n```\n\n```\n```":    {"```py\n```", "```\n```"},
			"a\n```\ncode\n```\nb":      {"a", "```\ncode\n```", "b"},
		} {
			if got := SplitMarkdownAtoms(in); !slices.Equal(got, want) {
				t.Fatalf("%q -> %q", in, got)
			}
		}
	})
}

type fakeDiscordChannel struct {
	name     any
	hasName  bool
	flags    any
	hasFlags bool
}

func (c fakeDiscordChannel) DiscordChannelFlags() (any, bool) { return c.flags, c.hasFlags }
func (c fakeDiscordChannel) DiscordChannelName() (any, bool)  { return c.name, c.hasName }

type namedOnlyChannel struct{ name string }

func (c namedOnlyChannel) DiscordChannelName() (any, bool) { return c.name, true }

func TestFlagOrSentinelNameMarksObfuscated(t *testing.T) {
	ch := func(name string, flag int) fakeDiscordChannel {
		return fakeDiscordChannel{name: name, hasName: true, flags: flag, hasFlags: true}
	}
	if !IsDiscordChannelObfuscated(ch("secret", DiscordChannelObfuscatedFlag|1<<4)) || !IsDiscordChannelObfuscated(ch(DiscordObfuscatedChannelName, 0)) {
		t.Fatal("flag or sentinel name must mark the channel obfuscated")
	}
	if IsDiscordChannelObfuscated(ch("general", 0)) || IsDiscordChannelObfuscated(namedOnlyChannel{"ok"}) {
		t.Fatal("visible channel marked obfuscated")
	}
}

func TestCompileMentionPatterns(t *testing.T) {
	telegram := "telegram"
	cfg := CompileMentionPatterns([]any{"^hey bot", 7, "  ", "(bad"}, MentionPatternOptions{LogPrefix: "Telegram", PlatformLabel: &telegram})
	if len(cfg) != 1 {
		t.Fatalf("compiled %d", len(cfg))
	}
	if ok, _ := cfg[0].MatchString("HEY BOT ping"); !ok {
		t.Fatal("config pattern must match case insensitively")
	}
	if CompileMentionPatterns(map[string]any{}, MentionPatternOptions{PlatformLabel: &telegram}) != nil || CompileMentionPatterns(nil, MentionPatternOptions{PlatformLabel: &telegram}) != nil {
		t.Fatal("non list config and missing config compile nothing")
	}
	defaults := CompileMentionPatterns(nil, MentionPatternOptions{LogPrefix: "Photon", Defaults: []string{"hermes"}})
	jsonList := CompileMentionPatterns(`["a", "b"]`, MentionPatternOptions{})
	csv := CompileMentionPatterns("a, b\nc", MentionPatternOptions{})
	scalar := CompileMentionPatterns(42, MentionPatternOptions{})
	if len(defaults) != 1 || len(jsonList) != 2 || len(csv) != 3 || len(scalar) != 1 {
		t.Fatalf("defaults %d json %d csv %d scalar %d", len(defaults), len(jsonList), len(csv), len(scalar))
	}
}

func TestSendChunksMarksPartialOverflow(t *testing.T) {
	ctx := context.Background()
	var sent []string
	res, err := SendChunks(ctx, []string{"a", "b", "c"}, func(_ context.Context, c string) (*SendResult, error) {
		sent = append(sent, c)
		return &SendResult{Success: c != "b", RawResponse: map[string]any{"code": 400}}, nil
	})
	raw, _ := res.RawResponse.(map[string]any)
	if err != nil || res.Success || !slices.Equal(sent, []string{"a", "b"}) || raw["partial_overflow"] != true || raw["delivered_chunks"] != 1 || raw["total_chunks"] != 3 || raw["code"] != 400 {
		t.Fatalf("res %+v sent %v", res, sent)
	}
	if empty, _ := SendChunks(ctx, nil, nil); empty.Success || empty.Error != "nothing to send" {
		t.Fatalf("empty %+v", empty)
	}
}

func TestRedactPhone(t *testing.T) {
	for in, want := range map[string]string{"": "<none>", "1234": "****", "+1555123": "+1****23", "+15551234567": "+155****4567"} {
		if got := RedactPhone(in); got != want {
			t.Fatalf("%q -> %q", in, got)
		}
	}
}

func trackerAt(t *testing.T) (*ThreadParticipationTracker, string) {
	path := filepath.Join(t.TempDir(), "matrix_threads.json")
	prev := threadTrackerStatePath
	threadTrackerStatePath = func(context.Context, string) string { return path }
	t.Cleanup(func() { threadTrackerStatePath = prev })
	return NewThreadParticipationTracker(context.Background(), "matrix", 0), path
}

func TestThreadTrackerMarkOffLoop(t *testing.T) {
	t.Run("two_concurrent_marks_do_not_lose_an_entry", func(t *testing.T) {
		tracker, path := trackerAt(t)
		var entered sync.WaitGroup
		entered.Add(2)
		release := make(chan struct{})
		prev := helpersAtomicJSONWrite
		helpersAtomicJSONWrite = func(ctx context.Context, p string, data any) error {
			entered.Done()
			select {
			case <-release:
			case <-time.After(time.Second):
			}
			return prev(ctx, p, data)
		}
		t.Cleanup(func() { helpersAtomicJSONWrite = prev })
		var wg sync.WaitGroup
		for _, id := range []string{"!a:example.org", "!b:example.org"} {
			wg.Add(1)
			go func() { defer wg.Done(); tracker.MarkAsync(context.Background(), id) }()
		}
		time.Sleep(50 * time.Millisecond)
		started := time.Now()
		if !tracker.Contains("!a:example.org") || time.Since(started) > 500*time.Millisecond {
			t.Fatal("membership stalled on the tracker lock while a worker persisted")
		}
		close(release)
		wg.Wait()
		raw, _ := os.ReadFile(path)
		var persisted []string
		json.Unmarshal(raw, &persisted)
		sort.Strings(persisted)
		if !slices.Equal(persisted, []string{"!a:example.org", "!b:example.org"}) {
			t.Fatalf("a concurrent mark was lost from the durable file: %v", persisted)
		}
	})
	t.Run("a_mark_is_visible_in_memory_before_the_persist_completes", func(t *testing.T) {
		tracker, _ := trackerAt(t)
		handed := make(chan struct{})
		prev := helpersToThread
		helpersToThread = func(ctx context.Context, fn func() error) error {
			close(handed)
			<-ctx.Done()
			return ctx.Err()
		}
		t.Cleanup(func() { helpersToThread = prev })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { tracker.MarkAsync(ctx, "!first:example.org"); close(done) }()
		<-handed
		if !tracker.Contains("!first:example.org") {
			t.Fatal("the in memory insert was deferred onto the worker")
		}
		cancel()
		<-done
	})
	t.Run("load_trims_and_persists_newest", func(t *testing.T) {
		tracker, path := trackerAt(t)
		os.WriteFile(path, []byte("\xef\xbb\xbf[1, \"t2\"]"), 0o600)
		tracker = NewThreadParticipationTracker(context.Background(), "matrix", 2)
		if !tracker.Contains("1") || tracker.Mark(context.Background(), "t3") != nil || tracker.Contains("1") {
			t.Fatal("load or trim to max tracked failed")
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != `["t2", "t3"]` {
			t.Fatalf("persisted %s", raw)
		}
		tracker.Clear()
		if tracker.Contains("t2") {
			t.Fatal("clear")
		}
	})
}

func TestPlatformHTTPClientLimits(t *testing.T) {
	t.Run("env_override_rejects_garbage", func(t *testing.T) {
		t.Setenv("ICEBATH_GATEWAY_HTTPX_KEEPALIVE_EXPIRY", "not-a-number")
		t.Setenv("ICEBATH_GATEWAY_HTTPX_MAX_KEEPALIVE", "-3")
		l := PlatformHTTPXLimits()
		if *l.KeepaliveExpiry != 2.0 || *l.MaxKeepaliveConnections != 10 || l.MaxConnections != nil {
			t.Fatalf("limits %+v", l)
		}
		t.Setenv("ICEBATH_GATEWAY_HTTPX_KEEPALIVE_EXPIRY", " 7.5 ")
		t.Setenv("ICEBATH_GATEWAY_HTTPX_MAX_KEEPALIVE", "4")
		if l = PlatformHTTPXLimits(); *l.KeepaliveExpiry != 7.5 || *l.MaxKeepaliveConnections != 4 {
			t.Fatalf("limits %+v", l)
		}
	})
	t.Run("platform_http_client_is_ssrf_guarded_with_shared_limits", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		ctx := context.Background()
		var hookSaw []int
		client := PlatformHTTPClient(ctx, PlatformHTTPClientOptions{ResponseHooks: []func(*http.Response) error{
			func(r *http.Response) error { hookSaw = append(hookSaw, r.StatusCode); return nil },
		}})
		hooked := client.Transport.(*platformHookTransport)
		tr := hooked.base.(*http.Transport)
		if tr.MaxIdleConns != 10 || tr.MaxIdleConnsPerHost != 10 || tr.IdleConnTimeout != 2*time.Second || client.Timeout != 30*time.Second {
			t.Fatalf("pool %d %d %v", tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
		}
		if _, err := client.Get("http://169.254.169.254/latest/meta-data/"); err == nil {
			t.Fatal("a private target must be refused at connect time")
		}
		hooked.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"http://169.254.169.254/x"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		})
		req, _ := http.NewRequest("GET", "https://example.com/start", nil)
		if _, err := hooked.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "Blocked redirect to private/internal address") || len(hookSaw) != 0 {
			t.Fatalf("err %v hooks %v", err, hookSaw)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func scopedSecrets(t *testing.T, env map[string]string) {
	prev := sharedGetScopedSecret
	sharedGetScopedSecret = func(_ context.Context, name, def string, _ bool) (string, error) {
		if v, ok := env[name]; ok {
			return v, nil
		}
		return def, nil
	}
	t.Cleanup(func() { sharedGetScopedSecret = prev })
}

func TestSharedPlatformBoilerplate(t *testing.T) {
	ctx := context.Background()
	t.Run("home_channel_name_defaults_to_home_everywhere", func(t *testing.T) {
		scopedSecrets(t, map[string]string{"X_HOME": "room"})
		seed, _ := SeedExtraFromEnv(ctx, nil, "X_HOME", "")
		if hc := seed["home_channel"].(map[string]any); hc["chat_id"] != "room" || hc["name"] != "Home" {
			t.Fatalf("seed %v", seed)
		}
		scopedSecrets(t, map[string]string{"X_HOME": "room", "X_HOME_NAME": "Ops"})
		seed, _ = SeedExtraFromEnv(ctx, nil, "X_HOME", "fallback")
		if hc := seed["home_channel"].(map[string]any); hc["name"] != "Ops" || hc["chat_id"] != "room" {
			t.Fatalf("seed %v", seed)
		}
		scopedSecrets(t, map[string]string{})
		seed, _ = SeedExtraFromEnv(ctx, nil, "X_HOME", "fallback")
		if seed["home_channel"].(map[string]any)["chat_id"] != "fallback" {
			t.Fatalf("seed %v", seed)
		}
		if seed, _ = SeedExtraFromEnv(ctx, nil, "X_HOME", ""); seed["home_channel"] != nil {
			t.Fatalf("seed %v", seed)
		}
	})
	t.Run("seed_rows_skip_blank_and_value_errors", func(t *testing.T) {
		scopedSecrets(t, map[string]string{"P": " 6697 ", "BAD": "x", "S": " s "})
		port := func(raw string) (any, error) {
			if n := CoercePort(raw, -1); n >= 0 {
				return n, nil
			}
			return nil, &PyError{"ValueError", "bad"}
		}
		seed, err := SeedExtraFromEnv(ctx, []EnvSeedRow{{"P", "port", port}, {"BAD", "bad", port}, {"S", "s", nil}, {"MISSING", "m", nil}}, "", "")
		if err != nil || seed["port"] != 6697 || seed["s"] != "s" || len(seed) != 2 {
			t.Fatalf("seed %v err %v", seed, err)
		}
	})
	t.Run("extra_or_secret_precedence_env_then_yaml_then_default", func(t *testing.T) {
		env := map[string]string{}
		scopedSecrets(t, env)
		get := func(extra map[string]any, key, def string, blank bool) any {
			v, _ := ExtraOrSecret(ctx, extra, key, "X", def, blank)
			return v
		}
		if get(map[string]any{"require_mention": false}, "require_mention", "true", true) != false ||
			get(map[string]any{"require_mention": ""}, "require_mention", "true", true) != "true" ||
			get(nil, "require_mention", "true", true) != "true" ||
			get(map[string]any{"allowed_channels": ""}, "allowed_channels", "dflt", false) != "" ||
			get(map[string]any{}, "allowed_channels", "dflt", false) != "dflt" {
			t.Fatal("yaml then default precedence")
		}
		env["X"] = "false"
		if get(map[string]any{"reactions": true}, "reactions", "true", true) != "false" {
			t.Fatal("env beats yaml")
		}
		env["X"] = "  "
		if get(map[string]any{"reactions": false}, "reactions", "true", true) != false {
			t.Fatal("blank env is unset")
		}
	})
	t.Run("extra_or_secret_scoped_miss_never_reads_launch_env", func(t *testing.T) {
		t.Setenv("X_FLAG", "launch-value")
		scoped := provider.WithMultiplexContext(provider.WithSecretScope(ctx, map[string]string{}, ""), true)
		if v, _ := ExtraOrSecret(scoped, map[string]any{"flag": "yaml-value"}, "flag", "X_FLAG", "dflt", true); v != "yaml-value" {
			t.Fatalf("got %v", v)
		}
		if v, _ := ExtraOrSecret(scoped, map[string]any{}, "flag", "X_FLAG", "dflt", true); v != "dflt" {
			t.Fatalf("got %v", v)
		}
		if v, _ := ExtraOrSecret(provider.WithMultiplexContext(ctx, false), map[string]any{"flag": "yaml-value"}, "flag", "X_FLAG", "dflt", true); v != "launch-value" {
			t.Fatalf("got %v", v)
		}
	})
	t.Run("gate_env_scoped_miss_takes_the_default", func(t *testing.T) {
		t.Setenv("GATE_X", " launch ")
		scoped := provider.WithMultiplexContext(provider.WithSecretScope(ctx, map[string]string{"GATE_Y": " mine "}, ""), true)
		if PlatformGateEnv(scoped, "GATE_X", "d") != "d" || PlatformGateEnv(scoped, "GATE_Y", "d") != "mine" ||
			PlatformGateEnv(provider.WithMultiplexContext(ctx, false), "GATE_X", "d") != "launch" || PlatformGateEnv(ctx, "", "d") != "d" {
			t.Fatal("gate env isolation")
		}
	})
	t.Run("external_fallback_consults_profile_scope_only_when_unscoped", func(t *testing.T) {
		t.Setenv("K", "")
		os.Unsetenv("K")
		unscopedProfileSecretsCache.Lock()
		prev := unscopedProfileSecretsCache.secrets
		unscopedProfileSecretsCache.secrets = map[string]string{"K": "external"}
		unscopedProfileSecretsCache.Unlock()
		t.Cleanup(func() {
			unscopedProfileSecretsCache.Lock()
			unscopedProfileSecretsCache.secrets = prev
			unscopedProfileSecretsCache.Unlock()
		})
		unscoped := provider.WithMultiplexContext(ctx, false)
		if v, err := GetScopedSecret(unscoped, "K", "<none>", false); err != nil || v != "<none>" {
			t.Fatalf("plain read %q %v", v, err)
		}
		if v, err := GetScopedSecret(unscoped, "K", "<none>", true); err != nil || v != "external" {
			t.Fatalf("external fallback %q %v", v, err)
		}
		scoped := provider.WithMultiplexContext(provider.WithSecretScope(ctx, map[string]string{"OTHER": "x"}, ""), true)
		if v, err := GetScopedSecret(scoped, "K", "dflt", true); err != nil || v != "dflt" {
			t.Fatalf("scoped read %q %v", v, err)
		}
	})
	t.Run("yaml_bridge_seeds_extra_and_writes_env_only_unscoped", func(t *testing.T) {
		for _, v := range []string{"BR_MODE", "BR_CHANNELS", "BR_MENTION", "BR_URL", "BR_JSON"} {
			t.Setenv(v, "")
			os.Unsetenv(v)
		}
		spec := []YAMLBridgeRow{{"reply_to_mode", "BR_MODE", "str"}, {"channels", "BR_CHANNELS", "csv"}, {"require_mention", "BR_MENTION", "lower"}, {"relay_url", "BR_URL", "str"}, {"opts", "BR_JSON", "json"}}
		cfg := map[string]any{"reply_to_mode": "off", "channels": []any{"a", "b"}, "require_mention": false, "relay_url": "", "opts": map[string]any{"k": 1}}
		scoped := provider.WithMultiplexContext(provider.WithSecretScope(ctx, map[string]string{}, ""), true)
		seeded, err := ApplyYAMLBridge(scoped, cfg, spec)
		if err != nil || len(seeded) != 4 || os.Getenv("BR_MODE") != "" {
			t.Fatalf("seeded %v err %v", seeded, err)
		}
		ApplyYAMLBridge(provider.WithMultiplexContext(ctx, false), cfg, spec)
		if os.Getenv("BR_MODE") != "off" || os.Getenv("BR_CHANNELS") != "a,b" || os.Getenv("BR_MENTION") != "false" || os.Getenv("BR_JSON") != `{"k": 1}` {
			t.Fatalf("env %q %q %q %q", os.Getenv("BR_MODE"), os.Getenv("BR_CHANNELS"), os.Getenv("BR_MENTION"), os.Getenv("BR_JSON"))
		}
		if none, _ := ApplyYAMLBridge(ctx, map[string]any{}, spec); none != nil {
			t.Fatalf("nothing matched must be nil, got %v", none)
		}
	})
	t.Run("connected_checks_and_coercions", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		t.Setenv("CONN_A", "x")
		t.Setenv("CONN_B", " ")
		if ok, _ := EnvIsConnected("CONN_A")(ctx, nil); !ok {
			t.Fatal("set var reads connected")
		}
		if ok, _ := EnvIsConnected("CONN_A", "CONN_B")(ctx, nil); ok {
			t.Fatal("blank var reads unconnected")
		}
		cfg := NewPlatformConfig()
		cfg.Extra = map[string]any{"a": "1"}
		if !ConfigConnectedExtra("a")(cfg) || ConfigConnectedExtra("a", "b")(cfg) {
			t.Fatal("extra keys")
		}
		cfg.Token = "t"
		if !ConfigConnectedExtra("a", "b")(cfg) {
			t.Fatal("token connects")
		}
		for _, c := range []struct {
			in   any
			want int
		}{{"8080", 8080}, {" 42 ", 42}, {8443.9, 8443}, {true, 1}, {math.Inf(1), 7}, {math.NaN(), 7}, {"8080.0", 7}, {nil, 7}, {"1_000", 1000}} {
			if got := CoercePort(c.in, 7); got != c.want {
				t.Fatalf("%v -> %d", c.in, got)
			}
		}
		if got := DecodeJSONListLiteral(` ["-100","-200"]`); !slices.Equal(got.([]any), []any{"-100", "-200"}) {
			t.Fatalf("got %v", got)
		}
		if DecodeJSONListLiteral("[bad") != "[bad" || DecodeJSONListLiteral("a,b") != "a,b" || DecodeJSONListLiteral(`{"a":1}`) != `{"a":1}` {
			t.Fatal("non list literals pass through")
		}
	})
}
