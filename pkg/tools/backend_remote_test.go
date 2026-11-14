package tools

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fmt"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func remoteTestEnv(t *testing.T) *Env {
	t.Helper()
	h := t.TempDir()
	return &Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}}
}

// Fake transport recording uploads and deletes
type fakeTransport struct {
	mu       sync.Mutex
	uploads  []syncFile
	deletes  []string
	failNext bool
	remote   map[string][]byte
}

func (f *fakeTransport) bulkUpload(files []syncFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("transport down")
	}
	for _, file := range files {
		data, _ := os.ReadFile(file.host)
		f.remote[file.remote] = data
		f.uploads = append(f.uploads, file)
	}
	return nil
}

func (f *fakeTransport) delete(paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, paths...)
	for _, p := range paths {
		delete(f.remote, p)
	}
	return nil
}

func TestFileSyncPlansUploadsDeletesAndRollsBack(t *testing.T) {
	env := remoteTestEnv(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	os.WriteFile(a, []byte("one"), 0o644)
	old := time.Now().Add(-10 * time.Second)
	os.Chtimes(a, old, old)
	files := []syncFile{{a, "/root/.icebath/skills/a.txt"}}
	ft := &fakeTransport{remote: map[string][]byte{}}
	m := NewFileSyncManager(env, func() []syncFile { return files }, nil, ft.delete, ft.bulkUpload, nil)
	m.Sync(true)
	if len(ft.uploads) != 1 || string(ft.remote["/root/.icebath/skills/a.txt"]) != "one" {
		t.Fatalf("%+v", ft.uploads)
	}
	m.Sync(true)
	if len(ft.uploads) != 1 {
		t.Fatal("unchanged file must not re-upload")
	}
	os.WriteFile(a, []byte("two"), 0o644)
	os.Chtimes(a, old.Add(time.Second), old.Add(time.Second))
	ft.failNext = true
	m.Sync(true)
	if len(ft.uploads) != 1 || m.pushedHashes["/root/.icebath/skills/a.txt"] == "" {
		t.Fatal("failed cycle must roll back and keep the first push state")
	}
	m.Sync(true)
	if len(ft.uploads) != 2 || string(ft.remote["/root/.icebath/skills/a.txt"]) != "two" {
		t.Fatal("retry after failure")
	}
	files = nil
	m.Sync(true)
	if len(ft.deletes) != 1 || ft.deletes[0] != "/root/.icebath/skills/a.txt" {
		t.Fatalf("%v", ft.deletes)
	}
	m.lastSync = time.Now()
	files = []syncFile{{a, "/root/.icebath/skills/a.txt"}}
	m.Sync(false)
	if len(ft.uploads) != 2 {
		t.Fatal("rate limit must skip the cycle")
	}
	t.Setenv(forceFileSyncEnv, "1")
	m.Sync(false)
	if len(ft.uploads) != 3 {
		t.Fatal("force env must run the cycle")
	}
}

func TestFileSyncBackAppliesChangedFilesOnly(t *testing.T) {
	env := remoteTestEnv(t)
	dir := t.TempDir()
	host := filepath.Join(dir, "skills", "s.md")
	os.MkdirAll(filepath.Dir(host), 0o755)
	os.WriteFile(host, []byte("pushed"), 0o644)
	files := []syncFile{{host, "/root/.icebath/skills/s.md"}}
	ft := &fakeTransport{remote: map[string][]byte{}}
	download := func(dest string) error {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, content := range map[string]string{"root/.icebath/skills/s.md": "changed", "root/.icebath/skills/new.md": "fresh", "root/.icebath/other/x": "unmapped"} {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
			tw.Write([]byte(content))
		}
		tw.Close()
		return os.WriteFile(dest, buf.Bytes(), 0o600)
	}
	m := NewFileSyncManager(env, func() []syncFile { return files }, nil, ft.delete, ft.bulkUpload, download)
	m.sleep = func(time.Duration) {}
	m.SyncBack(env.Home)
	if data, _ := os.ReadFile(host); string(data) != "pushed" {
		t.Fatal("no push state must skip sync back")
	}
	m.Sync(true)
	m.SyncBack(env.Home)
	if data, _ := os.ReadFile(host); string(data) != "changed" {
		t.Fatalf("changed remote file not applied: %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "skills", "new.md")); string(data) != "fresh" {
		t.Fatal("new remote file must be inferred next to its mapped sibling")
	}
	if _, err := os.Stat(filepath.Join(dir, "other")); err == nil {
		t.Fatal("unmapped remote file must be skipped")
	}
}

func TestExtractTarDataRefusesEscapes(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	p := filepath.Join(t.TempDir(), "t.tar")
	os.WriteFile(p, buf.Bytes(), 0o600)
	if err := extractTarData(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatal(err)
	}
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	tw.Close()
	os.WriteFile(p, buf.Bytes(), 0o600)
	if err := extractTarData(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatal(err)
	}
	for _, hdr := range []*tar.Header{
		{Name: "a/link", Linkname: "../../x", Typeflag: tar.TypeSymlink},
		{Name: "link", Linkname: `\\server\share`, Typeflag: tar.TypeSymlink},
		{Name: "hard", Linkname: "../x", Typeflag: tar.TypeLink},
	} {
		buf.Reset()
		tw = tar.NewWriter(&buf)
		tw.WriteHeader(hdr)
		tw.Close()
		os.WriteFile(p, buf.Bytes(), 0o600)
		if err := extractTarData(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("%s -> %s: %v", hdr.Name, hdr.Linkname, err)
		}
	}
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "d/f", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("y"))
	tw.WriteHeader(&tar.Header{Name: "d/l", Linkname: "../d/f", Typeflag: tar.TypeSymlink})
	tw.Close()
	os.WriteFile(p, buf.Bytes(), 0o600)
	dest := t.TempDir()
	if err := extractTarData(p, dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "d", "l")); err != nil || got != filepath.FromSlash("../d/f") {
		t.Fatal(got, err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "d", "l")); err != nil || string(data) != "y" {
		t.Fatal(string(data), err)
	}
}

func TestStaleSyncBackTempCleanup(t *testing.T) {
	env := remoteTestEnv(t)
	dir := t.TempDir()
	dead := filepath.Join(dir, syncBackTempPrefix+"999999-abc.tar")
	os.WriteFile(dead, []byte("x"), 0o600)
	mine := filepath.Join(dir, syncBackTempPrefixFor()+"live")
	os.MkdirAll(mine, 0o755)
	if n := cleanupStaleSyncBackTemp(env, dir); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("live owner's fresh entry must stay")
	}
	if syncTempEntryOwnerAlive(syncBackTempPrefix+"nopid") != true {
		t.Fatal("names without a pid count as alive")
	}
}

func TestSSHCommandShaping(t *testing.T) {
	s := &sshEnv{host: "h", user: "u", port: 2222, keyPath: "/k", controlSocket: "/tmp/icebath-ssh/abcdef0123456789.sock"}
	cmd := s.buildSSHCommand([]string{"-T"}, []string{"ZED", "ALPHA"})
	joined := strings.Join(cmd, " ")
	if !strings.HasSuffix(joined, "-p 2222 -i /k -T u@h") || !strings.Contains(joined, "-o SendEnv=ALPHA -o SendEnv=ZED") || !strings.Contains(joined, "BatchMode=yes") {
		t.Fatal(joined)
	}
	if sshMultiplex {
		if !strings.Contains(joined, "ControlMaster=auto") || !strings.Contains(joined, "ControlPersist=300") {
			t.Fatal(joined)
		}
		plain := s.controlSocketFor(nil)
		withEnv := s.controlSocketFor([]string{"ALPHA", "ZED"})
		if plain != s.controlSocket || withEnv == plain || !strings.HasPrefix(filepath.Base(withEnv), "abcdef01") || len(filepath.Base(withEnv)) != len(filepath.Base(plain)) {
			t.Fatal(withEnv)
		}
	}
	s.port = 22
	s.keyPath = ""
	if flags := s.targetFlags("-P"); len(flags) != 0 {
		t.Fatal(flags)
	}
	if rel, _ := relPosix("/home/u/.icebath", "/home/u/.icebath/skills/a"); rel != "skills/a" {
		t.Fatal(rel)
	}
	if rel, _ := relPosix("/home/u/.icebath", "/home/u/other"); !strings.HasPrefix(rel, "../") {
		t.Fatal(rel)
	}
	err := sshSyncError("scp failed: nope", "File sync to u@h", "")
	var connErr *EnvironmentConnectionError
	if !errors.As(err, &connErr) || !strings.Contains(connErr.RetryHint, "verify the SSH connection is healthy") {
		t.Fatal(err)
	}
	if quotedMkdirCommand([]string{"/a b", "/c"}) != "mkdir -p '/a b' /c" || quotedRmCommand([]string{"/x y"}) != "rm -f '/x y'" {
		t.Fatal("quoting")
	}
	if dirs := uniqueParentDirs([]syncFile{{"h1", "/r/b/x"}, {"h2", "/r/a/y"}, {"h3", "/r/b/z"}}); strings.Join(dirs, ",") != "/r/a,/r/b" {
		t.Fatal(dirs)
	}
}

func TestModalParamsAndSnapshotStore(t *testing.T) {
	p := modalCreateParams(map[string]any{"cpu": 2, "memory": float64(4096), "timeout": 120})
	if p.CPU != 2 || p.MemoryMiB != 4096 || p.Timeout != 120*time.Second || strings.Join(p.Command, " ") != "sleep infinity" {
		t.Fatalf("%+v", p)
	}
	if d := modalCreateParams(nil); d.Timeout != time.Hour || d.CPU != 0 {
		t.Fatalf("%+v", d)
	}
	env := remoteTestEnv(t)
	if id, _ := modalSnapshotRestoreCandidate(env, "t1"); id != "" {
		t.Fatal("empty store")
	}
	envUpdateJSONStore(context.Background(), modalSnapshotStore(env), func(s *pyjson.Object) { s.Set("t1", "im-legacy") })
	if id, legacy := modalSnapshotRestoreCandidate(env, "t1"); id != "im-legacy" || !legacy {
		t.Fatal(id)
	}
	modalStoreDirectSnapshot(context.Background(), env, "t1", "im-new")
	store := envLoadJSONStore(modalSnapshotStore(env))
	if store.Vals["direct:t1"] != "im-new" || store.Vals["t1"] != nil {
		t.Fatal(store)
	}
	if id, legacy := modalSnapshotRestoreCandidate(env, "t1"); id != "im-new" || legacy {
		t.Fatal(id)
	}
	modalDeleteDirectSnapshot(context.Background(), env, "t1", "im-other")
	if id, _ := modalSnapshotRestoreCandidate(env, "t1"); id != "im-new" {
		t.Fatal("mismatched id must not delete")
	}
	modalDeleteDirectSnapshot(context.Background(), env, "t1", "")
	if id, _ := modalSnapshotRestoreCandidate(env, "t1"); id != "" {
		t.Fatal("empty id deletes any")
	}
	var sink bytes.Buffer
	w := nopWriteCloser{&sink}
	payload := strings.Repeat("x", modalStdinChunkSize+10)
	if err := modalStreamStdin(w, []byte(payload)); err != nil || sink.Len() != len(payload) {
		t.Fatal(err, sink.Len())
	}
	data, err := decodeBase64Loose("aGVs\nbG8")
	if err != nil || string(data) != "hello" {
		t.Fatal(string(data), err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestManagedModalExecutePollsToCompletion(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/v1/sandboxes" && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["logicalKey"] != "task-1" || body["persistentFilesystem"] != true || r.Header.Get("x-idempotency-key") == "" {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "sb1"})
		case r.URL.Path == "/v1/sandboxes/sb1/execs" && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"execId": body["execId"], "status": "running"})
		case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sb1/execs/") && r.Method == "GET":
			polls++
			if polls < 2 {
				json.NewEncoder(w).Encode(map[string]any{"status": "running"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": "hi\n", "returncode": 0})
		case strings.HasSuffix(r.URL.Path, "/terminate"):
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	env := remoteTestEnv(t)
	m := &managedModalEnv{gatewayOrigin: srv.URL, userToken: "tok", taskID: "task-1", persistent: true, image: "img", kwargs: map[string]any{"cpu": 2},
		idempotency: "idem", connectTO: time.Second, pollReadTO: time.Second, cancelReadTO: time.Second}
	m.stdinMode = "payload"
	initBaseEnv(&m.baseEnv, m, env, "/root", 30, nil, true)
	id, err := m.createSandbox()
	if err != nil || id != "sb1" {
		t.Fatal(id, err)
	}
	m.sandboxID = id
	res, err := m.Execute(context.Background(), "echo hi", ExecOptions{})
	if err != nil || res.ReturnCode != 0 || res.Output != "hi\n" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := managedModalFormatError("x", &http.Response{StatusCode: 500}, []byte(`{"error":"boom"}`)); got != "x: boom" {
		t.Fatal(got)
	}
	if got := managedModalFormatError("x", &http.Response{StatusCode: 502}, []byte("")); got != "x: HTTP 502" {
		t.Fatal(got)
	}
	if managedModalResultFromBody(map[string]any{"status": "running"}) != nil {
		t.Fatal("running is not terminal")
	}
	if r := managedModalResultFromBody(map[string]any{"status": "timeout", "output": "o"}); r.ReturnCode != 1 || r.Output != "o" {
		t.Fatalf("%+v", r)
	}
	if err := m.Cleanup(false); err != nil || m.sandboxID != "" {
		t.Fatal(err)
	}
}

func TestVercelCredentialsAndAuthStatus(t *testing.T) {
	env := remoteTestEnv(t)
	for _, name := range append(vercelTokenTupleVars, "VERCEL_OIDC_TOKEN") {
		t.Setenv(name, "")
	}
	if _, err := resolveVercelCredentials(env); err == nil {
		t.Fatal("no auth must fail")
	}
	status := DescribeVercelAuth(env)
	if status.OK || status.Label != "not configured" {
		t.Fatalf("%+v", status)
	}
	t.Setenv("VERCEL_TOKEN", "tok")
	status = DescribeVercelAuth(env)
	if status.OK || !strings.HasPrefix(status.Label, "partial access-token auth (missing VERCEL_PROJECT_ID, VERCEL_TEAM_ID)") {
		t.Fatalf("%+v", status)
	}
	t.Setenv("VERCEL_PROJECT_ID", "prj")
	t.Setenv("VERCEL_TEAM_ID", "team")
	creds, err := resolveVercelCredentials(env)
	if err != nil || creds.token != "tok" || creds.projectID != "prj" || creds.teamID != "team" || !DescribeVercelAuth(env).OK {
		t.Fatal(creds, err)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"project_id":"p2","owner_id":"o2"}`))
	t.Setenv("VERCEL_OIDC_TOKEN", "h."+claims+".s")
	creds, err = resolveVercelCredentials(env)
	if err != nil || creds.projectID != "p2" || creds.teamID != "o2" || creds.token != "h."+claims+".s" {
		t.Fatal(creds, err)
	}
	status = DescribeVercelAuth(env)
	if !status.OK || status.Label != "OIDC token via VERCEL_OIDC_TOKEN" || !strings.Contains(strings.Join(status.DetailLines, "|"), "also present: VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID") {
		t.Fatalf("%+v", status)
	}
	t.Setenv("VERCEL_OIDC_TOKEN", "garbage")
	if _, err := resolveVercelCredentials(env); err == nil || !strings.Contains(err.Error(), "Invalid Vercel OIDC token") {
		t.Fatal(err)
	}
}

func TestVercelBackendRequirements(t *testing.T) {
	env := remoteTestEnv(t)
	for _, name := range append(vercelTokenTupleVars, "VERCEL_OIDC_TOKEN") {
		t.Setenv(name, "")
	}
	b := vercelSandboxBackend{}
	if err := b.CheckRequirements(env, map[string]any{"vercel_runtime": "node18"}); err == nil || !strings.Contains(err.Error(), "runtime 'node18' is not supported") {
		t.Fatal(err)
	}
	if err := b.CheckRequirements(env, map[string]any{"container_disk": 1024}); err == nil || !strings.Contains(err.Error(), "custom TERMINAL_CONTAINER_DISK=1024") {
		t.Fatal(err)
	}
	if err := b.CheckRequirements(env, map[string]any{}); err == nil || !strings.Contains(err.Error(), "no supported auth configuration was found") {
		t.Fatal(err)
	}
	t.Setenv("VERCEL_TOKEN", "t")
	if err := b.CheckRequirements(env, map[string]any{}); err == nil || !strings.Contains(err.Error(), "must all be set together") {
		t.Fatal(err)
	}
	t.Setenv("VERCEL_OIDC_TOKEN", "x")
	if err := b.CheckRequirements(env, map[string]any{"container_disk": 51200}); err != nil {
		t.Fatal(err)
	}
}

func TestVercelClientCreateAndRunCommand(t *testing.T) {
	var createBody map[string]any
	createPath := ""
	var written []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("teamId") != "team" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/v3/sandboxes" || r.URL.Path == "/v2/sandboxes":
			createPath = r.URL.Path
			createBody = nil
			json.NewDecoder(r.Body).Decode(&createBody)
			json.NewEncoder(w).Encode(map[string]any{"sandbox": map[string]any{"name": "x"}, "session": map[string]any{"id": "sbx_1", "cwd": "/vercel/sandbox", "status": "running"}})
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1/cmd":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["wait"] != true || body["command"] != "bash" {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			bw := bufio.NewWriter(w)
			bw.WriteString(`{"command":{"id":"cmd_1","exitCode":null}}` + "\n")
			bw.WriteString(`{"stream":"stdout","data":"out "}` + "\n")
			bw.WriteString(`{"stream":"stderr","data":"err"}` + "\n")
			bw.WriteString(`{"command":{"id":"cmd_1","exitCode":3}}` + "\n")
			bw.Flush()
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1/fs/write":
			if r.Header.Get("Content-Type") != "application/gzip" || r.Header.Get("x-cwd") != "/" {
				w.WriteHeader(400)
				return
			}
			gz, _ := gzip.NewReader(r.Body)
			tr := tar.NewReader(gz)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				written = append(written, hdr.Name)
			}
			w.Write([]byte("{}"))
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1/fs/read":
			w.Write([]byte("file-bytes"))
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1/snapshot":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{}, "snapshot": map[string]any{"id": "snap_9"}})
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1":
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "sbx_1", "status": "stopped"}})
		case r.URL.Path == "/v2/sandboxes/sessions/sbx_1/stop":
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "sbx_1", "status": "stopping"}})
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"not_found","message":"nope"}}`))
		}
	}))
	defer srv.Close()
	c := &vercelClient{creds: vercelCredentials{token: "tok", teamID: "team", projectID: "prj"}, http: srv.Client()}
	base := vercelAPIBase
	defer func() { vercelAPIBaseOverride = "" }()
	vercelAPIBaseOverride = srv.URL
	_ = base
	ctx := context.Background()
	session, err := c.createSession(ctx, vercelCreateParams{timeout: 5 * time.Minute, image: "vercel/sandbox/universal:latest", vcpus: 2, memoryMB: 4096}, "")
	if err != nil || session.id != "sbx_1" || createPath != "/v3/sandboxes" || createBody["image"] != "vercel/sandbox/universal:latest" || createBody["timeout"] != float64(300000) {
		t.Fatalf("%+v %v %s %v", session, err, createPath, createBody)
	}
	if res := createBody["resources"].(map[string]any); res["vcpus"] != float64(2) || res["memory"] != float64(4096) {
		t.Fatal(res)
	}
	c.createSession(ctx, vercelCreateParams{timeout: time.Minute, runtime: "node24", image: "ignored"}, "")
	if createPath != "/v2/sandboxes" || createBody["runtime"] != "node24" || createBody["image"] != nil {
		t.Fatal(createPath, createBody)
	}
	c.createSession(ctx, vercelCreateParams{timeout: time.Minute, runtime: "node24", image: "img"}, "snap_1")
	if createPath != "/v3/sandboxes" || createBody["source"].(map[string]any)["snapshotId"] != "snap_1" || createBody["runtime"] != nil {
		t.Fatal(createPath, createBody)
	}
	result, err := c.runCommand(ctx, "sbx_1", "bash", []string{"-c", "x"}, "/vercel/sandbox")
	if err != nil || result.exitCode != 3 || result.output != "out err" {
		t.Fatalf("%+v %v", result, err)
	}
	if err := c.writeFiles(ctx, "sbx_1", []vercelFile{{path: "/home/u/.icebath/a", content: []byte("1")}}); err != nil || strings.Join(written, ",") != "home/u/.icebath/a" {
		t.Fatal(written, err)
	}
	if data, err := c.readFile(ctx, "sbx_1", "/x"); err != nil || string(data) != "file-bytes" {
		t.Fatal(string(data), err)
	}
	if id, err := c.snapshotSession(ctx, "sbx_1"); err != nil || id != "snap_9" {
		t.Fatal(id, err)
	}
	_, err = c.json(ctx, "GET", "/v2/sandboxes/sessions/missing", nil, time.Second)
	var apiErr *vercelAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Code != "not_found" || apiErr.Body != "nope" {
		t.Fatal(err)
	}
	if isTransientVercelError(apiErr) || !isTransientVercelError(&vercelAPIError{Status: 503}) {
		t.Fatal("transient classification")
	}
}

func TestVercelPayloadStdinIsStagedByteExactNotInArgv(t *testing.T) {
	var mu sync.Mutex
	staged := map[string][]byte{}
	modes := map[string]int64{}
	var args [][]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v2/sandboxes/sessions/sbx_1/fs/write":
			gz, _ := gzip.NewReader(r.Body)
			tr := tar.NewReader(gz)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(tr)
				staged["/"+hdr.Name], modes["/"+hdr.Name] = data, hdr.Mode
			}
			w.Write([]byte("{}"))
		case "/v2/sandboxes/sessions/sbx_1/cmd":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			list, _ := body["args"].([]any)
			args = append(args, list)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Write([]byte(`{"command":{"id":"cmd_1","exitCode":null}}` + "\n" + `{"command":{"id":"cmd_1","exitCode":0}}` + "\n"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	defer func() { vercelAPIBaseOverride = "" }()
	vercelAPIBaseOverride = srv.URL
	v := &vercelEnv{client: &vercelClient{creds: vercelCredentials{token: "tok", teamID: "team", projectID: "prj"}, http: srv.Client()},
		session: &vercelSession{id: "sbx_1", cwd: "/vercel/sandbox", status: "running"}, workspaceRoot: "/vercel/sandbox"}
	initBaseEnv(&v.baseEnv, v, remoteTestEnv(t), "/vercel/sandbox", 30, nil, false)
	raw := append(bytes.Repeat([]byte("A"), 160*1024), []byte("\x00\xff\xfeTAIL")...)
	payload := pystr.SurrogateEscapeDecode(raw)
	h, err := v.runBash(context.Background(), "cat > /tmp/out", false, 30, &payload)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := h.Wait(5); err != nil || code != 0 {
		out, _ := io.ReadAll(h.Stdout())
		t.Fatal(code, err, string(out))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(staged) != 1 || len(args) != 1 {
		t.Fatalf("staged %d commands %d", len(staged), len(args))
	}
	for path, data := range staged {
		if !bytes.Equal(data, raw) || modes[path] != 0o600 {
			t.Fatalf("staged %d bytes mode %o", len(data), modes[path])
		}
		script, _ := args[0][1].(string)
		if args[0][0] != "-c" || !strings.HasPrefix(script, "exec 0< "+path+" || exit $?\n") || !strings.HasSuffix(script, "\ncat > /tmp/out") {
			t.Fatalf("argv %q", args[0])
		}
		if strings.Contains(script, "AAAAAAAAAAAAAAAA") {
			t.Fatal("payload leaked into argv")
		}
	}
	bad := "\xed\xa0\x80"
	h, err = v.runBash(context.Background(), "cat > /tmp/out", false, 30, &bad)
	if err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	code, err := h.Wait(5)
	mu.Lock()
	if err != nil || code != 1 || len(staged) != 1 || len(args) != 1 {
		t.Fatalf("unencodable payload: code %d err %v staged %d commands %d", code, err, len(staged), len(args))
	}
}

func TestManagedModalStdinPayloadEscapesSurrogatesLikeRequests(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes/sb1/execs" && r.Method == "POST" {
			body, _ = io.ReadAll(r.Body)
			json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": "", "returncode": 0})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	m := &managedModalEnv{gatewayOrigin: srv.URL, userToken: "tok", taskID: "task-1", image: "img", idempotency: "idem",
		connectTO: time.Second, pollReadTO: time.Second, cancelReadTO: time.Second, sandboxID: "sb1"}
	m.stdinMode = "payload"
	initBaseEnv(&m.baseEnv, m, remoteTestEnv(t), "/root", 30, nil, false)
	stdin := "caf\xc3\xa9 " + pystr.SurrogateEscapeDecode([]byte{0xff}) + "<"
	if res, err := m.Execute(context.Background(), "cat > /tmp/out", ExecOptions{Stdin: &stdin}); err != nil || res.ReturnCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if !bytes.Contains(body, []byte(`"stdinData":"caf\u00e9 \udcff<"`)) {
		t.Fatalf("body %s", body)
	}
}

func TestManagedModalFormatErrorPicksAndDumpsAsPythonDoes(t *testing.T) {
	resp := &http.Response{StatusCode: 500}
	for body, want := range map[string]string{
		`{"error": {"x": 1}, "message": "boom", "n": 1.0, "i": 1}`: `x: {"error": {"x": 1}, "message": "boom", "n": 1.0, "i": 1}`,
		`{"code": 7, "b": 1e2, "a": "é<"}`:                         `x: {"code": 7, "b": 100.0, "a": "é<"}`,
		`{"error": "", "message": "m"}`:                            "x: m",
		`{"error": 0, "message": null, "code": "E1"}`:              "x: E1",
		`null`:     "x: null",
		`[1, 1.0]`: "x: [1, 1.0]",
	} {
		if got := managedModalFormatError("x", resp, []byte(body)); got != want {
			t.Errorf("body %s: %q, want %q", body, got, want)
		}
	}
}

func TestManagedModalBodiesThatAreNotObjectsAnswerAsHermesDoes(t *testing.T) {
	var create, start, poll string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/sandboxes":
			io.WriteString(w, create)
		case r.URL.Path == "/v1/sandboxes/sb1/execs":
			body := start
			if body == "echo" {
				var sent map[string]any
				json.NewDecoder(r.Body).Decode(&sent)
				body = `{"execId": "` + sent["execId"].(string) + `", "status": "running"}`
			}
			io.WriteString(w, body)
		case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sb1/execs/") && r.Method == "GET":
			io.WriteString(w, poll)
		}
	}))
	defer srv.Close()
	env := remoteTestEnv(t)
	m := &managedModalEnv{gatewayOrigin: srv.URL, userToken: "tok", taskID: "task-1", image: "img", kwargs: map[string]any{},
		idempotency: "idem", connectTO: time.Second, pollReadTO: time.Second, cancelReadTO: time.Second}
	m.stdinMode = "payload"
	initBaseEnv(&m.baseEnv, m, env, "/root", 30, nil, true)
	for body, want := range map[string]string{
		"not json":      "Expecting value: line 1 column 1 (char 0)",
		`[1]`:           "'list' object has no attribute 'get'",
		`{"id": 7}`:     "Managed Modal create did not return a sandbox id",
		`{"id": "sb1"}`: "",
	} {
		create = body
		_, err := m.createSandbox()
		if got := fmt.Sprint(err); (want == "" && err != nil) || (want != "" && got != want) {
			t.Errorf("create body %s: %v, want %q", body, err, want)
		}
	}
	m.sandboxID = "sb1"
	for body, want := range map[string]string{
		"null":     "Managed Modal exec failed: null",
		"not json": "Managed Modal exec failed: Expecting value: line 1 column 1 (char 0)",
	} {
		start = body
		res, err := m.Execute(context.Background(), "echo hi", ExecOptions{})
		if err != nil || res.Output != want || res.ReturnCode != 1 {
			t.Errorf("start body %s: %+v %v, want %q", body, res, err, want)
		}
	}
	start = `"text"`
	if res, err := m.Execute(context.Background(), "echo hi", ExecOptions{}); res != nil || fmt.Sprint(err) != "'str' object has no attribute 'get'" {
		t.Errorf("start body of a string: %+v %v", res, err)
	}
	start = "echo"
	for body, want := range map[string]string{
		`[1.0]`: "Managed Modal exec failed: 'list' object has no attribute 'get'",
		`null`:  "Managed Modal exec failed: 'NoneType' object has no attribute 'get'",
		`{"a":`: "Managed Modal exec failed: Expecting value: line 1 column 6 (char 5)",
	} {
		poll = body
		res, err := m.Execute(context.Background(), "echo hi", ExecOptions{})
		if err != nil || res.Output != want || res.ReturnCode != 1 {
			t.Errorf("poll body %s: %+v %v, want %q", body, res, err, want)
		}
	}
}
