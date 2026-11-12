package discord

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter built the way the hermes fixtures build one, the instance state of a fresh constructor
func newTestAdapter(t *testing.T, extra map[string]any) *DiscordAdapter {
	t.Helper()
	if extra == nil {
		extra = map[string]any{}
	}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "x"
	cfg.Extra = extra
	ctx := context.Background()
	a := &DiscordAdapter{
		readyEvent: gateway.NewInterruptEvent(), allowedUserIDs: strSet{}, usernameResolvedIDs: strSet{}, allowedRoleIDs: strSet{},
		botTagDebounceUntil: map[string]float64{}, threads: gateway.NewThreadParticipationTracker(ctx, "discord", 500),
		dedup: gateway.NewMessageDeduplicator(2000, 300), recoveryStore: NewDiscordRecoveryStore(ctx, t.TempDir()),
		replyToMode: "first", nonconversationalMessages: newNonConversationalTracker(ctx, nonConversationalMaxTracked),
		lastSelfMessageID: map[string]string{}, lastOverflowPreview: map[[2]string]string{}, typingTasks: map[string]*typingTask{}, semanticThreadRenames: newSemanticThreadRenames(),
		textBatchDelaySeconds: 0.6, textBatchSplitDelaySeconds: 2.0,
	}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformDiscord, a)
	a.handleMessage = a.BasePlatformAdapter.HandleMessage
	a.voiceState = newVoiceState()
	if os.Getenv("ICEBATH_HOME") == "" {
		t.Setenv("ICEBATH_HOME", t.TempDir())
	}
	a.slashCommands = true
	return a
}

// In process discordgo session whose cache holds the bot user and the given guilds and channels
func attachClient(t *testing.T, a *DiscordAdapter, selfID string, guildIDs []string, channels ...*discordgo.Channel) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot x")
	if err != nil {
		t.Fatal(err)
	}
	s.State.User = &discordgo.User{ID: selfID, Bot: true, Username: "hermes"}
	for _, g := range guildIDs {
		if err := s.State.GuildAdd(&discordgo.Guild{ID: g}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ch := range channels {
		if err := s.State.ChannelAdd(ch); err != nil {
			t.Fatal(err)
		}
	}
	a.client = s
	return s
}

// One recorded REST call of the fake Discord API
type fakeCall struct {
	Method, Path string
	Body         map[string]any
}

// Loopback Discord REST API recording calls, failures scripted per call
type fakeDiscordAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []fakeCall
	nextID int
	fail   func(c fakeCall, n int) (int, string)
	thread func(body map[string]any) string
	get    map[string]string
	// Channel history served newest first by GET /channels/{id}/messages
	history map[string][]*discordgo.Message
}

// Routes every discordgo request of the session to a loopback fake API
func newFakeDiscordAPI(t *testing.T, s *discordgo.Session) *fakeDiscordAPI {
	f := &fakeDiscordAPI{nextID: 9000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	target, _ := url.Parse(f.srv.URL)
	s.Client = &http.Client{Transport: rewriteTransport{target: target}}
	return f
}

// Rewrites the Discord host of a request to the loopback fake
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = r.target.Scheme, r.target.Host, r.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

func (f *fakeDiscordAPI) serve(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	body := map[string]any{}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/") {
		if _, after, ok := strings.Cut(string(raw), `name="payload_json"`); ok {
			if i := strings.Index(after, "{"); i >= 0 {
				json.NewDecoder(strings.NewReader(after[i:])).Decode(&body)
			}
		}
		body["_files"] = strings.Count(string(raw), `filename="`)
	} else {
		json.Unmarshal(raw, &body)
	}
	path := strings.TrimPrefix(req.URL.Path, "/api/v"+discordgo.APIVersion)
	c := fakeCall{Method: req.Method, Path: path, Body: body}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	n := len(f.calls)
	fail, thread := f.fail, f.thread
	canned, hasCanned := f.get[path]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if fail != nil {
		if status, text := fail(c, n); status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, text)
			return
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if hasCanned && req.Method == http.MethodGet {
		io.WriteString(w, canned)
		return
	}
	switch {
	case req.Method == http.MethodPost && len(parts) == 3 && parts[2] == "messages":
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":"%d","channel_id":"%s"}`, id, parts[1])
	case req.Method == http.MethodPatch && len(parts) == 4:
		fmt.Fprintf(w, `{"id":"%s","channel_id":"%s"}`, parts[3], parts[1])
	case req.Method == http.MethodPost && len(parts) == 3 && parts[2] == "threads":
		resp := `{"id":"777","type":11,"parent_id":"` + parts[1] + `","message":{"id":"777","channel_id":"777","attachments":[]}}`
		if thread != nil {
			resp = thread(body)
		}
		io.WriteString(w, resp)
	case req.Method == http.MethodPost && len(parts) == 5 && parts[4] == "threads":
		resp := `{"id":"` + parts[3] + `","type":11,"parent_id":"` + parts[1] + `","name":"` + fmt.Sprint(body["name"]) + `"}`
		if thread != nil {
			resp = thread(body)
		}
		io.WriteString(w, resp)
	case req.Method == http.MethodGet && len(parts) == 3 && parts[2] == "messages":
		json.NewEncoder(w).Encode(f.historyPage(parts[1], req.URL.Query()))
	case req.Method == http.MethodGet && len(parts) == 2 && parts[0] == "channels":
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Unknown Channel","code":10003}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// Recorded calls matching the method and path suffix
func (f *fakeDiscordAPI) matching(method, suffix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			out = append(out, c)
		}
	}
	return out
}

// Bodies of every message posted to a channel
func (f *fakeDiscordAPI) sends(channelID string) []map[string]any {
	var out []map[string]any
	for _, c := range f.matching(http.MethodPost, "/channels/"+channelID+"/messages") {
		out = append(out, c.Body)
	}
	return out
}

// Contents of every message posted to a channel
func (f *fakeDiscordAPI) sentContents(channelID string) []string {
	var out []string
	for _, b := range f.sends(channelID) {
		s, _ := b["content"].(string)
		out = append(out, s)
	}
	return out
}

// Contents of every edit of one message
func (f *fakeDiscordAPI) editContents(channelID, messageID string) []string {
	var out []string
	for _, c := range f.matching(http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID) {
		s, _ := c.Body["content"].(string)
		out = append(out, s)
	}
	return out
}

// Adapter with a live fake client caching guild text channel 555
func sendAdapter(t *testing.T) (*DiscordAdapter, *fakeDiscordAPI) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	s := attachClient(t, a, "1", []string{"999"}, &discordgo.Channel{ID: "555", GuildID: "999", Type: discordgo.ChannelTypeGuildText})
	return a, newFakeDiscordAPI(t, s)
}

// History page newest first below before and above after, capped at limit
func (f *fakeDiscordAPI) historyPage(channelID string, q url.Values) []*discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	num := func(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
	before, after, limit := num(q.Get("before")), num(q.Get("after")), int(num(q.Get("limit")))
	var out []*discordgo.Message
	for _, m := range f.history[channelID] {
		id := num(m.ID)
		if (before == 0 || id < before) && id > after {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(x, y *discordgo.Message) int { return cmp.Compare(num(y.ID), num(x.ID)) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []*discordgo.Message{}
	}
	return out
}
