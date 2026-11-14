package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One request the fake RetainDB server received
type rdbRequest struct {
	method, path, query string
	header              http.Header
	body                map[string]any
	raw                 []byte
}

// Fake RetainDB API whose handler answers each request, every request recorded in order
type rdbServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []rdbRequest
	handle   func(r rdbRequest) (int, any)
}

func newRDBServer(t *testing.T, handle func(r rdbRequest) (int, any)) *rdbServer {
	t.Helper()
	s := &rdbServer{handle: handle}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := rdbRequest{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone(), raw: raw}
		_ = json.Unmarshal(raw, &req.body)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		status, payload := 200, any(map[string]any{"ok": true})
		if s.handle != nil {
			status, payload = s.handle(req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if text, ok := payload.(string); ok {
			_, _ = w.Write([]byte(text))
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *rdbServer) seen() []rdbRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rdbRequest(nil), s.requests...)
}

// Clears the RetainDB variables for the test and restores them after
func unsetRetainDBEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"RETAINDB_API_KEY", "RETAINDB_BASE_URL", "RETAINDB_PROJECT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Isolated home with the RetainDB variables cleared and fast retries
func rdbHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	unsetRetainDBEnv(t)
	prevDelay, prevJoin := retaindbRetryDelay, retaindbPrefetchJoin
	retaindbRetryDelay, retaindbPrefetchJoin = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { retaindbRetryDelay, retaindbPrefetchJoin = prevDelay, prevJoin })
	return h
}

func pendingRows(t *testing.T, dbPath string) []sql.NullString {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT last_error FROM pending")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sql.NullString
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestRetainDBClientBaseURLTrailingSlashStripped(t *testing.T) {
	if c := newRetainDBClient("rdb-test-key", "https://api.retaindb.com///", "test"); c.baseURL != "https://api.retaindb.com" {
		t.Fatalf("base url %q", c.baseURL)
	}
}

func TestRetainDBClientHeadersIncludeAuth(t *testing.T) {
	c := newRetainDBClient("Bearer rdb-test-key ", "https://api.retaindb.com", "test")
	h := c.headers("/v1/files", true)
	if h["Authorization"] != "Bearer rdb-test-key" || h["x-sdk-runtime"] != "icebath-plugin" || h["Content-Type"] != "application/json" {
		t.Fatalf("headers %v", h)
	}
	if _, ok := h["X-API-Key"]; ok {
		t.Fatal("file routes must not carry X-API-Key")
	}
	if h := c.headers("/v1/memory/search", false); h["X-API-Key"] != "rdb-test-key" || h["Content-Type"] != "" {
		t.Fatalf("memory headers %v", h)
	}
}

func TestRetainDBClientAddMemoryTriesFallback(t *testing.T) {
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		if r.path == "/v1/memory" {
			return 404, map[string]any{"message": "not here"}
		}
		return 200, map[string]any{"id": "mem-1"}
	})
	c := newRetainDBClient("k", srv.URL, "p")
	got, err := c.addMemory(context.Background(), "u1", "s1", "test fact", "factual", 0.7)
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["id"] != "mem-1" {
		t.Fatalf("result %v", got)
	}
	reqs := srv.seen()
	if len(reqs) != 2 || reqs[0].body["write_mode"] != "sync" || reqs[1].path != "/v1/memories" {
		t.Fatalf("requests %+v", reqs)
	}
	if _, ok := reqs[1].body["write_mode"]; ok {
		t.Fatal("legacy route got write_mode")
	}
	if reqs[1].body["project"] != "p" || reqs[1].body["user_id"] != "u1" || reqs[1].body["content"] != "test fact" {
		t.Fatalf("legacy body %v", reqs[1].body)
	}
}

func TestRetainDBClientDeleteMemoryTriesFallback(t *testing.T) {
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		if strings.HasPrefix(r.path, "/v1/memory/") {
			return 404, "missing"
		}
		return 200, map[string]any{"deleted": true}
	})
	c := newRetainDBClient("k", srv.URL, "p")
	got, err := c.deleteMemory(context.Background(), "mem 123/x")
	if err != nil || got.(map[string]any)["deleted"] != true {
		t.Fatalf("delete %v %v", got, err)
	}
	reqs := srv.seen()
	if len(reqs) != 2 || reqs[0].path != "/v1/memory/mem%20123%2Fx" || reqs[1].path != "/v1/memories/mem%20123%2Fx" || reqs[0].method != "DELETE" {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestRetainDBClientRequestErrorCarriesServerMessage(t *testing.T) {
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		if r.path == "/v1/files" {
			return 500, "plain failure"
		}
		return 422, map[string]any{"error": "bad query"}
	})
	c := newRetainDBClient("k", srv.URL, "p")
	if _, err := c.search(context.Background(), "u", "s", "q", 8); err == nil || err.Error() != "RetainDB POST /v1/memory/search failed (422): bad query" {
		t.Fatalf("search error %v", err)
	}
	if _, err := c.listFiles(context.Background(), "/reports/", 5); err == nil || err.Error() != "RetainDB GET /v1/files failed (500): plain failure" {
		t.Fatalf("list error %v", err)
	}
	reqs := srv.seen()
	if reqs[1].query != "limit=5&prefix=%2Freports%2F" {
		t.Fatalf("query %q", reqs[1].query)
	}
}

func TestRetainDBWriteQueueClosesItsDatabase(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, nil)
	q, err := newRetainDBWriteQueue(context.Background(), nil, newRetainDBClient("k", srv.URL, "p"), filepath.Join(t.TempDir(), "retaindb.db"))
	if err != nil {
		t.Fatal(err)
	}
	q.close()
	if err := q.db.Ping(); err == nil {
		t.Fatal("database still open after shutdown")
	}
}

func TestRetainDBWriteQueueIgnoresEnqueueAfterShutdown(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, nil)
	dbPath := filepath.Join(t.TempDir(), "retaindb.db")
	q, err := newRetainDBWriteQueue(context.Background(), nil, newRetainDBClient("k", srv.URL, "p"), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	q.close()
	if err := q.enqueue("user", "session", []any{}); err != nil {
		t.Fatal(err)
	}
	if rows := pendingRows(t, dbPath); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
	q.close()
}

func TestRetainDBWriteQueueFlushDeletesRowOnSuccess(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, func(r rdbRequest) (int, any) { return 200, map[string]any{"status": "ok"} })
	dbPath := filepath.Join(t.TempDir(), "test_queue.db")
	q, err := newRetainDBWriteQueue(context.Background(), nil, newRetainDBClient("k", srv.URL, "p"), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.enqueue("user1", "sess1", []any{map[string]any{"role": "user", "content": "hi"}}); err != nil {
		t.Fatal(err)
	}
	q.close()
	if rows := pendingRows(t, dbPath); len(rows) != 0 {
		t.Fatalf("rows left %v", rows)
	}
	reqs := srv.seen()
	if len(reqs) != 1 || reqs[0].path != "/v1/memory/ingest/session" || reqs[0].body["user_id"] != "user1" || reqs[0].body["write_mode"] != "sync" {
		t.Fatalf("ingest %+v", reqs)
	}
}

func TestRetainDBWriteQueueCrashRecoveryReplaysPending(t *testing.T) {
	rdbHome(t)
	dbPath := filepath.Join(t.TempDir(), "recovery_test.db")
	failing := newRDBServer(t, func(r rdbRequest) (int, any) { return 500, map[string]any{"message": "fail"} })
	q1, err := newRetainDBWriteQueue(context.Background(), nil, newRetainDBClient("k", failing.URL, "p"), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.enqueue("user1", "sess1", []any{map[string]any{"role": "user", "content": "lost turn"}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the ingest error to be recorded", func() bool {
		rows := pendingRows(t, dbPath)
		return len(rows) == 1 && rows[0].Valid && rows[0].String != ""
	})
	q1.close()
	ok := newRDBServer(t, func(r rdbRequest) (int, any) { return 200, map[string]any{"status": "ok"} })
	q2, err := newRetainDBWriteQueue(context.Background(), nil, newRetainDBClient("k", ok.URL, "p"), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the replay", func() bool { return len(ok.seen()) > 0 })
	q2.close()
	reqs := ok.seen()
	if len(reqs) != 1 || reqs[0].body["user_id"] != "user1" {
		t.Fatalf("replayed %+v", reqs)
	}
	msgs, _ := reqs[0].body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "lost turn" {
		t.Fatalf("replayed messages %v", reqs[0].body["messages"])
	}
	if rows := pendingRows(t, dbPath); len(rows) != 0 {
		t.Fatalf("replayed row kept %v", rows)
	}
}

func overlay(t *testing.T, profile, query any, local []string) string {
	t.Helper()
	got, err := buildRetainDBOverlay(profile, query, local)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRetainDBBuildOverlay(t *testing.T) {
	if got := overlay(t, map[string]any{}, map[string]any{}, nil); got != "" {
		t.Fatalf("empty %q", got)
	}
	if got := overlay(t, map[string]any{"memories": []any{map[string]any{"content": "User likes Python"}}}, map[string]any{}, nil); got !=
		"[RetainDB Context]\nProfile:\n- User likes Python\nRelevant memories:\n- None" {
		t.Fatalf("profile %q", got)
	}
	if got := overlay(t, nil, map[string]any{"results": []any{map[string]any{"content": "Previous discussion about Rust"}}}, nil); !strings.Contains(got, "- Previous discussion about Rust") {
		t.Fatalf("query %q", got)
	}
	dup := overlay(t, map[string]any{"memories": []any{map[string]any{"content": "User likes Python"}}},
		map[string]any{"results": []any{map[string]any{"content": "user  likes python!"}}}, nil)
	if strings.Count(dup, "ikes") != 1 {
		t.Fatalf("dedupe %q", dup)
	}
	if got := overlay(t, map[string]any{"memories": []any{map[string]any{"content": "Already known fact"}}}, map[string]any{}, []string{"Already known fact"}); got != "" {
		t.Fatalf("local filter %q", got)
	}
	var many []any
	for i := 0; i < 10; i++ {
		many = append(many, map[string]any{"content": "Fact " + itoa(i)})
	}
	five := overlay(t, map[string]any{"memories": many}, map[string]any{}, nil)
	if !strings.Contains(five, "Fact 0") || !strings.Contains(five, "Fact 4") || strings.Contains(five, "Fact 5") {
		t.Fatalf("five %q", five)
	}
	if got := overlay(t, map[string]any{"memories": []any{map[string]any{"content": nil}, nil, map[string]any{"content": "Real fact"}}}, map[string]any{}, nil); !strings.Contains(got, "Real fact") {
		t.Fatalf("none content %q", got)
	}
	long := overlay(t, map[string]any{"memories": []any{map[string]any{"content": strings.Repeat("x", 500)}}}, map[string]any{}, nil)
	for _, line := range strings.Split(long, "\n") {
		if strings.HasPrefix(line, "- ") && len(line) > 322 {
			t.Fatalf("line of %d characters", len(line))
		}
	}
	if _, err := buildRetainDBOverlay([]any{"x"}, nil, nil); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("list profile error %v", err)
	}
	if _, err := buildRetainDBOverlay(map[string]any{"memories": []any{"text"}}, nil, nil); err == nil || err.Error() != "'str' object has no attribute 'get'" {
		t.Fatalf("string item error %v", err)
	}
}

func TestRetainDBIsAvailableWithoutKey(t *testing.T) {
	rdbHome(t)
	ctx := context.Background()
	if NewRetainDBProvider(nil).Available(ctx) {
		t.Fatal("available without a key")
	}
	t.Setenv("RETAINDB_API_KEY", "rdb-test-key")
	if !NewRetainDBProvider(nil).Available(ctx) {
		t.Fatal("unavailable with a key")
	}
	scoped := provider.WithSecretScope(provider.WithMultiplexContext(ctx, true), map[string]string{"OTHER": "x"}, "")
	if NewRetainDBProvider(nil).Available(scoped) {
		t.Fatal("a multiplexed profile without its own key used the process key")
	}
}

func TestRetainDBHandleToolCallNotInitialized(t *testing.T) {
	out, err := NewRetainDBProvider(nil).HandleToolCall(context.Background(), "retaindb_profile", map[string]any{}, ToolCallOptions{})
	var m map[string]any
	if err != nil || json.Unmarshal([]byte(out), &m) != nil || m["error"] != "RetainDB not initialized" {
		t.Fatalf("result %q %v", out, err)
	}
}

func TestRetainDBQueuePrefetchSkipsWithoutClient(t *testing.T) {
	p := NewRetainDBProvider(nil)
	if err := p.QueuePrefetch(context.Background(), "test", ""); err != nil {
		t.Fatal(err)
	}
	if p.prefetchThreads != nil {
		t.Fatal("threads spawned without a client")
	}
}

func TestRetainDBPrefetchDoesNotSpawnWhenPreviousBatchIsAlive(t *testing.T) {
	rdbHome(t)
	p := NewRetainDBProvider(nil)
	p.client = newRetainDBClient("k", "http://127.0.0.1:1", "p")
	release := make(chan struct{})
	previous := SpawnContextThread(context.Background(), "still-running", func(context.Context) { <-release })
	previous.Start()
	defer close(release)
	p.prefetchThreads = []*ContextThread{previous}
	if err := p.QueuePrefetch(context.Background(), "query", ""); err != nil {
		t.Fatal(err)
	}
	if len(p.prefetchThreads) != 1 || p.prefetchThreads[0] != previous {
		t.Fatalf("threads replaced %v", p.prefetchThreads)
	}
}

func TestRetainDBPrefetchReturnsEmptyWhenNothingCached(t *testing.T) {
	h := rdbHome(t)
	t.Setenv("RETAINDB_API_KEY", "rdb-test-key")
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "test-session", InitOptions{Home: h}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	if got, err := p.Prefetch(context.Background(), "test", ""); got != "" || err != nil {
		t.Fatalf("prefetch %q %v", got, err)
	}
}

func TestRetainDBPrefetchConsumesBackgroundResults(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		switch {
		case r.path == "/v1/context/query":
			return 200, map[string]any{"results": []any{map[string]any{"content": "Deploys on Fridays"}}}
		case strings.HasSuffix(r.path, "/ask"):
			return 200, map[string]any{"answer": "Prefers small PRs"}
		case strings.HasSuffix(r.path, "/model"):
			return 200, map[string]any{"memory_count": 3, "persona": "Careful", "persistent_instructions": []any{"Test first", "Be brief"}}
		}
		return 200, map[string]any{"memories": []any{}}
	})
	p := NewRetainDBProvider(nil)
	p.client = newRetainDBClient("k", srv.URL, "proj")
	if err := p.QueuePrefetch(context.Background(), "what is my deploy day", ""); err != nil {
		t.Fatal(err)
	}
	for _, th := range p.prefetchThreads {
		th.Join(5 * time.Second)
	}
	got, err := p.Prefetch(context.Background(), "", "")
	want := "[RetainDB Context]\nProfile:\n- None\nRelevant memories:\n- Deploys on Fridays\n\n" +
		"[RetainDB User Synthesis]\nPrefers small PRs\n\n" +
		"[RetainDB Agent Self-Model]\nPersona: Careful\nInstructions:\n- Test first\n- Be brief"
	if err != nil || got != want {
		t.Fatalf("prefetch %q %v", got, err)
	}
	if again, _ := p.Prefetch(context.Background(), "", ""); again != "" {
		t.Fatalf("cache not consumed %q", again)
	}
	for _, r := range srv.seen() {
		if strings.HasSuffix(r.path, "/ask") && r.body["reasoning_level"] != "low" {
			t.Fatalf("reasoning level %v", r.body["reasoning_level"])
		}
	}
}

// Provider with a client pointed at the fake server and its home bound to the given environment
func rdbProvider(t *testing.T, env *tools.Env, srv *rdbServer) *RetainDBProvider {
	t.Helper()
	p := NewRetainDBProvider(env)
	p.client = newRetainDBClient("k", srv.URL, "p")
	return p
}

func TestRetainDBUploadFileRejectsCredentialStore(t *testing.T) {
	h := rdbHome(t)
	auth := filepath.Join(h, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"OPENAI_API_KEY":"sk-test-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newRDBServer(t, nil)
	p := rdbProvider(t, &tools.Env{Home: h}, srv)
	got, err := p.dispatch(context.Background(), p.client, "retaindb_upload_file", map[string]any{"local_path": auth})
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := got.(map[string]any)["error"].(string)
	if !strings.Contains(msg, "credential store") {
		t.Fatalf("result %v", got)
	}
	if len(srv.seen()) != 0 {
		t.Fatal("credential store uploaded")
	}
}

func TestRetainDBUploadFileAllowsRegularFile(t *testing.T) {
	h := rdbHome(t)
	note := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(note, []byte("# Note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var parts map[string]string
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		if r.path == "/v1/files" && r.method == "POST" {
			_, params, _ := mime.ParseMediaType(r.header.Get("Content-Type"))
			reader := multipart.NewReader(strings.NewReader(string(r.raw)), params["boundary"])
			mu.Lock()
			parts = map[string]string{}
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				key := part.FormName()
				if part.FileName() != "" {
					key = "file:" + part.FileName() + ":" + part.Header.Get("Content-Type")
				}
				parts[key] = string(data)
			}
			mu.Unlock()
			return 200, map[string]any{"file": map[string]any{"id": "file-1", "name": "note.md"}}
		}
		return 200, map[string]any{"ingested": true}
	})
	p := rdbProvider(t, &tools.Env{Home: h}, srv)
	got, err := p.dispatch(context.Background(), p.client, "retaindb_upload_file", map[string]any{"local_path": note, "ingest": true})
	if err != nil {
		t.Fatal(err)
	}
	result := got.(map[string]any)
	if result["file"].(map[string]any)["id"] != "file-1" || result["ingest"].(map[string]any)["ingested"] != true {
		t.Fatalf("result %v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	fileBody := ""
	for k, v := range parts {
		if strings.HasPrefix(k, "file:note.md:") {
			fileBody = v
		}
	}
	if fileBody != "# Note\n" || parts["path"] != "/note.md" || parts["scope"] != "PROJECT" {
		t.Fatalf("multipart %v", parts)
	}
	reqs := srv.seen()
	if reqs[1].path != "/v1/files/file-1/ingest" || reqs[1].body["user_id"] != "default" || reqs[1].body["agent_id"] != "icebath" {
		t.Fatalf("ingest %+v", reqs[1])
	}
	missing, _ := p.dispatch(context.Background(), p.client, "retaindb_upload_file", map[string]any{"local_path": filepath.Join(h, "nope.txt")})
	if missing.(map[string]any)["error"] != "File not found: "+filepath.Join(h, "nope.txt") {
		t.Fatalf("missing %v", missing)
	}
}

func TestRetainDBToolDispatch(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		switch {
		case r.path == "/v1/files/f1" && r.method == "GET":
			return 200, map[string]any{"file": map[string]any{"name": "report.pdf", "mime_type": "application/pdf", "rdb_uri": "rdb://f1"}}
		case r.path == "/v1/files/f2" && r.method == "GET":
			return 200, map[string]any{"file": map[string]any{"name": "notes", "mime_type": "Text/Plain"}}
		case strings.HasSuffix(r.path, "/content"):
			return 200, "hello text"
		}
		return 200, map[string]any{"ok": true}
	})
	p := rdbProvider(t, nil, srv)
	ctx := context.Background()
	call := func(tool string, args map[string]any) map[string]any {
		out, err := p.HandleToolCall(ctx, tool, args, ToolCallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s returned %q", tool, out)
		}
		return m
	}
	if got := call("retaindb_nope", nil); got["error"] != "Unknown tool: retaindb_nope" {
		t.Fatalf("unknown %v", got)
	}
	if got := call("retaindb_search", map[string]any{}); got["error"] != "query is required" {
		t.Fatalf("missing query %v", got)
	}
	call("retaindb_search", map[string]any{"query": "q", "top_k": "50"})
	call("retaindb_remember", map[string]any{"content": "fact", "importance": "0.9", "memory_type": "goal"})
	call("retaindb_list_files", map[string]any{"limit": 3.0})
	if got := call("retaindb_search", map[string]any{"query": "q", "top_k": "x"}); got["error"] != "invalid literal for int() with base 10: 'x'" {
		t.Fatalf("bad top_k %v", got)
	}
	if got := call("retaindb_forget", map[string]any{"memory_id": 7}); got["error"] != "quote() doesn't support 'int' objects" {
		t.Fatalf("bad memory id %v", got)
	}
	binary := call("retaindb_read_file", map[string]any{"file_id": "f1"})
	if binary["content"] != nil || binary["note"] != "Binary file — use retaindb_ingest_file to extract text into memory." || binary["rdb_uri"] != "rdb://f1" {
		t.Fatalf("binary %v", binary)
	}
	text := call("retaindb_read_file", map[string]any{"file_id": "f2"})
	if text["content"] != "hello text" || text["truncated"] != false {
		t.Fatalf("text %v", text)
	}
	reqs := srv.seen()
	byPath := map[string]rdbRequest{}
	for _, r := range reqs {
		byPath[r.method+" "+r.path] = r
	}
	if s := byPath["POST /v1/memory/search"]; s.body["top_k"] != 20.0 || s.body["include_pending"] != true {
		t.Fatalf("search body %v", s.body)
	}
	if m := byPath["POST /v1/memory"]; m.body["importance"] != 0.9 || m.body["memory_type"] != "goal" || m.body["write_mode"] != "sync" {
		t.Fatalf("remember body %v", m.body)
	}
	if l := byPath["GET /v1/files"]; l.query != "limit=3" {
		t.Fatalf("list query %q", l.query)
	}
}

func TestRetainDBInitializeReadsRealDashboardConfigFile(t *testing.T) {
	h := rdbHome(t)
	writeConfig(t, h, "memory:\n  provider: retaindb\n  retaindb:\n    base_url: https://retaindb.saved.example/\n    project: dashboard-project\n")
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "sess-1", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	if p.client.baseURL != "https://retaindb.saved.example" || p.client.project != "dashboard-project" {
		t.Fatalf("client %+v", p.client)
	}
}

func TestRetainDBInitializeEnvOverridesConfigYAML(t *testing.T) {
	h := rdbHome(t)
	t.Setenv("RETAINDB_BASE_URL", "https://env.example.com")
	writeConfig(t, h, "memory:\n  retaindb:\n    base_url: https://cfg.example.com\n    project: cfg-project\n")
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "sess-1", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	if p.client.baseURL != "https://env.example.com" || p.client.project != "cfg-project" {
		t.Fatalf("client %+v", p.client)
	}
}

func TestRetainDBInitializeCombinesScopedSecretWithDashboardConfig(t *testing.T) {
	h := rdbHome(t)
	t.Setenv("RETAINDB_API_KEY", "env-other-profile")
	writeConfig(t, h, "memory:\n  retaindb:\n    base_url: https://dashboard.example.com/\n    project: dashboard-project\n")
	ctx := provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), map[string]string{"RETAINDB_API_KEY": "scoped-key"}, "")
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(ctx, "sess-1", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	if p.client.apiKey != "scoped-key" || p.client.baseURL != "https://dashboard.example.com" || p.client.project != "dashboard-project" {
		t.Fatalf("client %+v", p.client)
	}
}

func TestRetainDBInitializeFallsBackToDefaultBaseURL(t *testing.T) {
	rdbHome(t)
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "sess-1", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	if p.client.baseURL != retaindbDefaultBaseURL || p.client.project != "default" {
		t.Fatalf("client %+v", p.client)
	}
}

func TestRetainDBInitializeSeedsIdentityFromSoul(t *testing.T) {
	h := rdbHome(t)
	srv := newRDBServer(t, nil)
	t.Setenv("RETAINDB_BASE_URL", srv.URL)
	if err := os.WriteFile(filepath.Join(h, "SOUL.md"), []byte("\xef\xbb\xbf  I am careful.  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "s", InitOptions{Home: h, AgentID: "agent-7"}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	waitUntil(t, "the soul seed", func() bool { return len(srv.seen()) > 0 })
	r := srv.seen()[0]
	if r.path != "/v1/memory/agent/agent-7/seed" || r.body["content"] != "I am careful." || r.body["source"] != "soul_md" || r.body["project"] != "default" {
		t.Fatalf("seed %+v", r)
	}
}

func TestRetainDBMirrorsAddActionByTarget(t *testing.T) {
	rdbHome(t)
	srv := newRDBServer(t, nil)
	p := rdbProvider(t, nil, srv)
	ctx := context.Background()
	if err := p.OnMemoryWrite(ctx, "add", "user", "User prefers dark mode", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.OnMemoryWrite(ctx, "remove", "user", "something", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.OnMemoryWrite(ctx, "add", "memory", "Some env fact", nil); err != nil {
		t.Fatal(err)
	}
	reqs := srv.seen()
	if len(reqs) != 2 || reqs[0].body["memory_type"] != "preference" || reqs[1].body["memory_type"] != "factual" {
		t.Fatalf("mirrors %+v", reqs)
	}
}

func TestRetainDBSyncTurnQueuesBothSides(t *testing.T) {
	h := rdbHome(t)
	srv := newRDBServer(t, nil)
	t.Setenv("RETAINDB_BASE_URL", srv.URL)
	t.Setenv("RETAINDB_API_KEY", "k")
	p := NewRetainDBProvider(nil)
	ctx := context.Background()
	if err := p.Initialize(ctx, "init-session", InitOptions{Home: h, UserID: "u9"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncTurn(ctx, "", "ignored", SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncTurn(ctx, "hello", "hi there", SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	soul, err := os.ReadFile(filepath.Join(h, "SOUL.md"))
	if err != nil {
		t.Fatal("config load did not seed the default SOUL.md", err)
	}
	var reqs, seeds []rdbRequest
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		reqs, seeds = nil, nil
		for _, r := range srv.seen() {
			if r.path == "/v1/memory/agent/icebath/seed" {
				seeds = append(seeds, r)
			} else {
				reqs = append(reqs, r)
			}
		}
		if len(seeds) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(seeds) != 1 || seeds[0].body["content"] != strings.TrimSpace(string(soul)) || seeds[0].body["source"] != "soul_md" {
		t.Fatalf("soul seed %+v", seeds)
	}
	if len(reqs) != 1 || reqs[0].body["session_id"] != "init-session" || reqs[0].body["user_id"] != "u9" {
		t.Fatalf("ingest %+v", reqs)
	}
	msgs := reqs[0].body["messages"].([]any)
	first, second := msgs[0].(map[string]any), msgs[1].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello" || second["role"] != "assistant" || second["timestamp"] != first["timestamp"] {
		t.Fatalf("messages %v", msgs)
	}
	if !strings.HasSuffix(first["timestamp"].(string), "+00:00") {
		t.Fatalf("timestamp %v", first["timestamp"])
	}
}

// Launch root plus alpha and beta profiles with a recording RetainDB server reachable from both
func rdbHomes(t *testing.T) (string, string, *rdbServer) {
	t.Helper()
	alpha, beta := alphaBetaHomes(t)
	unsetRetainDBEnv(t)
	srv := newRDBServer(t, nil)
	t.Setenv("RETAINDB_API_KEY", "rdb-key")
	t.Setenv("RETAINDB_BASE_URL", srv.URL)
	return alpha, beta, srv
}

// Launch root plus two named profiles alpha and beta, neither the launch profile
func alphaBetaHomes(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	alpha, beta := filepath.Join(root, "profiles", "alpha"), filepath.Join(root, "profiles", "beta")
	for _, h := range []string{alpha, beta} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	return alpha, beta
}

// Each profile's turns on their own goroutine and scope, a tool call, a turn sync and a write mirror per round
func runInterleaved(t *testing.T, managers map[string]*MemoryManager, tool string, argKey string, rounds int) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan string, 64)
	for h, m := range managers {
		wg.Add(1)
		go func(h string, m *MemoryManager) {
			defer wg.Done()
			ctx := home.WithOverride(context.Background(), h)
			name := filepath.Base(h)
			<-start
			for i := 0; i < rounds; i++ {
				args := map[string]any{argKey: name + "-tool-" + itoa(i)}
				var result map[string]any
				if err := json.Unmarshal([]byte(m.HandleToolCall(ctx, tool, args, "")), &result); err != nil {
					errs <- err.Error()
					return
				}
				if _, bad := result["error"]; bad {
					errs <- name + " tool error " + pyStr(result["error"])
					return
				}
				m.SyncAll(ctx, name+"-turn-"+itoa(i)+" what did we decide", "the plan", SyncOptions{SessionID: "s"})
				m.OnMemoryWrite(ctx, "add", "memory", name+"-write-"+itoa(i), nil)
			}
			if !m.FlushPending(10 * time.Second) {
				errs <- name + " flush timed out"
			}
		}(h, m)
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func itoa(i int) string { return pyStr(i) }

// Polls until cond holds or the deadline passes
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRetainDBBindsTheHomeInitializeReceives(t *testing.T) {
	alpha, _, _ := rdbHomes(t)
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(context.Background(), "s", InitOptions{Home: alpha}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	if _, err := os.Stat(filepath.Join(alpha, "retaindb_queue.db")); err != nil {
		t.Fatalf("queue not under the bound home %v", err)
	}
	if p.client.project != "icebath-alpha" {
		t.Fatalf("project %q", p.client.project)
	}
}

func TestRetainDBTwoProfilesInterleavingStayInTheirOwnProjects(t *testing.T) {
	alpha, beta, srv := rdbHomes(t)
	managers := map[string]*MemoryManager{}
	for _, h := range []string{alpha, beta} {
		ctx := home.WithOverride(context.Background(), h)
		m := NewMemoryManager(ctx, nil, nil)
		if err := m.AddProvider(ctx, NewRetainDBProvider(nil)); err != nil {
			t.Fatal(err)
		}
		m.InitializeAll(ctx, "s-"+filepath.Base(h), InitOptions{Platform: "api_server", Home: h})
		managers[h] = m
	}
	written := func() []rdbRequest {
		var out []rdbRequest
		for _, r := range srv.seen() {
			if r.path == "/v1/memory" || r.path == "/v1/memory/ingest/session" {
				out = append(out, r)
			}
		}
		return out
	}
	func() {
		defer func() {
			for _, m := range managers {
				m.ShutdownAll(context.Background())
			}
		}()
		runInterleaved(t, managers, "retaindb_remember", "content", 5)
		waitUntil(t, "every RetainDB write from both profiles", func() bool { return len(written()) >= 2*3*5 })
	}()
	for _, h := range []string{alpha, beta} {
		name := filepath.Base(h)
		kinds := map[string]bool{}
		for _, r := range written() {
			content := pyStr(r.body["content"])
			if r.path == "/v1/memory/ingest/session" {
				content = pyStr(r.body["messages"].([]any)[0].(map[string]any)["content"])
			}
			if !strings.Contains(content, name+"-") {
				continue
			}
			kinds[r.path] = true
			if r.body["project"] != "icebath-"+name {
				t.Fatalf("%s write landed in project %v", name, r.body["project"])
			}
		}
		if !kinds["/v1/memory"] || !kinds["/v1/memory/ingest/session"] || len(kinds) != 2 {
			t.Fatalf("%s writes %v", name, kinds)
		}
		if _, err := os.Stat(filepath.Join(h, "retaindb_queue.db")); err != nil {
			t.Fatal(err)
		}
	}
}

// Transport recording the home override each request ran under
type homeRecordingTransport struct {
	mu    sync.Mutex
	homes []string
	next  http.RoundTripper
}

func (rt *homeRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.homes = append(rt.homes, home.Override(r.Context()))
	rt.mu.Unlock()
	return rt.next.RoundTrip(r)
}

func TestRetainDBBackgroundThreadSeesSpawnerProfileHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "default"))
	profileHome := filepath.Join(tmp, "profiles", "b")
	srv := newRDBServer(t, func(r rdbRequest) (int, any) {
		if strings.HasSuffix(r.path, "/ask") {
			return 200, map[string]any{"answer": ""}
		}
		return 200, map[string]any{}
	})
	p := NewRetainDBProvider(nil)
	p.client = newRetainDBClient("k", srv.URL, "p")
	rt := &homeRecordingTransport{next: http.DefaultTransport}
	p.client.http = &http.Client{Transport: rt}
	ctx := home.WithOverride(context.Background(), profileHome)
	if err := p.QueuePrefetch(ctx, "what do you know", ""); err != nil {
		t.Fatal(err)
	}
	for _, th := range p.prefetchThreads {
		th.Join(10 * time.Second)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.homes) < 4 {
		t.Fatalf("requests %v", rt.homes)
	}
	for _, h := range rt.homes {
		if h != profileHome {
			t.Fatalf("background request resolved home %q", h)
		}
	}
}

func TestRetainDBSecondaryProfileIdentityNeverInheritsDefaultEnviron(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profB := filepath.Join(root, "profiles", "b")
	writeConfig(t, profB, "{}\n")
	t.Setenv("ICEBATH_HOME", profB)
	t.Setenv("RETAINDB_PROJECT", "proj-default")
	t.Setenv("RETAINDB_BASE_URL", "https://rdb.default")
	ctx := provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), map[string]string{"RETAINDB_API_KEY": "rdb-b"}, "")
	p := NewRetainDBProvider(nil)
	if err := p.Initialize(ctx, "s1", InitOptions{Home: profB}); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	if p.client.project != "icebath-b" || strings.Contains(p.client.baseURL, "default") || p.client.apiKey != "rdb-b" {
		t.Fatalf("client %+v", p.client)
	}
}

func TestRetainDBRegisteredBundled(t *testing.T) {
	r := FindProvider(context.Background(), nil, "retaindb")
	if r == nil || r.Source != SourceBundled {
		t.Fatalf("registration %+v", r)
	}
	p, err := r.New(context.Background(), nil, LoadOptions{})
	if err != nil || p.Name() != "retaindb" {
		t.Fatalf("factory %v %v", p, err)
	}
	if schemas, _ := p.ToolSchemas(context.Background()); len(schemas) != 10 {
		t.Fatalf("tool schemas %d", len(schemas))
	}
}

func TestRetainDBReasoningLevelAndISOFormat(t *testing.T) {
	if retaindbReasoningLevel(strings.Repeat("a", 119)) != "low" || retaindbReasoningLevel(strings.Repeat("a", 120)) != "medium" ||
		retaindbReasoningLevel(strings.Repeat("a", 400)) != "high" {
		t.Fatal("reasoning levels")
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if pyISOFormatUTC(at) != "2026-03-04T05:06:07+00:00" || pyISOFormatUTC(at.Add(1500*time.Microsecond)) != "2026-03-04T05:06:07.001500+00:00" {
		t.Fatal("isoformat")
	}
	if quoteAll("a b/c~_.-é") != "a%20b%2Fc~_.-%C3%A9" {
		t.Fatalf("quote %q", quoteAll("a b/c~_.-é"))
	}
}
