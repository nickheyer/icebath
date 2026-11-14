package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const platformWhere = "test-plugin/plugin.yaml"

func platformHostOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	}
	return "linux"
}

func writeBundle(t *testing.T, root, version string) string {
	t.Helper()
	app := filepath.Join(root, "Applications", "Thing.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleShortVersionString</key>
	<string>` + version + `</string>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

type m = map[string]any

func mustDecl(t *testing.T, rawApp, rawRequires any) *PlatformDeclaration {
	t.Helper()
	d, err := ParsePlatformDeclaration("thing-mcp", rawApp, rawRequires, platformWhere)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPlatformAppBlockParsesIntoOneAppDefPerOS(t *testing.T) {
	decl := mustDecl(t, m{
		"darwin": m{"presence": "bundle", "location": "/Applications/Thing.app", "version": m{"kind": "plist"}},
		"win32": m{
			"presence": "executable", "location": "%ProgramFiles%/Thing/thing.exe",
			"version":  m{"kind": "uninstall_registry", "display_name_prefix": "Thing"},
			"liveness": m{"kind": "server_json", "path": "%LOCALAPPDATA%/Thing/server.json", "endpoint_path": "/rpc"},
		},
	}, m{"app": true, "min_version": "2.3.0"})
	mac, okMac := decl.AppFor("darwin")
	win, okWin := decl.AppFor("win32")
	if !okMac || !okWin || len(decl.App.PerOS) != 2 {
		t.Fatalf("per os %v", decl.App.PerOS)
	}
	if mac.Presence != "bundle" || mac.VersionKind != "plist" || mac.LivenessKind != "none" {
		t.Fatalf("mac %+v", mac)
	}
	if win.VersionArg != "Thing" || win.EndpointPath != "/rpc" || win.LivenessPIDKey != "pid" {
		t.Fatalf("win %+v", win)
	}
	if !decl.RequiresApp() || *decl.MinVersion() != "2.3.0" {
		t.Fatalf("requires %+v", decl.Requires)
	}
}

func TestPlatformInvalidBlocksNameTheRule(t *testing.T) {
	exe := func(loc any) m { return m{"presence": "executable", "location": loc} }
	cases := []struct {
		app, requires any
		message       string
	}{
		{nil, m{"app": true}, "no 'app' block"},
		{nil, m{"min_version": "1.0\n"}, "dotted numeric"},
		{m{"linux": m{"presence": "executable", "location": "/x/y", "version": m{"kind": "plist"}}}, nil, "only valid under app.darwin"},
		{m{"win32": m{"presence": "executable", "location": "C:/x/y.exe", "version": m{"kind": "uninstall_registry"}}}, nil, "display_name_prefix"},
		{m{"freebsd": exe("/x")}, nil, "unknown OS keys"},
		{m{"win32": exe("C:/x/y.exe")}, m{"min_version": "1.0"}, "needs requires.app"},
		{m{"win32": exe("C:/x/y.exe")}, m{"app": true, "min_version": "latest"}, "dotted numeric"},
		{m{"win32": exe("C:/x/y.exe")}, m{"app": true, "min_version": "1.0"}, "needs a version source"},
		{m{"darwin": m{"presence": "bundle", "location": "../Up/Thing.app"}}, nil, "must be absolute"},
		{m{"darwin": m{"presence": "bundle", "location": "Relative/Thing.app"}}, nil, "must be absolute"},
		{m{"darwin": m{"presence": "bundle", "location": "https://example.test/Thing.app"}}, nil, "must be absolute"},
		{m{"win32": exe("Thing/thing.exe")}, nil, "must be absolute"},
		{nil, m{"gpu": "amd"}, "requires.gpu must be one of"},
		{m{"darwin": m{"presence": "bundle", "location": "/Applications/Thing */**/Thing.app"}}, nil, "must be absolute"},
		{m{"linux": exe([]any{})}, nil, "location is required"},
		{m{"linux": exe([]any{m{"kind": "registry", "name": "thing"}})}, nil, "must be a path or a mapping with kind one of"},
		{m{"linux": exe([]any{m{"kind": "app_bundle", "name": "Thing.app"}})}, nil, "only valid under app.darwin"},
		{m{"darwin": m{"presence": "bundle", "location": []any{m{"kind": "command", "name": "thing"}}}}, nil, "needs app.darwin.presence executable"},
		{m{"linux": exe([]any{m{"kind": "snap", "name": "../thing"}})}, nil, `location\[0\].name must be a bare name`},
		{m{"win32": exe([]any{m{"kind": "uninstall_registry", "display_name_prefix": "Thing"}})}, nil, `location\[0\].file must be a relative path`},
		{m{"linux": exe("/usr/bin/thing")}, m{"app": true, "min_version": "1.0"}, "needs a version source under app.linux"},
	}
	for _, c := range cases {
		_, err := ParsePlatformDeclaration("thing-mcp", c.app, c.requires, platformWhere)
		de, ok := err.(*PlatformDeclarationError)
		if !ok || !regexp.MustCompile(c.message).MatchString(de.Msg) {
			t.Errorf("%v %v: got %v, want %q", c.app, c.requires, err, c.message)
		}
	}
	_, err := ParsePlatformDeclaration("thing-mcp", m{"linux": exe([]any{m{"kind": "registry"}})}, nil, platformWhere)
	if want := "test-plugin/plugin.yaml: app.linux.location[0] must be a path or a mapping with kind one of ['app_bundle', 'command', 'flatpak', 'snap', 'uninstall_registry']"; err == nil || err.Error() != want {
		t.Fatalf("message %v", err)
	}
}

func TestPlatformAvailabilityNoRequirementsDoesNoIO(t *testing.T) {
	r := PlatformAvailabilityOf(mustDecl(t, nil, nil), "freebsd")
	if r.State != "no_requirements" || !r.Offerable() {
		t.Fatalf("%+v", r)
	}
}

func TestPlatformRequiresGPUGatesOnTheHostGPUClass(t *testing.T) {
	saved := platformGPUClass
	defer func() { platformGPUClass = saved }()
	for class, state := range map[string]string{"nvidia": "no_requirements", "unknown": "no_requirements", "intel": "unsupported_gpu", "apple_silicon": "unsupported_gpu", "none": "unsupported_gpu"} {
		platformGPUClass = func() string { return class }
		r := PlatformAvailabilityOf(mustDecl(t, nil, m{"gpu": "nvidia"}), "win32")
		if r.State != state || r.Offerable() != (state == "no_requirements") {
			t.Errorf("%s: %+v", class, r)
		}
	}
	platformGPUClass = func() string { t.Fatal("the OS gate must not read the GPU"); return "" }
	both := mustDecl(t, m{"win32": m{"presence": "executable", "location": "C:/x/y.exe"}}, m{"app": true, "gpu": "nvidia"})
	if got := PlatformAvailabilityOf(both, "darwin").State; got != "unsupported_os" {
		t.Fatal(got)
	}
}

func TestPlatformAvailabilityUnsupportedOSWhenNoBlockForHost(t *testing.T) {
	decl := mustDecl(t, m{"win32": m{"presence": "executable", "location": "C:/x/y.exe"}}, m{"app": true})
	if got := PlatformAvailabilityOf(decl, "darwin").State; got != "unsupported_os" {
		t.Fatal(got)
	}
}

func TestPlatformAvailabilityMissingThenPresentThenVersionGate(t *testing.T) {
	tmp := t.TempDir()
	location := filepath.Join(tmp, "Applications", "Thing.app")
	decl := mustDecl(t, m{"darwin": m{"presence": "bundle", "location": location, "version": m{"kind": "plist"}}}, m{"app": true, "min_version": "2.3.0"})
	missing := PlatformAvailabilityOf(decl, "darwin")
	if missing.State != "missing_app" || missing.Path == nil || !strings.Contains(*missing.Path, tmp) {
		t.Fatalf("missing %+v", missing)
	}
	writeBundle(t, tmp, "2.2.9")
	old := PlatformAvailabilityOf(decl, "darwin")
	if old.State != "version_too_old" || *old.Version != "2.2.9" || *old.MinVersion != "2.3.0" {
		t.Fatalf("old %+v", old)
	}
	writeBundle(t, tmp, "2.3.0")
	ok := PlatformAvailabilityOf(decl, "darwin")
	if ok.State != "available" || *ok.Version != "2.3.0" || !ok.Offerable() {
		t.Fatalf("ok %+v", ok)
	}
	if err := os.WriteFile(filepath.Join(location, "Contents", "Info.plist"), []byte("invalid plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := PlatformAvailabilityOf(decl, "darwin").State; got != "version_too_old" {
		t.Fatal(got)
	}
	insp := PlatformAppResolver{decl.App.PerOS["darwin"]}
	if v := insp.Inspect(insp.Locate(nil), nil).Version; v.State != checkError || v.Detail != "InvalidFileException" {
		t.Fatalf("invalid plist observation %+v", v)
	}
	if got := fmt.Sprint(ok.AsDict().Keys); got != "[state version path min_version]" {
		t.Fatal(got)
	}
}

func TestPlatformLocationListParsesEachKindInOrder(t *testing.T) {
	decl := mustDecl(t, m{
		"win32": m{"presence": "executable", "location": []any{
			m{"kind": "uninstall_registry", "display_name_prefix": "Thing", "file": "bin/thing.exe"},
			"%ProgramFiles%/Thing */thing.exe",
		}},
		"linux": m{"presence": "executable", "location": []any{m{"kind": "command", "name": "thing"}, m{"kind": "flatpak", "app_id": "org.thing.Thing"}}},
	}, nil)
	win, _ := decl.AppFor("win32")
	linux, _ := decl.AppFor("linux")
	if fmt.Sprint(win.Locations) != "[{uninstall_registry Thing bin/thing.exe} {path %ProgramFiles%/Thing */thing.exe }]" {
		t.Fatalf("win %v", win.Locations)
	}
	if fmt.Sprint(linux.Locations) != "[{command thing } {flatpak org.thing.Thing }]" {
		t.Fatalf("linux %v", linux.Locations)
	}
}

func TestPlatformMinVersionLetsALinuxBlockGoWithoutAVersionSource(t *testing.T) {
	decl := mustDecl(t, m{
		"darwin": m{"presence": "bundle", "location": "/Applications/Thing.app", "version": m{"kind": "plist"}},
		"linux":  m{"presence": "executable", "location": []any{m{"kind": "command", "name": "thing"}}},
	}, m{"app": true, "min_version": "2.0"})
	linux, ok := decl.AppFor("linux")
	if *decl.MinVersion() != "2.0" || !ok || linux.VersionKind != "none" {
		t.Fatalf("%+v", linux)
	}
}

func TestPlatformMinVersionPicksANewerCopyFoundAfterAnOldOne(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("THINGROOT", tmp)
	writeBundle(t, filepath.Join(tmp, "old"), "1.0")
	writeBundle(t, filepath.Join(tmp, "new"), "2.4")
	decl := mustDecl(t, m{"darwin": m{"presence": "bundle", "location": []any{"$THINGROOT/old/Applications/Thing.app", "$THINGROOT/new/Applications/Thing.app"}, "version": m{"kind": "plist"}}}, m{"app": true, "min_version": "2.0"})
	ok := PlatformAvailabilityOf(decl, "darwin")
	if ok.State != "available" || *ok.Version != "2.4" || !strings.Contains(*ok.Path, filepath.Join(tmp, "new")) {
		t.Fatalf("%+v", ok)
	}
	tooOld := mustDecl(t, m{"darwin": m{"presence": "bundle", "location": "$THINGROOT/old/Applications/Thing.app", "version": m{"kind": "plist"}}}, m{"app": true, "min_version": "2.0"})
	if got := PlatformAvailabilityOf(tooOld, "darwin"); got.Version == nil || *got.Version != "1.0" {
		t.Fatalf("%+v", got)
	}
}

func TestPlatformVersionAtLeastAcceptsOnlyBoundedASCIIComponents(t *testing.T) {
	for _, c := range []struct {
		found, minimum string
		want           bool
	}{{"2.3.0", "2.3.0", true}, {"2.3.0-beta", "2.3.0", false}, {"11.0.9.509", "11.0.9", true}, {"11.0.9", "11.0.9.509", false}, {"2.\u0663.0", "2.3.0", false}, {"1234567890", "2.3.0", false}} {
		if got := PlatformVersionAtLeast(c.found, c.minimum); got != c.want {
			t.Errorf("%q %q: %v", c.found, c.minimum, got)
		}
	}
}

func TestPlatformUnexpandedLocationsAreMissingEvenWhenCwdContainsThem(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	for _, location := range []string{"%ICEBATH_TEST_UNSET_VAR%/x.exe", "$ICEBATH_TEST_UNSET_VAR/x"} {
		path := filepath.Join(tmp, location)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("fixture"), 0o644)
		def := NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: location})
		if kind := (PlatformAppResolver{def}).Locate(nil).Kind; kind != "missing" {
			t.Fatalf("%s: %s", location, kind)
		}
	}
}

func TestPlatformRegistryMutationsAreVisibleAndNotifyOnce(t *testing.T) {
	decl := mustDecl(t, nil, nil)
	var mu sync.Mutex
	changes := 0
	SetPlatformDeclarationOnChange(func() { mu.Lock(); changes++; mu.Unlock() })
	defer SetPlatformDeclarationOnChange(nil)
	RegisterPlatformDeclaration("thing-mcp", decl)
	if LookupPlatformDeclaration("thing-mcp") != decl {
		t.Fatal("lookup")
	}
	UnregisterPlatformDeclaration("thing-mcp")
	if LookupPlatformDeclaration("thing-mcp") != nil {
		t.Fatal("unregister")
	}
	RegisterPlatformDeclaration("other", decl)
	ClearPlatformDeclarations()
	if LookupPlatformDeclaration("other") != nil || changes != 4 {
		t.Fatalf("changes %d", changes)
	}
}

func TestPlatformSkillRequiresAppsGate(t *testing.T) {
	tmp := t.TempDir()
	decl := mustDecl(t, m{platformHostOS(): m{"presence": "executable", "location": filepath.Join(tmp, "thing")}}, m{"app": true})
	ClearPlatformDeclarations()
	defer ClearPlatformDeclarations()
	if declared, _ := PlatformSkillAppAvailability("unknown"); declared {
		t.Fatal("unknown declared")
	}
	RegisterPlatformDeclaration("thing-mcp", decl)
	if declared, offerable := PlatformSkillAppAvailability("thing-mcp"); !declared || offerable {
		t.Fatal("missing app offerable")
	}
	os.WriteFile(filepath.Join(tmp, "thing"), []byte("fixture"), 0o644)
	if declared, offerable := PlatformSkillAppAvailability("thing-mcp"); !declared || !offerable {
		t.Fatal("present app not offerable")
	}
}

const platformToken = "tok-3e1f9c-unique-fixture-value"

func writeServerJSON(t *testing.T, dir, url string, pid int) string {
	t.Helper()
	if pid == 0 {
		pid = os.Getpid()
	}
	p := filepath.Join(dir, "server.json")
	data, _ := json.Marshal(map[string]any{"pid": pid, "http": url, "token": platformToken})
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func thingResolver(t *testing.T, dir, serverJSON string) PlatformAppResolver {
	exe := filepath.Join(dir, "thing")
	os.WriteFile(exe, nil, 0o644)
	def := NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: exe})
	if serverJSON != "" {
		def.LivenessKind, def.LivenessPath = "server_json", serverJSON
	}
	return PlatformAppResolver{def}
}

func mustProbe(t *testing.T, r PlatformAppResolver, effort ResolverEffort, deadline float64) resolverProbe {
	t.Helper()
	p, err := r.Probe(r.Locate(nil), effort, deadline)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlatformLocateReportsExpandedPathWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("THINGROOT", tmp)
	res := PlatformAppResolver{NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: "$THINGROOT/bin/thing"})}.Locate(nil)
	if res.Kind != "missing" || res.Candidates[0].Value != filepath.Join(tmp, "bin", "thing") {
		t.Fatalf("%+v", res)
	}
}

func TestPlatformBundlePresenceAndPlistVersion(t *testing.T) {
	app := writeBundle(t, t.TempDir(), "9.8.7")
	def := NewPlatformAppDef("thing", "darwin", "bundle", PlatformAppLocation{Kind: "path", Value: app})
	def.VersionKind = "plist"
	r := PlatformAppResolver{def}
	res := r.Locate(nil)
	if res.Kind != "known_path" || fmt.Sprint(res.Command()) != fmt.Sprint([]string{app}) {
		t.Fatalf("%+v", res)
	}
	insp := r.Inspect(res, nil)
	if insp.Version.State != checkPresent || *insp.Version.Value != "9.8.7" || insp.Signer.State != checkNotChecked || !insp.OK() {
		t.Fatalf("%+v", insp)
	}
}

func TestPlatformStarSegmentTriesTheHighestVersionFirst(t *testing.T) {
	tmp := t.TempDir()
	for _, v := range []string{"2.9", "2.10", "10.0b"} {
		os.MkdirAll(filepath.Join(tmp, "Thing "+v), 0o755)
		os.WriteFile(filepath.Join(tmp, "Thing "+v, "thing"), nil, 0o644)
	}
	res := PlatformAppResolver{NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: filepath.Join(tmp, "Thing *", "thing")})}.Locate(nil)
	var got []string
	for _, c := range res.Candidates {
		got = append(got, filepath.Base(filepath.Dir(c.Value)))
	}
	if res.Kind != "known_path" || strings.Join(got, ",") != "Thing 10.0b,Thing 2.10,Thing 2.9" {
		t.Fatalf("%s %v", res.Kind, got)
	}
}

func TestPlatformFirstPresentLocationSetsTheResolution(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "later", "thing")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, nil, 0o644)
	res := PlatformAppResolver{NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: filepath.Join(tmp, "absent", "thing")}, PlatformAppLocation{Kind: "path", Value: exe})}.Locate(nil)
	if res.Kind != "known_path" || res.Command()[0] != exe || res.Candidates[0].Present || !res.Candidates[1].Present {
		t.Fatalf("%+v", res)
	}
}

func TestPlatformInspectOnMissingIsNotChecked(t *testing.T) {
	def := NewPlatformAppDef("thing", platformHostOS(), "executable", PlatformAppLocation{Kind: "path", Value: filepath.Join(t.TempDir(), "none")})
	def.VersionKind = "plist"
	r := PlatformAppResolver{def}
	if v := r.Inspect(r.Locate(nil), nil).Version; v.State != checkNotChecked {
		t.Fatal(v.State)
	}
}

func TestPlatformProbeWithoutLivenessSourceIsAllNotChecked(t *testing.T) {
	pr := mustProbe(t, thingResolver(t, t.TempDir(), ""), EffortNetwork, 3)
	if pr.Running.State != checkNotChecked || pr.Answering.State != checkNotChecked || pr.Endpoint.State != checkNotChecked {
		t.Fatalf("%+v", pr)
	}
}

func TestPlatformProbeLocalReadsPIDAndEndpointButNeverConnects(t *testing.T) {
	tmp := t.TempDir()
	pr := mustProbe(t, thingResolver(t, tmp, writeServerJSON(t, tmp, "http://127.0.0.1:1/mcp", 0)), EffortLocal, 3)
	if pr.Running.State != checkPresent || !*pr.Running.Value || *pr.Endpoint.Value != "http://127.0.0.1:1/mcp" || pr.Answering.State != checkNotChecked {
		t.Fatalf("%+v", pr)
	}
}

func TestPlatformTokenNeverAppearsInAnyPublicResult(t *testing.T) {
	tmp := t.TempDir()
	r := thingResolver(t, tmp, writeServerJSON(t, tmp, "http://127.0.0.1:1/mcp", 0))
	res := r.Locate(nil)
	pr := mustProbe(t, r, EffortLocal, 3)
	for _, obj := range []any{res, r.Inspect(res, nil), pr} {
		if s := fmt.Sprintf("%#v %+v", obj, obj); strings.Contains(s, platformToken) {
			t.Fatalf("token leaked %s", s)
		}
	}
	ep, ok, err := r.Endpoint()
	if !ok || err != nil || strings.Contains(fmt.Sprintf("%v %#v %s", ep, ep, ep), platformToken) {
		t.Fatalf("endpoint repr leaked or missing %v %v", ok, err)
	}
	sess := platformReadServerJSON(r.Definition.LivenessPath, r.Definition)
	if strings.Contains(fmt.Sprintf("%v %#v", *sess, *sess), platformToken) {
		t.Fatal("session repr leaked")
	}
}

func TestPlatformNonLoopbackOrMalformedEndpointIsUnavailableAndNeverContacted(t *testing.T) {
	for _, url := range []string{"http://[::1", "https://127.0.0.1:1234/mcp", "http://user:pw@127.0.0.1:1234/mcp", "http://example.com:1234/mcp", "http://127.0.0.1/mcp", "http://127.0.0.1:99999/mcp"} {
		tmp := t.TempDir()
		pr := mustProbe(t, thingResolver(t, tmp, writeServerJSON(t, tmp, url, 0)), EffortNetwork, 3)
		if pr.Endpoint.State != checkUnavailable || pr.Answering.State != checkNotChecked {
			t.Errorf("%s: %+v", url, pr)
		}
	}
}

func TestPlatformDeadPIDIsAbsentAndSkipsNetwork(t *testing.T) {
	tmp := t.TempDir()
	pr := mustProbe(t, thingResolver(t, tmp, writeServerJSON(t, tmp, "http://127.0.0.1:1/mcp", 1<<22+12345)), EffortNetwork, 3)
	if pr.Running.State != checkAbsent || pr.Answering.State != checkNotChecked {
		t.Fatalf("%+v", pr)
	}
}

func TestPlatformEndpointReadsServerJSONEveryCall(t *testing.T) {
	tmp := t.TempDir()
	sj := writeServerJSON(t, tmp, "http://127.0.0.1:1111/ignored", 0)
	r := thingResolver(t, tmp, sj)
	if ep, ok, _ := r.Endpoint(); !ok || ep != (PlatformEndpoint{"http://127.0.0.1:1111/mcp", platformToken}) {
		t.Fatalf("%v", ep)
	}
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "http": "http://127.0.0.1:2222/ignored", "token": "next"})
	os.WriteFile(sj, data, 0o644)
	if ep, ok, _ := r.Endpoint(); !ok || ep != (PlatformEndpoint{"http://127.0.0.1:2222/mcp", "next"}) {
		t.Fatalf("%v", ep)
	}
}

func TestPlatformNetworkProbeReadsServerJSONEveryCallAndAnswers(t *testing.T) {
	var mu sync.Mutex
	var seen [][2]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, [2]string{r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		if body["method"] == "initialize" {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(400)
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	tmp := t.TempDir()
	sj := writeServerJSON(t, tmp, fmt.Sprintf("http://127.0.0.1:%d/ignored", port), 0)
	r := thingResolver(t, tmp, sj)
	res := r.Locate(nil)
	first, _ := r.Probe(res, EffortNetwork, 3)
	if first.Answering.State != checkPresent || !*first.Answering.Value {
		t.Fatalf("%+v", first)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != [2]string{"/mcp", "Bearer " + platformToken} {
		t.Fatalf("%v", last)
	}
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "http": "http://127.0.0.1:1/x", "token": platformToken})
	os.WriteFile(sj, data, 0o644)
	second, _ := r.Probe(res, EffortNetwork, 3)
	if *second.Endpoint.Value != "http://127.0.0.1:1/mcp" || second.Answering.State != checkAbsent {
		t.Fatalf("%+v", second)
	}
}

func TestPlatformNetworkProbeHonorsOneAbsoluteDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(200)
		for range 64 {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()
	tmp := t.TempDir()
	r := thingResolver(t, tmp, writeServerJSON(t, tmp, srv.URL+"/x", 0))
	start := time.Now()
	pr := mustProbe(t, r, EffortNetwork, 0.5)
	if elapsed := time.Since(start); elapsed >= 2*time.Second || pr.Answering.State != checkPresent {
		t.Fatalf("%v %+v", elapsed, pr)
	}
}
