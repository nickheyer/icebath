package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Transport answering every request through a handler, recording the URL and headers each request carried
type peIndexTransport struct {
	mu        sync.Mutex
	redirects map[string]string
	body      []byte
	sent      [][2]any
}

func (p *peIndexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.sent = append(p.sent, [2]any{req.URL.String(), req.Header.Clone()})
	destination := p.redirects[req.URL.String()]
	p.mu.Unlock()
	resp := &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(p.body)),
		Request: req, ContentLength: int64(len(p.body)), ProtoMajor: 1, ProtoMinor: 1}
	if destination != "" {
		resp.StatusCode, resp.Status = 302, "302 Found"
		resp.Header.Set("Location", destination)
	}
	return resp, nil
}

func TestUpdateIndexCredentialsMatchTheExactOrigin(t *testing.T) {
	for _, c := range []struct{ url, token string }{
		{"https://api.github.com/repos/example/tool/releases", "dummy-gh"},
		{"https://api.github.com:443/repos/example/tool/releases", "dummy-gh"},
		{"https://huggingface.co/api/buckets/example/tool/tree", "dummy-hf"},
		{"https://nodejs.org/dist/index.json", ""},
		{"https://registry.npmjs.org/npm", ""},
		{"https://evilhuggingface.co/index", ""},
		{"https://api.github.com.evil.invalid/index", ""},
		{"https://api.github.com:444/index", ""},
		{"http://api.github.com/index", ""},
	} {
		t.Run(c.url, func(t *testing.T) {
			transport := &peIndexTransport{body: []byte("{}")}
			peSwap[http.RoundTripper](t, &indexTransport, transport)
			t.Setenv("GH_TOKEN", "dummy-gh")
			t.Setenv("HF_TOKEN", "dummy-hf")
			got, err := fetchJSON(c.url)
			if err != nil || !reflect.DeepEqual(got, map[string]any{}) {
				t.Fatalf("got %v %v", got, err)
			}
			headers := transport.sent[len(transport.sent)-1][1].(http.Header)
			want := ""
			if c.token != "" {
				want = "Bearer " + c.token
			}
			if headers.Get("Authorization") != want || headers.Get("User-Agent") != "icebath-pm" {
				t.Fatalf("headers %v", headers)
			}
		})
	}
}

func TestUpdateIndexRedirectsUseTheCredentialSafeOpener(t *testing.T) {
	origin := "https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/b123"
	for _, reader := range []string{"json", "text", "release-digest"} {
		for _, c := range []struct {
			destination string
			survives    bool
		}{{"https://api.github.com/second", true}, {"https://unrelated.invalid/second", false}, {"https://api.github.com:444/second", false}, {"http://api.github.com/second", false}} {
			t.Run(reader+" "+c.destination, func(t *testing.T) {
				transport := &peIndexTransport{redirects: map[string]string{origin: c.destination},
					body: []byte(`{"assets": [{"name": "tool.zip", "digest": "sha256:abc123"}]}`)}
				peSwap[http.RoundTripper](t, &indexTransport, transport)
				t.Setenv("GH_TOKEN", "dummy-gh")
				switch reader {
				case "json":
					if _, err := fetchJSON(origin); err != nil {
						t.Fatal(err)
					}
				case "text":
					if _, err := fetchText(origin, nil); err != nil {
						t.Fatal(err)
					}
				default:
					peSwap(t, &releaseDigestCache, map[[2]string]map[string]string{})
					p, err := GetPackage("llamacpp-cpu")
					if err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 2; i++ {
						if sha, err := p.KnownSHA256("123", "https://github.com/example/tool.zip"); err != nil || sha != "abc123" {
							t.Fatalf("digest %q %v", sha, err)
						}
					}
				}
				if len(transport.sent) != 2 || transport.sent[0][0] != origin || transport.sent[1][0] != c.destination {
					t.Fatalf("sent %v", transport.sent)
				}
				initial, redirected := transport.sent[0][1].(http.Header), transport.sent[1][1].(http.Header)
				if initial.Get("Authorization") != "Bearer dummy-gh" || (redirected.Get("Authorization") == "Bearer dummy-gh") != c.survives {
					t.Fatalf("initial %v redirected %v", initial, redirected)
				}
			})
		}
	}
}

// Loopback upstream every index and artifact URL is routed to, recording paths, credentials and queued failures
type peUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	payloads map[string][]byte
	failures map[string][]int
	calls    [][2]string
}

// Starts the upstream and routes index reads and artifact downloads to it for one test
func newPEUpstream(t *testing.T) *peUpstream {
	t.Helper()
	u := &peUpstream{payloads: map[string][]byte{}, failures: map[string][]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls = append(u.calls, [2]string{r.RequestURI, r.Header.Get("Authorization")})
		pending := u.failures[r.RequestURI]
		var code int
		if len(pending) > 0 {
			code, u.failures[r.RequestURI] = pending[0], pending[1:]
		}
		body, ok := u.payloads[r.RequestURI]
		u.mu.Unlock()
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		if !ok {
			http.Error(w, "Not Found", 404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(u.srv.Close)
	target, _ := url.Parse(u.srv.URL)
	route := dltRoundTrip(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host, clone.Host = target.Scheme, target.Host, target.Host
		resp, err := http.DefaultTransport.RoundTrip(clone)
		if resp != nil {
			resp.Request = req
		}
		return resp, err
	})
	peSwap[http.RoundTripper](t, &indexTransport, route)
	peSwap(t, &downloadClient, &http.Client{Transport: route, CheckRedirect: downloadClient.CheckRedirect})
	return u
}

// Sets payloads, encoding non byte values as JSON
func (u *peUpstream) set(payloads map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.payloads = map[string][]byte{}
	for path, v := range payloads {
		u.payloads[path] = peBody(v)
	}
}

// Bytes of a payload value, strings verbatim and everything else as JSON
func peBody(v any) []byte {
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	data, _ := json.Marshal(v)
	return data
}

// Request counts per path
func (u *peUpstream) counts() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]int{}
	for _, c := range u.calls {
		out[c[0]]++
	}
	return out
}

// Recorded calls, cleared afterwards when asked
func (u *peUpstream) taken(clear bool) [][2]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := append([][2]string{}, u.calls...)
	if clear {
		u.calls = nil
	}
	return out
}

// Releases with one version tag first and prereleases filling the page
func peReleasePage(version string) []any {
	page := []any{map[string]any{"tag_name": version}}
	for i := 0; i < 29; i++ {
		page = append(page, map[string]any{"tag_name": "9.0.0", "prerelease": true})
	}
	return page
}

func TestUpdateResolutionReusesSuccessfulResponsesButRefreshesNextOperation(t *testing.T) {
	for _, name := range []string{"npm", "node", "uv", "python", "llamacpp-cpu", "ffmpeg"} {
		t.Run(name, func(t *testing.T) {
			tmp := isolateHome(t)
			upstream := newPEUpstream(t)
			p, err := GetPackage(name)
			if err != nil {
				t.Fatal(err)
			}
			var targets []string
			for _, target := range AllTargets {
				if p.MissingReason(target) == "" {
					targets = append(targets, target)
				}
			}
			var version, retryPath string
			var expected map[string]int
			var payloadPaths []string
			for generation := 1; generation <= 2; generation++ {
				t.Setenv("ICEBATH_HOME", filepath.Join(tmp, fmt.Sprintf("profile-%d", generation)))
				t.Setenv(EnvRuntimeDir, filepath.Join(tmp, fmt.Sprintf("store-%d", generation)))
				t.Setenv("GH_TOKEN", fmt.Sprintf("github-%d", generation))
				t.Setenv("HF_TOKEN", fmt.Sprintf("huggingface-%d", generation))
				upstream.taken(true)
				version = fmt.Sprintf("2.0.%d", generation)
				locked := "1.0.0"
				expectedTargets := targets
				var artifacts map[string]any
				var ordered [][2]any
				btbnTag := ""
				switch name {
				case "npm":
					ordered = [][2]any{{"/-/package/npm/dist-tags", map[string]any{"latest": version}}}
				case "node":
					ordered = [][2]any{{"/dist/index.json", []any{map[string]any{"version": "v" + version}, map[string]any{"version": "nightly"}}}}
				case "uv":
					for page := 1; page <= 3; page++ {
						ordered = append(ordered, [2]any{fmt.Sprintf("/repos/astral-sh/uv/releases?per_page=30&page=%d", page), peReleasePage(version)})
					}
				case "python":
					version = fmt.Sprintf("3.14.%d+20990102", generation)
					locked = "3.14.0+20990101"
					expectedTargets = []string{"linux-x64"}
					ordered = [][2]any{{"/repos/astral-sh/python-build-standalone/releases?per_page=30&page=1", []any{map[string]any{
						"tag_name": "20990102", "assets": []any{
							map[string]any{"name": "cpython-" + version + "-x86_64-unknown-linux-gnu-install_only.tar.gz"},
							map[string]any{"name": "cpython-3.15.0+20990102-aarch64-apple-darwin-install_only.tar.gz"},
						}}}}}
				case "llamacpp-cpu":
					version = strconv.Itoa(100 + generation)
					locked = "100"
					ordered = [][2]any{{"/buckets/ggml-org/install.sh/resolve/latest", "b" + version + "\n"},
						{"/api/buckets/ggml-org/install.sh/tree?limit=1000&offset=0", []any{}}}
				default:
					version, locked = "9.1.2", "9.1.2"
					artifacts = map[string]any{}
					expectedTargets = nil
					for _, target := range targets {
						if target != "linux-arm64-bionic" {
							expectedTargets = append(expectedTargets, target)
						}
					}
					var links []string
					for _, osName := range []string{"linux", "macos"} {
						for _, arch := range []string{"amd64", "arm64"} {
							links = append(links, fmt.Sprintf(`<a href="/download/%s/%s/%d_%s/ffmpeg.zip">build</a>`, osName, arch, generation, version))
						}
					}
					var assets []any
					for _, arch := range []string{"64", "arm64"} {
						assets = append(assets, map[string]any{"name": fmt.Sprintf("ffmpeg-n%s-1-gabcdef-win%s-gpl-9.1.zip", version, arch)},
							map[string]any{"name": fmt.Sprintf("ffmpeg-n%s-1-gabcdef-linux%s-gpl-9.1.tar.xz", version, arch)})
					}
					btbnTag = fmt.Sprintf("autobuild-2026-%02d-28-12-00", generation)
					ordered = [][2]any{{"/", strings.Join(links, "\n")}, {"/repos/BtbN/FFmpeg-Builds/releases?per_page=30&page=1", []any{
						map[string]any{"tag_name": "autobuild-2026-12-01-12-00", "assets": []any{}},
						map[string]any{"tag_name": btbnTag, "assets": assets}}}}
				}
				payloads := map[string]any{}
				payloadPaths = nil
				for _, kv := range ordered {
					payloads[kv[0].(string)] = kv[1]
					payloadPaths = append(payloadPaths, kv[0].(string))
				}
				upstream.set(payloads)
				retryPath = payloadPaths[0]
				upstream.mu.Lock()
				upstream.failures[retryPath] = []int{503}
				upstream.mu.Unlock()
				decision, err := ResolvePackage(p, targets, &locked, artifacts)
				if err != nil {
					t.Fatal(err)
				}
				per := map[string]string{}
				for _, tv := range decision.PerTarget {
					per[tv.Target] = tv.Version
				}
				want := map[string]string{}
				for _, target := range expectedTargets {
					want[target] = version
				}
				if decision.Version == nil || *decision.Version != version || !reflect.DeepEqual(per, want) || !decision.Changed() {
					t.Fatalf("generation %d decision %v %v", generation, decision.Version, per)
				}
				if name == "ffmpeg" {
					keys := sortedKeys(decision.ArtifactUpdates)
					if !reflect.DeepEqual(keys, sortedSet(expectedTargets)) {
						t.Fatalf("artifact updates %v", keys)
					}
					for _, urls := range decision.ArtifactUpdates {
						if !strings.Contains(urls[0], fmt.Sprintf("/%d_%s/", generation, version)) && !strings.Contains(urls[0], "/"+btbnTag+"/") {
							t.Fatalf("artifact url %s", urls[0])
						}
					}
				}
				expected = map[string]int{}
				for _, path := range payloadPaths {
					expected[path] = 1
				}
				expected[retryPath]++
				if got := upstream.counts(); !reflect.DeepEqual(got, expected) {
					t.Fatalf("generation %d calls %v want %v", generation, got, expected)
				}
				for _, call := range upstream.taken(false) {
					token := ""
					switch {
					case strings.HasPrefix(call[0], "/repos/"):
						token = fmt.Sprintf("github-%d", generation)
					case strings.Contains(call[0], "ggml-org/install.sh"):
						token = fmt.Sprintf("huggingface-%d", generation)
					}
					if (token == "" && call[1] != "") || (token != "" && call[1] != "Bearer "+token) {
						t.Fatalf("%s carried %q", call[0], call[1])
					}
				}
			}
			upstream.taken(true)
			upstream.mu.Lock()
			upstream.failures[retryPath] = []int{404}
			upstream.mu.Unlock()
			locked := "1.0.0"
			switch name {
			case "npm":
				_, err := ResolvePackage(p, targets, &locked, nil)
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.Code != 404 || len(upstream.taken(false)) != 1 {
					t.Fatalf("404 resolution %v", err)
				}
				if d, err := ResolvePackage(p, targets, &locked, nil); err != nil || *d.Version != version {
					t.Fatalf("recovered resolution %v %v", d, err)
				}
				if got := upstream.counts(); !reflect.DeepEqual(got, map[string]int{retryPath: 2}) {
					t.Fatalf("calls %v", got)
				}
			case "llamacpp-cpu":
				fallback := "/repos/ggml-org/llama.cpp/releases?per_page=30&page=1"
				upstream.mu.Lock()
				upstream.payloads[fallback] = peBody([]any{map[string]any{"tag_name": "b" + version}})
				upstream.mu.Unlock()
				locked = "100"
				if d, err := ResolvePackage(p, targets, &locked, nil); err != nil || *d.Version != version {
					t.Fatalf("fallback resolution %v %v", d, err)
				}
				want := map[string]int{fallback: 1}
				for k, v := range expected {
					want[k] = v
				}
				if got := upstream.counts(); !reflect.DeepEqual(got, want) {
					t.Fatalf("calls %v want %v", got, want)
				}
			case "uv":
				upstream.mu.Lock()
				upstream.failures = map[string][]int{}
				upstream.mu.Unlock()
				endpoint := "https://api.github.com" + retryPath
				_, err := ReuseIndexResponses(func() (any, error) {
					for _, credential := range []string{"first", "second"} {
						t.Setenv("GH_TOKEN", credential)
						for i := 0; i < 2; i++ {
							got, err := getJSON(endpoint)
							if err != nil {
								return nil, err
							}
							if len(got.([]any)) != 30 {
								t.Fatalf("page %v", got)
							}
						}
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := upstream.taken(false); !reflect.DeepEqual(got, [][2]string{{retryPath, "Bearer first"}, {retryPath, "Bearer second"}}) {
					t.Fatalf("calls %v", got)
				}
			}
		})
	}
}

func TestUpdateLlamaRealIndexFallback(t *testing.T) {
	for i, c := range []struct {
		pointer  *string
		tree     []any
		expected []string
	}{
		{strPtr("b10679\n"), []any{map[string]any{"path": "b10326/linux"}, map[string]any{"path": "b10098/win"}, map[string]any{"path": "b10326/mac"},
			map[string]any{"path": "b9733/mac"}, map[string]any{"path": "latest/other"}}, []string{"10679", "10326", "10098", "9733"}},
		{nil, []any{}, []string{"10362", "10217"}},
		{strPtr("malformed"), []any{map[string]any{"path": "not-a-build"}, map[string]any{}}, []string{"10362", "10217"}},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			upstream := newPEUpstream(t)
			for _, k := range []string{"HF_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			github := "/repos/ggml-org/llama.cpp/releases?per_page=30&page=1"
			payloads := map[string]any{"/api/buckets/ggml-org/install.sh/tree?limit=1000&offset=0": c.tree, github: `[{"tag_name":"b10362"},{"tag_name":"b10217"}]`}
			if c.pointer != nil {
				payloads["/buckets/ggml-org/install.sh/resolve/latest"] = *c.pointer
			}
			upstream.set(payloads)
			p, _ := GetPackage("llamacpp-cpu")
			got, err := p.LatestVersions("linux-x64", "")
			if err != nil || !reflect.DeepEqual(got, c.expected) {
				t.Fatalf("got %v %v", got, err)
			}
			usedGitHub := false
			for _, call := range upstream.taken(false) {
				if call[1] != "" {
					t.Fatalf("%s carried credentials", call[0])
				}
				usedGitHub = usedGitHub || call[0] == github
			}
			if usedGitHub != (c.pointer == nil || *c.pointer != "b10679\n") {
				t.Fatalf("github consulted %v", usedGitHub)
			}
		})
	}
}

func TestUpdatePinningHashesNewNpmURLOnceAndKeepsExistingRows(t *testing.T) {
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "store"))
	upstream := newPEUpstream(t)
	p, _ := GetPackage("npm")
	artifactURL, err := p.FetchURL("2.0.0", AllTargets[0])
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(artifactURL)
	tarball := makeTarGz(t, []member{{Name: "package/package.json", Data: []byte(`{"version":"2.0.0"}`)}}, 0o755)
	upstream.set(map[string]any{parsed.Path: string(tarball)})
	oldURL, _ := p.FetchURL("1.0.0", AllTargets[0])
	old := map[string]any{"url": oldURL, "sha256": strings.Repeat("a", 64)}
	current := map[string]any{"any": old, "unresolved-target": old}
	var per []TargetVersion
	for _, target := range AllTargets {
		per = append(per, TargetVersion{target, "2.0.0"})
	}
	decision := &Resolved{Name: "npm", Locked: strPtr("1.0.0"), Style: "semver", Version: strPtr("2.0.0"), PerTarget: per, ArtifactUpdates: map[string][]string{}}
	pinned, err := pinArtifacts(p, decision, current)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"any": old, "unresolved-target": old}
	for _, target := range AllTargets {
		want[target] = map[string]any{"url": artifactURL, "sha256": hexSHA256(tarball)}
	}
	if !reflect.DeepEqual(pinned, want) || !reflect.DeepEqual(current, map[string]any{"any": old, "unresolved-target": old}) {
		t.Fatalf("pinned %v", pinned)
	}
	if got := upstream.counts(); !reflect.DeepEqual(got, map[string]int{parsed.Path: 1}) {
		t.Fatalf("calls %v", got)
	}
	upstream.taken(true)
	if again, err := pinArtifacts(p, decision, pinned); err != nil || !reflect.DeepEqual(again, pinned) || len(upstream.taken(false)) != 0 {
		t.Fatalf("repin of current rows %v %v", again, err)
	}
	replacement := []byte("changed upstream bytes")
	upstream.mu.Lock()
	upstream.payloads[parsed.Path] = replacement
	upstream.failures[parsed.Path] = []int{404}
	upstream.mu.Unlock()
	_, err = pinArtifacts(p, decision, current)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != 404 {
		t.Fatalf("404 pin %v", err)
	}
	upstream.taken(true)
	repinned, err := pinArtifacts(p, decision, current)
	if err != nil || repinned[AllTargets[len(AllTargets)-1]].(map[string]any)["sha256"] != hexSHA256(replacement) {
		t.Fatalf("repinned %v %v", repinned, err)
	}
	if got := upstream.counts(); !reflect.DeepEqual(got, map[string]int{parsed.Path: 1}) {
		t.Fatalf("calls %v", got)
	}
}

func TestUpdateMetadataReadsRetryTransientHTTPFailure(t *testing.T) {
	body := []byte(`{"assets": [{"name": "tool.zip", "digest": "sha256:abc123"}]}`)
	for _, reader := range []string{"json", "text", "digests"} {
		for _, shortBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", reader, shortBody), func(t *testing.T) {
				srv := newRangeServer(t)
				srv.set("/metadata", body)
				requests := &dltLog{}
				dltConfigure(srv, func(s *rangeServer) {
					s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
						if requests.add(r.URL.Path) != 1 {
							return false
						}
						if !shortBody {
							dltSendError(w, 503)
							return true
						}
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
						w.Header().Set("Connection", "close")
						w.WriteHeader(200)
						w.Write(body[:len(body)/2])
						w.(http.Flusher).Flush()
						return true
					}
				})
				endpoint := srv.url("/metadata")
				var got, want any
				var err error
				switch reader {
				case "json":
					got, err = fetchJSON(endpoint)
					want = map[string]any{"assets": []any{map[string]any{"name": "tool.zip", "digest": "sha256:abc123"}}}
				case "text":
					got, err = fetchText(endpoint, nil)
					want = string(body)
				default:
					local, _ := url.Parse(endpoint)
					peSwap[http.RoundTripper](t, &indexTransport, dltRoundTrip(func(req *http.Request) (*http.Response, error) {
						if req.URL.Host != "api.github.com" {
							t.Fatalf("release digests asked %s", req.URL)
						}
						clone := req.Clone(req.Context())
						clone.URL, clone.Host = local, local.Host
						resp, err := http.DefaultTransport.RoundTrip(clone)
						if resp != nil {
							resp.Request = req
						}
						return resp, err
					}))
					peSwap(t, &releaseDigestCache, map[[2]string]map[string]string{})
					got, err = githubReleaseDigests("test/repo", "test")
					want = map[string]string{"tool.zip": "abc123"}
				}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("got %v %v", got, err)
				}
				if n := len(requests.list()); n != 2 {
					t.Fatalf("%d requests", n)
				}
			})
		}
	}
}

// Dependency refresh project, a fixture pinned in its own lock with the update reading from it
func peDependencyProject(t *testing.T, root string) (string, string, *Lockfile) {
	t.Helper()
	repo := filepath.Join(root, "repo")
	peWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\nname=\"update-proof\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n[tool.uv]\npackage=false\n")
	caller := filepath.Join(root, "unrelated-project")
	peWrite(t, filepath.Join(caller, "pyproject.toml"), "not a project")
	lockPath := filepath.Join(repo, "pm", "lock.json")
	ieEmptyLock(t, lockPath)
	lock := NewLockfile(lockPath)
	lock.SetPin("manual-fixture", "1", map[string]any{})
	lock.Save()
	peRegister(t, &peUpdateFixture{BasePackage: BasePackage{Name: "manual-fixture"}})
	peSwap(t, &repoRoot, func() string { return repo })
	peSwap(t, &lockfilePath, func() string { return lockPath })
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "home"))
	t.Setenv(EnvRuntimeDir, filepath.Join(root, "store"))
	t.Chdir(caller)
	run := updateRunLive
	peSwap(t, &updateRunLive, func(ctx context.Context, out io.Writer, command []string, cwd string, env map[string]string, timeout time.Duration) (int, string, error) {
		if resolvePath(cwd) != resolvePath(repo) || !isRelativeTo(resolvePath(command[0]), resolvePath(root)) {
			t.Fatalf("dependency command %v in %s outside the temporary project", command, cwd)
		}
		return run(ctx, out, command, cwd, env, timeout)
	})
	return repo, caller, lock
}

// Why a prefix cannot serve as a standalone Python entry, empty when it can
func peSharedPrefix(root string) string {
	count := 0
	reason := ""
	filepathWalkStop(root, func(path string, regular bool) bool {
		count++
		if count > 50000 {
			reason = "more than 50000 files"
			return false
		}
		if regular && !canRead(path) {
			reason = path + " is unreadable"
			return false
		}
		return true
	})
	return reason
}

func TestUpdateUVRefreshUsesRealInstalledToolAndOnlyTheOwnedProject(t *testing.T) {
	root := t.TempDir()
	repo, caller, lock := peDependencyProject(t, root)
	uv := peUV(t)
	store := filepath.Join(root, "store")
	managed := filepath.Join(store, "uv-fixture", filepath.Base(uv))
	writeFile(t, managed, readFile(t, uv), 0o755)
	python := resolvePath(pePinnedPython(t))
	pythonRoot := filepath.Dir(filepath.Dir(python))
	if shared := peSharedPrefix(pythonRoot); shared != "" {
		t.Fatalf("pinned Python prefix %s is not self-contained: %s", pythonRoot, shared)
	}
	target, _ := CurrentTarget()
	facts, _ := NewFacts(filepath.Join(store, "facts.json"), false)
	for _, row := range [][2]string{{"uv", filepath.Dir(managed)}, {"python", pythonRoot}} {
		p, _ := GetPackage(row[0])
		lock.SetPin(row[0], "fixture", map[string]any{target: map[string]any{"url": "https://example.invalid/" + row[0], "sha256": strings.Repeat("1", 64)}})
		digest, err := TreeDigest(row[1])
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(store, row[1])
		if err := facts.Record(row[0], "fixture", rel, p.Env(row[1], target), store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("1", 64)}, Digest: &digest}); err != nil {
			t.Fatal(err)
		}
	}
	lock.Save()
	beforeLock := peRead(t, lock.Path)
	parentEnv := os.Environ()
	var syncs int
	peSwap(t, &updateSyncVenvStep, func(ctx context.Context, out io.Writer) (bool, error) {
		syncs++
		fmt.Fprintln(out, "✓ venv")
		return true, nil
	})
	ctx := context.Background()
	args := UpdateArgs{Names: []string{"manual-fixture"}, Check: true, UV: true}
	var out bytes.Buffer
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 0 || existsOrLink(filepath.Join(repo, "uv.lock")) || syncs != 0 {
		t.Fatalf("check %d %v", rc, err)
	}
	if !strings.Contains(out.String(), "uv lock --upgrade") {
		t.Fatalf("check output %q", out.String())
	}
	args.Check = false
	if rc, err := CmdUpdate(ctx, args, io.Discard); err != nil || rc != 0 {
		t.Fatalf("apply %d %v", rc, err)
	}
	if !isRegularFile(filepath.Join(repo, "uv.lock")) || existsOrLink(filepath.Join(caller, "uv.lock")) || syncs != 1 ||
		peRead(t, lock.Path) != beforeLock || !reflect.DeepEqual(os.Environ(), parentEnv) {
		t.Fatal("uv refresh touched the wrong project or state")
	}
	original := peRead(t, filepath.Join(repo, "uv.lock"))
	syncs = 0
	peWrite(t, filepath.Join(repo, "pyproject.toml"), "invalid project [")
	out.Reset()
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 1 || peRead(t, filepath.Join(repo, "uv.lock")) != original || syncs != 0 ||
		!strings.Contains(out.String(), "Python lock refresh failed") {
		t.Fatalf("invalid project %d %v %q", rc, err, out.String())
	}
	os.Remove(managed)
	out.Reset()
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 1 || peRead(t, filepath.Join(repo, "uv.lock")) != original || syncs != 0 ||
		!strings.Contains(out.String(), "Python lock refresh failed") {
		t.Fatalf("missing uv %d %v %q", rc, err, out.String())
	}
}

func TestUpdateNpmRefreshUsesItsInstalledEntryAndOwnedProject(t *testing.T) {
	root := t.TempDir()
	repo, caller, lock := peDependencyProject(t, root)
	user := filepath.Join(root, "user")
	os.MkdirAll(user, 0o777)
	for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(k, user)
	}
	target, _ := CurrentTarget()
	store := filepath.Join(root, "store")
	node, _ := GetPackage("node")
	npm, _ := GetPackage("npm")
	nodeEntry := filepath.Join(store, "node-fixture")
	nodeBinary := node.Binary(nodeEntry, target)
	writeFile(t, nodeBinary, []byte("#!/bin/sh\nexit 99\n"), 0o755)
	facts, _ := NewFacts(filepath.Join(store, "facts.json"), false)
	lock.SetPin("node", "fixture", map[string]any{target: map[string]any{"url": "https://example.invalid/node", "sha256": strings.Repeat("1", 64)}})
	if err := facts.Record("node", "fixture", "node-fixture", node.Env(nodeEntry, target), store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("1", 64)}}); err != nil {
		t.Fatal(err)
	}
	npmEntry := filepath.Join(store, "npm-fixture")
	cliScript := filepath.Join(npmEntry, "lib", "npm_cli.py")
	peWrite(t, cliScript, "import json\nfrom pathlib import Path\ntry:\n    package = json.loads(Path('package.json').read_text(encoding='utf-8'))\n"+
		"    Path('package-lock.json').write_text(json.dumps({\n        'name': package['name'], 'lockfileVersion': 3,\n    }), encoding='utf-8')\n"+
		"except Exception as error:\n    raise SystemExit(str(error))\n")
	npmBinary := npm.Binary(npmEntry, target)
	writeFile(t, npmBinary, []byte("#!/bin/sh\nexec \""+peHostPython(t)+"\" \""+cliScript+"\" \"$@\"\n"), 0o755)
	lock.SetPin("npm", "fixture", map[string]any{target: map[string]any{"url": "https://example.invalid/npm", "sha256": strings.Repeat("2", 64)}})
	if err := facts.Record("npm", "fixture", "npm-fixture", npm.Env(npmEntry, target), store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("2", 64)}}); err != nil {
		t.Fatal(err)
	}
	lock.Save()
	peWrite(t, filepath.Join(repo, "package.json"), `{"name": "pm-leg-proof", "version": "1.0.0", "private": true}`)
	peWrite(t, filepath.Join(repo, ".npmrc"), "offline=true\naudit=false\nfund=false\nignore-scripts=true\n")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("NODE_OPTIONS", "--invalid-option-to-be-scrubbed")
	t.Setenv("npm_config_cache", filepath.Join(root, "ambient-cache"))
	before := os.Environ()
	beforeLock := peRead(t, lock.Path)
	ctx := context.Background()
	args := UpdateArgs{Names: []string{"manual-fixture"}, Check: true, NPM: true}
	if rc, err := CmdUpdate(ctx, args, io.Discard); err != nil || rc != 0 || existsOrLink(filepath.Join(repo, "package-lock.json")) {
		t.Fatalf("check %d %v", rc, err)
	}
	args.Check = false
	var out bytes.Buffer
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 0 {
		t.Fatalf("apply %d %v %s", rc, err, out.String())
	}
	var written map[string]any
	json.Unmarshal(readFile(t, filepath.Join(repo, "package-lock.json")), &written)
	if written["name"] != "pm-leg-proof" || existsOrLink(filepath.Join(caller, "package-lock.json")) || !reflect.DeepEqual(os.Environ(), before) ||
		peRead(t, lock.Path) != beforeLock || existsOrLink(filepath.Join(root, "ambient-cache")) {
		t.Fatal("npm refresh touched the wrong project or state")
	}
	lockBytes := peRead(t, filepath.Join(repo, "package-lock.json"))
	peWrite(t, filepath.Join(repo, "package.json"), "not json")
	out.Reset()
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 1 || peRead(t, filepath.Join(repo, "package-lock.json")) != lockBytes || !strings.Contains(out.String(), "npm update failed") {
		t.Fatalf("broken package.json %d %v %q", rc, err, out.String())
	}
	os.Rename(nodeBinary, nodeBinary+".held")
	out.Reset()
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 1 || !strings.Contains(out.String(), "not installed") {
		t.Fatalf("missing node %d %v %q", rc, err, out.String())
	}
	os.Rename(nodeBinary+".held", nodeBinary)
	os.Remove(npmBinary)
	out.Reset()
	if rc, err := CmdUpdate(ctx, args, &out); err != nil || rc != 1 || !strings.Contains(out.String(), "not installed") {
		t.Fatalf("missing npm %d %v %q", rc, err, out.String())
	}
	if peRead(t, filepath.Join(repo, "package-lock.json")) != lockBytes || !reflect.DeepEqual(os.Environ(), before) || peRead(t, lock.Path) != beforeLock {
		t.Fatal("failed refreshes changed state")
	}
}
