package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config/roundtrip"
)

var rtSchema = sync.OnceValue(func() *roundtrip.Schema {
	return roundtrip.NewSchema(map[string]any(Defaults()), MappingKeys, FastSafeLoad)
})

func rtGen(t *testing.T, seed int64, o roundtrip.GenOptions) *roundtrip.Case {
	t.Helper()
	c, err := rtSchema().GenCase(seed, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Writes config text on a new inode and drops the load caches, the last good copy unless kept
func rtWrite(t *testing.T, h, text string, keepLastKnownGood bool) {
	t.Helper()
	path := filepath.Join(h, "config.yaml")
	tmp := filepath.Join(h, ".config.yaml.c18tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	loadCacheMu.Lock()
	loadCache, rawCache = map[string]*loadEntry{}, map[string]rawEntry{}
	if !keepLastKnownGood {
		lastExpanded = map[string]map[string]any{}
	}
	loadCacheMu.Unlock()
}

// Plants a case's env references and its text as the home config
func rtInstall(t *testing.T, h string, c *roundtrip.Case) string {
	t.Helper()
	for k, v := range c.Env {
		t.Setenv(k, v)
	}
	rtWrite(t, h, c.Text, false)
	return filepath.Join(h, "config.yaml")
}

func rtParse(t *testing.T, path string) (string, any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := FastSafeLoad(data)
	if err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	return string(data), doc
}

func rtLoad(t *testing.T, h string) map[string]any {
	t.Helper()
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any(cfg)
}

func rtSave(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	if err := SaveConfig(context.Background(), h, cfg, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Value the loader yields for a file leaf, a reference replaced by its env value
func rtExpanded(c *roundtrip.Case, v any) any {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "${") {
		if e, has := c.Env[strings.TrimSuffix(s[2:], "}")]; has {
			return e
		}
	}
	return v
}

func rtNoopSaveRoundtrip(t *testing.T, h string, c *roundtrip.Case) {
	t.Helper()
	path := rtInstall(t, h, c)
	loaded := rtLoad(t, h)
	for k, v := range roundtrip.Leaves(c.Tree) {
		p := roundtrip.Split(k)
		if got := roundtrip.Get(loaded, p); !roundtrip.Equal(got, rtExpanded(c, v)) {
			t.Fatalf("[seed=%d] load_config()%v = %#v, file says %#v", c.Seed, p, got, v)
		}
	}
	captureFacade(t)
	rtSave(t, h, loaded)
	after, got := rtParse(t, path)
	if !roundtrip.Equal(got, c.Tree) {
		t.Fatalf("[seed=%d] load(save(x)) != x:\n%s", c.Seed, roundtrip.UDiff(c.Text, after))
	}
	if touched, inserted := roundtrip.LineDiff(c.Text, after); len(touched) > 0 || len(inserted) > 0 {
		t.Fatalf("[seed=%d] a no-op save changed bytes:\n%s", c.Seed, roundtrip.UDiff(c.Text, after))
	}
	for name, secret := range c.Env {
		if strings.Contains(after, secret) {
			t.Fatalf("[seed=%d] expanded ${%s} leaked into config.yaml", c.Seed, name)
		}
	}
}

func TestP1NoopSaveIsByteIdentical(t *testing.T) {
	for seed := int64(101); seed <= 112; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			rtNoopSaveRoundtrip(t, h, rtGen(t, seed, roundtrip.GenOptions{NoNullLeaves: true}))
		})
	}
}

func TestP1ExplicitNullLeavesSurviveANoopSave(t *testing.T) {
	for _, seed := range []int64{101, 102} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			c := rtGen(t, seed, roundtrip.GenOptions{})
			hasNull := false
			for _, v := range roundtrip.Leaves(c.Tree) {
				hasNull = hasNull || v == nil
			}
			if !hasNull {
				t.Fatalf("seed %d generated no null leaf", seed)
			}
			rtNoopSaveRoundtrip(t, h, c)
		})
	}
}

func TestP1LongScalarsSurviveANoopSave(t *testing.T) {
	for _, seed := range []int64{201, 202, 203, 204} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			rtNoopSaveRoundtrip(t, h, rtGen(t, seed, roundtrip.GenOptions{Long: true, NoNullLeaves: true}))
		})
	}
}

// Scalar leaf paths of a tree, empty mappings left out
func rtScalarLeaves(tree map[string]any) [][]string {
	var out [][]string
	for k := range roundtrip.Leaves(tree) {
		p := roundtrip.Split(k)
		if _, isMap := roundtrip.Get(tree, p).(map[string]any); !isMap {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b []string) int { return strings.Compare(roundtrip.Key(a...), roundtrip.Key(b...)) })
	return out
}

func TestP1SaveOfOneChangedKeyTouchesOnlyThatKey(t *testing.T) {
	s := rtSchema()
	for seed := int64(101); seed <= 112; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			captureFacade(t)
			c := rtGen(t, seed, roundtrip.GenOptions{NoNullLeaves: true})
			g := roundtrip.NewRNG(seed * 7919)
			var present, absent [][]string
			for _, p := range c.LeafOrder {
				if p[0] != "_config_version" && p[0] != "providers" {
					present = append(present, p)
				}
			}
			scalars := rtScalarLeaves(c.Tree)
			for _, l := range s.GenLeaves {
				if roundtrip.Get(c.Tree, l.Path) == roundtrip.Missing && !roundtrip.Under(l.Path, scalars) {
					absent = append(absent, l.Path)
				}
			}
			for round := 0; round < 4; round++ {
				path := rtInstall(t, h, c)
				target := roundtrip.Choice(g, present)
				if round%2 == 1 {
					target = roundtrip.Choice(g, absent)
				}
				old := roundtrip.Get(c.Tree, target)
				dflt := roundtrip.Missing
				if old == roundtrip.Missing {
					dflt = s.Default(target)
				}
				next := old
				for roundtrip.Equal(next, old) || roundtrip.Equal(next, dflt) || strings.HasPrefix(fmt.Sprint(next), "${") {
					next = roundtrip.ValueFor(s.Default(target), g, false, map[string]string{}, seed)
					if next == nil {
						next = fmt.Sprintf("c18-%d", g.RandRange(1000000))
					}
				}
				cfg := rtLoad(t, h)
				roundtrip.Set(cfg, target, next)
				rtSave(t, h, cfg)
				after, got := rtParse(t, path)
				ctx := fmt.Sprintf("seed=%d round=%d target=%v new=%#v", seed, round, target, next)
				expect := roundtrip.Clone(c.Tree).(map[string]any)
				roundtrip.Set(expect, target, next)
				if !roundtrip.Equal(got, expect) {
					t.Fatalf("[%s] changed paths %v:\n%s", ctx, roundtrip.Paths(roundtrip.ChangedPaths(expect, got)), roundtrip.UDiff(c.Text, after))
				}
				if err := roundtrip.OneKeyWrite(ctx, c.Text, after, [][]string{target}, c.LinesOf(target), !c.HasLine(target)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

var rtSetExclude = map[string]bool{
	roundtrip.Key("model"): true, roundtrip.Key("model", "provider"): true, roundtrip.Key("model", "api_base"): true,
	roundtrip.Key("_config_version"): true, roundtrip.Key("display", "language"): true,
}

func TestP2EveryDocumentedKeyIsSettableAndChangesExactlyOneKey(t *testing.T) {
	var leaves []roundtrip.Leaf
	for _, l := range rtSchema().Documented {
		if !rtSetExclude[roundtrip.Key(l.Path...)] {
			leaves = append(leaves, l)
		}
	}
	const chunks = 8
	for chunk := 0; chunk < chunks; chunk++ {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			out, errOut := captureFacade(t)
			c := rtGen(t, int64(300+chunk), roundtrip.GenOptions{})
			g := roundtrip.NewRNG(int64(300 + chunk))
			var failures []string
			for i := chunk; i < len(leaves); i += chunks {
				path, def := leaves[i].Path, leaves[i].Default
				key := roundtrip.CLIKey(path)
				if IsEnvConfigKey(key) {
					continue
				}
				cfgPath := rtInstall(t, h, c)
				current := roundtrip.Get(c.Tree, path)
				if current == roundtrip.Missing {
					current = nil
				}
				switch current.(type) {
				case map[string]any, []any:
					continue
				}
				blocked := false
				for k := range roundtrip.Leaves(c.Tree) {
					q := roundtrip.Split(k)
					blocked = blocked || (len(q) < len(path) && slices.Equal(path[:len(q)], q))
				}
				if blocked {
					continue
				}
				s, want := roundtrip.SetValueFor(def, current, g)
				out.Reset()
				errOut.Reset()
				if err := SetConfigValue(context.Background(), h, key, s, false, testCommandSources()); err != nil {
					failures = append(failures, fmt.Sprintf("%v: refused: %v %.200s", path, err, strings.TrimSpace(out.String()+errOut.String())))
					continue
				}
				after, got := rtParse(t, cfgPath)
				changed := roundtrip.ChangedPaths(c.Tree, got)
				if len(changed) != 1 || !changed[roundtrip.Key(path...)] || !roundtrip.Same(roundtrip.Get(got, path), want) {
					failures = append(failures, fmt.Sprintf("%v=%q: changed %v, stored %#v", path, s, roundtrip.Paths(changed), roundtrip.Get(got, path)))
					continue
				}
				if err := roundtrip.OneKeyWrite(fmt.Sprintf("chunk=%d %v", chunk, path), c.Text, after, [][]string{path}, c.LinesOf(path), !c.HasLine(path)); err != nil {
					failures = append(failures, fmt.Sprintf("%.600s", err))
				}
			}
			if len(failures) > 0 {
				t.Fatalf("[chunk=%d seed=%d] %d documented key(s) failed:\n%s", chunk, c.Seed, len(failures), strings.Join(failures, "\n"))
			}
		})
	}
}

var rtP3Target = []string{"display", "skin"}

type rtOp struct {
	run     func(h string) error
	targets [][]string
}

// The facade writers of the read fault properties, each a refusal or a targeted write
func rtP3Ops(t *testing.T) map[string]rtOp {
	ctx := context.Background()
	return map[string]rtOp{
		"cli_set": {func(h string) error {
			return SetConfigValue(ctx, h, "display.skin", "ares", false, testCommandSources())
		}, [][]string{rtP3Target}},
		"save_config": {func(h string) error {
			cfg, err := Load(h, facadeLookup)
			if err != nil {
				return err
			}
			roundtrip.Set(map[string]any(cfg), rtP3Target, "ares")
			return SaveConfig(ctx, h, cfg, SaveOptions{})
		}, [][]string{rtP3Target}},
		"migrate": {func(h string) error {
			_, err := MigrateConfig(ctx, h, false, true)
			return err
		}, [][]string{{"_config_version"}, {"delegation", "max_iterations"}}},
	}
}

func rtP3Case(t *testing.T, seed int64) *roundtrip.Case {
	t.Helper()
	c := rtGen(t, seed, roundtrip.GenOptions{NoNullLeaves: true, ExcludeTop: []string{"display", "delegation", "agent"}})
	text, err := roundtrip.BeforeVersion(c.Text, "display:\n  skin: mono\n  compact: true\ndelegation:\n  max_iterations: 50\n"+
		"agent:\n  verify_on_stop: false\n")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := FastSafeLoad([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return &roundtrip.Case{Seed: seed, Text: text, Tree: tree.(map[string]any), LeafLines: map[string]int{}, Env: c.Env}
}

func rtP3Check(label, beforeText, afterText string, targets [][]string, outcome string) string {
	before, _ := FastSafeLoad([]byte(beforeText))
	after, err := FastSafeLoad([]byte(afterText))
	if _, ok := after.(map[string]any); err != nil || !ok {
		return fmt.Sprintf("%s: file no longer a mapping (%s)", label, outcome)
	}
	var stray []string
	for k := range roundtrip.ChangedPaths(before, after) {
		if !roundtrip.Under(roundtrip.Split(k), targets) {
			stray = append(stray, strings.Join(roundtrip.Split(k), "."))
		}
	}
	if len(stray) > 0 {
		var lost []string
		for k := range before.(map[string]any) {
			if _, ok := after.(map[string]any)[k]; !ok {
				lost = append(lost, k)
			}
		}
		slices.Sort(stray)
		slices.Sort(lost)
		return fmt.Sprintf("%s (%s): clobbered %d untargeted path(s) %v; lost sections %v", label, outcome, len(stray), stray[:min(5, len(stray))], lost)
	}
	if outcome != "ok" && afterText != beforeText && strings.Fields(label)[0] != "migrate" {
		return fmt.Sprintf("%s: refused (%s) but still rewrote the file", label, outcome)
	}
	return ""
}

func rtOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	var exit *ConfigExit
	if errors.As(err, &exit) {
		return "refused: SystemExit"
	}
	return fmt.Sprintf("refused: %T", err)
}

func TestP3ATransientErrorOnAnySingleReadNeverClobbers(t *testing.T) {
	for _, name := range []string{"cli_set", "save_config", "migrate"} {
		t.Run(name, func(t *testing.T) {
			h, cfg := facadeHome(t, "")
			if name == "migrate" {
				registerMigrationFacade(t)
			}
			captureFacade(t)
			op := rtP3Ops(t)[name]
			c := rtP3Case(t, 700)
			for k, v := range c.Env {
				t.Setenv(k, v)
			}
			text := c.Text
			if name == "migrate" {
				text = roundtrip.Restamp(text, 35)
			}
			rtWrite(t, h, c.Text, false)
			rtLoad(t, h)
			faults := installReadFaults(t, cfg)
			run := func() string {
				rtWrite(t, h, text, false)
				return rtOutcome(op.run(h))
			}
			faults.arm(0)
			outcome := run()
			reads := faults.count
			if msg := rtP3Check(name+" clean run", text, readText(cfg), op.targets, outcome); msg != "" || outcome != "ok" {
				t.Fatalf("%s clean run %s %s", name, outcome, msg)
			}
			if reads < 1 {
				t.Fatalf("%s performed no config read through the read seam", name)
			}
			var problems []string
			for k := 1; k <= reads; k++ {
				faults.arm(k)
				outcome := run()
				if msg := rtP3Check(fmt.Sprintf("%s EMFILE on read %d/%d", name, k, reads), text, readText(cfg), op.targets, outcome); msg != "" {
					problems = append(problems, msg)
				}
			}
			if len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}

// Text cut five bytes into the first double quoted scalar after the header
func rtTruncateMidScalar(text string) string {
	nl := 10 + strings.Index(text[10:], "\n")
	idx := nl + strings.Index(text[nl:], `"`)
	return text[:idx+5]
}

func TestP3UnreadableOrPartialFileIsNeverRewritten(t *testing.T) {
	faults := map[string]func(string) string{
		"non_mapping_root":    func(string) string { return "- c18\n- not a mapping\n" },
		"truncated_mid_write": rtTruncateMidScalar,
		"unreadable":          nil,
	}
	for _, name := range []string{"cli_set", "save_config", "migrate"} {
		for _, fault := range []string{"non_mapping_root", "truncated_mid_write", "unreadable"} {
			t.Run(name+"/"+fault, func(t *testing.T) {
				h, cfg := facadeHome(t, "")
				if name == "migrate" {
					registerMigrationFacade(t)
				}
				captureFacade(t)
				c := rtGen(t, 710, roundtrip.GenOptions{NoNullLeaves: true, ExcludeTop: []string{"display"}})
				for k, v := range c.Env {
					t.Setenv(k, v)
				}
				text := c.Text
				if faults[fault] != nil {
					text = faults[fault](c.Text)
				}
				if fault == "truncated_mid_write" {
					if _, err := FastSafeLoad([]byte(text)); err == nil {
						t.Fatalf("truncation still parses:\n%s", text)
					}
				}
				rtWrite(t, h, text, false)
				if fault == "unreadable" {
					denyConfigReads(t, cfg)
				}
				rtP3Ops(t)[name].run(h)
				if err := os.Chmod(cfg, 0o600); err != nil {
					t.Fatal(err)
				}
				if got := readText(cfg); got != text {
					t.Fatalf("[%s/%s] an unreadable/partial config.yaml was rewritten:\n%s", name, fault, roundtrip.UDiff(text, got))
				}
			})
		}
	}
}

func TestP4EnvParserSanitizerAndWriterRoundTrip(t *testing.T) {
	for _, seed := range []int64{801, 802, 803, 804, 805, 806} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			h, _ := facadeHome(t, "")
			captureFacade(t)
			text, keys, _ := roundtrip.GenDotenv(seed)
			for _, k := range append(keys, "PATH") {
				prior, had := os.LookupEnv(k)
				t.Setenv(k, prior)
				if !had {
					os.Unsetenv(k)
				}
			}
			t.Setenv("PATH", roundtrip.BasePath)
			t.Setenv("ICEBATH_MULTIPLEX_PROFILES", "0")
			envPath := filepath.Join(h, ".env")
			if err := os.WriteFile(envPath, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			first := LoadEnv(h)
			if again := LoadEnv(h); !roundtrip.Equal(rtAnyMap(again), rtAnyMap(first)) {
				t.Fatalf("[seed=%d] load_env() is not a pure function of the file", seed)
			}
			if _, err := SanitizeEnvFile(context.Background(), h); err != nil {
				t.Fatal(err)
			}
			sanitized := readText(envPath)
			if n, err := SanitizeEnvFile(context.Background(), h); err != nil || n != 0 || readText(envPath) != sanitized {
				t.Fatalf("[seed=%d] sanitize is not a fixed point (%d, %v)", seed, n, err)
			}
			parsed := LoadEnv(h)
			target := fmt.Sprintf("C18_%d_A", seed)
			if err := SaveEnvValue(h, target, "rewritten value # with hash"); err != nil {
				t.Fatal(err)
			}
			after := LoadEnv(h)
			var changed []string
			for k := range rtAnyMap(parsed) {
				if parsed[k] != after[k] {
					changed = append(changed, k)
				}
			}
			for k := range after {
				if _, ok := parsed[k]; !ok {
					changed = append(changed, k)
				}
			}
			if len(changed) != 1 || changed[0] != target {
				t.Fatalf("[seed=%d] save_env_value(%s) changed %v", seed, target, changed)
			}
			if after[target] != "rewritten value # with hash" {
				t.Fatalf("[seed=%d] stored %q", seed, after[target])
			}
			touched, inserted := roundtrip.LineDiff(sanitized, readText(envPath))
			if len(touched) != 1 || roundtrip.InsertedLines(inserted) != 1 {
				t.Fatalf("[seed=%d]\n%s", seed, roundtrip.UDiff(sanitized, readText(envPath)))
			}
		})
	}
}

func rtAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Config with one top level section null among generated sections, its parse and the null line index
func rtP6Setup(t *testing.T, h, section string) (string, map[string]any, int) {
	t.Helper()
	idx := slices.Index(rtSchema().TopLevel, section)
	c := rtGen(t, int64(1000+idx), roundtrip.GenOptions{NSections: [2]int{3, 5}, NoNullLeaves: true, ExcludeTop: []string{section, "display", "agent"}})
	text, err := roundtrip.BeforeVersion(c.Text, section+": null\n")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.Env {
		t.Setenv(k, v)
	}
	rtWrite(t, h, text, false)
	tree, err := FastSafeLoad([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return text, tree.(map[string]any), slices.Index(roundtrip.Lines(text), section+": null")
}

func TestP6NoopSaveKeepsTheEffectiveValueOfANullSection(t *testing.T) {
	for _, section := range rtSchema().TopLevel {
		t.Run(section, func(t *testing.T) {
			h, _ := facadeHome(t, "")
			captureFacade(t)
			rtP6Setup(t, h, section)
			effective := rtLoad(t, h)
			rtSave(t, h, roundtrip.Clone(effective).(map[string]any))
			loadCacheMu.Lock()
			loadCache, rawCache, lastExpanded = map[string]*loadEntry{}, map[string]rawEntry{}, map[string]map[string]any{}
			loadCacheMu.Unlock()
			after := rtLoad(t, h)
			if !roundtrip.Equal(after[section], effective[section]) {
				t.Fatalf("[%s: null] save_config(load_config()) changed %q: %#v -> %#v", section, section, effective[section], after[section])
			}
		})
	}
}

func TestP6NullSectionSurvivesTheFacadeSurfaces(t *testing.T) {
	defaults := map[string]any(Defaults())
	for _, section := range rtSchema().TopLevel {
		t.Run(section, func(t *testing.T) {
			h, cfg := facadeHome(t, "")
			captureFacade(t)
			text, tree, nullLine := rtP6Setup(t, h, section)
			ctx := section + ": null"
			effective := rtLoad(t, h)
			if _, isMap := defaults[section].(map[string]any); isMap {
				if _, ok := effective[section].(map[string]any); !ok {
					t.Fatalf("[%s] load_config()[%q] = %#v", ctx, section, effective[section])
				}
			}
			if _, err := LoadUserEffective(cfg, facadeLookup); err != nil {
				t.Fatalf("[%s] load_user_config_effective: %v", ctx, err)
			}
			if _, err := ReadUserConfigRaw(context.Background(), cfg); err != nil {
				t.Fatalf("[%s] read_user_config_raw: %v", ctx, err)
			}
			rtSave(t, h, rtLoad(t, h))
			after := readText(cfg)
			touched, inserted := roundtrip.LineDiff(text, after)
			delete(touched, nullLine)
			if len(touched) > 0 || len(inserted) > 1 {
				t.Fatalf("[%s] save touched other lines:\n%s", ctx, roundtrip.UDiff(text, after))
			}
			if err := roundtrip.NoDuplicateTopLevel(after, ctx); err != nil {
				t.Fatal(err)
			}
			rtWrite(t, h, text, false)
			if err := SetConfigValue(context.Background(), h, "terminal.timeout", "4242", false, testCommandSources()); err != nil {
				t.Fatalf("[%s] cli_set: %v", ctx, err)
			}
			_, got := rtParse(t, cfg)
			target := []string{"terminal", "timeout"}
			if v := roundtrip.Get(got, target); v == roundtrip.Missing || v == nil {
				t.Fatalf("[%s] cli_set did not write %v", ctx, target)
			}
			for k := range roundtrip.ChangedPaths(tree, got) {
				if !roundtrip.Under(roundtrip.Split(k), [][]string{target, {section}}) {
					t.Fatalf("[%s] cli_set changed untargeted path %v:\n%s", ctx, roundtrip.Split(k), roundtrip.UDiff(text, readText(cfg)))
				}
			}
		})
	}
}

func TestP6AllSectionsNullAtOnceThroughTheEffectiveLoaderAndMigration(t *testing.T) {
	h, cfg := facadeHome(t, "")
	registerMigrationFacade(t)
	captureFacade(t)
	s := rtSchema()
	var b strings.Builder
	b.WriteString("# every section null\n")
	for _, k := range s.TopLevel {
		b.WriteString(k + ": null\n")
	}
	fmt.Fprintf(&b, "_config_version: %d\n", s.Latest-1)
	rtWrite(t, h, b.String(), false)
	if _, err := LoadUserEffective(cfg, facadeLookup); err != nil {
		t.Fatal(err)
	}
	effective := rtLoad(t, h)
	for _, k := range s.TopLevel {
		if _, isMap := s.Defaults[k].(map[string]any); isMap {
			if _, ok := effective[k].(map[string]any); !ok {
				t.Fatalf("load_config()[%q] = %#v", k, effective[k])
			}
		}
	}
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	after, got := rtParse(t, cfg)
	if v := roundtrip.Get(got, []string{"_config_version"}); !roundtrip.Equal(v, s.Latest) {
		t.Fatalf("stamp %#v:\n%s", v, after)
	}
	if !strings.Contains(after, "# every section null") {
		t.Fatalf("comment dropped:\n%s", after)
	}
	if err := roundtrip.NoDuplicateTopLevel(after, "all-null migrate"); err != nil {
		t.Fatal(err)
	}
}

func TestP5MigrationIsIdempotentAndKeepsUserValuesThroughTheLadderDoubles(t *testing.T) {
	for _, version := range roundtrip.MigrationStarts(MigrationVersions(), SupportFloorVersion) {
		name, seed := roundtrip.MigrationStartName(version)
		t.Run(name, func(t *testing.T) {
			h, cfg := facadeHome(t, "")
			registerMigrationFacade(t)
			captureFacade(t)
			envPath := filepath.Join(h, ".env")
			read := func(path string) func() (string, error) {
				return func() (string, error) { data, err := os.ReadFile(path); return string(data), err }
			}
			m := roundtrip.MigrationHome{
				WriteConfig: func(text string) error { rtWrite(t, h, text, false); return nil },
				WriteEnv:    func(text string) error { return os.WriteFile(envPath, []byte(text), 0o600) },
				Migrate: func() error {
					ResetCache()
					_, err := MigrateConfig(context.Background(), h, false, true)
					return err
				},
				ReadConfig: read(cfg), ReadEnv: read(envPath), Load: FastSafeLoad,
			}
			if err := roundtrip.MigrationProperty(m, version, seed, rtSchema().Latest); err != nil {
				t.Fatal(err)
			}
		})
	}
}
