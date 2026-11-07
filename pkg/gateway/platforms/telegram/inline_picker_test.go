package telegram

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeInlineCommands struct {
	core, skills       []CatalogItem
	coreErr, skillErr  error
	platform           string
	maxSlots, descLim  int
	reserved           map[string]bool
	sanitizedViaSource string
}

func (f *fakeInlineCommands) TelegramBotCommands(context.Context) ([]CatalogItem, error) {
	return f.core, f.coreErr
}

func (f *fakeInlineCommands) GatewaySkillEntries(_ context.Context, platform string, maxSlots int, reserved map[string]bool,
	descLimit int, sanitize func(string) string) ([]CatalogItem, error) {
	f.platform, f.maxSlots, f.reserved, f.descLim = platform, maxSlots, reserved, descLimit
	f.sanitizedViaSource = sanitize("Zzz-Last")
	return f.skills, f.skillErr
}

func (f *fakeInlineCommands) SanitizeTelegramName(raw string) string {
	return strings.ReplaceAll(strings.ToLower(raw), "-", "_")
}

func useInlineCatalog(t *testing.T, items []CatalogItem) *fakeInlineCommands {
	t.Helper()
	fake := &fakeInlineCommands{core: items}
	wired := InlineCommands
	InlineCommands = fake
	t.Cleanup(func() { InlineCommands = wired })
	return fake
}

var filterCatalogFixture = []CatalogItem{
	{"help", "Show available commands"},
	{"plan", "Write a markdown implementation plan"},
	{"plugin_tools", "List plugin tools"},
	{"songsee", "Audio spectrograms and planning aids"},
}

func catalogNames(items []CatalogItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.Name)
	}
	return out
}

func TestFilterCatalogRanksPrefixThenSubstringThenDescription(t *testing.T) {
	for _, term := range []string{"", "   "} {
		if got := FilterCatalog(filterCatalogFixture, term); !reflect.DeepEqual(got, filterCatalogFixture) {
			t.Fatalf("empty term %q = %v", term, got)
		}
	}
	ranked := catalogNames(FilterCatalog(filterCatalogFixture, "plan"))
	if ranked[0] != "plan" || ranked[len(ranked)-1] != "songsee" {
		t.Fatalf("plan ranking = %v", ranked)
	}
	if got := FilterCatalog([]CatalogItem{{"gif_search", "Find GIFs"}}, "gif-se"); len(got) != 1 || got[0].Name != "gif_search" {
		t.Fatalf("hyphen term = %v", got)
	}
	if got := FilterCatalog(filterCatalogFixture, "/plan"); got[0].Name != "plan" {
		t.Fatalf("leading slash = %v", got)
	}
	if got := FilterCatalog(filterCatalogFixture, "zzzznope"); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}
	if got := catalogNames(FilterCatalog(filterCatalogFixture, "  PLU ")); !reflect.DeepEqual(got, []string{"plugin_tools"}) {
		t.Fatalf("case folded term = %v", got)
	}
	if got := catalogNames(FilterCatalog([]CatalogItem{{"a", "has-dash"}, {"b", "has_dash"}}, "has-dash")); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("description match keeps the raw term = %v", got)
	}
}

func fakeCatalog(n int) []CatalogItem {
	out := make([]CatalogItem, n)
	for i := range out {
		out[i] = CatalogItem{fmt.Sprintf("cmd-%03d", i), fmt.Sprintf("desc %d", i)}
	}
	return out
}

func TestBuildInlineResultsPagesAndCarriesArguments(t *testing.T) {
	ctx := context.Background()
	useInlineCatalog(t, fakeCatalog(PageSize+10))
	results, next := BuildInlineResults(ctx, "", "", PageSize)
	if len(results) != PageSize || next != "50" {
		t.Fatalf("first page = %d %q", len(results), next)
	}
	results, next = BuildInlineResults(ctx, "", "50", PageSize)
	if len(results) != 10 || next != "" {
		t.Fatalf("last page = %d %q", len(results), next)
	}
	useInlineCatalog(t, []CatalogItem{{"plan", "Plan mode"}})
	results, _ = BuildInlineResults(ctx, "plan migrate auth to OIDC", "", PageSize)
	if results[0].MessageText != "/plan migrate auth to OIDC" || results[0].Title != "/plan" {
		t.Fatalf("args = %+v", results[0])
	}
	if results, _ = BuildInlineResults(ctx, "plan", "", PageSize); results[0].MessageText != "/plan" {
		t.Fatalf("bare = %+v", results[0])
	}
	useInlineCatalog(t, fakeCatalog(3))
	if results, _ = BuildInlineResults(ctx, "", "not-a-number", PageSize); len(results) != 3 {
		t.Fatalf("garbage offset = %d", len(results))
	}
	useInlineCatalog(t, fakeCatalog(PageSize*2))
	page1, off := BuildInlineResults(ctx, "", "", PageSize)
	page2, _ := BuildInlineResults(ctx, "", off, PageSize)
	ids := map[string]bool{}
	for _, r := range append(page1, page2...) {
		ids[r.ID] = true
	}
	if len(ids) != PageSize*2 {
		t.Fatalf("unique ids = %d", len(ids))
	}
	long := strings.Repeat("é", 200)
	useInlineCatalog(t, []CatalogItem{{long, long}})
	results, _ = BuildInlineResults(ctx, "", "", PageSize)
	if n := len([]rune(results[0].ID)); n != 64 {
		t.Fatalf("id code points = %d", n)
	}
	if n := len([]rune(results[0].Description)); n != 100 {
		t.Fatalf("description code points = %d", n)
	}
}

func offsetCatalog(t *testing.T, n int) {
	items := make([]CatalogItem, n)
	for i := range items {
		items[i] = CatalogItem{fmt.Sprintf("cmd%03d", i), fmt.Sprintf("command %d", i)}
	}
	useInlineCatalog(t, items)
}

func TestBuildInlineResultsOffsetHandling(t *testing.T) {
	ctx := context.Background()
	offsetCatalog(t, 120)
	results, next := BuildInlineResults(ctx, "cmd", "-5", PageSize)
	first, firstNext := BuildInlineResults(ctx, "cmd", "0", PageSize)
	if !reflect.DeepEqual(results, first) || next != "50" || firstNext != "50" {
		t.Fatalf("negative offset = %q %q", next, firstNext)
	}
	for _, offset := range []string{"-1", "-50", "-1000"} {
		results, next := BuildInlineResults(ctx, "cmd", offset, PageSize)
		if len(results) != 50 || results[0].Title != "/cmd000" || next != "50" {
			t.Fatalf("offset %q = %d %q", offset, len(results), next)
		}
	}
	for _, offset := range []string{"", "abc", "3.5", "--5"} {
		if results, next := BuildInlineResults(ctx, "cmd", offset, PageSize); len(results) != 50 || next != "50" {
			t.Fatalf("malformed offset %q = %d %q", offset, len(results), next)
		}
	}
	results, next = BuildInlineResults(ctx, "cmd", "50", PageSize)
	if results[0].Title != "/cmd050" || next != "100" {
		t.Fatalf("valid offset = %q %q", results[0].Title, next)
	}
	if last, lastNext := BuildInlineResults(ctx, "cmd", "100", PageSize); len(last) != 20 || lastNext != "" {
		t.Fatalf("tail page = %d %q", len(last), lastNext)
	}
	for _, offset := range []string{"99999", "99999999999999999999999999"} {
		if results, next := BuildInlineResults(ctx, "cmd", offset, PageSize); len(results) != 0 || next != "" {
			t.Fatalf("past end %q = %d %q", offset, len(results), next)
		}
	}
	if results, next := BuildInlineResults(ctx, "cmd", " 1_0 ", PageSize); results[0].Title != "/cmd010" || next != "60" {
		t.Fatalf("python int offset = %q %q", results[0].Title, next)
	}
	offsetCatalog(t, 40)
	if results, next := BuildInlineResults(ctx, "cmd", "-3", PageSize); len(results) != 40 || next != "" {
		t.Fatalf("small catalog = %d %q", len(results), next)
	}
}

func TestCollectInlineCatalogIsUncappedAndDeduplicated(t *testing.T) {
	core := []CatalogItem{{"help", "Show help"}, {"plan", "Plan mode"}, {"help", "again"}, {"", "nameless"}}
	var skills []CatalogItem
	for i := 0; i < 70; i++ {
		skills = append(skills, CatalogItem{Name: fmt.Sprintf("filler_%02d", i), Description: "test skill"})
	}
	skills = append(skills, CatalogItem{"zzz_last_skill", ""}, CatalogItem{"plan", "skill shadow"})
	fake := useInlineCatalog(t, core)
	fake.skills = skills
	catalog := CollectInlineCatalog(context.Background())
	names := catalogNames(catalog)
	seen := map[string]bool{}
	fillers := 0
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate %q in %v", n, names)
		}
		seen[n] = true
		if strings.HasPrefix(n, "filler_") {
			fillers++
		}
	}
	if fillers != 70 || !seen["zzz_last_skill"] || !seen["help"] || !seen["plan"] || len(names) != 73 {
		t.Fatalf("catalog = %v", names)
	}
	if catalog[0] != (CatalogItem{"help", "Show help"}) || catalog[1] != (CatalogItem{"plan", "Plan mode"}) {
		t.Fatalf("core first = %v", catalog[:2])
	}
	if fake.platform != "telegram" || fake.maxSlots != -1 || fake.descLim != 100 || fake.sanitizedViaSource != "zzz_last" {
		t.Fatalf("skill collection args = %+v", fake)
	}
	if !reflect.DeepEqual(fake.reserved, map[string]bool{"help": true, "plan": true}) {
		t.Fatalf("reserved = %v", fake.reserved)
	}
	fake.coreErr, fake.core = errors.New("registry down"), nil
	if got := CollectInlineCatalog(context.Background()); len(got) != 72 || len(fake.reserved) != 0 {
		t.Fatalf("core failure = %d reserved %v", len(got), fake.reserved)
	}
	fake.coreErr, fake.core, fake.skillErr = nil, core, errors.New("skills down")
	if got := catalogNames(CollectInlineCatalog(context.Background())); !reflect.DeepEqual(got, []string{"help", "plan"}) {
		t.Fatalf("skill failure = %v", got)
	}
}

func TestCollectInlineCatalogPanicsWithoutTheWiredRegistry(t *testing.T) {
	wired := InlineCommands
	InlineCommands = nil
	defer func() {
		InlineCommands = wired
		if r := recover(); r == nil || !strings.HasPrefix(r.(string), "telegram.InlineCommands unassigned") {
			t.Fatalf("recover = %v", r)
		}
	}()
	CollectInlineCatalog(context.Background())
}
