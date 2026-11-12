package dashboard

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Directive name to source text of a policy header
func parsePolicy(header string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		name, sources, _ := strings.Cut(strings.TrimSpace(part), " ")
		out[name] = sources
	}
	return out
}

// Inline script bodies and external sources of a document
func pageScripts(html string) (inline []string, external []string) {
	rest := html
	for {
		i := strings.Index(rest, "<script")
		if i < 0 {
			return
		}
		rest = rest[i:]
		gt := strings.IndexByte(rest, '>')
		tag := rest[:gt]
		rest = rest[gt+1:]
		end := strings.Index(rest, "</script>")
		if j := strings.Index(tag, `src="`); j >= 0 {
			src := tag[j+5:]
			external = append(external, src[:strings.IndexByte(src, '"')])
		} else {
			inline = append(inline, rest[:end])
		}
		rest = rest[end:]
	}
}

// Webapp app over a temp dist holding an index with an inline and a module script
func cspPage(t *testing.T) (*FastAPI, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	dist := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dist, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", `<html><head><script>window.booted = "<&>";</script>`+
		`<script type="module" src="/assets/app.js"></script></head><body></body></html>`)
	write("assets/app.js", `import("./lazy.js");`)
	write("assets/lazy.js", `export const loaded = true;`)
	app := NewFastAPI("", "", nil, nil)
	app.State.Set("ui_surface", "webapp")
	app.State.Set("auth_required", false)
	app.State.Set("web_dist", dist)
	mountSPA(app)
	return app, dist
}

func TestPageAllowsItsScriptsAndChunksButLocksPrivilegedExecution(t *testing.T) {
	for _, host := range []string{"testserver:9120", "webapp.example:9443"} {
		for _, path := range []string{"/", "/index.html", "/chat/session"} {
			app, _ := cspPage(t)
			client := newTestClient(t, app)
			response := client.get(path, "host", host, "X-Forwarded-Prefix", "/hermes")
			if response.Status != 200 {
				t.Fatalf("%s %s: %d %s %v", host, path, response.Status, response.Body, response.AppErr)
			}
			policy := parsePolicy(response.Headers.Get("content-security-policy"))
			if policy["object-src"] != "'none'" || policy["base-uri"] != "'none'" || policy["frame-ancestors"] != "'none'" ||
				policy["script-src-attr"] != "'none'" {
				t.Fatalf("policy %v", policy)
			}
			scripts := strings.Fields(policy["script-src"])
			if !slices.Contains(scripts, "'self'") || !slices.Contains(scripts, "blob:") {
				t.Fatalf("script-src %v", scripts)
			}
			for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "https:", "http:", "*"} {
				if slices.Contains(scripts, banned) {
					t.Fatalf("script-src allows %s", banned)
				}
			}
			inline, external := pageScripts(string(response.Body))
			if !slices.Equal(external, []string{"/hermes/assets/app.js"}) {
				t.Fatalf("external %v", external)
			}
			if !slices.ContainsFunc(inline, func(s string) bool { return strings.Contains(s, "__ICEBATH_BASE_PATH__") }) {
				t.Fatalf("inline %v", inline)
			}
			for _, script := range inline {
				d := sha256.Sum256([]byte(script))
				if !slices.Contains(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(d[:])+"'") {
					t.Fatalf("unhashed inline script %q", script)
				}
			}
			injected := sha256.Sum256([]byte("window.injected=true"))
			if slices.Contains(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(injected[:])+"'") {
				t.Fatal("foreign script hashed")
			}
			if policy["connect-src"] != "* ws: wss: data: blob:" {
				t.Fatalf("connect-src %q", policy["connect-src"])
			}
			for _, asset := range []string{"app.js", "lazy.js"} {
				chunk := client.get("/assets/" + asset)
				if chunk.Status != 200 || !strings.Contains(chunk.Headers.Get("content-type"), "javascript") {
					t.Fatalf("%s: %d %q", asset, chunk.Status, chunk.Headers.Get("content-type"))
				}
			}
			if !slices.Contains(strings.Fields(policy["img-src"]), "'self'") || policy["media-src"] != "* data: blob:" {
				t.Fatalf("policy %v", policy)
			}
			if !strings.Contains(response.Headers.Get("cache-control"), "no-store") {
				t.Fatalf("cache-control %q", response.Headers.Get("cache-control"))
			}
			preview := client.get("/assets/webapp-preview.html")
			previewScripts := strings.Fields(parsePolicy(preview.Headers.Get("content-security-policy"))["script-src"])
			if !slices.Contains(previewScripts, "'unsafe-inline'") || !slices.Contains(previewScripts, "'unsafe-eval'") {
				t.Fatalf("preview script-src %v", previewScripts)
			}
		}
	}
}

func TestEverySpellingOfTheEntryDocumentGetsThePolicy(t *testing.T) {
	for _, path := range []string{"/%2e%2findex.html", "/%2e/index.html", "/INDEX.HTML"} {
		app, dist := cspPage(t)
		if path == "/INDEX.HTML" {
			if _, err := os.Stat(filepath.Join(dist, "INDEX.HTML")); err != nil {
				continue
			}
		}
		response := newTestClient(t, app).get(path)
		if response.Status != 200 || !strings.Contains(response.Headers.Get("content-security-policy"), "'sha256-") ||
			!strings.Contains(string(response.Body), "__ICEBATH_BASE_PATH__") {
			t.Fatalf("%s: %d %q %v", path, response.Status, response.Body, response.AppErr)
		}
	}
}

func TestDashboardKeepsItsExistingDocumentHeaders(t *testing.T) {
	app, _ := cspPage(t)
	app.State.Set("ui_surface", "dashboard")
	if newTestClient(t, app).get("/").Headers.Contains("content-security-policy") {
		t.Fatal("dashboard page carries a CSP")
	}
}

// Whether the policy lets the page load the URL under the first of the directives it sets
func policyAllows(policy string, directives []string, raw string) bool {
	parsed := map[string][]string{}
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		name := strings.ToLower(fields[0])
		if _, ok := parsed[name]; !ok {
			parsed[name] = fields[1:]
		}
	}
	var sources []string
	found := false
	for _, d := range directives {
		if s, ok := parsed[d]; ok {
			sources, found = s, true
			break
		}
	}
	if !found {
		return true
	}
	u, _ := url.Parse(raw)
	host := u.Hostname()
	for _, s := range sources {
		if s == "*" || s == "https:" || s == host || s == "https://"+host ||
			(strings.HasPrefix(s, "https://*.") && strings.HasSuffix(host, s[len("https://*"):])) {
			return true
		}
	}
	return false
}

func TestALivePreviewCanLoadItsCdnLibrariesUnderItsOwnPolicy(t *testing.T) {
	loads := []struct {
		directives []string
		url        string
	}{
		{[]string{"script-src-elem", "script-src", "default-src"}, "https://cdn.jsdelivr.net/npm/d3@7.9.0/dist/d3.min.js"},
		{[]string{"style-src-elem", "style-src", "default-src"}, "https://cdn.jsdelivr.net/npm/katex@0.16.11/dist/katex.min.css"},
	}
	for _, load := range loads {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		t.Setenv("ICEBATH_SERVE_HEADLESS", "")
		dist := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dist, "assets"), 0o755)
		_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><head></head><body></body></html>"), 0o644)
		app := NewFastAPI("", "", nil, nil)
		app.State.Set("ui_surface", "webapp")
		app.State.Set("auth_required", false)
		app.State.Set("web_dist", dist)
		mountSPA(app)
		response := newTestClient(t, app).get("/assets/webapp-preview.html")
		if response.Status != 200 {
			t.Fatalf("%d %v", response.Status, response.AppErr)
		}
		for _, policy := range response.Headers.GetList("content-security-policy") {
			if !policyAllows(policy, load.directives, load.url) {
				t.Fatalf("%s blocks %s: %s", load.directives[0], load.url, policy)
			}
		}
	}
}

const spaPolicyHTML = `<html><head><script src="/assets/app.js"></script></head><body>fixture</body></html>`

// App over a temp dist with the policy fixture, the surface unset for nil
func spaPolicyApp(t *testing.T, surface any, gated bool) *testClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	saved := SessionToken()
	setSessionToken("spa-policy-session-token")
	t.Cleanup(func() { setSessionToken(saved) })
	dist := t.TempDir()
	_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte(spaPolicyHTML), 0o644)
	_ = os.WriteFile(filepath.Join(dist, "help.html"), []byte("<html><body>help</body></html>"), 0o644)
	app := NewFastAPI("", "", nil, nil)
	if surface != nil {
		app.State.Set("ui_surface", surface)
	}
	app.State.Set("auth_required", gated)
	app.State.Set("web_dist", dist)
	mountSPA(app)
	return newTestClient(t, app)
}

func TestSpaDocumentsShareTheBootstrapPolicy(t *testing.T) {
	for _, surface := range []any{"webapp", nil} {
		for _, prefix := range []string{"", "/hermes"} {
			for _, path := range []string{"/", "/chat/session", "/index.html", "/help.html"} {
				response := spaPolicyApp(t, surface, true).get(path, "X-Forwarded-Prefix", prefix)
				text := string(response.Body)
				if response.Status != 200 {
					t.Fatalf("%v %q %s: %d %v", surface, prefix, path, response.Status, response.AppErr)
				}
				if path == "/" || path == "/chat/session" || (path == "/index.html" && surface == "webapp") {
					if !strings.Contains(response.Headers.Get("cache-control"), "no-store") ||
						!strings.Contains(text, `window.__ICEBATH_BASE_PATH__="`+prefix+`"`) ||
						!strings.Contains(text, `src="`+prefix+`/assets/app.js"`) {
						t.Fatalf("%v %q %s: %q", surface, prefix, path, text)
					}
				}
				if strings.Contains(text, SessionToken()) {
					t.Fatalf("%v %q %s: token leaked", surface, prefix, path)
				}
			}
		}
	}
}

func TestTheDashboardServesIndexHtmlAndItsBootstrapAsOnMain(t *testing.T) {
	for _, gated := range []bool{false, true} {
		client := spaPolicyApp(t, nil, gated)
		raw := client.get("/index.html")
		if raw.Status != 200 || string(raw.Body) != spaPolicyHTML {
			t.Fatalf("gated %v: %d %q", gated, raw.Status, raw.Body)
		}
		if strings.Contains(string(client.get("/").Body), "__ICEBATH_UI_SURFACE__") {
			t.Fatalf("gated %v: surface named", gated)
		}
	}
}

func TestTheWebappPageNamesItsSurface(t *testing.T) {
	client := spaPolicyApp(t, "webapp", false)
	for _, path := range []string{"/", "/index.html"} {
		if !strings.Contains(string(client.get(path).Body), `window.__ICEBATH_UI_SURFACE__="webapp"`) {
			t.Fatalf("%s: surface not named", path)
		}
	}
}

// App over a dashboard dist with an initial profile set
func initialProfileApp(t *testing.T, initial string) *testClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	dist := filepath.Join(t.TempDir(), "web_dist")
	_ = os.MkdirAll(filepath.Join(dist, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><head></head><body>Dashboard</body></html>"), 0o644)
	app := NewFastAPI("", "", nil, nil)
	app.State.Set("web_dist", dist)
	app.State.Set("initial_profile", initial)
	mountSPA(app)
	return newTestClient(t, app)
}

func TestSpaBootstrapIncludesDashboardInitialProfile(t *testing.T) {
	response := initialProfileApp(t, "worker_x").get("/chat?resume=session-1")
	if response.Status != 200 || !strings.Contains(string(response.Body), `window.__ICEBATH_INITIAL_PROFILE__="worker_x";`) {
		t.Fatalf("%d %q", response.Status, response.Body)
	}
}

func TestSpaBootstrapEscapesInitialProfileForScriptContext(t *testing.T) {
	response := initialProfileApp(t, "bad</script><script>alert(1)</script>").get("/chat")
	text := string(response.Body)
	if response.Status != 200 || !strings.Contains(text, `bad<\/script><script>alert(1)<\/script>`) ||
		strings.Contains(text, "bad</script><script>alert(1)</script>") {
		t.Fatalf("%d %q", response.Status, text)
	}
}

// App over a dist with the shell fixture and an assets dir
func spaShellApp(t *testing.T, dist string, surface any) *testClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><head></head><body>spa shell</body></html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dist, "assets"), 0o755)
	app := NewFastAPI("", "", nil, nil)
	if surface != nil {
		app.State.Set("ui_surface", surface)
	}
	app.State.Set("web_dist", dist)
	mountSPA(app)
	return newTestClient(t, app)
}

func TestAFontInTheServedBundleWins(t *testing.T) {
	dist := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dist, "fonts"), 0o755)
	_ = os.WriteFile(filepath.Join(dist, "fonts", "Collapse-Regular.woff2"), []byte("bundle copy"), 0o644)
	response := spaShellApp(t, dist, "webapp").get("/fonts/Collapse-Regular.woff2")
	if response.Status != 200 || string(response.Body) != "bundle copy" {
		t.Fatalf("%d %q", response.Status, response.Body)
	}
}

func TestAMissingAssetIs404NotTheSpaShell(t *testing.T) {
	for _, surface := range []any{"webapp", nil} {
		for _, path := range []string{"/fonts/Missing-Regular.woff2", "/fonts-terminal/missing.woff2", "/ds-assets/missing.png",
			"/assets/missing-abc123.js", "/fonts/..%2f..%2fpyproject.toml"} {
			response := spaShellApp(t, t.TempDir(), surface).get(path)
			if response.Status != 404 || strings.Contains(string(response.Body), "spa shell") {
				t.Fatalf("%v %s: %d %q", surface, path, response.Status, response.Body)
			}
		}
	}
}

func TestClientRoutesStillGetTheSpaShell(t *testing.T) {
	for _, surface := range []any{"webapp", nil} {
		response := spaShellApp(t, t.TempDir(), surface).get("/chat/fonts-and-assets")
		if response.Status != 200 || !strings.Contains(string(response.Body), "spa shell") {
			t.Fatalf("%v: %d %q", surface, response.Status, response.Body)
		}
	}
}
