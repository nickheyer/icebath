package dashboard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/roundtrip"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Client of a fresh dashboard app with quiet facade output and no managed overlay
func p2DashboardClient(t *testing.T) *testClient {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	config.InvalidateManagedCache()
	prevOut, prevNotice := config.FacadeStdout, config.FacadeNoticeOut
	config.FacadeStdout, config.FacadeNoticeOut = io.Discard, io.Discard
	t.Cleanup(func() { config.FacadeStdout, config.FacadeNoticeOut = prevOut, prevNotice; config.ResetCache() })
	return c
}

// Generated case of the given options from the schema of the defaults
func p2DashboardCase(t *testing.T, seed int64, o roundtrip.GenOptions) *roundtrip.Case {
	t.Helper()
	schema := roundtrip.NewSchema(map[string]any(config.Defaults()), config.MappingKeys, config.FastSafeLoad)
	c, err := schema.GenCase(seed, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Writes the case config and env into the home and drops every config cache, returning the config path
func p2DashboardInstall(t *testing.T, c *roundtrip.Case) string {
	t.Helper()
	for k, v := range c.Env {
		t.Setenv(k, v)
	}
	path := home.ConfigPath(home.Root())
	writeNullSectionConfig(t, path, c.Text)
	return path
}

// Nested object holding value at path, the setNested({}, key, value) of the Desktop PUT
func p2NestedObject(path []string, value any) *pyjson.Object {
	var v any = value
	for i := len(path) - 1; i >= 0; i-- {
		v = pyjson.ObjectOf(path[i], v)
	}
	return v.(*pyjson.Object)
}

// GET then PUT of the whole config leaves the file unchanged and a partial PUT changes exactly its key
func p2DashboardRoundtrip(t *testing.T, c *testClient, rc *roundtrip.Case) {
	t.Helper()
	seed := rc.Seed
	cfg := p2DashboardInstall(t, rc)
	body := c.request("GET", "/api/config", nil, sessionHeaders()...)
	if body.Status != 200 {
		t.Fatalf("GET /api/config: %d %s", body.Status, body.Body)
	}
	r := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", body.JSON(t)))
	if r.Status != 200 {
		t.Fatalf("PUT /api/config: %s", r.Body)
	}
	after, _ := readNullSectionConfig(t, cfg)
	touched, inserted := roundtrip.LineDiff(rc.Text, after)
	if len(touched) > 0 || len(inserted) > 0 {
		t.Fatalf("[seed=%d] GET->PUT with no edits changed the file:\n%s", seed, roundtrip.UDiff(rc.Text, after))
	}
	g := roundtrip.NewRNG(seed)
	var pool [][]string
	for _, p := range rc.LeafOrder {
		if rc.HasLine(p) && p[0] != "_config_version" && p[0] != "providers" && p[0] != "c18_custom_root" {
			pool = append(pool, p)
		}
	}
	targets := append(roundtrip.Sample(g, pool, 2), []string{"display", "resume_last_session"})
	defaults := map[string]any(config.Defaults())
	for _, target := range targets {
		p2DashboardInstall(t, rc)
		cur := roundtrip.Get(rc.Tree, target)
		if cur == roundtrip.Missing {
			cur = roundtrip.Get(defaults, target)
			if cur == roundtrip.Missing {
				cur = nil
			}
		}
		var next any = fmt.Sprintf("c18-dash-%d", g.RandRange(1000000))
		if b, isBool := cur.(bool); isBool {
			next = !b
		}
		r := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", p2NestedObject(target, next)))
		if r.Status != 200 {
			t.Fatalf("PUT /api/config: %s", r.Body)
		}
		after, got := readNullSectionConfig(t, cfg)
		changed := roundtrip.ChangedPaths(rc.Tree, got)
		if len(changed) != 1 || !changed[roundtrip.Key(target...)] {
			t.Fatalf("[seed=%d %v] changed %v:\n%s", seed, target, roundtrip.Paths(changed), roundtrip.UDiff(rc.Text, after))
		}
		ctx := fmt.Sprintf("dashboard seed=%d %v", seed, target)
		if err := roundtrip.OneKeyWrite(ctx, rc.Text, after, [][]string{target}, rc.LinesOf(target), !rc.HasLine(target)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestP2DashboardGetPutRoundtripAndPartialPut(t *testing.T) {
	for _, seed := range []int64{601, 602, 603, 604} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			c := p2DashboardClient(t)
			p2DashboardRoundtrip(t, c, p2DashboardCase(t, seed, roundtrip.GenOptions{NoNullLeaves: true}))
		})
	}
}

func TestP2DashboardPartialPutAddsNoPhantomSection(t *testing.T) {
	c := p2DashboardClient(t)
	rc := p2DashboardCase(t, 606, roundtrip.GenOptions{NoNullLeaves: true, ExcludeTop: []string{"agent", "display"}})
	cfg := p2DashboardInstall(t, rc)
	r := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", pyjson.ObjectOf("display", pyjson.ObjectOf("skin", "ares"))))
	if r.Status != 200 {
		t.Fatalf("PUT /api/config: %s", r.Body)
	}
	_, got := readNullSectionConfig(t, cfg)
	want := map[string]bool{"display": true}
	for k := range rc.Tree {
		want[k] = true
	}
	var extra []string
	for k := range got.(map[string]any) {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(got.(map[string]any)) != len(want) || len(extra) > 0 {
		raw, _ := os.ReadFile(cfg)
		t.Fatalf("PUT display.skin added sections %v:\n%s", extra, roundtrip.UDiff(rc.Text, string(raw)))
	}
}

func TestP2DashboardNoopPutKeepsExplicitNulls(t *testing.T) {
	c := p2DashboardClient(t)
	rc := p2DashboardCase(t, 607, roundtrip.GenOptions{})
	hasNull := false
	for _, v := range roundtrip.Leaves(rc.Tree) {
		if v == nil {
			hasNull = true
		}
	}
	if !hasNull {
		t.Fatalf("seed 607 generated no null leaf")
	}
	p2DashboardRoundtrip(t, c, rc)
}
