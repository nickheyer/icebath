package builtin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Runs a hallmark script on the managed node in dir and returns stdout, stderr and the exit code
func runHallmark(t *testing.T, dir, script string, args ...string) (string, string, int) {
	t.Helper()
	cmd, err := HallmarkCommand(context.Background(), script, args...)
	if err != nil {
		t.Fatalf("managed node runtime: %v", err)
	}
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s: %v", script, err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// Decodes a script's JSON stdout keeping numbers as their literal text
func decodeJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return v
}

// Number field of a summary as an int
func num(v any) int {
	n, _ := v.(json.Number).Int64()
	return int(n)
}

// Findings of a sloplint JSON report with gate ids as strings
func lintFindings(j map[string]any) []map[string]string {
	var out []map[string]string
	list, _ := j["findings"].([]any)
	for _, raw := range list {
		f, _ := raw.(map[string]any)
		out = append(out, map[string]string{"gate": fmt.Sprint(f["gate"]), "grade": fmt.Sprint(f["grade"]), "evidence": fmt.Sprint(f["evidence"])})
	}
	return out
}

func TestHallmarkSloplintFixtureExpectations(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tree, err := Materialize()
	if err != nil {
		t.Fatal(err)
	}
	fix := tree.Payload(hallmarkSkill + "/scripts/fixtures")
	lint := func(target string, extra ...string) map[string]any {
		out, stderr, _ := runHallmark(t, t.TempDir(), "sloplint", append([]string{target, "--json"}, extra...)...)
		if out == "" {
			t.Fatalf("sloplint did not run on %s: %s", target, stderr)
		}
		return decodeJSON(t, out)
	}
	gates := func(j map[string]any) map[string]bool {
		g := map[string]bool{}
		for _, f := range lintFindings(j) {
			g[f["gate"]] = true
		}
		return g
	}
	exemplars, _ := filepath.Glob(filepath.Join(fix, "exemplars", "*.html"))
	if len(exemplars) != 4 {
		t.Fatalf("exemplars %v", exemplars)
	}
	for _, target := range append(exemplars, filepath.Join(fix, "eyebrow-pass"), filepath.Join(fix, "tailwind-pass")) {
		s := lint(target)["summary"].(map[string]any)
		if num(s["fails"]) != 0 || num(s["reflex"]) != 0 || num(s["warns"]) != 0 {
			t.Errorf("%s must lint clean, has %v FAIL, %v REFLEX, %v WARN", target, s["fails"], s["reflex"], s["warns"])
		}
	}
	fails54 := 0
	for _, f := range lintFindings(lint(filepath.Join(fix, "eyebrow-fail"))) {
		if f["gate"] == "54" && f["grade"] == "FAIL" {
			fails54++
		}
	}
	if fails54 != 6 {
		t.Errorf("eyebrow-fail: gate 54 must FAIL six times, got %d", fails54)
	}
	if gates(lint(filepath.Join(fix, "eyebrow-pass")))["54"] {
		t.Error("eyebrow-pass: gate 54 must not fire")
	}
	s := lint(filepath.Join(fix, "waiver"))["summary"].(map[string]any)
	for k, v := range map[string]int{"fails": 0, "reflex": 0, "waived": 5, "rejected": 2, "answered": 1, "notes": 1} {
		if num(s[k]) != v {
			t.Errorf("waiver: %s is %v, want %d", k, s[k], v)
		}
	}
	for name, want := range map[string][]string{
		"slopped.html":  {"1", "2a", "4", "7b", "10a", "15", "19", "20", "22", "46a", "48", "54"},
		"tailwind-fail": {"67", "54", "64", "10a", "12", "11", "2a", "2b", "29", "6a", "34", "48", "74", "3"},
	} {
		got := gates(lint(filepath.Join(fix, name)))
		for _, g := range want {
			if !got[g] {
				t.Errorf("%s must fire gate %s", name, g)
			}
		}
	}
	tmp := t.TempDir()
	project, shared := filepath.Join(tmp, "project"), filepath.Join(tmp, "shared")
	os.Mkdir(project, 0o755)
	os.Mkdir(shared, 0o755)
	os.WriteFile(filepath.Join(shared, "style.css"), []byte("p { color: var(--color-text); }"), 0o644)
	for link, target := range map[string]string{"a-shared": shared, "b-shared": shared, "parent": project} {
		if err := os.Symlink(target, filepath.Join(project, link)); err != nil {
			t.Fatal(err)
		}
	}
	if s := lint(project, "--scope", "component")["summary"].(map[string]any); num(s["files"]) != 1 {
		t.Errorf("directory symlink cycles must terminate and each real file must be linted once, files %v", s["files"])
	}
	work := t.TempDir()
	copyFile := func(src string) {
		data, _ := os.ReadFile(filepath.Join(fix, "kept", src))
		os.WriteFile(filepath.Join(work, "index.html"), data, 0o644)
	}
	copyFile("before.html")
	runHallmark(t, work, "sloplint", "index.html", "--before", "--json")
	copyFile("after.html")
	out, _, _ := runHallmark(t, work, "sloplint", "index.html", "--kept", "--json")
	var failed, warned []string
	for _, f := range lintFindings(decodeJSON(t, out)) {
		if f["gate"] == "kept" && f["grade"] == "FAIL" {
			failed = append(failed, f["evidence"])
		} else if f["gate"] == "kept" && f["grade"] == "WARN" {
			warned = append(warned, f["evidence"])
		}
	}
	fails, warns := strings.Join(failed, " | "), strings.Join(warned, " | ")
	for _, needle := range []string{"rooms.html", "nights", "track('book')", "book-button", "#nav-toggle"} {
		if !strings.Contains(fails, needle) {
			t.Errorf("the kept check must fail %s: %s", needle, fails)
		}
	}
	if !strings.Contains(warns, "nav-toggle") || strings.Contains(fails, "mailto:") {
		t.Errorf("kept warnings %q fails %q", warns, fails)
	}
}

func TestHallmarkRefcheckReportsClean(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, stderr, code := runHallmark(t, t.TempDir(), "refcheck")
	if code != 0 || !regexp.MustCompile(`(?m)^refcheck: (clean across \d+ reference files|0 error\(s\), \d+ warning\(s\) across \d+ reference files)$`).MatchString(out) {
		t.Fatalf("refcheck exit %d\n%s%s", code, out, stderr)
	}
	j, _, _ := runHallmark(t, t.TempDir(), "refcheck", "--json")
	if r := decodeJSON(t, j); num(r["errors"]) != 0 {
		t.Fatalf("refcheck errors %v", r["findings"])
	}
}

func TestHallmarkThemePhotosPicksFromLibrary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	raw, err := payloadFile(hallmarkSkill + "/scripts/photos.json")
	if err != nil {
		t.Fatal(err)
	}
	var lib struct {
		Photos []struct{ ID string } `json:"photos"`
	}
	json.Unmarshal(raw, &lib)
	ids := map[string]bool{}
	for _, p := range lib.Photos {
		ids[p.ID] = true
	}
	pick := func() []string {
		out, stderr, code := runHallmark(t, t.TempDir(), "theme", "photos", "golden hour street", "--seed", "s1", "--n", "3", "--json")
		if code != 0 {
			t.Fatalf("photos exit %d: %s", code, stderr)
		}
		j := decodeJSON(t, out)
		subjects := j["subjects"].([]any)
		first := subjects[0].(map[string]any)
		picks := first["picks"].([]any)
		if j["theme"] != nil || first["subject"] != "golden hour street" || len(picks) == 0 || len(picks) > 3 {
			t.Fatalf("photos %v", j)
		}
		var got []string
		for _, raw := range picks {
			p := raw.(map[string]any)
			id := fmt.Sprint(p["id"])
			if !ids[id] || (p["match"] != "close" && p["match"] != "partial") || fmt.Sprint(p["credit"]) == "" {
				t.Errorf("pick %v is not a library photo with a credit", p)
			}
			got = append(got, id)
		}
		return got
	}
	if a, b := pick(), pick(); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("--seed must repeat a pick: %v %v", a, b)
	}
}

// Listener handing connections from a channel to an http.Server
type chanListener struct {
	ch   chan net.Conn
	once sync.Once
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// CONNECT proxy terminating TLS for api.together.xyz with a test CA, returning the proxy URL and CA file
func togetherProxy(t *testing.T, handler http.Handler) (string, string) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "icebath test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api.together.xyz"}, DNSNames: []string{"api.together.xyz"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
	ln := &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
	inner := &http.Server{Handler: handler}
	go inner.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.together.xyz:443" {
			http.Error(w, "unexpected "+r.Method+" "+r.Host, http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		select {
		case ln.ch <- conn:
		case <-ln.done:
			conn.Close()
		}
	}))
	t.Cleanup(func() { proxy.Close(); inner.Close(); ln.Close() })
	return proxy.URL, caFile
}

func TestHallmarkImagegenTogetherEndpoint(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	png := []byte("\x89PNG\r\n\x1a\nhallmark-test")
	var mu sync.Mutex
	var auth string
	var body map[string]any
	requests, status := 0, http.StatusOK
	proxyURL, caFile := togetherProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		auth = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		if r.URL.Path != "/v1/images/generations" || status != http.StatusOK {
			http.Error(w, "quota exhausted", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data": [{"b64_json": %q}]}`, base64.StdEncoding.EncodeToString(png))
	}))
	work := t.TempDir()
	t.Setenv("TOGETHER_API_KEY", "")
	out, stderr, code := runHallmark(t, work, "imagegen", "a quiet harbour at dawn")
	if code != 3 || !strings.Contains(stderr, "TOGETHER_API_KEY not set. Image generation is opt-in") || out != "" {
		t.Fatalf("no key: exit %d %q %q", code, out, stderr)
	}
	if out, _, code = runHallmark(t, work, "imagegen", "x", "--size", "big", "--json"); code != 2 || decodeJSON(t, out)["error"] != "bad --size big (want WxH, e.g. 1536x1024)" {
		t.Fatalf("bad size: exit %d %s", code, out)
	}
	t.Setenv("TOGETHER_API_KEY", "tk-test")
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("https_proxy", proxyURL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("NODE_USE_ENV_PROXY", "1")
	t.Setenv("NODE_EXTRA_CA_CERTS", caFile)
	target := filepath.Join(work, "art", "hero.png")
	out, stderr, code = runHallmark(t, work, "imagegen", "a quiet harbour at dawn", "--size", "1536x1024", "--out", target, "--json")
	if code != 0 {
		t.Fatalf("imagegen exit %d %s %s", code, out, stderr)
	}
	j := decodeJSON(t, out)
	prov, _ := j["provenance"].(map[string]any)
	if j["path"] != target || j["cached"] != false || j["model"] != "black-forest-labs/FLUX.1.1-pro" || prov["generatedVia"] != "together" {
		t.Fatalf("imagegen result %v", j)
	}
	if data, _ := os.ReadFile(target); !bytes.Equal(data, png) {
		t.Fatal("generated image bytes differ from the API payload")
	}
	meta := decodeJSON(t, string(must(os.ReadFile(filepath.Join(work, "art", "hero.json")))))
	if meta["hash"] != prov["hash"] || meta["size"] != "1536x1024" {
		t.Fatalf("provenance file %v", meta)
	}
	mu.Lock()
	gotAuth, gotBody := auth, body
	mu.Unlock()
	auth, body = gotAuth, gotBody
	if auth != "Bearer tk-test" || body["model"] != "black-forest-labs/FLUX.1.1-pro" || body["prompt"] != "a quiet harbour at dawn" ||
		body["width"] != 1536.0 || body["height"] != 1024.0 || body["steps"] != 4.0 || body["n"] != 1.0 || body["response_format"] != "b64_json" {
		t.Fatalf("request auth %q body %v", auth, body)
	}
	out, _, code = runHallmark(t, work, "imagegen", "a quiet harbour at dawn", "--size", "1536x1024", "--out", target, "--json")
	mu.Lock()
	seen := requests
	status = http.StatusInternalServerError
	mu.Unlock()
	if j := decodeJSON(t, out); code != 0 || j["cached"] != true || seen != 1 {
		t.Fatalf("second identical request must hit the cache: %v after %d requests", j, seen)
	}
	out, _, code = runHallmark(t, work, "imagegen", "a storm", "--json")
	if j := decodeJSON(t, out); code != 1 || j["error"] != "together 500" || !strings.HasPrefix(fmt.Sprint(j["message"]), "image API 500: quota exhausted") {
		t.Fatalf("API failure exit %d %v", code, j)
	}
}

// Value of a two result call, failing nothing
func must[T any](v T, _ error) T { return v }

// Installs puppeteer-core with the managed npm into a temp prefix and points HALLMARK_PUPPETEER at it
func hallmarkRenderTier(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	runner, err := pm.Ensure(ctx, "npm", pm.EnsureOptions{BaseEnv: pm.WithIcebathNodePath(environMap())})
	if err != nil {
		t.Fatalf("managed npm: %v", err)
	}
	prefix := t.TempDir()
	cmd := runner.Command(ctx, "npm", "install", "--prefix", prefix, "--no-audit", "--no-fund", "--loglevel", "error", "puppeteer-core")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm install puppeteer-core: %v\n%s", err, out)
	}
	t.Setenv("HALLMARK_PUPPETEER", prefix)
	chromes := []string{os.Getenv("CHROME_PATH"), "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium"}
	for _, c := range chromes {
		if c != "" && isFile(c) {
			return
		}
	}
	t.Fatal("no Chrome or Chromium found; set CHROME_PATH")
}

func TestHallmarkRenderTierScripts(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tree, err := Materialize()
	if err != nil {
		t.Fatal(err)
	}
	fix := tree.Payload(hallmarkSkill + "/scripts/fixtures")
	_, stderr, code := runHallmark(t, t.TempDir(), "picture", "hum", filepath.Join(fix, "eyebrow-pass", "index.html"))
	if code != 1 || !strings.Contains(stderr, "picture.mjs needs puppeteer-core or puppeteer (local, global or HALLMARK_PUPPETEER)") {
		t.Fatalf("picture without puppeteer: exit %d %s", code, stderr)
	}
	out, _, _ := runHallmark(t, t.TempDir(), "sloplint", filepath.Join(fix, "eyebrow-pass"), "--render", "--json")
	if r := fmt.Sprint(decodeJSON(t, out)["summary"].(map[string]any)["render"]); !strings.HasPrefix(r, "not run: no puppeteer-core found") {
		t.Fatalf("render summary without puppeteer %q", r)
	}
	hallmarkRenderTier(t)
	page := t.TempDir()
	for _, name := range []string{"index.html"} {
		data, _ := os.ReadFile(filepath.Join(fix, "eyebrow-fail", name))
		os.WriteFile(filepath.Join(page, name), data, 0o644)
	}
	out, stderr, code = runHallmark(t, page, "sloplint-render", "index.html")
	var findings []any
	if err := json.Unmarshal([]byte(out), &findings); err != nil || code != 0 {
		t.Fatalf("sloplint-render exit %d %v\n%s%s", code, err, out, stderr)
	}
	shots, _ := filepath.Glob(filepath.Join(page, ".hallmark", "shots", "*.png"))
	if len(shots) == 0 {
		t.Fatalf("sloplint-render wrote no screenshots under .hallmark/shots")
	}
	out, _, _ = runHallmark(t, page, "sloplint", "index.html", "--render", "--json")
	if r := decodeJSON(t, out)["summary"].(map[string]any)["render"]; r != "ran" {
		t.Fatalf("sloplint --render summary %v", r)
	}
	shotsDir := filepath.Join(t.TempDir(), "screens")
	out, stderr, code = runHallmark(t, page, "capture", "index.html", "--out", shotsDir, "--name", "hero", "--sizes", "desktop", "--wait", "100")
	if code != 0 || !regexp.MustCompile(`(?m)^\S*hero\S*\.png  2880 x 1800  \(/, desktop\)$`).MatchString(out) || !strings.Contains(out, "1 screenshot.") {
		t.Fatalf("capture exit %d\n%s%s", code, out, stderr)
	}
	if pngs, _ := filepath.Glob(filepath.Join(shotsDir, "hero*.png")); len(pngs) != 1 {
		t.Fatalf("capture files %v", pngs)
	}
	out, stderr, code = runHallmark(t, page, "picture", "hum", "index.html", "1200")
	jpg := tree.Payload(hallmarkSkill + "/references/themes/hum.jpg")
	if code != 0 || !strings.HasPrefix(out, jpg+" (lower screen at y 1200") || !isFile(jpg) {
		t.Fatalf("picture exit %d\n%s%s", code, out, stderr)
	}
}

func TestHallmarkInstallCommandsMapOntoPluginSelection(t *testing.T) {
	h := hallmarkManagerHome(t)
	cmds := loadHallmark(t, h)
	ctx := context.Background()
	install, hook := cmds["hallmark-install"], cmds["hallmark-install-hook"]
	if install == nil || hook == nil {
		t.Fatalf("hallmark commands %v", cmds)
	}
	call := func(c func(context.Context, string) (any, error), args string) string {
		out, err := c(ctx, args)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		return fmt.Sprint(out)
	}
	if out := call(install.Handler, "--claude"); !strings.HasPrefix(out, "Unknown option: --claude\n\n/hallmark-install [--dry-run]") {
		t.Fatalf("unknown option %q", out)
	}
	if out := call(install.Handler, "--dry-run"); !strings.Contains(out, "Hallmark installer (dry run) · payload: ") || !strings.Contains(out, "would enable hallmark in plugins.enabled") {
		t.Fatalf("dry run %q", out)
	}
	out := call(install.Handler, "")
	if !strings.Contains(out, "installed -> plugins.enabled of "+filepath.Join(h, "config.yaml")) || strings.Contains(out, "failed:") {
		t.Fatalf("install %q", out)
	}
	if !enabledIn(t, h, "hallmark") {
		t.Fatal("hallmark is not in plugins.enabled after install")
	}
	if out := call(hook.Handler, "--global"); !strings.HasPrefix(out, "installed the Hallmark edit-time lint hook into "+filepath.Join(h, "config.yaml")+"\n") {
		t.Fatalf("install-hook %q", out)
	}
	if v, _ := config.FastSafeLoad(must(os.ReadFile(filepath.Join(h, "config.yaml")))); !strings.Contains(fmt.Sprint(v), "lint_hook:true") {
		t.Fatalf("lint hook setting not saved: %v", v)
	}
	if out := call(install.Handler, "--remove --dry-run"); !strings.Contains(out, "would disable the hallmark plugin") || !enabledIn(t, h, "hallmark") {
		t.Fatalf("remove dry run %q", out)
	}
	hallmarkUVCore(t)
	call(install.Handler, "--remove")
	if enabledIn(t, h, "hallmark") {
		t.Fatal("hallmark still enabled after --remove")
	}
}

// Loads the bundled plugins through a manager and returns its plugin commands
func loadHallmark(t *testing.T, h string) map[string]*plugins.PluginCommand {
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(context.Background(), true)
	return m.PluginCommands()
}

// Fresh profile home enabling the hallmark plugin
func hallmarkManagerHome(t *testing.T) string {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n    - hallmark\n"), 0o644)
	return h
}

// Points pm at a uv locked core project and an offline isolated tool store for the test
func hallmarkUVCore(t *testing.T) {
	ctx := context.Background()
	uv, python, found, err := pm.Toolchain(ctx, true, false)
	if err != nil || !found {
		t.Fatalf("uv toolchain: %v found %v", err, found)
	}
	tmp := t.TempDir()
	core := filepath.Join(tmp, "core")
	if err := os.MkdirAll(core, 0o755); err != nil {
		t.Fatal(err)
	}
	pyproject := "[project]\nname=\"hallmark-proof-core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\ndependencies=[]\n[tool.uv]\npackage=false\n"
	if err := os.WriteFile(filepath.Join(core, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(tmp, "store"))
	t.Setenv("UV_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("UV_OFFLINE", "1")
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	lock := exec.CommandContext(ctx, uv, "lock", "--python", python)
	lock.Dir = core
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, out)
	}
	t.Cleanup(pm.OverrideCoreRepo(core))
	t.Cleanup(pm.OverrideUVToolchain(uv, python))
}

// Reports whether plugins.enabled in the profile config lists a plugin
func enabledIn(t *testing.T, h, name string) bool {
	v, err := config.FastSafeLoad(must(os.ReadFile(filepath.Join(h, "config.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := v.(map[string]any)["plugins"].(map[string]any)
	list, _ := p["enabled"].([]any)
	for _, x := range list {
		if x == name {
			return true
		}
	}
	return false
}

func TestHallmarkInstallGatesAndHookScopes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	settings := map[string]any{}
	var enabled []string
	project := t.TempDir()
	inst := &hallmarkInstaller{
		command: func(ctx context.Context, script string, args ...string) (*exec.Cmd, error) {
			if script == "refcheck" {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'ERROR  SKILL.md:1  [link] broken'; exit 1"), nil
			}
			return exec.CommandContext(ctx, "true"), nil
		},
		enable:  func(_ context.Context, name string, _ io.Writer) error { enabled = append(enabled, name); return nil },
		disable: func(context.Context, string, io.Writer) error { return nil },
		cwd:     func(context.Context) string { return project },
		get: func(_ context.Context, key string, def any) (any, error) {
			if v, ok := settings[key]; ok {
				return v, nil
			}
			return def, nil
		},
		set: func(_ context.Context, key string, value any) error { settings[key] = value; return nil },
	}
	out, _ := inst.install(ctx, "")
	if !strings.Contains(fmt.Sprint(out), "\nrefcheck failed:\nERROR  SKILL.md:1  [link] broken\n") || !strings.Contains(fmt.Sprint(out), "Nothing installed. Fix the findings above, or pass --force to install anyway.") || len(enabled) != 0 {
		t.Fatalf("failed gate %q %v", out, enabled)
	}
	if out, _ := inst.install(ctx, "--force"); len(enabled) != 1 || !strings.Contains(fmt.Sprint(out), "Done. Invoke as: /hallmark:hallmark (icebath).") {
		t.Fatalf("forced install %q %v", out, enabled)
	}
	cfg := filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml")
	if out, _ := inst.installHook(ctx, "--remove"); out != "no Hallmark lint hook in "+cfg+" for "+project+"; nothing to remove\n" {
		t.Fatalf("remove before install %q", out)
	}
	if out, _ := inst.installHook(ctx, "--print"); !strings.HasPrefix(fmt.Sprint(out), "# add to "+cfg+" for "+project+" :\n{\n  \"plugins\": {") || !strings.Contains(fmt.Sprint(out), `"lint_hook_projects": [`) {
		t.Fatalf("print %q", out)
	}
	if out, _ := inst.installHook(ctx, ""); out != "installed the Hallmark edit-time lint hook into "+cfg+" for "+project+"\n"+hallmarkHookAdvice+"\n" {
		t.Fatalf("project install %q", out)
	}
	inst.installHook(ctx, "")
	if list := settings[hallmarkHookProjectsKey].([]any); len(list) != 1 || list[0] != project {
		t.Fatalf("project list %v", list)
	}
	if !inst.hookInstalled(ctx, filepath.Join(project, "src")) || inst.hookInstalled(ctx, t.TempDir()) {
		t.Fatal("project scope must cover the project tree only")
	}
	if out, _ := inst.installHook(ctx, "--remove"); out != "removed the Hallmark lint hook from "+cfg+" for "+project+"\n" || inst.hookInstalled(ctx, project) {
		t.Fatalf("project remove %q", out)
	}
	inst.installHook(ctx, "--global")
	if !inst.hookInstalled(ctx, t.TempDir()) {
		t.Fatal("global hook must cover every project")
	}
	if out, _ := inst.installHook(ctx, "--global --remove"); out != "removed the Hallmark lint hook from "+cfg+"\n" || inst.hookInstalled(ctx, project) {
		t.Fatalf("global remove %q", out)
	}
	settings[hallmarkHookProjectsKey] = "oops"
	if _, err := inst.installHook(ctx, ""); err == nil || !strings.Contains(err.Error(), "is not a list") {
		t.Fatalf("non list projects %v", err)
	}
}
