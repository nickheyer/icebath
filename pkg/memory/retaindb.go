package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const retaindbManifest = `name: retaindb
version: 1.0.0
description: "RetainDB — cloud memory API with hybrid search and 7 memory types."
requires_env:
  - RETAINDB_API_KEY
config_keys:
  - memory.retaindb  # base_url / project persisted by the dashboard
`

const retaindbDefaultBaseURL = "https://api.retaindb.com"

// Pause after a failed ingest and the wait on a previous prefetch batch, replaced in tests
var (
	retaindbRetryDelay   = 2 * time.Second
	retaindbPrefetchJoin = 2 * time.Second
)

// Separators os.path.basename splits on for this platform
var pathSeparators = string(os.PathSeparator) + "/"

var retaindbTextExts = []string{".txt", ".md", ".json", ".csv", ".yaml", ".yml", ".xml", ".html"}

// The memory.retaindb block from config, empty on error, the api key stays in scoped secrets
func loadRetainDBConfig(ctx context.Context, env *tools.Env) map[string]any {
	tree, err := registryConfig(ctx, env)
	if err != nil || tree == nil {
		return map[string]any{}
	}
	block, ok := tree.Get("memory.retaindb").(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(block))
	for k, v := range block {
		out[k] = v
	}
	return out
}

// Percent encodes every byte outside the unreserved set like quote with no safe characters
func quoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Runs fn and logs a failure at debug, nil and false on failure
func retaindbQuiet[T any](env *tools.Env, label string, fn func() (T, error)) (T, bool) {
	var v T
	err := callProvider(func() error {
		var err error
		v, err = fn()
		return err
	})
	if err != nil {
		envLog(env).Debug(fmt.Sprintf("RetainDB %s failed: %s", label, err))
		var zero T
		return zero, false
	}
	return v, true
}

func retaindbSchema(name, description string, properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": req}}
}

func retaindbProp(description, typ string, extra map[string]any) map[string]any {
	if typ == "" {
		typ = "string"
	}
	out := map[string]any{"type": typ}
	for k, v := range extra {
		out[k] = v
	}
	out["description"] = description
	return out
}

func retaindbSchemas() []map[string]any {
	p := retaindbProp
	return []map[string]any{
		retaindbSchema("retaindb_profile", "Get the user's stable profile — preferences, facts, and patterns recalled from long-term memory.", nil),
		retaindbSchema("retaindb_search", "Semantic search across stored memories. Returns ranked results with relevance scores.",
			map[string]any{"query": p("What to search for.", "", nil), "top_k": p("Max results (default: 8, max: 20).", "integer", nil)}, "query"),
		retaindbSchema("retaindb_context", "Synthesized context block — what matters most for the current task, pulled from long-term memory.",
			map[string]any{"query": p("Current task or question.", "", nil)}, "query"),
		retaindbSchema("retaindb_remember", "Persist an explicit fact, preference, or decision to long-term memory.",
			map[string]any{"content": p("The fact to remember.", "", nil),
				"memory_type": p("Category (default: factual).", "", map[string]any{"enum": []any{"factual", "preference", "goal", "instruction", "event", "opinion"}}),
				"importance":  p("Importance 0-1 (default: 0.7).", "number", nil)}, "content"),
		retaindbSchema("retaindb_forget", "Delete a specific memory by ID.", map[string]any{"memory_id": p("Memory ID to delete.", "", nil)}, "memory_id"),
		retaindbSchema("retaindb_upload_file", "Upload a file to the shared RetainDB file store. Returns an rdb:// URI any agent can reference.",
			map[string]any{"local_path": p("Local file path to upload.", "", nil), "remote_path": p("Destination path, e.g. /reports/q1.pdf", "", nil),
				"scope":  p("Access scope (default: PROJECT).", "", map[string]any{"enum": []any{"USER", "PROJECT", "ORG"}}),
				"ingest": p("Also extract memories from file after upload (default: false).", "boolean", nil)}, "local_path"),
		retaindbSchema("retaindb_list_files", "List files in the shared file store.",
			map[string]any{"prefix": p("Path prefix to filter by, e.g. /reports/", "", nil), "limit": p("Max results (default: 50).", "integer", nil)}),
		retaindbSchema("retaindb_read_file", "Read the text content of a stored file by its file ID.",
			map[string]any{"file_id": p("File ID returned from upload or list.", "", nil)}, "file_id"),
		retaindbSchema("retaindb_ingest_file", "Chunk, embed, and extract memories from a stored file. Makes its contents searchable.",
			map[string]any{"file_id": p("File ID to ingest.", "", nil)}, "file_id"),
		retaindbSchema("retaindb_delete_file", "Delete a stored file.", map[string]any{"file_id": p("File ID to delete.", "", nil)}, "file_id"),
	}
}

// One query parameter, insertion order kept on the wire
type queryParam struct {
	key   string
	value string
}

// Thin HTTP client over the RetainDB REST API
type retaindbClient struct {
	apiKey  string
	baseURL string
	project string
	http    *http.Client
}

var trailingSlashes = regexp.MustCompile(`/+$`)

func newRetainDBClient(apiKey, baseURL, project string) *retaindbClient {
	return &retaindbClient{apiKey: apiKey, baseURL: trailingSlashes.ReplaceAllString(baseURL, ""), project: project, http: &http.Client{}}
}

func (c *retaindbClient) headers(path string, jsonBody bool) map[string]string {
	token := strings.TrimSpace(strings.ReplaceAll(c.apiKey, "Bearer ", ""))
	h := map[string]string{"Authorization": "Bearer " + token, "x-sdk-runtime": "icebath-plugin"}
	if jsonBody {
		h["Content-Type"] = "application/json"
	}
	if strings.HasPrefix(path, "/v1/memory") || strings.HasPrefix(path, "/v1/context") {
		h["X-API-Key"] = token
	}
	return h
}

// One HTTP exchange with the body read, redirects followed
func (c *retaindbClient) do(ctx context.Context, method, path string, jsonBody bool, timeout time.Duration, params []queryParam, body io.Reader, contentType string) (*http.Response, []byte, error) {
	target := c.baseURL + path
	if len(params) > 0 {
		parts := make([]string, len(params))
		for i, p := range params {
			parts[i] = quotePlus(p.key) + "=" + quotePlus(p.value)
		}
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + strings.Join(parts, "&")
	}
	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, target, body)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range c.headers(path, jsonBody) {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}

// Form encoding of one query value, spaces as plus and every other reserved byte escaped
func quotePlus(s string) string {
	return strings.ReplaceAll(quoteAll(strings.ReplaceAll(s, " ", "\x00")), "%00", "+")
}

// Decoded JSON body, the text when the body is not JSON
func decodeBody(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return decodeReplace(data), false
	}
	if _, err := dec.Token(); err != io.EOF {
		return decodeReplace(data), false
	}
	return v, true
}

// JSON request whose non 2xx response raises with the server message
func (c *retaindbClient) request(ctx context.Context, method, path string, params []queryParam, jsonBody map[string]any, timeout time.Duration) (any, error) {
	method = strings.ToUpper(method)
	var body io.Reader
	if jsonBody != nil && method != "GET" && method != "DELETE" {
		encoded, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	resp, data, err := c.do(ctx, method, path, true, timeout, params, body, "")
	if err != nil {
		return nil, err
	}
	payload, _ := decodeBody(data)
	if resp.StatusCode >= 400 {
		msg := ""
		if m, ok := payload.(map[string]any); ok {
			if pyTruthy(m["message"]) {
				msg = pyStr(m["message"])
			} else if pyTruthy(m["error"]) {
				msg = pyStr(m["error"])
			}
		}
		if msg == "" {
			msg = pyStr(payload)
		}
		return nil, fmt.Errorf("RetainDB %s %s failed (%d): %s", method, path, resp.StatusCode, msg)
	}
	return payload, nil
}

// Error of a non 2xx raw response in the requests raise_for_status wording
func rawStatusError(resp *http.Response) error {
	reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	kind := ""
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		kind = "Client"
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		kind = "Server"
	default:
		return nil
	}
	return fmt.Errorf("%d %s Error: %s for url: %s", resp.StatusCode, kind, reason, resp.Request.URL.String())
}

// Current route, retried through the legacy route on any failure
func withFallback(primary, fallback func() (any, error)) (any, error) {
	v, err := primary()
	if err != nil {
		return fallback()
	}
	return v, nil
}

func (c *retaindbClient) scoped(userID, sessionID string, extra map[string]any) map[string]any {
	out := map[string]any{"project": c.project, "user_id": userID, "session_id": sessionID}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (c *retaindbClient) queryContext(ctx context.Context, userID, sessionID string, query any) (any, error) {
	return c.request(ctx, "POST", "/v1/context/query", nil, c.scoped(userID, sessionID, map[string]any{"query": query, "include_memories": true, "max_tokens": 1200}), 8*time.Second)
}

func (c *retaindbClient) search(ctx context.Context, userID, sessionID string, query any, topK int) (any, error) {
	return c.request(ctx, "POST", "/v1/memory/search", nil, c.scoped(userID, sessionID, map[string]any{"query": query, "top_k": topK, "include_pending": true}), 8*time.Second)
}

func (c *retaindbClient) getProfile(ctx context.Context, userID string) (any, error) {
	return withFallback(
		func() (any, error) {
			return c.request(ctx, "GET", "/v1/memory/profile/"+quoteAll(userID), []queryParam{{"project", c.project}, {"include_pending", "true"}}, nil, 8*time.Second)
		},
		func() (any, error) {
			return c.request(ctx, "GET", "/v1/memories", []queryParam{{"project", c.project}, {"user_id", userID}, {"limit", "200"}}, nil, 8*time.Second)
		})
}

func (c *retaindbClient) addMemory(ctx context.Context, userID, sessionID string, content, memoryType any, importance float64) (any, error) {
	body := c.scoped(userID, sessionID, map[string]any{"content": content, "memory_type": memoryType, "importance": importance})
	return withFallback(
		func() (any, error) {
			sync := map[string]any{}
			for k, v := range body {
				sync[k] = v
			}
			sync["write_mode"] = "sync"
			return c.request(ctx, "POST", "/v1/memory", nil, sync, 5*time.Second)
		},
		func() (any, error) { return c.request(ctx, "POST", "/v1/memories", nil, body, 5*time.Second) })
}

func (c *retaindbClient) deleteMemory(ctx context.Context, memoryID string) (any, error) {
	return withFallback(
		func() (any, error) {
			return c.request(ctx, "DELETE", "/v1/memory/"+quoteAll(memoryID), nil, nil, 5*time.Second)
		},
		func() (any, error) {
			return c.request(ctx, "DELETE", "/v1/memories/"+quoteAll(memoryID), nil, nil, 5*time.Second)
		})
}

func (c *retaindbClient) ingestSession(ctx context.Context, userID, sessionID string, messages []any) (any, error) {
	return c.request(ctx, "POST", "/v1/memory/ingest/session", nil, c.scoped(userID, sessionID, map[string]any{"messages": messages, "write_mode": "sync"}), 15*time.Second)
}

func (c *retaindbClient) askUser(ctx context.Context, userID, query, reasoningLevel string) (any, error) {
	return c.request(ctx, "POST", "/v1/memory/profile/"+quoteAll(userID)+"/ask", nil, map[string]any{"project": c.project, "query": query, "reasoning_level": reasoningLevel}, 8*time.Second)
}

func (c *retaindbClient) getAgentModel(ctx context.Context, agentID string) (any, error) {
	return c.request(ctx, "GET", "/v1/memory/agent/"+quoteAll(agentID)+"/model", []queryParam{{"project", c.project}}, nil, 4*time.Second)
}

func (c *retaindbClient) seedAgentIdentity(ctx context.Context, agentID, content, source string) (any, error) {
	return c.request(ctx, "POST", "/v1/memory/agent/"+quoteAll(agentID)+"/seed", nil, map[string]any{"project": c.project, "content": content, "source": source}, 20*time.Second)
}

// Multipart upload into the shared file store, the decoded JSON response returned
func (c *retaindbClient) uploadFile(ctx context.Context, data []byte, filename, remotePath, mimeType, scope string) (any, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("path", remotePath); err != nil {
		return nil, err
	}
	if err := w.WriteField("scope", strings.ToUpper(scope)); err != nil {
		return nil, err
	}
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename="%s"`, strings.ReplaceAll(filename, `"`, `%22`))}
	header["Content-Type"] = []string{mimeType}
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	resp, body, err := c.do(ctx, "POST", "/v1/files", false, 30*time.Second, nil, &buf, w.FormDataContentType())
	if err != nil {
		return nil, err
	}
	if err := rawStatusError(resp); err != nil {
		return nil, err
	}
	payload, ok := decodeBody(body)
	if !ok {
		return nil, fmt.Errorf("Expecting value: line 1 column 1 (char 0)")
	}
	return payload, nil
}

func (c *retaindbClient) listFiles(ctx context.Context, prefix any, limit int) (any, error) {
	params := []queryParam{{"limit", strconv.Itoa(limit)}}
	if pyTruthy(prefix) {
		params = append(params, queryParam{"prefix", pyStr(prefix)})
	}
	return c.request(ctx, "GET", "/v1/files", params, nil, 8*time.Second)
}

func (c *retaindbClient) getFile(ctx context.Context, fileID string) (any, error) {
	return c.request(ctx, "GET", "/v1/files/"+quoteAll(fileID), nil, nil, 8*time.Second)
}

func (c *retaindbClient) readFileContent(ctx context.Context, fileID string) ([]byte, error) {
	resp, body, err := c.do(ctx, "GET", "/v1/files/"+quoteAll(fileID)+"/content", false, 30*time.Second, nil, nil, "")
	if err != nil {
		return nil, err
	}
	if err := rawStatusError(resp); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *retaindbClient) ingestFile(ctx context.Context, fileID, userID, agentID string) (any, error) {
	body := map[string]any{}
	if userID != "" {
		body["user_id"] = userID
	}
	if agentID != "" {
		body["agent_id"] = agentID
	}
	return c.request(ctx, "POST", "/v1/files/"+quoteAll(fileID)+"/ingest", nil, body, 60*time.Second)
}

func (c *retaindbClient) deleteFile(ctx context.Context, fileID string) (any, error) {
	return c.request(ctx, "DELETE", "/v1/files/"+quoteAll(fileID), nil, nil, 5*time.Second)
}

// Python datetime isoformat of a UTC instant, microseconds only when non zero
func pyISOFormatUTC(t time.Time) string {
	t = t.UTC()
	if us := t.Nanosecond() / 1000; us != 0 {
		return t.Format("2006-01-02T15:04:05") + fmt.Sprintf(".%06d", us) + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05") + "+00:00"
}

// One queued ingest row
type retaindbRow struct {
	id        int64
	userID    string
	sessionID string
	messages  []any
	shutdown  bool
}

// Unbounded FIFO of rows for the writer
type rowFIFO struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []retaindbRow
}

func newRowFIFO() *rowFIFO {
	f := &rowFIFO{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *rowFIFO) put(r retaindbRow) {
	f.mu.Lock()
	f.items = append(f.items, r)
	f.cond.Signal()
	f.mu.Unlock()
}

func (f *rowFIFO) get() retaindbRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.items) == 0 {
		f.cond.Wait()
	}
	r := f.items[0]
	f.items = f.items[1:]
	return r
}

// SQLite backed write behind queue whose pending rows replay on startup
type retaindbWriteQueue struct {
	env      *tools.Env
	client   *retaindbClient
	db       *sql.DB
	queue    *rowFIFO
	thread   *ContextThread
	mu       sync.Mutex
	shutdown bool
}

// Escapes the bytes a sqlite file URI would read as query or fragment syntax
var sqliteURIPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func newRetainDBWriteQueue(ctx context.Context, env *tools.Env, client *retaindbClient, dbPath string) (*retaindbWriteQueue, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o777); err != nil {
		return nil, pyOSError(err)
	}
	db, err := sql.Open("sqlite", "file:"+sqliteURIPath.Replace(dbPath)+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, err
	}
	q := &retaindbWriteQueue{env: env, client: client, db: db, queue: newRowFIFO()}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS pending (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT, " +
		"session_id TEXT, messages_json TEXT, created_at TEXT, last_error TEXT)"); err != nil {
		db.Close()
		return nil, err
	}
	q.thread = SpawnContextThread(ctx, "retaindb-writer", q.loop)
	q.thread.Start()
	rows, err := db.Query("SELECT id, user_id, session_id, messages_json FROM pending ORDER BY id ASC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id                int64
			userID, sessionID sql.NullString
			messagesJSON      sql.NullString
		)
		if err := rows.Scan(&id, &userID, &sessionID, &messagesJSON); err != nil {
			return nil, err
		}
		var msgs []any
		if err := json.Unmarshal([]byte(messagesJSON.String), &msgs); err != nil {
			return nil, err
		}
		q.queue.put(retaindbRow{id: id, userID: userID.String, sessionID: sessionID.String, messages: msgs})
	}
	return q, rows.Err()
}

// Persists the turn and queues it for ingest, nothing once shut down
func (q *retaindbWriteQueue) enqueue(userID, sessionID string, messages []any) error {
	now := pyISOFormatUTC(time.Now())
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return nil
	}
	encoded := tools.JSON(messages)
	res, err := q.db.Exec("INSERT INTO pending (user_id, session_id, messages_json, created_at) VALUES (?,?,?,?)",
		userID, sessionID, encoded, now)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	q.queue.put(retaindbRow{id: id, userID: userID, sessionID: sessionID, messages: messages})
	return nil
}

// Ingests one row, deleting it on success and recording the error for the next startup replay on failure
func (q *retaindbWriteQueue) flushRow(ctx context.Context, row retaindbRow) error {
	if _, err := q.client.ingestSession(ctx, row.userID, row.sessionID, row.messages); err != nil {
		envLog(q.env).Warn(fmt.Sprintf("RetainDB ingest failed (will retry): %s", err))
		if _, uerr := q.db.Exec("UPDATE pending SET last_error = ? WHERE id = ?", err.Error(), row.id); uerr != nil {
			return uerr
		}
		time.Sleep(retaindbRetryDelay)
		return nil
	}
	_, err := q.db.Exec("DELETE FROM pending WHERE id = ?", row.id)
	return err
}

func (q *retaindbWriteQueue) loop(ctx context.Context) {
	for {
		row := q.queue.get()
		if row.shutdown {
			return
		}
		if err := callProvider(func() error { return q.flushRow(ctx, row) }); err != nil {
			envLog(q.env).Error(fmt.Sprintf("RetainDB writer error: %s", err))
		}
	}
}

// Stops the writer after the queued rows, waiting up to ten seconds before closing the database
func (q *retaindbWriteQueue) close() {
	q.mu.Lock()
	if q.shutdown {
		q.mu.Unlock()
		return
	}
	q.shutdown = true
	q.queue.put(retaindbRow{shutdown: true})
	q.mu.Unlock()
	q.thread.Join(10 * time.Second)
	if !q.thread.Alive() {
		q.db.Close()
	}
}

var (
	whitespaceRun = regexp.MustCompile(`[` + pySpaceClass + `]+`)
	nonAlnumSpace = regexp.MustCompile(`[^a-z0-9 ]`)
	retaindbNoGet = func(v any) error { return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v)) }
)

func retaindbCompact(v any) string {
	s := ""
	if pyTruthy(v) {
		s = pyStr(v)
	}
	return runePrefix(pyStrip(whitespaceRun.ReplaceAllString(s, " ")), 320)
}

func retaindbNorm(v any) string {
	return nonAlnumSpace.ReplaceAllString(pyLower(retaindbCompact(v)), "")
}

// Mapping a value stands in for, an empty one for falsy values and an error for any other non mapping
func mappingOrEmpty(v any) (map[string]any, error) {
	if !pyTruthy(v) {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, retaindbNoGet(v)
	}
	return m, nil
}

// Profile and query memories, five each compacted and deduplicated against each other and local entries
func buildRetainDBOverlay(profile, queryResult any, localEntries []string) (string, error) {
	seen := map[string]bool{}
	for _, e := range localEntries {
		if n := retaindbNorm(e); n != "" {
			seen[n] = true
		}
	}
	dedupe := func(items any) ([]string, error) {
		var list []any
		if pyTruthy(items) {
			switch x := items.(type) {
			case []any:
				list = x
			case string, map[string]any:
				return nil, retaindbNoGet("")
			default:
				return nil, fmt.Errorf("'%s' object is not iterable", pyTypeName(items))
			}
		}
		if len(list) > 5 {
			list = list[:5]
		}
		var out []string
		for _, m := range list {
			mm, err := mappingOrEmpty(m)
			if err != nil {
				return nil, err
			}
			c := retaindbCompact(mm["content"])
			if c != "" && !seen[retaindbNorm(c)] {
				seen[retaindbNorm(c)] = true
				out = append(out, c)
			}
		}
		return out, nil
	}
	pm, err := mappingOrEmpty(profile)
	if err != nil {
		return "", err
	}
	profileItems, err := dedupe(pm["memories"])
	if err != nil {
		return "", err
	}
	qm, err := mappingOrEmpty(queryResult)
	if err != nil {
		return "", err
	}
	queryItems, err := dedupe(qm["results"])
	if err != nil {
		return "", err
	}
	if len(profileItems) == 0 && len(queryItems) == 0 {
		return "", nil
	}
	lines := []string{"[RetainDB Context]", "Profile:"}
	lines = append(lines, bulletsOrNone(profileItems)...)
	lines = append(lines, "Relevant memories:")
	lines = append(lines, bulletsOrNone(queryItems)...)
	return strings.Join(lines, "\n"), nil
}

func bulletsOrNone(items []string) []string {
	if len(items) == 0 {
		return []string{"- None"}
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = "- " + item
	}
	return out
}

// Numeric value of a decoded JSON field for comparisons, an error for non numbers
func pyNumber(v any) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, errors.New("'>' not supported between instances of 'NoneType' and 'int'")
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	}
	return 0, fmt.Errorf("'>' not supported between instances of '%s' and 'int'", pyTypeName(v))
}

// Count field of an agent model, zero when absent
func memoryCount(model map[string]any) (float64, error) {
	v, ok := model["memory_count"]
	if !ok {
		return 0, nil
	}
	return pyNumber(v)
}

// Python iteration of a JSON value as strings, list items, string characters or mapping keys
func pyIterStrings(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, len(x))
		for i, item := range x {
			out[i] = pyStr(item)
		}
		return out
	case string:
		var out []string
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		return keys
	}
	return nil
}

// Self model keys in display order with their line formatter
var agentModelFields = []struct {
	key    string
	format func(v any) string
}{
	{"persona", func(v any) string { return "Persona: " + pyStr(v) }},
	{"persistent_instructions", func(v any) string {
		lines := pyIterStrings(v)
		for i, l := range lines {
			lines[i] = "- " + l
		}
		return "Instructions:\n" + strings.Join(lines, "\n")
	}},
	{"working_style", func(v any) string { return "Working style: " + pyStr(v) }},
}

// RetainDB cloud memory with a durable queue, semantic search, dialectic synthesis and shared files
type RetainDBProvider struct {
	BaseProvider
	env *tools.Env

	mu              sync.Mutex
	client          *retaindbClient
	queue           *retaindbWriteQueue
	userID          string
	sessionID       string
	agentID         string
	contextResult   string
	dialecticResult string
	agentModel      map[string]any
	prefetchThreads []*ContextThread
}

// Uninitialized provider bound to the session environment
func NewRetainDBProvider(env *tools.Env) *RetainDBProvider {
	return &RetainDBProvider{env: env, userID: "default", agentID: "icebath", agentModel: map[string]any{}}
}

func (p *RetainDBProvider) Name() string { return "retaindb" }

// Whether the scoped API key is set
func (p *RetainDBProvider) Available(ctx context.Context) bool {
	v, _, err := provider.GetSecret(ctx, "RETAINDB_API_KEY")
	return err == nil && v != ""
}

func (p *RetainDBProvider) ConfigSchema() []map[string]any {
	return []map[string]any{
		{"key": "api_key", "description": "RetainDB API key", "secret": true, "required": true, "env_var": "RETAINDB_API_KEY", "url": "https://retaindb.com"},
		{"key": "base_url", "description": "API endpoint", "default": retaindbDefaultBaseURL},
		{"key": "project", "description": "Project identifier (optional — uses 'default' project if not set)", "default": ""},
	}
}

// Scoped secret by name, empty when unset
func scopedSecret(ctx context.Context, name string) (string, error) {
	v, _, err := provider.GetSecret(ctx, name)
	return v, err
}

// Binds client, queue and identity to the profile the manager names and seeds identity from SOUL.md
func (p *RetainDBProvider) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	cfg := map[string]string{}
	for k, v := range loadRetainDBConfig(ctx, p.env) {
		if s, ok := v.(string); ok {
			cfg[k] = pyStrip(s)
		}
	}
	baseURL, err := scopedSecret(ctx, "RETAINDB_BASE_URL")
	if err != nil {
		return err
	}
	if baseURL == "" {
		baseURL = cfg["base_url"]
	}
	if baseURL == "" {
		baseURL = retaindbDefaultBaseURL
	}
	baseURL = trailingSlashes.ReplaceAllString(baseURL, "")
	project, err := scopedSecret(ctx, "RETAINDB_PROJECT")
	if err != nil {
		return err
	}
	if project == "" {
		project = cfg["project"]
	}
	if project == "" {
		profileName := opts.Home[strings.LastIndexAny(opts.Home, pathSeparators)+1:]
		if profileName != "" && profileName != ".icebath" {
			project = "icebath-" + profileName
		} else {
			project = "default"
		}
	}
	apiKey, err := scopedSecret(ctx, "RETAINDB_API_KEY")
	if err != nil {
		return err
	}
	client := newRetainDBClient(apiKey, baseURL, project)
	userID := opts.UserID
	if userID == "" {
		userID = "default"
	}
	agentID := opts.AgentID
	if agentID == "" {
		agentID = "icebath"
	}
	homeDir := opts.Home
	if homeDir == "" {
		homeDir = registryHome(ctx, p.env)
	}
	p.mu.Lock()
	p.client, p.sessionID, p.userID, p.agentID = client, sessionID, userID, agentID
	p.mu.Unlock()
	queue, err := newRetainDBWriteQueue(ctx, p.env, client, filepath.Join(homeDir, "retaindb_queue.db"))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.queue = queue
	p.mu.Unlock()
	soulPath := filepath.Join(homeDir, "SOUL.md")
	soul := ""
	if raw, err := os.ReadFile(soulPath); err == nil {
		soul = pyStrip(strings.TrimPrefix(decodeReplace(raw), "\xef\xbb\xbf"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return pyOSError(err)
	}
	if soul != "" {
		SpawnContextThread(ctx, "retaindb-soul-seed", func(ctx context.Context) {
			retaindbQuiet(p.env, "soul seed", func() (any, error) { return client.seedAgentIdentity(ctx, agentID, soul, "soul_md") })
		}).Start()
	}
	return nil
}

func (p *RetainDBProvider) SystemPromptBlock(ctx context.Context) (string, error) {
	p.mu.Lock()
	project := "retaindb"
	if p.client != nil {
		project = p.client.project
	}
	p.mu.Unlock()
	return fmt.Sprintf("# RetainDB Memory\nActive. Project: %s.\nUse retaindb_search to find memories, retaindb_remember to store facts, "+
		"retaindb_profile for a user overview, retaindb_context for current-task context.", project), nil
}

func retaindbReasoningLevel(query string) string {
	n := runeLen(query)
	switch {
	case n < 120:
		return "low"
	case n < 400:
		return "medium"
	}
	return "high"
}

// Context overlay for a query with the raw query result
func (p *RetainDBProvider) contextOverlay(ctx context.Context, client *retaindbClient, userID, sessionID string, query any) (map[string]any, error) {
	queryResult, err := client.queryContext(ctx, userID, sessionID, query)
	if err != nil {
		return nil, err
	}
	profile, err := client.getProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	overlay, err := buildRetainDBOverlay(profile, queryResult, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"context": overlay, "raw": queryResult}, nil
}

// Fires context, dialectic and self model prefetches in the background for the next prefetch to consume
func (p *RetainDBProvider) QueuePrefetch(ctx context.Context, query, sessionID string) error {
	p.mu.Lock()
	client, userID, session, agentID, previous := p.client, p.userID, p.sessionID, p.agentID, p.prefetchThreads
	p.mu.Unlock()
	if client == nil {
		return nil
	}
	for _, t := range previous {
		t.Join(retaindbPrefetchJoin)
	}
	for _, t := range previous {
		if t.Alive() {
			envLog(p.env).Debug("RetainDB prefetch still running; skipping new batch")
			return nil
		}
	}
	jobs := []struct {
		name, label string
		fetch       func(ctx context.Context) (any, bool, error)
		store       func(v any)
	}{
		{"retaindb-ctx", "context", func(ctx context.Context) (any, bool, error) {
			overlay, err := p.contextOverlay(ctx, client, userID, session, query)
			if err != nil {
				return nil, false, err
			}
			return overlay["context"], true, nil
		}, func(v any) { p.contextResult = v.(string) }},
		{"retaindb-dialectic", "dialectic", func(ctx context.Context) (any, bool, error) {
			answer, err := client.askUser(ctx, userID, query, retaindbReasoningLevel(query))
			if err != nil {
				return nil, false, err
			}
			m, ok := answer.(map[string]any)
			if !ok {
				return nil, false, retaindbNoGet(answer)
			}
			text := ""
			if pyTruthy(m["answer"]) {
				text = pyStr(m["answer"])
			}
			return text, text != "", nil
		}, func(v any) { p.dialecticResult = v.(string) }},
		{"retaindb-agent-model", "agent model", func(ctx context.Context) (any, bool, error) {
			raw, err := client.getAgentModel(ctx, agentID)
			if err != nil {
				return nil, false, err
			}
			model, ok := raw.(map[string]any)
			if !ok {
				return nil, false, retaindbNoGet(raw)
			}
			count, err := memoryCount(model)
			if err != nil {
				return nil, false, err
			}
			return model, count > 0, nil
		}, func(v any) { p.agentModel = v.(map[string]any) }},
	}
	threads := make([]*ContextThread, 0, len(jobs))
	for _, job := range jobs {
		job := job
		threads = append(threads, SpawnContextThread(ctx, job.name, func(tctx context.Context) {
			type fetched struct {
				v    any
				keep bool
			}
			got, ok := retaindbQuiet(p.env, job.label+" prefetch", func() (fetched, error) {
				v, keep, err := job.fetch(tctx)
				return fetched{v, keep}, err
			})
			if ok && got.keep {
				p.mu.Lock()
				job.store(got.v)
				p.mu.Unlock()
			}
		}))
	}
	p.mu.Lock()
	p.prefetchThreads = threads
	p.mu.Unlock()
	for _, t := range threads {
		t.Start()
	}
	return nil
}

// Consumes the prefetched results as one context block
func (p *RetainDBProvider) Prefetch(ctx context.Context, query, sessionID string) (string, error) {
	p.mu.Lock()
	contextText, dialectic, agentModel := p.contextResult, p.dialecticResult, p.agentModel
	p.contextResult, p.dialecticResult, p.agentModel = "", "", map[string]any{}
	p.mu.Unlock()
	var modelLines []string
	count, err := memoryCount(agentModel)
	if err != nil {
		return "", err
	}
	if count > 0 {
		for _, f := range agentModelFields {
			if v := agentModel[f.key]; pyTruthy(v) {
				modelLines = append(modelLines, f.format(v))
			}
		}
	}
	var parts []string
	if contextText != "" {
		parts = append(parts, contextText)
	}
	if dialectic != "" {
		parts = append(parts, "[RetainDB User Synthesis]\n"+dialectic)
	}
	if len(modelLines) > 0 {
		parts = append(parts, "[RetainDB Agent Self-Model]\n"+strings.Join(modelLines, "\n"))
	}
	return strings.Join(parts, "\n\n"), nil
}

// Queues the turn for asynchronous ingest
func (p *RetainDBProvider) SyncTurn(ctx context.Context, userContent, assistantContent string, opts SyncOptions) error {
	p.mu.Lock()
	queue, userID, session := p.queue, p.userID, p.sessionID
	p.mu.Unlock()
	if queue == nil || userContent == "" {
		return nil
	}
	if opts.SessionID != "" {
		session = opts.SessionID
	}
	now := pyISOFormatUTC(time.Now())
	return queue.enqueue(userID, session, []any{
		map[string]any{"role": "user", "content": userContent, "timestamp": now},
		map[string]any{"role": "assistant", "content": assistantContent, "timestamp": now},
	})
}

func (p *RetainDBProvider) ToolSchemas(ctx context.Context) ([]map[string]any, error) {
	return retaindbSchemas(), nil
}

// Python int conversion of a tool argument with the conversion error text
func pyIntArg(v any) (int, error) {
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.Atoi(string(x)); err == nil {
			return n, nil
		}
		f, err := x.Float64()
		if err != nil {
			return 0, err
		}
		v = f
	case float64:
		if math.IsInf(x, 0) {
			return 0, errors.New("cannot convert float infinity to integer")
		}
		if math.IsNaN(x) {
			return 0, errors.New("cannot convert float NaN to integer")
		}
	case string:
		if n, ok := pyInt(x); ok {
			return n, nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(x))
	case nil, []any, map[string]any:
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
	}
	n, ok := pyInt(v)
	if !ok {
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
	}
	return n, nil
}

// Python float conversion of a tool argument with the conversion error text
func pyFloatArg(v any) (float64, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	case string:
		s := strings.ReplaceAll(pyStrip(x), "_", "")
		if f, err := strconv.ParseFloat(s, 64); err == nil && pyStrip(x) != "" {
			return f, nil
		}
		return 0, fmt.Errorf("could not convert string to float: %s", pyRepr(x))
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", pyTypeName(v))
}

// String argument a client route needs, the Python attribute error for anything else
func strArg(v any, method string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute '%s'", pyTypeName(v), method)
	}
	return s, nil
}

// One RetainDB tool as a JSON string, a client failure returned as a tool error
func (p *RetainDBProvider) HandleToolCall(ctx context.Context, toolName string, args map[string]any, opts ToolCallOptions) (string, error) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return toolError("RetainDB not initialized", nil), nil
	}
	result, err := p.dispatch(ctx, client, toolName, args)
	if err != nil {
		return toolError(err.Error(), nil), nil
	}
	return tools.JSON(result), nil
}

func argOr(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

func (p *RetainDBProvider) dispatch(ctx context.Context, client *retaindbClient, toolName string, args map[string]any) (any, error) {
	required := map[string]string{
		"retaindb_profile": "", "retaindb_search": "query", "retaindb_context": "query", "retaindb_remember": "content",
		"retaindb_forget": "memory_id", "retaindb_upload_file": "local_path", "retaindb_list_files": "",
		"retaindb_read_file": "file_id", "retaindb_ingest_file": "file_id", "retaindb_delete_file": "file_id",
	}
	req, known := required[toolName]
	if !known {
		return map[string]any{"error": "Unknown tool: " + toolName}, nil
	}
	var value any
	if req != "" {
		value = argOr(args, req, "")
		if !pyTruthy(value) {
			return map[string]any{"error": req + " is required"}, nil
		}
	}
	p.mu.Lock()
	userID, session, agentID := p.userID, p.sessionID, p.agentID
	p.mu.Unlock()
	switch toolName {
	case "retaindb_profile":
		return client.getProfile(ctx, userID)
	case "retaindb_search":
		topK, err := pyIntArg(argOr(args, "top_k", 8))
		if err != nil {
			return nil, err
		}
		return client.search(ctx, userID, session, value, min(topK, 20))
	case "retaindb_context":
		return p.contextOverlay(ctx, client, userID, session, value)
	case "retaindb_remember":
		importance, err := pyFloatArg(argOr(args, "importance", 0.7))
		if err != nil {
			return nil, err
		}
		return client.addMemory(ctx, userID, session, value, argOr(args, "memory_type", "factual"), importance)
	case "retaindb_forget":
		id, err := quoteArg(value)
		if err != nil {
			return nil, err
		}
		return client.deleteMemory(ctx, id)
	case "retaindb_upload_file":
		path, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyTypeName(value))
		}
		return p.toolUploadFile(ctx, client, userID, agentID, args, path)
	case "retaindb_list_files":
		limit, err := pyIntArg(argOr(args, "limit", 50))
		if err != nil {
			return nil, err
		}
		return client.listFiles(ctx, args["prefix"], limit)
	}
	id, err := quoteArg(value)
	if err != nil {
		return nil, err
	}
	switch toolName {
	case "retaindb_read_file":
		return p.toolReadFile(ctx, client, id)
	case "retaindb_ingest_file":
		return client.ingestFile(ctx, id, userID, agentID)
	}
	return client.deleteFile(ctx, id)
}

// Path segment argument, the quote type error for anything but a string
func quoteArg(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("quote() doesn't support '%s' objects", pyTypeName(v))
	}
	return s, nil
}

// Uploads a local file after the read guard, ingesting it when asked
func (p *RetainDBProvider) toolUploadFile(ctx context.Context, client *retaindbClient, userID, agentID string, args map[string]any, localPath string) (any, error) {
	if _, err := os.Stat(localPath); err != nil {
		return map[string]any{"error": "File not found: " + localPath}, nil
	}
	if blocked := tools.ReadBlockError(p.env, localPath); blocked != "" {
		return map[string]any{"error": blocked}, nil
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, pyOSError(err)
	}
	name := filepath.Base(localPath)
	remote := "/" + name
	if v := args["remote_path"]; pyTruthy(v) {
		remote = pyStr(v)
	}
	mimeType := "application/octet-stream"
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		mimeType, _, _ = strings.Cut(t, ";")
	}
	scopeValue := argOr(args, "scope", "PROJECT")
	scope, err := strArg(scopeValue, "upper")
	if err != nil {
		return nil, err
	}
	result, err := client.uploadFile(ctx, data, name, remote, mimeType, scope)
	if err != nil {
		return nil, err
	}
	if pyTruthy(args["ingest"]) {
		rm, ok := result.(map[string]any)
		if !ok {
			return nil, retaindbNoGet(result)
		}
		file := map[string]any{}
		if raw, present := rm["file"]; present {
			fm, ok := raw.(map[string]any)
			if !ok {
				return nil, retaindbNoGet(raw)
			}
			file = fm
		}
		if id := file["id"]; pyTruthy(id) {
			ingested, err := client.ingestFile(ctx, pyStr(id), userID, agentID)
			if err != nil {
				return nil, err
			}
			rm["ingest"] = ingested
		}
	}
	return result, nil
}

// Reads a stored file, text content capped at thirty two thousand characters and binary files described
func (p *RetainDBProvider) toolReadFile(ctx context.Context, client *retaindbClient, fileID string) (any, error) {
	info, err := client.getFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	im, ok := info.(map[string]any)
	if !ok {
		return nil, retaindbNoGet(info)
	}
	fileInfo, err := mappingOrEmpty(im["file"])
	if err != nil {
		return nil, err
	}
	raw, err := client.readFileContent(ctx, fileID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"file_id": fileID, "rdb_uri": fileInfo["rdb_uri"], "name": fileInfo["name"]}
	mimeType := ""
	if pyTruthy(fileInfo["mime_type"]) {
		mimeType = pyLower(pyStr(fileInfo["mime_type"]))
	}
	nameValue, present := fileInfo["name"]
	if !present {
		nameValue = ""
	}
	name, ok := nameValue.(string)
	if !strings.HasPrefix(mimeType, "text/") {
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'endswith'", pyTypeName(nameValue))
		}
		text := false
		for _, ext := range retaindbTextExts {
			if strings.HasSuffix(name, ext) {
				text = true
				break
			}
		}
		if !text {
			out["content"] = nil
			out["note"] = "Binary file — use retaindb_ingest_file to extract text into memory."
			return out, nil
		}
	}
	text := decodeReplace(raw)
	out["content"] = runePrefix(text, 32000)
	out["truncated"] = runeLen(text) > 32000
	return out, nil
}

// Mirrors built in memory adds to RetainDB
func (p *RetainDBProvider) OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) error {
	p.mu.Lock()
	client, userID, session := p.client, p.userID, p.sessionID
	p.mu.Unlock()
	if action != "add" || content == "" || client == nil {
		return nil
	}
	memoryType := "factual"
	if target == "user" {
		memoryType = "preference"
	}
	retaindbQuiet(p.env, "memory mirror", func() (any, error) { return client.addMemory(ctx, userID, session, content, memoryType, 0.7) })
	return nil
}

// Waits for prefetches, then drains and stops the write queue
func (p *RetainDBProvider) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	threads := p.prefetchThreads
	p.mu.Unlock()
	for _, t := range threads {
		t.Join(3 * time.Second)
	}
	p.mu.Lock()
	queue := p.queue
	p.prefetchThreads, p.queue, p.client = nil, nil, nil
	p.mu.Unlock()
	if queue != nil {
		queue.close()
	}
	return nil
}

// Registers RetainDB as a bundled memory provider
func registerRetainDBProvider() {
	RegisterProvider(ProviderRegistration{
		Name:     "retaindb",
		Source:   SourceBundled,
		Manifest: retaindbManifest,
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			return NewRetainDBProvider(env), nil
		},
	})
}
