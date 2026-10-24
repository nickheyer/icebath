package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// Chat completions server that streams one text reply per request and counts what it saw
func fakeOpenAI(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req-fake")
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from \"},\"finish_reason\":null}]}\n\n", req.Model)
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", req.Model, req.Model)
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n", req.Model)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testConfig(t *testing.T, base string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, filepath.Join(dir, "cfg"))
	list := fmt.Sprintf(`[{"name":"fake","vendor":"openai","protocol":"openai-completions","base_url":%q,"models":["m-fast","m-slow"]}]`, base)
	path := filepath.Join(dir, "profiles.json")
	os.WriteFile(path, []byte(list), 0o644)
	catalog := filepath.Join(dir, "catalog.json")
	os.WriteFile(catalog, []byte(`[{"id":"m-fast","context":1000,"pricing":{"input":1,"output":1}}]`), 0o644)
	return Config{Profile: "fake", ProfileFiles: []string{path}, CatalogFiles: []string{catalog}, Permissions: rulesAllowAll(), Compaction: Compaction{Threshold: 100000}, Subagents: Subagents{Model: &Target{Profile: "fake", Model: "m-slow"}}, Fallbacks: []Target{{Profile: "fake", Model: "m-slow"}}, Judge: &Judge{Target: Target{Profile: "fake"}}, Web: WebConfig{AllowPrivate: true}}, dir
}

func rulesAllowAll() rules.Set {
	return rules.Set{Allow: []string{"Read", "Glob", "Grep", "Record", "Plan"}}
}

func TestBuildAssemblesTheApp(t *testing.T) {
	srv, hits := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	cfg.Hooks = hooks.Config{hooks.SessionStart: []hooks.Group{{Hooks: []hooks.Command{{Command: "echo started"}}}}, hooks.Stop: []hooks.Group{{Hooks: []hooks.Command{{Command: "echo stop-seen"}}}}}
	cfg.Record = &RecordConfig{Path: filepath.Join(dir, "cassette.json"), Mode: "record"}
	cfg.MCP = []ServerConfig{{Name: "later", Command: "icebath-no-such-binary", Disabled: true}}
	s := newSink()
	ctx := context.Background()
	if _, err := Build(ctx, cfg, nil, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("build without a sender accepted")
	}
	app, err := Build(ctx, cfg, s.send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if app.Primary.Model != "m-fast" || app.Primary.Catalog.Context != 1000 || len(app.Fallbacks) != 1 || app.Fallbacks[0].Model != "m-slow" || app.Root != dir {
		t.Fatalf("%+v", app.Primary)
	}
	if app.Mode.Mode() != mode.Default || app.Session == nil || app.Session.Model() != "m-fast" || app.Cassette == nil {
		t.Fatal("defaults")
	}
	names := app.Tools.Names()
	for _, want := range []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash", "BashOutput", "KillShell", "WebFetch", facts.ToolName, mission.ToolName, "Agent"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool %s missing from %v", want, names)
		}
	}
	if st := app.MCP.Status(); len(st) != 1 || st[0].State != mcp.StateDisconnected {
		t.Fatalf("%+v", st)
	}
	if err := app.SessionStart(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	s.wait(t, func(m any) bool { h, ok := m.(HookOutputMsg); return ok && strings.Contains(h.Text, "started") })
	ag := app.NewAgent("root", app.Session)
	if ag.MaxTokens != DefaultMaxTokens || len(ag.Hooks) != 6 || ag.ModelID != "m-fast" {
		t.Fatalf("hooks %d max %d", len(ag.Hooks), ag.MaxTokens)
	}
	m, _ := app.Missions.Create("say hello")
	r, err := app.Run(ctx, m.ID, []protocol.Part{protocol.Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	done := s.wait(t, func(m any) bool { d, ok := m.(RunDoneMsg); return ok && d.Run == r.ID }).(RunDoneMsg)
	if done.Err != nil || done.Response.Text() != "hello from m-fast" {
		t.Fatalf("%+v", done)
	}
	stopSeen := false
	for _, m := range s.all() {
		if h, ok := m.(HookOutputMsg); ok && strings.Contains(h.Text, "stop-seen") {
			stopSeen = true
		}
	}
	if !stopSeen {
		t.Fatal("stop hook output must reach the program")
	}
	if hits.Load() < 1 {
		t.Fatal("model never called")
	}
	snap := app.Meter.Snapshot()
	if snap.Requests < 1 || snap.LastRequestID != "req-fake" || snap.Usage.Output != 3 || snap.Cost <= 0 || snap.Provider != "fake" {
		t.Fatalf("%+v", snap)
	}
	if ok, _ := app.Store.Exists(ctx, app.Session.ID()); !ok {
		t.Fatal("session must be stored")
	}
	child, err := app.childFactory(ctx)
	if err != nil || child.ModelID != "m-slow" || child.Session.Labels()["parent"] != app.Session.ID() {
		t.Fatalf("%v %+v", err, child)
	}
	first := app.Session.ID()
	if err := app.Fresh(); err != nil || app.Session.ID() == first || app.Session.Len() != 0 {
		t.Fatalf("%v", err)
	}
	if err := app.Switch(ctx, first); err != nil || app.Session.ID() != first || app.Session.Len() != 2 {
		t.Fatalf("switch %v len %d", err, app.Session.Len())
	}
	if err := app.Switch(ctx, "missing"); err == nil {
		t.Fatal("missing session switched")
	}
	if m2, _ := app.Missions.Get(m.ID); m2.Objective != "say hello" {
		t.Fatal("missions must restore with the session")
	}
	f, err := app.ForkAt(1)
	if err != nil || f.Len() != 1 || f.Parent() != first || app.Session != f {
		t.Fatalf("%v", err)
	}
	app.SetModel("m-slow")
	if app.Primary.Model != "m-slow" || app.Session.Model() != "m-slow" || app.Meter.Snapshot().Model != "m-slow" {
		t.Fatal("set model")
	}
	app.SetModel("m-fast")
	if app.Primary.Catalog.Context != 1000 {
		t.Fatal("catalog lookup on set model")
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal("second close must be harmless")
	}
	if _, err := os.Stat(cfg.Record.Path); err != nil {
		t.Fatal("cassette must be saved on close")
	}
}

func TestBuildFailures(t *testing.T) {
	srv, _ := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	s := newSink()
	ctx := context.Background()
	bad := cfg
	bad.Profile = "nope"
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "not known") {
		t.Fatalf("%v", err)
	}
	bad = cfg
	bad.Model = ""
	bad.Profile = "fake"
	bad.Fallbacks = []Target{{Profile: "nope"}}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("%v", err)
	}
	bad = cfg
	bad.Judge = &Judge{Target: Target{Profile: "nope"}}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("%v", err)
	}
	bad = cfg
	bad.Subagents.Model = &Target{Profile: "nope"}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "subagents") {
		t.Fatalf("%v", err)
	}
	bad = cfg
	bad.Compaction.Model = &Target{Profile: "nope"}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "compaction") {
		t.Fatalf("%v", err)
	}
	bad = cfg
	bad.Mode = "weird"
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("bad mode accepted")
	}
	bad = cfg
	bad.Record = &RecordConfig{Path: filepath.Join(dir, "c.json"), Mode: "weird"}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("bad record mode accepted")
	}
	bad = cfg
	bad.Permissions.Allow = []string{"Bash(("}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("bad rule accepted")
	}
	bad = cfg
	bad.SystemFile = filepath.Join(dir, "nope.md")
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("missing system file accepted")
	}
	bad = cfg
	bad.CatalogFiles = []string{filepath.Join(dir, "nope.json")}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("missing catalog accepted")
	}
	bad = cfg
	bad.ProfileFiles = []string{filepath.Join(dir, "nope.json")}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("missing profile list accepted")
	}
	bad = cfg
	bad.MCP = []ServerConfig{{Name: ""}}
	if _, err := Build(ctx, bad, s.send, BuildOptions{Dir: dir}); err == nil {
		t.Fatal("bad mcp server accepted")
	}
	if _, err := Build(ctx, cfg, s.send, BuildOptions{Dir: dir, Resume: "missing"}); err == nil {
		t.Fatal("missing resume accepted")
	}
	nomodel := cfg
	nomodel.ProfileFiles = nil
	list := fmt.Sprintf(`[{"name":"fake","protocol":"openai-completions","base_url":%q}]`, srv.URL)
	path := filepath.Join(dir, "nomodel.json")
	os.WriteFile(path, []byte(list), 0o644)
	nomodel.ProfileFiles = []string{path}
	nomodel.Fallbacks, nomodel.Judge, nomodel.Subagents.Model = nil, nil, nil
	if _, err := Build(ctx, nomodel, s.send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), "names no model") {
		t.Fatalf("%v", err)
	}
	app, err := Build(ctx, cfg, s.send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	app.Control.mu.Lock()
	app.Control.runs["fake-run"] = &Run{ID: "fake-run", Session: app.Session, running: true, done: hold, cancel: func() {}}
	app.Control.order = append(app.Control.order, "fake-run")
	app.Control.mu.Unlock()
	if err := app.Fresh(); err == nil {
		t.Fatal("fresh during a run accepted")
	}
	if err := app.Switch(ctx, "x"); err == nil {
		t.Fatal("switch during a run accepted")
	}
	if _, err := app.ForkAt(0); err == nil {
		t.Fatal("fork during a run accepted")
	}
	app.Control.mu.Lock()
	fake := app.Control.runs["fake-run"]
	app.Control.mu.Unlock()
	fake.mu.Lock()
	fake.running = false
	fake.mu.Unlock()
	app.Close(ctx)
}

func TestBuildUsesTheDefaultNamedByTheUserFile(t *testing.T) {
	srv, hits := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	cfg.Profile, cfg.Fallbacks, cfg.Subagents.Model, cfg.Judge = "", nil, nil, nil
	user, _ := profiles.UserPath()
	os.MkdirAll(filepath.Dir(user), 0o755)
	os.WriteFile(user, fmt.Appendf(nil, `{"live":"idk","profiles":{"idk":{"API_KEY":"k-1","BASE_URL":%q,"MODEL":"freddie"}}}`, srv.URL), 0o600)
	ctx := context.Background()
	s := newSink()
	app, err := Build(ctx, cfg, s.send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if app.Primary.Profile.Name != "idk" || app.Primary.Model != "freddie" || app.Primary.Provider.BaseURL != srv.URL+"/v1" {
		t.Fatalf("%+v", app.Primary)
	}
	r, err := app.Run(ctx, "", []protocol.Part{protocol.Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	done := s.wait(t, func(m any) bool { d, ok := m.(RunDoneMsg); return ok && d.Run == r.ID }).(RunDoneMsg)
	if done.Err != nil || hits.Load() != 1 || r.Response().Text() != "hello from freddie" {
		t.Fatalf("the earlier profile must serve a turn: %v hits %d %q", done.Err, hits.Load(), r.Response().Text())
	}
	app.Close(ctx)
	cfg.Profile = "fake"
	if app, err = Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir}); err != nil || app.Primary.Profile.Name != "fake" {
		t.Fatalf("the configuration must beat the file default: %v %+v", err, app.Primary)
	}
	app.Close(ctx)
	cfg.Profile = ""
	os.WriteFile(user, []byte(`{"live":"ghost","profiles":{}}`), 0o600)
	if _, err := Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("an unknown default must be reported: %v", err)
	}
	os.WriteFile(user, []byte(`{"profiles":{"idk":{"BASE_URL":}}}`), 0o600)
	if _, err := Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir}); err == nil || !strings.Contains(err.Error(), user) {
		t.Fatalf("a broken user file must be named: %v", err)
	}
}

func TestEndpointSwitchOpensThenAdopts(t *testing.T) {
	srv, _ := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	ctx := context.Background()
	app, err := Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.OpenEndpoint(ctx, "", ""); err == nil {
		t.Fatal("a name is required")
	}
	if _, err := app.OpenEndpoint(ctx, "nope", ""); err == nil {
		t.Fatal("unknown profile opens")
	}
	ep, err := app.OpenEndpoint(ctx, "fake", "m-slow")
	if err != nil || ep.Model != "m-slow" || app.Primary.Model != "m-fast" || app.Session.Model() != "m-fast" {
		t.Fatalf("%v %+v", err, ep)
	}
	if err := app.SetEndpoint("fake", ep); err != nil {
		t.Fatal(err)
	}
	if app.Primary.Model != "m-slow" || app.Session.Model() != "m-slow" || app.Config.Model != "m-slow" || app.Config.Profile != "fake" || app.Meter.Snapshot().Model != "m-slow" {
		t.Fatalf("%+v", app.Primary)
	}
	r, err := app.Run(ctx, "", []protocol.Part{protocol.Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetEndpoint("fake", ep); err == nil {
		t.Fatal("a running app refuses the switch")
	}
	<-r.done
	app.Close(ctx)
}

func TestLivesCarryTheirEndpoints(t *testing.T) {
	srv, _ := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	ctx := context.Background()
	app, err := Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	first := app.Current()
	if first.Endpoint.Model != "m-fast" || first.Session.Labels()["profile"] != "fake" || first.Session.Model() != "m-fast" {
		t.Fatal("a new session is labeled with the default profile and model")
	}
	second, err := app.Create()
	if err != nil {
		t.Fatal(err)
	}
	app.SetModelOn(second, "m-slow")
	if first.Endpoint.Model != "m-fast" || second.Endpoint.Model != "m-slow" || second.Session.Model() != "m-slow" || app.Primary.Model != "m-slow" || app.Config.Model != "m-slow" {
		t.Fatal("a session moves alone and the default follows")
	}
	if app.NewAgent("", second.Session).ModelID != "m-slow" || app.NewAgent("", first.Session).ModelID != "m-fast" {
		t.Fatal("agents run on the endpoint of their session")
	}
	f, err := app.Fork(second, 0)
	if err != nil || f.Endpoint.Model != "m-slow" || f.Session.Labels()["profile"] != "fake" {
		t.Fatal("a fork keeps the endpoint")
	}
	if err := app.SaveProfile("", profiles.Profile{Name: "other", Vendor: "openai", Protocol: "openai-completions", BaseURL: srv.URL, Models: []string{"m-x"}}); err != nil {
		t.Fatal(err)
	}
	ep, err := app.OpenEndpoint(ctx, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetEndpointOn(second, "other", ep); err != nil {
		t.Fatal(err)
	}
	if second.Endpoint.Profile.Name != "other" || first.Endpoint.Profile.Name != "fake" || app.Primary.Profile.Name != "other" || !app.ProfileInUse("fake") || !app.ProfileInUse("other") {
		t.Fatal("a session moves to a profile alone, both profiles in use")
	}
	if err := app.RemoveProfile("other"); err == nil {
		t.Fatal("a profile in use may not be deleted")
	}
	id := second.Session.ID()
	second.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "kept"}}})
	if err := app.Release(second); err != nil {
		t.Fatal(err)
	}
	back, err := app.Open(ctx, id)
	if err != nil || back.Endpoint.Profile.Name != "other" || back.Endpoint.Model != "m-x" {
		t.Fatalf("a resumed session comes back on its own endpoint %v", err)
	}
	app.Release(back)
	fake, err := app.OpenEndpoint(ctx, "fake", "m-fast")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetEndpointOn(first, "fake", fake); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveProfile("other"); err != nil {
		t.Fatal(err)
	}
	gone, err := app.Open(ctx, id)
	if err != nil || gone.Endpoint.Profile.Name != "fake" || gone.Session.Labels()["profile"] != "fake" {
		t.Fatalf("a session whose profile is gone resumes on the default %v", err)
	}
	app.Close(ctx)
}

func TestReviveOpensAnEmptySessionUnderItsId(t *testing.T) {
	srv, _ := fakeOpenAI(t)
	cfg, dir := testConfig(t, srv.URL)
	ctx := context.Background()
	app, err := Build(ctx, cfg, newSink().send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	gone := session.New().ID()
	if _, err := app.Open(ctx, gone); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a session never written is not found, %v", err)
	}
	l, err := app.Revive(gone)
	if err != nil || l.Session.ID() != gone || l.Session.Len() != 0 || l.Endpoint.Profile.Name != app.Primary.Profile.Name || app.LiveByID(gone) != l {
		t.Fatalf("revive opens an empty session under the id on the default endpoint, %v", err)
	}
	if same, err := app.Revive(gone); err != nil || same != l {
		t.Fatal("reviving an open session returns it")
	}
	if _, err := app.Revive("bad id!"); err == nil {
		t.Fatal("an invalid id is refused")
	}
	if err := app.Release(l); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Open(ctx, gone); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("releasing a revived empty session writes nothing")
	}
}
