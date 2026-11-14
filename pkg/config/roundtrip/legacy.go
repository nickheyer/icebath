package roundtrip

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Shell PATH the dotenv properties boot with
const BasePath = "/usr/bin:/bin"

// Dotenv text with quoting, self references, chains and placeholders, its keys and the shell exports before load
func GenDotenv(seed int64) (string, []string, map[string]string) {
	g := NewRNG(seed)
	p := fmt.Sprintf("C18_%d_", seed)
	shell := map[string]string{p + "SELF": "shellval", p + "SHADOW": "from-shell"}
	entries := []string{
		p + "A=plain-" + Choice(g, Words),
		fmt.Sprintf(`export %sB="double quoted # not a comment %s"`, p, Choice(g, Unicode)),
		p + "C='single ${NOT_EXPANDED} $HOME'",
		p + "D=${" + p + "A}-chained",
		fmt.Sprintf("%sE=${C18_UNDEFINED_%d}", p, seed),
		fmt.Sprintf("PATH=/c18/%d/bin:${PATH}", seed),
		p + "SELF=pre-${" + p + "SELF}",
		p + "SP = spaced value",
		p + "U=" + Choice(g, Unicode),
		p + "EQ=a=b=c",
		p + "SHADOW=from-dotenv",
		p + `ESC="tab\tnewline\n"`,
		p + "CHAIN2=${" + p + "D}/${" + p + "U}",
	}
	Shuffle(g, entries)
	keyOf := func(e string) string {
		k, _, _ := strings.Cut(e, "=")
		return strings.TrimSpace(strings.ReplaceAll(k, "export ", ""))
	}
	rank := map[string]int{p + "A": 0, p + "U": 0, p + "D": 1, p + "CHAIN2": 2}
	sort.SliceStable(entries, func(i, j int) bool { return rank[keyOf(entries[i])] < rank[keyOf(entries[j])] })
	lines := []string{"# C18 generated .env", ""}
	keySet := map[string]bool{}
	for _, e := range entries {
		if g.Random() < 0.2 {
			lines = append(lines, "# note")
		}
		lines = append(lines, e)
		keySet[keyOf(e)] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(lines, "\n") + "\n", keys, shell
}

// Leaf with the value a property plants
type Planted struct {
	Path  []string
	Value any
}

// Old defaults the migration ladder rewrites
var Stale = []Planted{
	{[]string{"model_catalog", "ttl_hours"}, 24},
	{[]string{"agent", "verify_on_stop"}, "auto"},
	{[]string{"display", "background_process_notifications"}, "all"},
	{[]string{"delegation", "max_iterations"}, 50},
	{[]string{"delegation", "max_concurrent_children"}, 3},
	{[]string{"curator", "stale_after_days"}, 30},
	{[]string{"curator", "archive_after_days"}, 90},
}

// User values no migration may clobber
var Canaries = []Planted{
	{[]string{"model", "default"}, "c18/canary-model"},
	{[]string{"terminal", "timeout"}, 4321},
	{[]string{"c18_custom_root"}, "keep me — ü 😀"},
	{[]string{"approvals", "mode"}, "off"},
	{[]string{"agent", "system_prompt"}, "Réponds brièvement."},
}

func structural() []Planted {
	return []Planted{
		{[]string{"stt"}, map[string]any{"provider": "local", "model": "base"}},
		{[]string{"display", "tool_progress_overrides"}, map[string]any{"telegram": "all"}},
		{[]string{"compression", "summary_model"}, "c18/summary"},
		{[]string{"plugins", "disabled"}, []any{"c18-off"}},
		{[]string{"memory", "write_mode"}, "approve"},
		{[]string{"delegation", "max_async_children"}, 5},
		{[]string{"display", "personality"}, "kawaii"},
		{[]string{"platform_toolsets"}, map[string]any{"cli": []any{"terminal", "bfl"}}},
		{[]string{"mcp_servers"}, map[string]any{"c18srv": map[string]any{"command": "c18-mcp-server", "args": []any{"--stdio"}, "disabled": true}}},
		{[]string{"cron", "model_drift_guard"}, true},
		{[]string{"gateway", "multiplex_profile_allowlist"}, []any{"c18"}},
		{[]string{"providers"}, map[string]any{"acme.v1": map[string]any{"base_url": "http://127.0.0.1:9/v1", "api_mode": "chat_completions"}}},
	}
}

// Ordered node of a plain value, mapping keys sorted for a stable dump
func toNode(v any) any {
	switch x := v.(type) {
	case map[string]any:
		n := newNode()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.set(k, toNode(x[k]))
		}
		return n
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toNode(e)
		}
		return out
	}
	return v
}

// Legacy config text at a stamp (nil for unversioned) and the stale leaves it customised
func GenLegacy(version *int, seed int64) (string, []Planted, error) {
	g := NewRNG(seed)
	tree := newNode()
	var customized []Planted
	for _, st := range Stale {
		if g.Random() < 0.5 {
			tree.setPath(st.Path, st.Value)
			continue
		}
		val := any("c18-custom")
		if _, ok := st.Value.(int); ok {
			val = 77
		}
		tree.setPath(st.Path, val)
		customized = append(customized, Planted{st.Path, val})
	}
	for _, c := range Canaries {
		tree.setPath(c.Path, c.Value)
	}
	for _, st := range structural() {
		if g.Random() < 0.8 {
			tree.setPath(st.Path, toNode(st.Value))
		}
	}
	if version == nil {
		tree.set("custom_providers", []any{toNode(map[string]any{"name": "C18 Local", "base_url": "http://127.0.0.1:9/v1"})})
	} else {
		tree.set("_config_version", *version)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(tree.yamlNode()); err != nil {
		return "", nil, err
	}
	text := "# C18 legacy config — this comment must survive migration\n" + buf.String()
	text = strings.Replace(text, "\nterminal:\n", "\n# terminal: user note\nterminal:\n", 1)
	return text, customized, nil
}

// One home a migration property drives, the writer seeding config.yaml and .env and the migrate_config call
type MigrationHome struct {
	WriteConfig func(text string) error
	WriteEnv    func(text string) error
	Migrate     func() error
	ReadConfig  func() (string, error)
	ReadEnv     func() (string, error)
	Load        func([]byte) (any, error)
}

// Error unless migrating a legacy config stamps latest, keeps user values and comments and reapplying changes nothing
func MigrationProperty(m MigrationHome, version *int, seed int64, latest int) error {
	text, customized, err := GenLegacy(version, seed)
	if err != nil {
		return err
	}
	ctx := fmt.Sprintf("from unversioned seed=%d", seed)
	if version != nil {
		ctx = fmt.Sprintf("from v%d seed=%d", *version, seed)
	}
	if err := m.WriteConfig(text); err != nil {
		return err
	}
	if err := m.WriteEnv("LLM_MODEL=legacy-model\nC18_KEEP=1\n"); err != nil {
		return err
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("[%s] migrate: %w", ctx, err)
	}
	once, err := m.ReadConfig()
	if err != nil {
		return err
	}
	envOnce, err := m.ReadEnv()
	if err != nil {
		return err
	}
	parsed, err := m.Load([]byte(once))
	if err != nil {
		return fmt.Errorf("[%s] %v:\n%s", ctx, err, once)
	}
	if v := Get(parsed, []string{"_config_version"}); !Same(v, latest) {
		return fmt.Errorf("[%s] not stamped latest:\n%s", ctx, once)
	}
	for _, p := range append(slices.Clone(Canaries), customized...) {
		if v := Get(parsed, p.Path); !Same(v, p.Value) {
			return fmt.Errorf("[%s] migration clobbered user value %v: %#v != %#v\n%s", ctx, p.Path, v, p.Value, once)
		}
	}
	for _, comment := range []string{"# C18 legacy config", "# terminal: user note"} {
		if !strings.Contains(once, comment) {
			return fmt.Errorf("[%s] migration dropped the comment %q:\n%s", ctx, comment, once)
		}
	}
	if err := NoDuplicateTopLevel(once, ctx); err != nil {
		return err
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("[%s] second migrate: %w", ctx, err)
	}
	if again, _ := m.ReadConfig(); again != once {
		return fmt.Errorf("[%s] a second migrate changed the file:\n%s", ctx, UDiff(once, again))
	}
	if envAgain, _ := m.ReadEnv(); envAgain != envOnce {
		return fmt.Errorf("[%s] a second migrate changed .env:\n%s", ctx, UDiff(envOnce, envAgain))
	}
	if version == nil {
		return nil
	}
	if err := m.WriteConfig(Restamp(once, *version)); err != nil {
		return err
	}
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("[%s] rewound migrate: %w", ctx, err)
	}
	after, _ := m.ReadConfig()
	again, err := m.Load([]byte(after))
	if err != nil || !Equal(again, parsed) {
		return fmt.Errorf("[%s] re-running the steps on migrated output changed %v:\n%s", ctx, Paths(ChangedPaths(parsed, again)), UDiff(once, after))
	}
	return nil
}

// Every historical start stamp at or above the support floor, nil first for the unversioned ladder
func MigrationStarts(versions []int, floor int) []*int {
	set := map[int]bool{}
	for _, v := range versions {
		if v-1 >= floor {
			set[v-1] = true
		}
	}
	starts := make([]int, 0, len(set))
	for v := range set {
		starts = append(starts, v)
	}
	sort.Ints(starts)
	out := []*int{nil}
	for i := range starts {
		out = append(out, &starts[i])
	}
	return out
}

// Subtest name and seed of a migration start
func MigrationStartName(version *int) (string, int64) {
	if version == nil {
		return "unversioned", 900
	}
	return fmt.Sprintf("from_v%d", *version), int64(900 + *version)
}
