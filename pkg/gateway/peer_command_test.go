package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Stdout and stderr a run of peer commands accumulates until read, the capsys analogue
type peerCapture struct {
	t   *testing.T
	out bytes.Buffer
	err bytes.Buffer
}

func newPeerCapture(t *testing.T) *peerCapture { return &peerCapture{t: t} }

// Runs one peer invocation with stdin a terminal, an uncaught error failing the test
func (p *peerCapture) cmd(ns map[string]any) int {
	p.t.Helper()
	c := &peerCLI{ctx: context.Background(), home: p.t.TempDir(), out: &p.out, errOut: &p.err,
		stdin: strings.NewReader(""), stdinIsatty: func() bool { return true }}
	code, err := cmdPeer(c, ns)
	if err != nil {
		p.t.Fatalf("cmdPeer %v raised %v", ns, err)
	}
	return code
}

// Captured stdout and stderr since the last read, both buffers reset
func (p *peerCapture) readouterr() (string, string) {
	out, err := p.out.String(), p.err.String()
	p.out.Reset()
	p.err.Reset()
	return out, err
}

// Installs a fixed registry and peer key the way the hermes tests monkeypatch them
func peerRegistry(t *testing.T, peers func() map[string]any, secret string) {
	swapSeam(t, &loadPeers, func(context.Context, string) (map[string]any, error) { return peers(), nil })
	swapSeam(t, &peerSecret, func(context.Context, string) string { return secret })
}

// Single peer registry entry named name at url
func onePeer(name, url string) func() map[string]any {
	return func() map[string]any { return map[string]any{name: map[string]any{"url": url}} }
}

// Decoded JSON document a peer command printed
func peerJSON(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("not JSON %q: %v", text, err)
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		t.Fatalf("not an object %q", text)
	}
	return obj
}

// Writes payload as a JSON response with its length like the hermes test handlers
func writePeerJSON(w http.ResponseWriter, status int, payload any) {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	io.WriteString(w, text)
}

func TestParseTargetBarePeer(t *testing.T) {
	peer, profile, err := peerParseTarget("spark")
	if err != nil || peer != "spark" || profile != "" {
		t.Fatalf("%q %q %v", peer, profile, err)
	}
}

func TestParseTargetPeerAndProfile(t *testing.T) {
	peer, profile, err := peerParseTarget("spark/researcher")
	if err != nil || peer != "spark" || profile != "researcher" {
		t.Fatalf("%q %q %v", peer, profile, err)
	}
}

func TestParseTargetRejectsEmpty(t *testing.T) {
	_, _, err := peerParseTarget("")
	if _, ok := err.(*peerValueError); !ok {
		t.Fatalf("want ValueError, got %v", err)
	}
}

func TestParseTargetRejectsBadProfile(t *testing.T) {
	_, _, err := peerParseTarget("spark/../etc")
	if _, ok := err.(*peerValueError); !ok {
		t.Fatalf("want ValueError, got %v", err)
	}
}

func TestBaseURLBareAndProfile(t *testing.T) {
	peer := map[string]any{"url": "http://spark.lan:8377/"}
	if got := peerBaseURL(peer, ""); got != "http://spark.lan:8377" {
		t.Fatal(got)
	}
	if got := peerBaseURL(peer, "researcher"); got != "http://spark.lan:8377/p/researcher" {
		t.Fatal(got)
	}
}

// Peer whose profile prefix answers like the real api_server middleware, unserved profiles a 404 before any handler
type profilePeer struct {
	mu          sync.Mutex
	served      map[string]bool
	probeStatus int
	refusal     *pyjson.Object
	hits        []string
}

func (p *profilePeer) answer(path string) (int, any) {
	parts := append(strings.SplitN(path, "/", 4), "", "", "", "")[:4]
	profile, rest := parts[2], parts[3]
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits = append(p.hits, path)
	if !p.served[profile] {
		return 404, p.refusal
	}
	if rest == "health" {
		return p.probeStatus, pyjson.ObjectOf("status", "ok")
	}
	if strings.HasPrefix(rest, "api/sessions?") {
		return 200, pyjson.ObjectOf("data", []any{pyjson.ObjectOf("id", "bc_"+profile, "title", "Bot Chat")})
	}
	if strings.HasSuffix(rest, "/chat") {
		return 200, pyjson.ObjectOf("session_id", "bc_"+profile, "message", pyjson.ObjectOf("content", "from "+profile))
	}
	return 200, pyjson.ObjectOf("run_id", "run_1", "status", "run in "+profile)
}

func (p *profilePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	status, payload := p.answer(r.URL.RequestURI())
	writePeerJSON(w, status, payload)
}

// Profile segments of the recorded hits, the health probes dropped when asked
func (p *profilePeer) profiles(dropHealth bool) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, hit := range p.hits {
		if dropHealth && strings.HasSuffix(hit, "/health") {
			continue
		}
		seen[strings.Split(hit, "/")[2]] = true
	}
	return seen
}

func peerSet(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func TestHermesHandleReachesTheProfileThePeerServes(t *testing.T) {
	notServed := pyjson.ObjectOf("error", pyjson.ObjectOf("message", "Profile 'icebath' exists on this host but this gateway does not serve it"))
	cases := []struct {
		name        string
		served      map[string]bool
		probeStatus int
		refusal     *pyjson.Object
		expected    string
	}{
		{"legacy profile keeps precedence", peerSet("default", "icebath"), 200, nil, "icebath"},
		{"generic 404 is ambiguous", peerSet("default"), 200, nil, ""},
		{"transient failure never reroutes", peerSet("default", "icebath"), 503, nil, "icebath"},
		{"distinct refusal is relayed", peerSet("default"), 200, notServed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peer := &profilePeer{served: tc.served, probeStatus: tc.probeStatus, refusal: tc.refusal}
			if peer.refusal == nil {
				peer.refusal = pyjson.ObjectOf("error", "Unknown or unconfigured profile")
			}
			server := httptest.NewServer(peer)
			defer server.Close()
			peerRegistry(t, onePeer("box", server.URL), "secret-key-123456")
			capture := newPeerCapture(t)
			replies := []int{
				capture.cmd(map[string]any{"peer_action": "dm", "target": "box/icebath", "message": "hi", "json": true}),
				capture.cmd(map[string]any{"peer_action": "status", "target": "box/icebath", "run_id": "run_1", "json": true}),
				capture.cmd(map[string]any{"peer_action": "stop", "target": "box/icebath", "run_id": "run_1", "json": true}),
			}
			stdout, stderr := capture.readouterr()
			var out []*pyjson.Object
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				if line != "" {
					out = append(out, peerJSON(t, line))
				}
			}
			if tc.expected == "" {
				if !slices.Equal(replies, []int{1, 1, 1}) || len(out) != 0 {
					t.Fatalf("replies %v out %v", replies, out)
				}
				want := "box/default"
				if tc.refusal != nil {
					want = "Profile 'icebath' exists"
				}
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q lacks %q", stderr, want)
				}
				if got := peer.profiles(false); len(got) != 1 || !got["icebath"] {
					t.Fatalf("hits %v", peer.hits)
				}
				return
			}
			if !slices.Equal(replies, []int{0, 0, 0}) || len(out) != 3 {
				t.Fatalf("replies %v out %q err %q", replies, stdout, stderr)
			}
			got := []any{out[0].GetOr("reply", nil), out[1].GetOr("status", nil), out[2].GetOr("status", nil)}
			want := []any{"from " + tc.expected, "run in " + tc.expected, "run in " + tc.expected}
			if !pyjson.Equal(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if seen := peer.profiles(true); len(seen) != 1 || !seen[tc.expected] {
				t.Fatalf("hits %v", peer.hits)
			}
		})
	}
}

func TestMixedCaseHermesTargetReachesThePeersRealHermesProfile(t *testing.T) {
	for _, target := range []string{"box/Icebath", "box/ICEBATH"} {
		t.Run(target, func(t *testing.T) {
			peer := &profilePeer{served: peerSet("default", "icebath"), probeStatus: 200,
				refusal: pyjson.ObjectOf("error", "Unknown or unconfigured profile")}
			server := httptest.NewServer(peer)
			defer server.Close()
			peerRegistry(t, onePeer("box", server.URL), "secret-key-123456")
			capture := newPeerCapture(t)
			rc := capture.cmd(map[string]any{"peer_action": "dm", "target": target, "message": "hi", "json": true})
			stdout, _ := capture.readouterr()
			if reply := peerJSON(t, stdout).GetOr("reply", nil); rc != 0 || reply != "from icebath" {
				t.Fatalf("rc %d reply %v", rc, reply)
			}
		})
	}
}

func TestAddListRemoveRoundtrip(t *testing.T) {
	store := map[string]any{}
	swapSeam(t, &loadPeers, func(context.Context, string) (map[string]any, error) {
		cp := map[string]any{}
		for k, v := range store {
			cp[k] = v
		}
		return cp, nil
	})
	swapSeam(t, &savePeers, func(_ context.Context, _ string, peers map[string]any) error {
		clear(store)
		for k, v := range peers {
			store[k] = v
		}
		return nil
	})
	swapSeam(t, &peerSecret, func(context.Context, string) string { return strings.Repeat("k", 20) })
	capture := newPeerCapture(t)
	if rc := capture.cmd(map[string]any{"peer_action": "add", "name": "spark", "url": "http://spark.lan:8377", "key": "", "note": ""}); rc != 0 {
		t.Fatalf("add rc %d", rc)
	}
	if entry, _ := store["spark"].(map[string]any); entry["url"] != "http://spark.lan:8377" {
		t.Fatalf("store %v", store)
	}
	capture.readouterr()
	if rc := capture.cmd(map[string]any{"peer_action": "list"}); rc != 0 {
		t.Fatalf("list rc %d", rc)
	}
	if out, _ := capture.readouterr(); !strings.Contains(out, "spark") {
		t.Fatalf("list %q", out)
	}
	if rc := capture.cmd(map[string]any{"peer_action": "remove", "name": "spark"}); rc != 0 {
		t.Fatalf("remove rc %d", rc)
	}
	if _, ok := store["spark"]; ok {
		t.Fatalf("store %v", store)
	}
}

func TestAddRejectsBadNameAndURL(t *testing.T) {
	swapSeam(t, &loadPeers, func(context.Context, string) (map[string]any, error) { return map[string]any{}, nil })
	swapSeam(t, &savePeers, func(context.Context, string, map[string]any) error { return nil })
	capture := newPeerCapture(t)
	if rc := capture.cmd(map[string]any{"peer_action": "add", "name": "Bad Name!", "url": "http://x", "key": "", "note": ""}); rc != 2 {
		t.Fatalf("bad name rc %d", rc)
	}
	if rc := capture.cmd(map[string]any{"peer_action": "add", "name": "ok", "url": "ftp://x", "key": "", "note": ""}); rc != 2 {
		t.Fatalf("bad url rc %d", rc)
	}
}

func TestDMUnknownPeerAndMissingKey(t *testing.T) {
	peerRegistry(t, onePeer("spark", "http://x"), "")
	capture := newPeerCapture(t)
	if rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "nope", "message": "hi", "json": false}); rc != 1 {
		t.Fatalf("unknown rc %d", rc)
	}
	if rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "hi", "json": false}); rc != 1 {
		t.Fatalf("missing key rc %d", rc)
	}
}

// Requests a fake peer recorded, the class level lists of the hermes fake handlers
type fakePeerState struct {
	sessions           []string
	hiddenSessions     []string
	chats              []any
	chatBodies         []any
	runs               []any
	runIdempotencyKeys []string
	authSeen           []string
	getQueries         []map[string][]string
}

// Loopback peer gateway recording what the dm and run flows send, the hermes fake handler classes in one server
type fakePeer struct {
	mu               sync.Mutex
	variant          string
	chatReplyContent string
	url              string
	fakePeerState
}

// Fresh fake peer server of one handler variant, closed when the test ends
func newFakePeer(t *testing.T, variant string) *fakePeer {
	p := &fakePeer{variant: variant, chatReplyContent: "reply from the other machine"}
	if variant == "hidden" {
		p.hiddenSessions = []string{"bc_hidden"}
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	p.url = server.URL
	return p
}

func (p *fakePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authSeen = append(p.authSeen, r.Header.Get("Authorization"))
	path := r.URL.RequestURI()
	switch {
	case r.Method == http.MethodGet && p.variant == "hidden":
		p.hiddenGET(w, r, path)
	case r.Method == http.MethodGet && p.variant == "old":
		p.oldGET(w, path)
	case r.Method == http.MethodGet:
		p.baseGET(w, path)
	case p.variant == "old":
		io.Copy(io.Discard, r.Body)
		p.oldPOST(w, path)
	default:
		p.basePOST(w, r, path)
	}
}

func (p *fakePeer) baseGET(w http.ResponseWriter, path string) {
	switch {
	case path == "/v1/capabilities":
		writePeerJSON(w, 200, pyjson.ObjectOf("features", pyjson.ObjectOf("runs_idempotency",
			pyjson.ObjectOf("supported", true, "durable", true, "retention_seconds", 86400))))
	case path == "/v1/runs/run_1":
		writePeerJSON(w, 200, pyjson.ObjectOf("object", "hermes.run", "run_id", "run_1", "status", "completed",
			"session_id", "bc_existing", "output", "async reply from the other machine"))
	case strings.HasPrefix(path, "/api/sessions"):
		data := []any{}
		for _, s := range p.sessions {
			data = append(data, pyjson.ObjectOf("id", s, "title", "Bot Chat"))
		}
		writePeerJSON(w, 200, pyjson.ObjectOf("object", "list", "data", data))
	default:
		writePeerJSON(w, 404, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "not found")))
	}
}

func (p *fakePeer) basePOST(w http.ResponseWriter, r *http.Request, path string) {
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		panic(err)
	}
	body := decoded.(*pyjson.Object)
	switch {
	case path == "/api/sessions":
		p.sessions = append(p.sessions, "bc_1")
		writePeerJSON(w, 201, pyjson.ObjectOf("object", "hermes.session", "session", pyjson.ObjectOf("id", "bc_1", "title", body.GetOr("title", nil))))
	case strings.HasPrefix(path, "/api/sessions/") && strings.HasSuffix(path, "/chat"):
		p.chats = append(p.chats, body.GetOr("message", nil))
		p.chatBodies = append(p.chatBodies, body)
		writePeerJSON(w, 200, pyjson.ObjectOf("object", "hermes.session.chat.completion", "session_id", "bc_1",
			"message", pyjson.ObjectOf("role", "assistant", "content", p.chatReplyContent)))
	case path == "/v1/runs":
		p.runs = append(p.runs, body)
		p.runIdempotencyKeys = append(p.runIdempotencyKeys, r.Header.Get("Idempotency-Key"))
		writePeerJSON(w, 202, pyjson.ObjectOf("run_id", "run_1", "status", "started", "replayed", false))
	case path == "/v1/runs/run_1/stop":
		writePeerJSON(w, 200, pyjson.ObjectOf("run_id", "run_1", "status", "stopping"))
	default:
		writePeerJSON(w, 404, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "not found")))
	}
}

// New style peer whose hidden Bot Chat appears only for the exact title and include_hidden lookup
func (p *fakePeer) hiddenGET(w http.ResponseWriter, r *http.Request, path string) {
	if !strings.HasPrefix(path, "/api/sessions") {
		writePeerJSON(w, 404, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "not found")))
		return
	}
	query := pyurl.ParseQS(r.URL.RawQuery, false)
	p.getQueries = append(p.getQueries, query)
	data := []any{}
	for _, s := range p.sessions {
		data = append(data, pyjson.ObjectOf("id", s, "title", "Bot Chat", "hidden", false))
	}
	first := func(key, def string) string {
		if v := query[key]; len(v) > 0 {
			return v[0]
		}
		return def
	}
	if first("title", "") == "Bot Chat" && (first("include_hidden", "0") == "1" || first("include_hidden", "0") == "true") {
		for _, s := range p.hiddenSessions {
			data = append(data, pyjson.ObjectOf("id", s, "title", "Bot Chat", "hidden", true))
		}
	}
	writePeerJSON(w, 200, pyjson.ObjectOf("object", "list", "data", data))
}

// Old peer that ignores the hidden lookup and refuses the duplicate create with the UNIQUE title 400
func (p *fakePeer) oldGET(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "/api/sessions") {
		writePeerJSON(w, 200, pyjson.ObjectOf("object", "list", "data", []any{}))
		return
	}
	writePeerJSON(w, 404, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "not found")))
}

func (p *fakePeer) oldPOST(w http.ResponseWriter, path string) {
	if path == "/api/sessions" {
		writePeerJSON(w, 400, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "Title already in use by session hidden_bc_1", "code", "invalid_title")))
		return
	}
	writePeerJSON(w, 404, pyjson.ObjectOf("error", pyjson.ObjectOf("message", "not found")))
}

// Copy of the recorded state under the server lock
func (p *fakePeer) snapshot() fakePeerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fakePeerState{sessions: slices.Clone(p.sessions), chats: slices.Clone(p.chats), chatBodies: slices.Clone(p.chatBodies),
		runs: slices.Clone(p.runs), runIdempotencyKeys: slices.Clone(p.runIdempotencyKeys), authSeen: slices.Clone(p.authSeen),
		getQueries: slices.Clone(p.getQueries)}
}

func (p *fakePeer) setReply(content string) {
	p.mu.Lock()
	p.chatReplyContent = content
	p.mu.Unlock()
}

func (p *fakePeer) setSessions(ids ...string) {
	p.mu.Lock()
	p.sessions = ids
	p.mu.Unlock()
}

func TestDMCreatesBotChatThenChats(t *testing.T) {
	fake := newFakePeer(t, "base")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "Message from 🤖 dixie (@dixie): disk status?", "json": false})
	out, _ := capture.readouterr()
	if rc != 0 || !strings.Contains(out, "reply from the other machine") {
		t.Fatalf("rc %d out %q", rc, out)
	}
	got := fake.snapshot()
	if !slices.Equal(got.sessions, []string{"bc_1"}) || !pyjson.Equal(got.chats, []any{"Message from 🤖 dixie (@dixie): disk status?"}) {
		t.Fatalf("sessions %v chats %v", got.sessions, got.chats)
	}
	for _, a := range got.authSeen {
		if a != "Bearer secret-key-123456" {
			t.Fatalf("auth %v", got.authSeen)
		}
	}
}

func TestDMHidesABareSilenceMarkerFromTheSendingAgent(t *testing.T) {
	fake := newFakePeer(t, "base")
	fake.setReply("NO_REPLY")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": false})
	out, _ := capture.readouterr()
	if rc != 0 || strings.Contains(out, "NO_REPLY") || !strings.Contains(out, "(no reply)") {
		t.Fatalf("rc %d out %q", rc, out)
	}
}

func TestDMJSONOutputHidesABareSilenceMarkerToo(t *testing.T) {
	fake := newFakePeer(t, "base")
	fake.setReply("[SILENT]")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": true})
	out, _ := capture.readouterr()
	if reply := peerJSON(t, out).GetOr("reply", nil); rc != 0 || reply != "" {
		t.Fatalf("rc %d reply %v", rc, reply)
	}
}

func TestDMDeliversProseThatOnlyMentionsTheMarkerNormally(t *testing.T) {
	fake := newFakePeer(t, "base")
	fake.setReply("The status is NO_REPLY needed right now, all clear.")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": false})
	out, _ := capture.readouterr()
	if rc != 0 || !strings.Contains(out, "The status is NO_REPLY needed right now, all clear.") {
		t.Fatalf("rc %d out %q", rc, out)
	}
}

func TestDMReusesExistingBotChat(t *testing.T) {
	fake := newFakePeer(t, "base")
	fake.setSessions("bc_existing")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": true})
	out, _ := capture.readouterr()
	if reply := peerJSON(t, out).GetOr("reply", nil); rc != 0 || reply != "reply from the other machine" {
		t.Fatalf("rc %d reply %v", rc, reply)
	}
	if got := fake.snapshot().sessions; !slices.Equal(got, []string{"bc_existing"}) {
		t.Fatalf("sessions %v", got)
	}
}

// Turn author the message_agent runner sets, as the hermes AUTHOR constant
func peerAuthor() *pyjson.Object {
	return pyjson.ObjectOf("id", "bot:dixie", "name", "dixie", "is_bot", true)
}

// Registers spark at url with the canonical chat existing and the turn author variable set or unset
func peerSpark(t *testing.T, fake *fakePeer, authorEnv *pyjson.Object) {
	fake.setSessions("bc_existing")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	t.Setenv(provider.TurnAuthorEnvName, "")
	if authorEnv == nil {
		os.Unsetenv(provider.TurnAuthorEnvName)
		return
	}
	text, err := pyjson.Dumps(authorEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(provider.TurnAuthorEnvName, text)
}

func TestDMBodyCarriesAuthorOnlyFromEnv(t *testing.T) {
	withExtra := peerAuthor()
	withExtra.Set("x", 1)
	cases := []struct {
		id       string
		env      *pyjson.Object
		expected *pyjson.Object
	}{
		{"author from env", withExtra, pyjson.ObjectOf("message", "ping", "author", peerAuthor())},
		{"no env", nil, pyjson.ObjectOf("message", "ping")},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			fake := newFakePeer(t, "base")
			peerSpark(t, fake, tc.env)
			capture := newPeerCapture(t)
			rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": true})
			if got := fake.snapshot().chatBodies; rc != 0 || !pyjson.Equal(got, []any{tc.expected}) {
				t.Fatalf("rc %d bodies %v", rc, got)
			}
		})
	}
}

func TestRunBodyCarriesAuthorFromEnv(t *testing.T) {
	fake := newFakePeer(t, "base")
	peerSpark(t, fake, peerAuthor())
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "run", "target": "spark", "message": "long task", "idempotency_key": "ticket-1", "json": true})
	want := []any{pyjson.ObjectOf("input", "long task", "session_id", "bc_existing", "author", peerAuthor())}
	if got := fake.snapshot().runs; rc != 0 || !pyjson.Equal(got, want) {
		t.Fatalf("rc %d runs %v", rc, got)
	}
}

func TestFindBotChatSendsHiddenAwareLookup(t *testing.T) {
	fake := newFakePeer(t, "hidden")
	found, err := peerFindBotChat(context.Background(), fake.url, "secret-key-123456")
	if err != nil || found != "bc_hidden" {
		t.Fatalf("found %q %v", found, err)
	}
	queries := fake.snapshot().getQueries
	query := queries[len(queries)-1]
	if !slices.Equal(query["title"], []string{"Bot Chat"}) || !slices.Equal(query["include_hidden"], []string{"1"}) {
		t.Fatalf("query %v", query)
	}
}

func TestDMResolvesHiddenBotChatWithoutDuplicateCreate(t *testing.T) {
	fake := newFakePeer(t, "hidden")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": true})
	out, _ := capture.readouterr()
	if reply := peerJSON(t, out).GetOr("reply", nil); rc != 0 || reply != "reply from the other machine" {
		t.Fatalf("rc %d reply %v", rc, reply)
	}
	got := fake.snapshot()
	if len(got.sessions) != 0 || !pyjson.Equal(got.chats, []any{"ping"}) {
		t.Fatalf("sessions %v chats %v", got.sessions, got.chats)
	}
}

func TestDMOlderPeerHiddenDuplicateGivesClearError(t *testing.T) {
	fake := newFakePeer(t, "old")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "dm", "target": "spark", "message": "ping", "json": false})
	_, stderr := capture.readouterr()
	if rc != 1 || !strings.Contains(stderr, "hidden") || !strings.Contains(stderr, "Bot Chat") || !strings.Contains(stderr, "Title already in use") {
		t.Fatalf("rc %d stderr %q", rc, stderr)
	}
}

func TestRunStartsAsyncTurnWithCanonicalSessionAndIdempotency(t *testing.T) {
	fake := newFakePeer(t, "base")
	fake.setSessions("bc_existing")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "run", "target": "spark", "message": "long task", "idempotency_key": "ticket-123", "json": true})
	out, _ := capture.readouterr()
	want := pyjson.ObjectOf("peer", "spark", "profile", nil, "session_id", "bc_existing", "run_id", "run_1",
		"status", "started", "idempotency_key", "ticket-123", "replayed", false)
	if payload := peerJSON(t, out); rc != 0 || !pyjson.Equal(payload, want) {
		t.Fatalf("rc %d payload %q", rc, out)
	}
	got := fake.snapshot()
	if !pyjson.Equal(got.runs, []any{pyjson.ObjectOf("input", "long task", "session_id", "bc_existing")}) ||
		!slices.Equal(got.runIdempotencyKeys, []string{"ticket-123"}) {
		t.Fatalf("runs %v keys %v", got.runs, got.runIdempotencyKeys)
	}
}

func TestStatusReadsAsyncRunOutput(t *testing.T) {
	fake := newFakePeer(t, "base")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "status", "target": "spark", "run_id": "run_1", "json": true})
	out, _ := capture.readouterr()
	payload := peerJSON(t, out)
	if rc != 0 || payload.GetOr("status", nil) != "completed" || payload.GetOr("output", nil) != "async reply from the other machine" {
		t.Fatalf("rc %d payload %q", rc, out)
	}
}

func TestStopRequestsExactAsyncRun(t *testing.T) {
	fake := newFakePeer(t, "base")
	peerRegistry(t, onePeer("spark", fake.url), "secret-key-123456")
	capture := newPeerCapture(t)
	rc := capture.cmd(map[string]any{"peer_action": "stop", "target": "spark", "run_id": "run_1", "json": true})
	out, _ := capture.readouterr()
	payload := peerJSON(t, out)
	if rc != 0 || payload.GetOr("run_id", nil) != "run_1" || payload.GetOr("status", nil) != "stopping" {
		t.Fatalf("rc %d payload %q", rc, out)
	}
}

func TestRequestStripsBearerKeyAcrossRedirectOrigin(t *testing.T) {
	var mu sync.Mutex
	var attackerAuth [][]string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attackerAuth = append(attackerAuth, r.Header.Values("Authorization"))
		mu.Unlock()
		writePeerJSON(w, 200, pyjson.ObjectOf("object", "list", "data", []any{}))
	}))
	defer attacker.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Location", attacker.URL+r.URL.RequestURI())
		w.WriteHeader(http.StatusFound)
	}))
	defer peer.Close()
	result, err := peerRequest(context.Background(), peer.URL+"/api/sessions", "top-secret-peer-key", peerRequestOptions{})
	if err != nil || !pyjson.Equal(result, pyjson.ObjectOf("object", "list", "data", []any{})) {
		t.Fatalf("result %v %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attackerAuth) == 0 {
		t.Fatal("redirect target was never reached")
	}
	for _, header := range attackerAuth {
		if len(header) != 0 {
			raw, _ := json.Marshal(attackerAuth)
			t.Fatalf("peer's Bearer key leaked to the redirect target: %s", raw)
		}
	}
}
