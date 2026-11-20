package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const ledgerValidSkill = "---\nname: my-skill\ndescription: test skill\n---\n\n# My Skill\n\nOriginal body.\n"

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func ledgerHome(t *testing.T) (*Scope, string, *lockedBuffer) {
	t.Helper()
	s, skills := usageHome(t)
	logs := &lockedBuffer{}
	s.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s, skills, logs
}

func ledgerItems(t *testing.T, pairs ...string) []any {
	t.Helper()
	var out []any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, newLedgerItem(pairs[i], pairs[i+1]))
	}
	return out
}

func ledgerManifest(t *testing.T, s *Scope, skills, name string, files map[string]string) []any {
	t.Helper()
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var out []any
	for _, rel := range rels {
		sha, err := s.ledgerStoreBlob([]byte(files[rel]))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, newLedgerItem(filepath.Join(skills, name, filepath.FromSlash(rel)), sha))
	}
	return out
}

func ledgerAppendOK(t *testing.T, s *Scope, action, skill string, before, after []any, actor string, evidence *pyjson.Object) string {
	t.Helper()
	id, ok := s.ledgerAppend(context.Background(), action, skill, before, after, actor, evidence)
	if !ok || id == "" {
		t.Fatalf("append %s failed", action)
	}
	return id
}

func ledgerEntry(t *testing.T, s *Scope, id string) *pyjson.Object {
	t.Helper()
	e, err := s.LedgerGetEntry(id)
	if err != nil || e == nil {
		t.Fatalf("entry %s: %v %v", id, e, err)
	}
	return e
}

func ledgerNames(v any) map[string]bool {
	out := map[string]bool{}
	items, _ := v.([]any)
	for _, it := range items {
		p, _ := it.(*pyjson.Object).Get("path")
		out[filepath.Base(p.(string))] = true
	}
	return out
}

func writeSkillsTarball(t *testing.T, h string, files [][2]string, stamp string) {
	t.Helper()
	snap := filepath.Join(h, "skills", ".curator_backups", stamp)
	os.MkdirAll(snap, 0o755)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(filepath.Join(snap, "skills.tar.gz"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func blobNames(t *testing.T, s *Scope) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := os.ReadDir(s.LedgerBlobsDir())
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(s.LedgerBlobsDir(), e.Name()))
		out[e.Name()] = string(data)
	}
	return out
}

func TestLedgerRollbackRefusesPathsOutsideHome(t *testing.T) {
	s, _, _ := ledgerHome(t)
	id := ledgerAppendOK(t, s, "patch", "evil", ledgerItems(t, "/etc/passwd", strings.Repeat("0", 64)), []any{}, "", nil)
	ok, msg, err := s.LedgerRollbackEntry(context.Background(), id)
	if err != nil || ok || !strings.Contains(msg, "outside") {
		t.Fatalf("%v %q %v", ok, msg, err)
	}
}

func TestLedgerBlobDedupe(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	d := filepath.Join(skills, "dedupe-src")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "a.md"), []byte("identical content"), 0o644)
	os.WriteFile(filepath.Join(d, "b.md"), []byte("identical content"), 0o644)
	m, err := s.LedgerSnapshotPaths(d, false)
	if err != nil || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	a, _ := m[0].Get("sha256")
	b, _ := m[1].Get("sha256")
	if a != b || len(blobNames(t, s)) != 1 {
		t.Fatalf("%v %v %v", a, b, blobNames(t, s))
	}
}

func TestLedgerSnapshotSkipsTransientDirs(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	d := filepath.Join(skills, "has-venv")
	for rel, body := range map[string]string{"SKILL.md": "# skill", "scripts/run.py": "print('hi')",
		"node_modules/pkg/index.js": "junk", "venv/bin/python": "junk",
		"__pycache__/run.cpython-311.pyc": "junk", ".git/config": "junk"} {
		p := filepath.Join(d, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	m, err := s.LedgerSnapshotPaths(d, false)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, it := range m {
		p, _ := it.Get("path")
		r, _ := filepath.Rel(d, p.(string))
		rel = append(rel, r)
	}
	if strings.Join(rel, ",") != "SKILL.md,"+filepath.Join("scripts", "run.py") {
		t.Fatalf("%v", rel)
	}
	sum := sha256.Sum256([]byte("junk"))
	if _, ok := blobNames(t, s)[hex.EncodeToString(sum[:])]; ok {
		t.Fatal("transient content stored")
	}
}

func TestLedgerSnapshotKeepsFileNamedLikeTransientDir(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	d := filepath.Join(skills, "edge")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("# skill"), 0o644)
	os.WriteFile(filepath.Join(d, "venv"), []byte("#!/bin/sh\n"), 0o644)
	m, _ := s.LedgerSnapshotPaths(d, false)
	names := ledgerNames(ledgerObjects(m))
	if !names["venv"] || !names["SKILL.md"] {
		t.Fatalf("%v", names)
	}
}

func TestLedgerListEntriesFilteringAndLimit(t *testing.T) {
	s, _, _ := ledgerHome(t)
	for i := 0; i < 5; i++ {
		ledgerAppendOK(t, s, "patch", []string{"skill-0", "skill-1"}[i%2], []any{}, []any{}, "", nil)
	}
	if rows, _ := s.LedgerListEntries("", 3); len(rows) != 3 {
		t.Fatalf("%d", len(rows))
	}
	rows, _ := s.LedgerListEntries("skill-0", -1)
	if len(rows) != 3 {
		t.Fatalf("%d", len(rows))
	}
	for _, r := range rows {
		if v, _ := r.Get("skill"); v != "skill-0" {
			t.Fatalf("%v", v)
		}
	}
}

func TestLedgerUserActorOverride(t *testing.T) {
	s, _, _ := ledgerHome(t)
	id, ok := s.ledgerAppend(WithLedgerActor(context.Background(), "user"), "archive", "some-skill", nil, nil, "", nil)
	if !ok {
		t.Fatal("append failed")
	}
	if a, _ := ledgerEntry(t, s, id).Get("actor"); a != "user" {
		t.Fatalf("%v", a)
	}
	if a := s.LedgerDeriveActor(context.Background()); a != "agent" {
		t.Fatalf("%v", a)
	}
}

func TestLedgerRollbackHistoricalHollowEntryRestoresFullPackage(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	dir := filepath.Join(skills, "my-skill")
	os.MkdirAll(dir, 0o755)
	md := filepath.Join(dir, "SKILL.md")
	os.WriteFile(md, []byte(ledgerValidSkill), 0o644)
	writeSkillsTarball(t, s.Home, [][2]string{{"my-skill/SKILL.md", ledgerValidSkill}, {"my-skill/references/roadmap.md", "week 1"}}, "2026-08-01T00-00-00Z")
	os.Remove(md)
	os.Remove(dir)
	sha, _ := s.ledgerStoreBlob([]byte(ledgerValidSkill))
	id := ledgerAppendOK(t, s, "delete", "my-skill", ledgerItems(t, md, sha), []any{}, "", nil)
	ok, msg, err := s.LedgerRollbackEntry(context.Background(), id)
	if !ok || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
	roadmap := filepath.Join(dir, "references", "roadmap.md")
	if data, _ := os.ReadFile(roadmap); string(data) != "week 1" || !lexistsFollow(md) {
		t.Fatalf("hollow rollback: %q", data)
	}
}

func TestLedgerBackupFillDoesNotClobberDiskHash(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	dir := filepath.Join(skills, "my-skill")
	os.MkdirAll(dir, 0o755)
	live := strings.Replace(ledgerValidSkill, "Original body.", "Live body.", 1)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(live), 0o644)
	writeSkillsTarball(t, s.Home, [][2]string{{"my-skill/SKILL.md", ledgerValidSkill}, {"my-skill/references/extra.md", "from tar"}}, "2026-08-01T00-00-00Z")
	captured, err := s.LedgerSnapshotPaths(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]any{}
	for _, it := range captured {
		p, _ := it.Get("path")
		byName[filepath.Base(p.(string))], _ = it.Get("sha256")
	}
	liveHash, _ := s.ledgerStoreBlob([]byte(live))
	tarHash, _ := s.ledgerStoreBlob([]byte(ledgerValidSkill))
	extraHash, _ := s.ledgerStoreBlob([]byte("from tar"))
	if byName["SKILL.md"] != liveHash || byName["SKILL.md"] == tarHash || byName["extra.md"] != extraHash {
		t.Fatalf("%v", byName)
	}
}

func TestLedgerBackupFillIgnoresTarPathTraversal(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	dir := filepath.Join(skills, "my-skill")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(ledgerValidSkill), 0o644)
	writeSkillsTarball(t, s.Home, [][2]string{{"my-skill/SKILL.md", ledgerValidSkill}, {"my-skill/references/legit.md", "legit body"},
		{"../evil.md", "nope"}, {"my-skill/../outside.md", "nope"}}, "2026-08-01T00-00-00Z")
	captured, _ := s.LedgerSnapshotPaths(dir, true)
	legit := false
	for _, it := range captured {
		p, _ := it.Get("path")
		ps := filepath.ToSlash(p.(string))
		legit = legit || strings.HasSuffix(ps, "references/legit.md")
		if strings.HasSuffix(ps, "evil.md") || strings.HasSuffix(ps, "outside.md") {
			t.Fatalf("traversal member filled %s", ps)
		}
	}
	if !legit {
		t.Fatal("package fill did not restore the missing support file")
	}
}

func ledgerPad(pad string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("pad", pad)
	return o
}

func ledgerFileSize(t *testing.T, s *Scope) int64 {
	t.Helper()
	st, err := os.Stat(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestLedgerAutoCompactTriggersAtThreshold(t *testing.T) {
	s, _, _ := ledgerHome(t)
	s.Config["skills"] = map[string]any{"ledger_max_bytes": 8192}
	first := ledgerAppendOK(t, s, "patch", "my-skill", []any{}, []any{}, "", nil)
	raw, _ := os.ReadFile(s.LedgerPath())
	tmplV, err := pyjson.Loads(strings.SplitN(string(raw), "\n", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	tmpl := tmplV.(*pyjson.Object)
	f, _ := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	for i := 0; i < 3; i++ {
		var fat []any
		for j := 0; j < 40; j++ {
			fat = append(fat, newLedgerItem("my-skill/f"+itoa(i)+"j"+itoa(j)+".md", strings.Repeat("a", 64)))
		}
		row := pyjson.NewObject()
		for _, k := range tmpl.Keys {
			row.Set(k, tmpl.Vals[k])
		}
		row.Set("id", "legacy"+itoa(i))
		row.Set("before", fat)
		row.Set("after", append([]any{}, fat...))
		line, _ := pyjson.Dumps(row)
		f.WriteString(line + "\n")
	}
	f.Close()
	if ledgerFileSize(t, s) <= 8192 {
		t.Fatal("precondition: ledger over the cap")
	}
	last := ledgerAppendOK(t, s, "patch", "my-skill", []any{}, []any{}, "", nil)
	if ledgerFileSize(t, s) > 8192 {
		t.Fatal("sweep did not bring the ledger under the cap")
	}
	rows, _ := s.LedgerListEntries("", -1)
	ids := map[any]bool{}
	for _, r := range rows {
		id, _ := r.Get("id")
		ids[id] = true
		b, _ := r.Get("before")
		a, _ := r.Get("after")
		if len(b.([]any)) != 0 || len(a.([]any)) != 0 {
			t.Fatalf("identical manifests survived compaction %v", r)
		}
	}
	if len(ids) != 5 || !ids[first] || !ids[last] || !ids["legacy0"] || !ids["legacy1"] || !ids["legacy2"] {
		t.Fatalf("%v", ids)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestLedgerTrimOldestWhenStillOverCap(t *testing.T) {
	s, _, _ := ledgerHome(t)
	cap := map[string]any{"ledger_max_bytes": 0}
	s.Config["skills"] = cap
	newest := ""
	for i := 0; i < 5; i++ {
		before := ledgerItems(t, "my-skill/old"+itoa(i)+".md", strings.Repeat(itoa(i), 64))
		after := ledgerItems(t, "my-skill/new"+itoa(i)+".md", strings.Repeat(itoa(i+1), 64))
		newest = ledgerAppendOK(t, s, "edit", "my-skill", before, after, "", ledgerPad(strings.Repeat("y", 2048)))
	}
	f, _ := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{not json at all\n")
	f.Close()
	if ledgerFileSize(t, s) <= 8192 {
		t.Fatal("precondition: ledger over the cap")
	}
	cap["ledger_max_bytes"] = 8192
	if err := s.ledgerMaintainSize(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.LedgerListEntries("", -1)
	if id, _ := rows[0].Get("id"); len(rows) >= 5 || id != newest {
		t.Fatalf("%d rows newest %v", len(rows), id)
	}
	raw, _ := os.ReadFile(s.LedgerPath())
	if !strings.Contains(string(raw), "{not json at all") || ledgerFileSize(t, s) > 6553 {
		t.Fatalf("trim did not stop at the low water mark %d", ledgerFileSize(t, s))
	}
	compactions := 0
	realCompact := ledgerSteps.compact
	ledgerSteps.compact = func(*Scope, context.Context) (int, int, int, error) { compactions++; return 0, 0, 0, nil }
	defer func() { ledgerSteps.compact = realCompact }()
	ledgerAppendOK(t, s, "edit", "my-skill", ledgerItems(t, "my-skill/z.md", strings.Repeat("a", 64)),
		ledgerItems(t, "my-skill/z2.md", strings.Repeat("b", 64)), "", ledgerPad(strings.Repeat("z", 1000)))
	raw, _ = os.ReadFile(s.LedgerPath())
	if compactions != 0 || ledgerFileSize(t, s) > 8192 || !strings.Contains(string(raw), "{not json at all") {
		t.Fatalf("append under the cap re-ran the sweep %d", compactions)
	}
	uRow := []byte("{\"id\": \"u2028\", \"skill\": \"my-skill\", \"action\": \"edit\", \"evidence\": {\"note\": \"line one\xE2\x80\xA8line two\"}}\n")
	f, _ = os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(uRow)
	f.Close()
	if n, err := s.ledgerTrimOldest(context.Background(), int64(len(uRow)+8)); n < 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if raw, _ = os.ReadFile(s.LedgerPath()); !bytes.Equal(raw, uRow) {
		t.Fatalf("U+2028 row not kept intact %q", raw)
	}
}

type ledgerWriterKey struct{}

func TestLedgerConcurrentAppendsNeverLoseMiddleRow(t *testing.T) {
	s, _, _ := ledgerHome(t)
	s.Config["skills"] = map[string]any{"ledger_max_bytes": 4096}
	const n = 40
	ids := map[string][]string{"A": nil, "B": nil}
	bGo, bDone := make(chan struct{}), make(chan struct{})
	var bGoSet atomic.Bool
	var bDoneOnce sync.Once
	realRewrite, realTrim := ledgerSteps.rewrite, ledgerSteps.trim
	t.Cleanup(func() { ledgerSteps.rewrite, ledgerSteps.trim = realRewrite, realTrim })
	ledgerSteps.rewrite = func(ctx context.Context, path string, lines [][]byte, op string) ([]byte, error) {
		if ctx.Value(ledgerWriterKey{}) == "A" && bGoSet.CompareAndSwap(false, true) {
			close(bGo)
			select {
			case <-bDone:
			case <-time.After(time.Second):
			}
		}
		return realRewrite(ctx, path, lines, op)
	}
	var mu sync.Mutex
	var dropped []int
	ledgerSteps.trim = func(s *Scope, ctx context.Context, maxBytes int64) (int, error) {
		d, err := realTrim(s, ctx, maxBytes)
		mu.Lock()
		dropped = append(dropped, d)
		mu.Unlock()
		return d, err
	}
	const seeds = 8
	for i := 0; i < seeds; i++ {
		ledgerAppendOK(t, s, "edit", "my-skill", ledgerItems(t, "s", strings.Repeat("0", 64)), ledgerItems(t, "s", strings.Repeat("1", 64)), "", ledgerPad(strings.Repeat("x", 500)))
	}
	fresh, _ := s.ledgerStoreBlob([]byte("in-flight"))
	aged, _ := s.ledgerStoreBlob([]byte("stale orphan"))
	old := time.Now().Add(-7200 * time.Second)
	os.Chtimes(filepath.Join(s.LedgerBlobsDir(), aged), old, old)
	var wg sync.WaitGroup
	var idsMu sync.Mutex
	for _, k := range []string{"A", "B"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			if k == "B" {
				select {
				case <-bGo:
				case <-time.After(10 * time.Second):
				}
			}
			ctx := context.WithValue(context.Background(), ledgerWriterKey{}, k)
			for i := 0; i < n; i++ {
				before := ledgerItems(t, "my-skill/"+k+"-"+itoa(i)+".md", strings.Repeat("a", 64))
				after := ledgerItems(t, "my-skill/"+k+"-"+itoa(i)+".md", strings.Repeat(itoa(i%10), 64))
				id, _ := s.ledgerAppend(ctx, "edit", "my-skill", before, after, "", ledgerPad(strings.Repeat("x", 500)))
				idsMu.Lock()
				ids[k] = append(ids[k], id)
				idsMu.Unlock()
				if k == "B" {
					bDoneOnce.Do(func() { close(bDone) })
				}
			}
		}(k)
	}
	wg.Wait()
	if !bGoSet.Load() {
		t.Fatal("writer A's first append must have swept")
	}
	for k, seq := range ids {
		if len(seq) != n {
			t.Fatalf("writer %s appended %d", k, len(seq))
		}
		for _, id := range seq {
			if id == "" {
				t.Fatalf("writer %s append failed", k)
			}
		}
	}
	raw, _ := os.ReadFile(s.LedgerPath())
	var present []string
	presentSet := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := v.(*pyjson.Object).Get("id")
		present = append(present, id.(string))
		presentSet[id.(string)] = true
	}
	if len(present) == 0 {
		t.Fatal("the newest row always survives a trim")
	}
	if !lexistsFollow(filepath.Join(s.LedgerBlobsDir(), fresh)) || lexistsFollow(filepath.Join(s.LedgerBlobsDir(), aged)) {
		t.Fatal("blob GC grace window not honoured")
	}
	sum := 0
	for _, d := range dropped {
		sum += d
	}
	if len(present) != seeds+2*n-sum {
		t.Fatalf("rows silently lost: present %d seeds %d dropped %d", len(present), seeds, sum)
	}
	for k, seq := range ids {
		var survivors []string
		for _, id := range seq {
			if presentSet[id] {
				survivors = append(survivors, id)
			}
		}
		if strings.Join(survivors, ",") != strings.Join(seq[len(seq)-len(survivors):], ",") {
			t.Fatalf("writer %s: rows missing from the middle", k)
		}
	}
}

func TestLedgerEntryStoresOnlyChangedPathsAndRollbackRestores(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	before := ledgerManifest(t, s, skills, "big", map[string]string{"SKILL.md": "v1", "references/a.md": "same", "references/gone.md": "old"})
	after := ledgerManifest(t, s, skills, "big", map[string]string{"SKILL.md": "v2", "references/a.md": "same", "references/new.md": "added"})
	id := ledgerAppendOK(t, s, "patch", "big", before, after, "agent", nil)
	e := ledgerEntry(t, s, id)
	b, _ := e.Get("before")
	a, _ := e.Get("after")
	if nb, na := ledgerNames(b), ledgerNames(a); len(nb) != 2 || !nb["SKILL.md"] || !nb["gone.md"] || len(na) != 2 || !na["SKILL.md"] || !na["new.md"] {
		t.Fatalf("%v %v", nb, na)
	}
	for _, it := range after {
		p, _ := it.(*pyjson.Object).Get("path")
		sha, _ := it.(*pyjson.Object).Get("sha256")
		data, _ := s.LedgerReadBlob(sha.(string))
		os.MkdirAll(filepath.Dir(p.(string)), 0o755)
		os.WriteFile(p.(string), data, 0o644)
	}
	if ok, msg, err := s.LedgerRollbackEntry(context.Background(), id); !ok || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
	for rel, want := range map[string]string{"SKILL.md": "v1", "references/a.md": "same", "references/gone.md": "old"} {
		if data, _ := os.ReadFile(filepath.Join(skills, "big", filepath.FromSlash(rel))); string(data) != want {
			t.Fatalf("%s %q", rel, data)
		}
	}
	if lexistsFollow(filepath.Join(skills, "big", "references", "new.md")) {
		t.Fatal("created file not removed")
	}
}

func TestLedgerCompactRewritesLegacyFullManifests(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files["references/"+itoa(i)+".md"] = "same"
	}
	full := ledgerManifest(t, s, skills, "big", files)
	changedBefore := append(append([]any{}, full...), ledgerManifest(t, s, skills, "big", map[string]string{"SKILL.md": "v1"})...)
	changedAfter := append(append([]any{}, full...), ledgerManifest(t, s, skills, "big", map[string]string{"SKILL.md": "v2"})...)
	row := func(id, action string, b, a []any) string {
		o := pyjson.NewObject()
		for _, kv := range [][2]string{{"id", id}, {"ts", "2026-01-01T00:00:00+00:00"}, {"actor", "agent"}, {"action", action}, {"skill", "big"}} {
			o.Set(kv[0], kv[1])
		}
		o.Set("evidence", pyjson.NewObject())
		o.Set("before", b)
		o.Set("after", a)
		line, _ := pyjson.Dumps(o)
		return line
	}
	os.WriteFile(s.LedgerPath(), []byte(row("abc123", "patch", changedBefore, changedAfter)+"\n"+row("def456", "pre-rollback", full, full)+"\nnot json\n"), 0o644)
	sizeBefore := int(ledgerFileSize(t, s))
	entries, rawBefore, rawAfter, err := s.LedgerCompact(context.Background())
	if err != nil || entries != 2 || rawBefore != sizeBefore || float64(rawAfter) >= float64(rawBefore)*0.6 {
		t.Fatalf("%d %d %d %v", entries, rawBefore, rawAfter, err)
	}
	b, _ := ledgerEntry(t, s, "abc123").Get("before")
	if n := ledgerNames(b); len(n) != 1 || !n["SKILL.md"] {
		t.Fatalf("%v", n)
	}
	if b, _ := ledgerEntry(t, s, "def456").Get("before"); len(b.([]any)) != 50 {
		t.Fatal("pre-rollback safety entries keep their full capture")
	}
	if raw, _ := os.ReadFile(s.LedgerPath()); !strings.Contains(string(raw), "not json") {
		t.Fatal("malformed line dropped")
	}
}

func TestLedgerCompactLeavesUndecodableLedgerUntouched(t *testing.T) {
	s, _, logs := ledgerHome(t)
	os.WriteFile(s.LedgerPath(), []byte("\xff"), 0o644)
	if e, b, a, err := s.LedgerCompact(context.Background()); e != 0 || b != 0 || a != 0 || err != nil {
		t.Fatalf("%d %d %d %v", e, b, a, err)
	}
	if raw, _ := os.ReadFile(s.LedgerPath()); string(raw) != "\xff" || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("%q %s", raw, logs.String())
	}
}

func TestLedgerListEntriesUndecodableIsEmptyAndWarns(t *testing.T) {
	s, _, logs := ledgerHome(t)
	os.WriteFile(s.LedgerPath(), []byte("\xff"), 0o644)
	rows, err := s.LedgerListEntries("", -1)
	e, _ := s.LedgerGetEntry("deadbeef")
	if len(rows) != 0 || err != nil || e != nil || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("%v %v %v %s", rows, err, e, logs.String())
	}
}

func TestLedgerListEntriesSilentWhenMissing(t *testing.T) {
	s, _, logs := ledgerHome(t)
	if rows, _ := s.LedgerListEntries("", -1); len(rows) != 0 || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("%v %s", rows, logs.String())
	}
}

func ageBlob(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	aged := st.ModTime().Add(-2 * ledgerBlobGCGrace)
	os.Chtimes(p, aged, aged)
}

func TestLedgerGCBlobsRemovesOnlyUnreferenced(t *testing.T) {
	s, skills, _ := ledgerHome(t)
	kept := ledgerManifest(t, s, skills, "s", map[string]string{"SKILL.md": "v1"})
	ledgerAppendOK(t, s, "create", "s", []any{}, kept, "agent", nil)
	orphan, _ := s.ledgerStoreBlob([]byte("never referenced by any entry"))
	ageBlob(t, filepath.Join(s.LedgerBlobsDir(), orphan))
	d, freed, err := s.LedgerGCBlobs(context.Background())
	if d != 1 || freed != int64(len("never referenced by any entry")) || err != nil {
		t.Fatalf("%d %d %v", d, freed, err)
	}
	sha, _ := kept[0].(*pyjson.Object).Get("sha256")
	if data, ok := s.LedgerReadBlob(sha.(string)); lexistsFollow(filepath.Join(s.LedgerBlobsDir(), orphan)) || !ok || string(data) != "v1" {
		t.Fatal("referenced blob lost or orphan kept")
	}
	f, _ := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{broken\n")
	f.Close()
	two, _ := s.ledgerStoreBlob([]byte("orphan two"))
	ageBlob(t, filepath.Join(s.LedgerBlobsDir(), two))
	if d, freed, err := s.LedgerGCBlobs(context.Background()); d != 0 || freed != 0 || err != nil {
		t.Fatalf("%d %d %v", d, freed, err)
	}
}

func TestLedgerGCKeepsRollbackBlobsWhenLedgerCannotBeRead(t *testing.T) {
	for _, failure := range []string{"unreadable", "missing", "invalid-encoding", "malformed-json", "non-dict-row"} {
		t.Run(failure, func(t *testing.T) {
			s, skills, logs := ledgerHome(t)
			ctx := context.Background()
			md := filepath.Join(skills, "demo", "SKILL.md")
			os.MkdirAll(filepath.Dir(md), 0o755)
			os.WriteFile(md, []byte("original"), 0o644)
			before, _ := s.ledgerSnapshot(md, false)
			os.WriteFile(md, []byte("edited"), 0o644)
			id, ok := s.LedgerRecordMutation(ctx, "patch", "demo", LedgerMutation{Before: ledgerToMaps(before), BeforeCaptured: true, AfterRoot: md})
			if !ok {
				t.Fatal("record failed")
			}
			saved, _ := os.ReadFile(s.LedgerPath())
			blobsBefore := blobNames(t, s)
			switch failure {
			case "unreadable":
				os.Remove(s.LedgerPath())
				os.Mkdir(s.LedgerPath(), 0o755)
			case "missing":
				os.Remove(s.LedgerPath())
			case "malformed-json":
				os.WriteFile(s.LedgerPath(), append(append([]byte{}, saved...), "{broken\n"...), 0o644)
			case "non-dict-row":
				os.WriteFile(s.LedgerPath(), append(append([]byte{}, saved...), "[]\n"...), 0o644)
			default:
				os.WriteFile(s.LedgerPath(), []byte("\xff"), 0o644)
			}
			if d, freed, err := s.LedgerGCBlobs(ctx); d != 0 || freed != 0 || err != nil {
				t.Fatalf("%d %d %v", d, freed, err)
			}
			if !strings.Contains(logs.String(), "level=WARN") {
				t.Fatal("no warning")
			}
			after := blobNames(t, s)
			if len(after) != len(blobsBefore) {
				t.Fatalf("%v %v", after, blobsBefore)
			}
			for k, v := range blobsBefore {
				if after[k] != v {
					t.Fatalf("blob %s changed", k)
				}
			}
			os.Remove(s.LedgerPath())
			os.WriteFile(s.LedgerPath(), saved, 0o644)
			if ok, msg, err := s.LedgerRollbackEntry(ctx, id); !ok || err != nil {
				t.Fatalf("%q %v", msg, err)
			}
			if data, _ := os.ReadFile(md); string(data) != "original" {
				t.Fatalf("%q", data)
			}
		})
	}
}
