package memory

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const journeySkill = "---\nname: my-skill\ndescription: A test skill.\n---\n\n# My Skill\n\nBody.\n"

func journeyHome(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, h := learningHome(t)
	writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), []byte("alpha note\nline two\n\u00a7\nbeta note"))
	writeTestFile(t, filepath.Join(h, "memories", "USER.md"), []byte("user profile note"))
	writeTestFile(t, filepath.Join(h, "skills", "my-skill", "SKILL.md"), []byte(journeySkill))
	return ctx, h
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustNode(t *testing.T) func(map[string]any, error) map[string]any {
	return func(out map[string]any, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func freshStore(t *testing.T, h string) *Store {
	t.Helper()
	s := NewStore(home.MemoriesDir(h), DefaultMemoryCharLimit, DefaultUserCharLimit, true, true)
	if err := s.LoadFromDisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseNodeKind(t *testing.T) {
	for id, want := range map[string]string{"memory:memory:0": "memory", "memory:profile:3": "memory", "debugging-icebath": "skill"} {
		if got := ParseNodeKind(id); got != want {
			t.Fatalf("ParseNodeKind(%q) = %q", id, got)
		}
	}
}

func TestEditMemoryReplacesChunk(t *testing.T) {
	ctx, h := journeyHome(t)
	if res := mustNode(t)(EditNode(ctx, nil, "memory:profile:2", "rewritten profile")); res["ok"] != true {
		t.Fatalf("edit = %v", res)
	}
	if got := strings.TrimSpace(readText(t, filepath.Join(h, "memories", "USER.md"))); got != "rewritten profile" {
		t.Fatalf("USER.md = %q", got)
	}
}

func TestEditMemoryRefusesAnEntryOverTheMemoryToolsLimit(t *testing.T) {
	ctx, h := journeyHome(t)
	res := mustNode(t)(EditNode(ctx, nil, "memory:profile:2", strings.Repeat("x", 5000)))
	if res["ok"] != false || !strings.Contains(pyStr(res["message"]), "Shorten") {
		t.Fatalf("edit = %v", res)
	}
	userPath := filepath.Join(h, "memories", "USER.md")
	if readText(t, userPath) != "user profile note" {
		t.Fatal("USER.md changed")
	}
	r, err := freshStore(t, h).Remove(ctx, "user", "user profile note", nil)
	if err != nil || !r.Success() {
		t.Fatalf("remove = %v, %v", r, err)
	}
	var entries []string
	for i := 0; i < 4; i++ {
		entries = append(entries, "entry "+string(rune('0'+i))+" "+strings.Repeat("y", 500))
	}
	writeTestFile(t, userPath, []byte(strings.Join(entries, EntryDelimiter)))
	if res := mustNode(t)(DeleteNode(ctx, nil, "memory:profile:2")); res["ok"] != true {
		t.Fatalf("delete = %v", res)
	}
	if n := len(ReadEntriesFile(userPath)); n != 3 {
		t.Fatalf("entries = %d", n)
	}
}

func TestSkillDetailReturnsSkillMD(t *testing.T) {
	ctx, _ := journeyHome(t)
	d := mustNode(t)(NodeDetail(ctx, nil, "my-skill"))
	if d["ok"] != true || d["kind"] != "skill" || !strings.Contains(d["content"].(string), "name: my-skill") {
		t.Fatalf("detail = %v", d)
	}
}

func TestMemoryDetailAndMissingSkill(t *testing.T) {
	ctx, _ := journeyHome(t)
	d := mustNode(t)(NodeDetail(ctx, nil, "memory:memory:0"))
	if d["ok"] != true || d["label"] != "alpha note" || d["content"] != "alpha note\nline two" {
		t.Fatalf("detail = %v", d)
	}
	d = mustNode(t)(NodeDetail(ctx, nil, "nope"))
	if d["ok"] != false || d["message"] != "skill 'nope' not found" {
		t.Fatalf("detail = %v", d)
	}
	d = mustNode(t)(NodeDetail(ctx, nil, "memory:bogus:0"))
	if d["ok"] != false || d["message"] != "bad memory node id: 'memory:bogus:0'" {
		t.Fatalf("detail = %v", d)
	}
	d = mustNode(t)(NodeDetail(ctx, nil, "memory:memory:9"))
	if d["ok"] != false || d["message"] != "memory index 9 out of range" {
		t.Fatalf("detail = %v", d)
	}
}

func TestDeletePinnedSkillRefused(t *testing.T) {
	ctx, h := journeyHome(t)
	writeTestFile(t, filepath.Join(h, "skills", ".usage.json"), []byte(`{"my-skill": {"pinned": true}}`))
	res := mustNode(t)(DeleteNode(ctx, nil, "my-skill"))
	if res["ok"] != false || !strings.Contains(pyStr(res["message"]), "pinned") {
		t.Fatalf("delete = %v", res)
	}
	if _, err := os.Stat(filepath.Join(h, "skills", "my-skill")); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyMemoryEditRefusedAfterIDCheck(t *testing.T) {
	ctx, _ := journeyHome(t)
	if res := mustNode(t)(EditNode(ctx, nil, "memory:memory:0", "   ")); res["message"] != "empty memory \u2014 use delete to remove it" {
		t.Fatalf("edit = %v", res)
	}
	if res := mustNode(t)(EditNode(ctx, nil, "memory:x", "   ")); res["message"] != "bad memory node id: 'memory:x'" {
		t.Fatalf("edit = %v", res)
	}
}

func TestMemoryWritesMatchMemoryToolFormat(t *testing.T) {
	ctx, h := journeyHome(t)
	if res := mustNode(t)(EditNode(ctx, nil, "memory:memory:0", "alpha rewritten")); res["ok"] != true {
		t.Fatalf("edit = %v", res)
	}
	path := filepath.Join(h, "memories", "MEMORY.md")
	entries := ReadEntriesFile(path)
	if !reflect.DeepEqual(entries, []string{"alpha rewritten", "beta note"}) {
		t.Fatalf("entries = %q", entries)
	}
	if readText(t, path) != platformNewlines(strings.Join(entries, EntryDelimiter)) {
		t.Fatalf("file = %q", readText(t, path))
	}
	writeTestFile(t, path, []byte("\xef\xbb\xbfalpha note\n\u00a7\nbeta note"))
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	for _, n := range graph["nodes"].([]map[string]any) {
		if n["kind"] == "memory" {
			first = n
			break
		}
	}
	if first["label"] != "alpha note" {
		t.Fatalf("label = %q", first["label"])
	}
	if res := mustNode(t)(EditNode(ctx, nil, first["id"].(string), "alpha sans bom")); res["ok"] != true {
		t.Fatalf("edit = %v", res)
	}
	if got := ReadEntriesFile(path); !reflect.DeepEqual(got, []string{"alpha sans bom", "beta note"}) {
		t.Fatalf("entries = %q", got)
	}
}

// Lock respecting memory tool writer that appends the moment the journey mutation resolves its id
func raceMemoryToolAdd(t *testing.T, h, content string) *sync.WaitGroup {
	t.Helper()
	located, landed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	real := memoryCardsReader
	memoryCardsReader = func(h string) ([]map[string]any, error) {
		cards, err := real(h)
		once.Do(func() { close(located) })
		select {
		case <-landed:
		case <-time.After(500 * time.Millisecond):
		}
		return cards, err
	}
	t.Cleanup(func() { memoryCardsReader = real })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-located:
		case <-time.After(5 * time.Second):
		}
		s := NewStore(home.MemoriesDir(h), DefaultMemoryCharLimit, DefaultUserCharLimit, true, true)
		if err := s.LoadFromDisk(context.Background()); err == nil {
			_, _ = s.Add(context.Background(), "memory", content)
		}
		close(landed)
	}()
	return &wg
}

func TestDeleteMemoryKeepsConcurrentMemoryToolAdd(t *testing.T) {
	ctx, h := journeyHome(t)
	writer := raceMemoryToolAdd(t, h, "gamma note")
	if res := mustNode(t)(DeleteNode(ctx, nil, "memory:memory:0")); res["ok"] != true {
		t.Fatalf("delete = %v", res)
	}
	writer.Wait()
	if got := ReadEntriesFile(filepath.Join(h, "memories", "MEMORY.md")); !reflect.DeepEqual(got, []string{"beta note", "gamma note"}) {
		t.Fatalf("entries = %q", got)
	}
}

func TestEditMemoryKeepsConcurrentMemoryToolAdd(t *testing.T) {
	ctx, h := journeyHome(t)
	writer := raceMemoryToolAdd(t, h, "gamma note")
	if res := mustNode(t)(EditNode(ctx, nil, "memory:memory:0", "alpha rewritten")); res["ok"] != true {
		t.Fatalf("edit = %v", res)
	}
	writer.Wait()
	want := []string{"alpha rewritten", "beta note", "gamma note"}
	if got := ReadEntriesFile(filepath.Join(h, "memories", "MEMORY.md")); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %q", got)
	}
}

func TestMemoryDriftIsRefusedWithBackupLikeMemoryTool(t *testing.T) {
	ctx, h := journeyHome(t)
	path := filepath.Join(h, "memories", "MEMORY.md")
	raw := "alpha note\n\u00a7\nbeta note\n\n## Vendor Master\n" + strings.Repeat("x", 2300) + "\n"
	writeTestFile(t, path, []byte(raw))
	res := mustNode(t)(EditNode(ctx, nil, "memory:memory:0", "alpha rewritten"))
	if res["ok"] != false || readText(t, path) != raw {
		t.Fatalf("edit = %v", res)
	}
	backups, _ := filepath.Glob(filepath.Join(h, "memories", "MEMORY.md.bak.*"))
	if len(backups) != 1 || readText(t, backups[0]) != raw {
		t.Fatalf("backups = %v", backups)
	}
	if want := driftError(path, backups[0]).Data["error"]; res["message"] != want {
		t.Fatalf("message = %v, want %v", res["message"], want)
	}
	if res := mustNode(t)(DeleteNode(ctx, nil, "memory:memory:0")); res["ok"] != true {
		t.Fatalf("delete = %v", res)
	}
	want := strings.SplitN(strings.TrimSpace(raw), "\n\u00a7\n", 2)[1]
	if got := readText(t, path); got != platformNewlines(want) {
		t.Fatalf("file = %q", got)
	}
}

func TestUnreadableMemoryFileIsRefusedUnchanged(t *testing.T) {
	ctx, h := journeyHome(t)
	path := filepath.Join(h, "memories", "MEMORY.md")
	raw := []byte("alpha note\n\xc3\x28\n\xc2\xa7\nbeta")
	writeTestFile(t, path, raw)
	res := mustNode(t)(DeleteNode(ctx, nil, "memory:memory:0"))
	if res["ok"] != false || !strings.Contains(pyStr(res["message"]), "could not be read") {
		t.Fatalf("delete = %v", res)
	}
	if readText(t, path) != string(raw) {
		t.Fatal("file changed")
	}
}

func journeyID(t *testing.T, ctx context.Context, h, label string) string {
	t.Helper()
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	for _, n := range graph["nodes"].([]map[string]any) {
		if n["kind"] == "memory" {
			nodes = append(nodes, n)
		}
	}
	for i, entry := range ReadEntriesFile(filepath.Join(h, "memories", "MEMORY.md")) {
		if strings.HasPrefix(entry, label) {
			return nodes[i]["id"].(string)
		}
	}
	t.Fatalf("no card starts with %q", label)
	return ""
}

func TestMutationTargetsTheClickedCardWhenTheListShiftedBeforeSubmit(t *testing.T) {
	for _, op := range []string{"edit", "delete"} {
		t.Run(op, func(t *testing.T) {
			ctx, h := journeyHome(t)
			nodeID := journeyID(t, ctx, h, "beta")
			writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), []byte(strings.Join([]string{"beta note", "gamma note"}, EntryDelimiter)))
			mutate := func() map[string]any {
				if op == "edit" {
					return mustNode(t)(EditNode(ctx, nil, nodeID, "beta rewritten"))
				}
				return mustNode(t)(DeleteNode(ctx, nil, nodeID))
			}
			if res := mutate(); res["ok"] != true {
				t.Fatalf("first = %v", res)
			}
			want := []string{"gamma note"}
			if op == "edit" {
				want = []string{"beta rewritten", "gamma note"}
			}
			if got := ReadEntriesFile(filepath.Join(h, "memories", "MEMORY.md")); !reflect.DeepEqual(got, want) {
				t.Fatalf("entries = %q", got)
			}
			res := mutate()
			if res["ok"] != false || !strings.Contains(pyStr(res["message"]), "stale") {
				t.Fatalf("second = %v", res)
			}
		})
	}
}
