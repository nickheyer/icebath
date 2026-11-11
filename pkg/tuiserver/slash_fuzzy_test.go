package tuiserver

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
)

// Completion item with the kind the ranker reads
func fuzzyItem(text, meta, kind string) map[string]any {
	return map[string]any{"text": text, "display": text, "meta": meta, "kind": kind}
}

// Texts of ranked items in order
func itemTexts(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item["text"].(string)
	}
	return out
}

// Usage lookup over a fixed table, zero for any other name
func usageTable(table map[string]int64) func(string) *big.Int {
	return func(name string) *big.Int { return big.NewInt(table[name]) }
}

func userOrigin(string) string { return "user" }

func TestNormalizeSlashSearchQuery(t *testing.T) {
	for in, want := range map[string]string{" /Model ": "model", "//help": "help", "plain": "plain"} {
		if got := normalizeSlashSearchQuery(in); got != want {
			t.Fatalf("%q -> %q", in, got)
		}
	}
}

func TestNameMatchBeatsDescriptionMatch(t *testing.T) {
	if got := scoreSlashCompletionItem(fuzzyItem("/recap", "Turn session recaps on/off", "command"), "recap"); got != 0 {
		t.Fatalf("score %v", got)
	}
}

func TestFuzzyRankMergesDescriptionMatchesFromCatalog(t *testing.T) {
	prefixHits := []map[string]any{fuzzyItem("/summon", "", "command")}
	catalog := []map[string]any{fuzzyItem("/summon", "", "command"), fuzzyItem("/recaps", "Show a summary of the session", "command"),
		fuzzyItem("/help", "Show available commands", "command")}
	ranked, scoreOf := fuzzyRankSlashItems(prefixHits, catalog, "summ")
	if !slices.Equal(itemTexts(ranked), []string{"/summon", "/recaps"}) {
		t.Fatalf("ranked %v", itemTexts(ranked))
	}
	if scoreOf(ranked[0]) != 1 || scoreOf(ranked[1]) != 4 {
		t.Fatalf("scores %v %v", scoreOf(ranked[0]), scoreOf(ranked[1]))
	}
	if !math.IsInf(scoreOf(fuzzyItem("/help", "Show available commands", "command")), 1) {
		t.Fatal("an item outside the merge has a score")
	}
}

func TestFuzzyRankIsStableWithinATier(t *testing.T) {
	ranked, _ := fuzzyRankSlashItems([]map[string]any{fuzzyItem("/mod-b", "", "command"), fuzzyItem("/mod-a", "", "command")}, nil, "mod")
	if !slices.Equal(itemTexts(ranked), []string{"/mod-b", "/mod-a"}) {
		t.Fatalf("ranked %v", itemTexts(ranked))
	}
}

func TestFuzzyRankDropsNonMatchingPrefixRows(t *testing.T) {
	ranked, _ := fuzzyRankSlashItems([]map[string]any{fuzzyItem("/other", "", "command")}, nil, "model")
	if len(ranked) != 0 {
		t.Fatalf("ranked %v", ranked)
	}
}

func TestRankSlashCompletionsUsesScoreBeforeUsage(t *testing.T) {
	nameHit := fuzzyItem("/summarize", "Condense text", "skill")
	descHit := fuzzyItem("/notes", "Write a summary of a meeting", "skill")
	items := []map[string]any{descHit, nameHit}
	usage := usageTable(map[string]int64{"notes": 50, "summarize": 1})
	scores := map[uintptr]float64{itemID(nameHit): 1.0, itemID(descHit): 4.0}
	scoreOf := func(item map[string]any) float64 {
		if s, ok := scores[itemID(item)]; ok {
			return s
		}
		return math.Inf(1)
	}
	if got := itemTexts(rankSlashCompletions(items, usage, userOrigin, false, scoreOf, nil)); !slices.Equal(got, []string{"/summarize", "/notes"}) {
		t.Fatalf("scored %v", got)
	}
	if got := itemTexts(rankSlashCompletions(items, usage, userOrigin, false, nil, nil)); !slices.Equal(got, []string{"/notes", "/summarize"}) {
		t.Fatalf("plain %v", got)
	}
}

func TestRankSlashCompletionsDoesNotTruncateCommandsWhenBrowsing(t *testing.T) {
	var items []map[string]any
	registry := map[string]bool{}
	for i := range 40 {
		items = append(items, fuzzyItem(fmt.Sprintf("/cmd%d", i), "", "command"))
		registry[fmt.Sprintf("cmd%d", i)] = true
	}
	ranked := rankSlashCompletions(items, usageTable(nil), userOrigin, true, nil, registry)
	if len(ranked) != 40 || ranked[39]["text"] != "/cmd39" {
		t.Fatalf("ranked %v", itemTexts(ranked))
	}
}

func TestRankSlashCompletionsStillCapsPluginCommandsWhenBrowsing(t *testing.T) {
	var items []map[string]any
	registry := map[string]bool{}
	for i := range 40 {
		items = append(items, fuzzyItem(fmt.Sprintf("/reg%d", i), "", "command"))
		registry[fmt.Sprintf("reg%d", i)] = true
	}
	for i := range 40 {
		items = append(items, fuzzyItem(fmt.Sprintf("/plugin%d", i), "", "command"))
	}
	texts := itemTexts(rankSlashCompletions(items, usageTable(nil), userOrigin, true, nil, registry))
	for i := range 40 {
		if texts[i] != fmt.Sprintf("/reg%d", i) {
			t.Fatalf("registry order %v", texts[:40])
		}
	}
	plugins := texts[40:]
	if len(plugins) != 30 {
		t.Fatalf("plugin rows %d", len(plugins))
	}
	for i, text := range plugins {
		if text != fmt.Sprintf("/plugin%d", i) {
			t.Fatalf("plugin order %v", plugins)
		}
	}
}

func TestRankSlashCompletionsDefaultsToTheRealCommandRegistry(t *testing.T) {
	saved := GatewayKnownCommands
	GatewayKnownCommands = commands.GatewayKnownCommands
	t.Cleanup(func() { GatewayKnownCommands = saved })
	items := []map[string]any{fuzzyItem("/help", "", "command")}
	for i := range 40 {
		items = append(items, fuzzyItem(fmt.Sprintf("/plugin%d", i), "", "command"))
	}
	texts := itemTexts(rankSlashCompletions(items, usageTable(nil), userOrigin, true, nil, nil))
	plugins := 0
	for _, text := range texts {
		if strings.HasPrefix(text, "/plugin") {
			plugins++
		}
	}
	if texts[0] != "/help" || plugins != 30 {
		t.Fatalf("ranked %v", texts)
	}
}

func TestRankSlashCompletionsStillCapsCommandsWhenSearching(t *testing.T) {
	var items []map[string]any
	for i := range 40 {
		items = append(items, fuzzyItem(fmt.Sprintf("/cmd%d", i), "", "command"))
	}
	if ranked := rankSlashCompletions(items, usageTable(nil), userOrigin, false, nil, nil); len(ranked) != 30 {
		t.Fatalf("ranked %d", len(ranked))
	}
}

func TestRankSlashCompletionsTiesBreakOnUsageThenName(t *testing.T) {
	items := []map[string]any{fuzzyItem("/beta", "", "skill"), fuzzyItem("/alpha", "", "skill")}
	ranked := rankSlashCompletions(items, usageTable(map[string]int64{"alpha": 3, "beta": 3}), userOrigin, false, func(map[string]any) float64 { return 1.0 }, nil)
	if got := itemTexts(ranked); !slices.Equal(got, []string{"/alpha", "/beta"}) {
		t.Fatalf("ranked %v", got)
	}
}

func TestRepoFileCacheSweepsExpiredRootsOnWrite(t *testing.T) {
	now := time.Unix(1000, 0)
	savedClock := fuzzyClock
	fuzzyClock = func() time.Time { return now }
	t.Cleanup(func() { fuzzyClock = savedClock })
	var roots []string
	for i := range 3 {
		root := filepath.Join(t.TempDir(), fmt.Sprintf("wt%d", i))
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "a.py"), "")
		roots = append(roots, root)
	}
	clearFuzzyCache := func() {
		fuzzyCacheMu.Lock()
		clear(fuzzyCache)
		fuzzyCacheMu.Unlock()
	}
	clearFuzzyCache()
	t.Cleanup(clearFuzzyCache)
	listRepoFiles(roots[0])
	now = now.Add(fuzzyCacheTTL / 2)
	listRepoFiles(roots[1])
	now = now.Add(fuzzyCacheTTL/2 + 100*time.Millisecond)
	listRepoFiles(roots[2])
	fuzzyCacheMu.Lock()
	cached := slices.Sorted(maps.Keys(fuzzyCache))
	fuzzyCacheMu.Unlock()
	if !slices.Equal(cached, []string{roots[1], roots[2]}) {
		t.Fatalf("cached roots %v", cached)
	}
}
