package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Object store serving channel keys under /bucket/ or the root, every GET recorded
type chArchive struct {
	url      string
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
}

func newChArchive(t *testing.T) *chArchive {
	t.Helper()
	a := &chArchive{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bucket"), "/")
		a.mu.Lock()
		a.requests = append(a.requests, r.URL.Path)
		body, ok := a.objects[key]
		a.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	a.url = server.URL
	return a
}

// pyjson value of a Go map or slice tree, the shape json.loads returns
func jv(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func chCanon(t *testing.T, v any) []byte {
	t.Helper()
	body, err := canonicalJSON(jv(t, v))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func chCopy(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(v)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (a *chArchive) putRaw(key string, body []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.objects[key] = body
}

func (a *chArchive) put(t *testing.T, key string, v any) []byte {
	body := chCanon(t, v)
	a.putRaw(key, body)
	return body
}

func (a *chArchive) get(t *testing.T, key string) map[string]any {
	a.mu.Lock()
	raw := a.objects[key]
	a.mu.Unlock()
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (a *chArchive) reader(t *testing.T, repository string) *ChannelReader {
	t.Helper()
	reader, err := NewChannelReader(a.url+"/bucket", &repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func chIdentity(name, token string) map[string]any {
	pascal := "IcebathChannel" + token
	return map[string]any{"token": token, "displayName": "Icebath " + name, "appId": "ai.icebath.channel.h" + token,
		"appNamePascal": pascal, "artifactNamePascal": pascal, "cliName": "icebath-" + name,
		"windowsExecutableName": pascal, "msixAppIdWithOrg": "NickHeyer." + pascal}
}

func chHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Fresh active preview record with a reserved identity, the publisher create shape
func (a *chArchive) create(t *testing.T, name string) map[string]any {
	record := map[string]any{"schema": 1, "name": name, "repository": "example/icebath-agent", "policy": "preview",
		"state": "active", "revision": 1, "nextSequence": 1, "head": nil, "identity": chIdentity(name, chHex(t, 8))}
	a.put(t, "releases/channels/"+name+".json", record)
	return record
}

// Next sequence request of a channel with the record advanced past it, the publisher allocate shape
func (a *chArchive) allocate(t *testing.T, name, commit, sourceVersion string) map[string]any {
	record := a.get(t, "releases/channels/"+name+".json")
	seq, _ := record["nextSequence"].(json.Number).Int64()
	version, windows, err := channelPackageVersions(seq)
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"schema": 1, "buildId": chHex(t, 16), "channel": name, "sequence": seq,
		"repository": "example/icebath-agent", "commit": commit, "sourceVersion": sourceVersion, "version": version,
		"windowsVersion": windows, "identity": record["identity"], "bundleEnv": map[string]any{}, "publicBase": a.url + "/bucket"}
	rev, _ := record["revision"].(json.Number).Int64()
	record["revision"], record["nextSequence"] = rev+1, seq+1
	record["lastAllocation"] = map[string]any{"buildId": request["buildId"], "sequence": seq}
	a.put(t, "releases/channels/"+name+".json", record)
	a.put(t, "releases/channel-builds/"+request["buildId"].(string)+"/request.json", request)
	return request
}

// Darwin manifest of a request published as its channel head
func (a *chArchive) seedManifest(t *testing.T, request map[string]any) (map[string]any, map[string]any) {
	prefix := "releases/channel-builds/" + request["buildId"].(string) + "/"
	identity := request["identity"].(map[string]any)
	manifest := map[string]any{"schema": 1, "receiverProtocol": 1, "request": request, "packages": []any{map[string]any{
		"platform": "darwin", "arch": "arm64", "variant": "bundled",
		"artifact": map[string]any{"key": prefix + "darwin/package.zip", "sha256": strings.Repeat("f", 64), "size": 123},
		"version":  request["version"], "identity": identity["appId"], "teamId": "ABCDEFGHIJ",
		"feed": map[string]any{"key": prefix + "darwin/stable-mac.yml", "channel": "stable"}}}}
	body := a.put(t, prefix+"build.json", manifest)
	sum := sha256.Sum256(body)
	head := map[string]any{"buildId": request["buildId"], "sequence": request["sequence"], "manifestKey": prefix + "build.json",
		"sha256": hex.EncodeToString(sum[:])}
	key := "releases/channels/" + request["channel"].(string) + ".json"
	record := a.get(t, key)
	record["head"] = head
	a.put(t, key, record)
	return manifest, record
}

func chExpectErr(t *testing.T, err error, fragment string) {
	t.Helper()
	var chErr *ChannelError
	if err == nil || !errors.As(err, &chErr) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("expected a channel error naming %q, got %v", fragment, err)
	}
}

func TestChannelResolveVerifiesExactManifestAndPreservesRetirementConstraints(t *testing.T) {
	a := newChArchive(t)
	ctx := context.Background()
	for _, name := range []string{"old-preview", "next-preview"} {
		a.create(t, name)
	}
	request := a.allocate(t, "next-preview", strings.Repeat("a", 40), "1.2.3")
	request["version"], request["windowsVersion"], request["releaseTag"] = "1.2.3", "1.2.3.0", "v1.2.3"
	manifest, record := a.seedManifest(t, request)
	record["policy"] = "stable-release"
	a.put(t, "releases/channels/next-preview.json", record)
	reader := a.reader(t, "example/icebath-agent")
	res, err := reader.Resolve(ctx, "next-preview")
	if err != nil || !pyjson.Equal(res.Manifest, jv(t, manifest)) {
		t.Fatalf("resolve %v %v", res, err)
	}
	old := a.get(t, "releases/channels/old-preview.json")
	old["state"], old["destination"], old["minimumVersion"] = "retired", "next-preview", "1.0.0"
	old["destinationHead"], old["receiverProtocol"] = record["head"], 1
	old["receiver"], old["lastHead"] = map[string]any{"kind": "discontinued"}, nil
	a.put(t, "releases/channels/old-preview.json", old)
	resolved, err := reader.Resolve(ctx, "old-preview")
	if err != nil || !pyjson.Equal(resolved.Requested, jv(t, old)) || !pyjson.Equal(resolved.Terminal, jv(t, record)) ||
		resolved.Requested.Vals["minimumVersion"] != "1.0.0" {
		t.Fatalf("retired resolve %v %v", resolved, err)
	}
	newer := chCopy(t, request)
	newer["buildId"], newer["sequence"], newer["sourceVersion"] = strings.Repeat("f", 32), 2, "1.3.0"
	newer["version"], newer["windowsVersion"], newer["releaseTag"] = "1.3.0", "1.3.0.0", "v1.3.0"
	record["nextSequence"] = 3
	a.put(t, "releases/channels/next-preview.json", record)
	a.seedManifest(t, newer)
	after, err := reader.Resolve(ctx, "old-preview")
	if err != nil || !pyjson.Equal(after.Manifest, jv(t, manifest)) ||
		after.Terminal.Vals["head"].(*pyjson.Object).Vals["buildId"] != newer["buildId"] {
		t.Fatalf("pinned resolve %v %v", after, err)
	}
	key := record["head"].(map[string]any)["manifestKey"].(string)
	a.mu.Lock()
	a.objects[key] = append(a.objects[key], ' ')
	a.mu.Unlock()
	_, err = reader.Resolve(ctx, "old-preview")
	chExpectErr(t, err, "SHA256")
}

func TestChannelNamesAreValidatedWithoutNormalizing(t *testing.T) {
	for _, name := range []string{"X", "../x", "two--parts", "a/b", "a%2fb", "a_1", "con", "nul", "x-", strings.Repeat("x", 33), " a", "a\n"} {
		if _, err := ValidateChannelName(name); err == nil {
			t.Fatalf("%q accepted", name)
		} else {
			chExpectErr(t, err, "Invalid channel name")
		}
	}
}

func chWaits(t *testing.T) *[]time.Duration {
	waits := &[]time.Duration{}
	saved := channelRetryWait
	channelRetryWait = func(d time.Duration) error { *waits = append(*waits, d); return nil }
	t.Cleanup(func() { channelRetryWait = saved })
	return waits
}

func chResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: header,
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestChannelUpdateReadsRetryTransientHTTPAndHonorRetryAfter(t *testing.T) {
	target := "http://127.0.0.1:12345/releases/fixture.json"
	waits := chWaits(t)
	var attempts []string
	opener := func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req.URL.String())
		if len(attempts) == 1 {
			return chResponse(req, 503, http.Header{"Retry-After": {"7"}}, ""), nil
		}
		return chResponse(req, 200, nil, "fixture"), nil
	}
	reader, err := NewChannelReader("http://127.0.0.1:12345", nil, opener)
	if err != nil {
		t.Fatal(err)
	}
	body, err := reader.ReadBytes(WithRetryingReads(context.Background()), "releases/fixture.json", nil)
	if err != nil || string(body) != "fixture" {
		t.Fatalf("read %q %v", body, err)
	}
	if len(attempts) != 2 || attempts[0] != target || attempts[1] != target {
		t.Fatalf("attempts %v", attempts)
	}
	if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Fatalf("waits %v", *waits)
	}
}

func TestChannelPassiveReadsAndMissingObjectsMakeOneAttempt(t *testing.T) {
	waits := chWaits(t)
	var calls []string
	reader := func(fault func(*http.Request) (*http.Response, error)) *ChannelReader {
		r, err := NewChannelReader("https://releases.example", nil, func(req *http.Request) (*http.Response, error) {
			calls = append(calls, req.URL.String())
			return fault(req)
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	offline := func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: &net.OpError{Op: "dial", Net: "tcp",
			Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}}
	}
	_, err := reader(offline).ReadBytes(context.Background(), "releases/channels/main.json", nil)
	chExpectErr(t, err, "unavailable")
	if len(calls) != 1 {
		t.Fatalf("passive read retried %v", calls)
	}
	calls = nil
	missing := func(req *http.Request) (*http.Response, error) { return chResponse(req, 404, nil, ""), nil }
	_, err = reader(missing).ReadBytes(WithRetryingReads(context.Background()), "releases/channels/stable.json", nil)
	var notFound *ChannelNotFound
	if !errors.As(err, &notFound) || len(calls) != 1 {
		t.Fatalf("missing object %v after %v", err, calls)
	}
	if len(*waits) != 0 {
		t.Fatalf("waits %v", *waits)
	}
}

func TestChannelCDNFirewall403NamesTheNetworkNotTheInstall(t *testing.T) {
	cases := []struct {
		base    string
		headers map[string]string
		blocked bool
	}{
		{"https://hermes-assets.nousresearch.com", map[string]string{"Server": "cloudflare", "Content-Type": "text/html"}, true},
		{"https://hermes-assets.nousresearch.com", map[string]string{"Server": "cloudflare", "cf-cache-status": "DYNAMIC"}, false},
		{"https://other-r2.example", map[string]string{"Server": "cloudflare", "Content-Type": "text/html"}, false},
	}
	for _, c := range cases {
		header := http.Header{}
		for k, v := range c.headers {
			header.Set(k, v)
		}
		reader, err := NewChannelReader(c.base, nil, func(req *http.Request) (*http.Response, error) {
			return chResponse(req, 403, header, ""), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = reader.ReadBytes(context.Background(), "releases/channels/pm-preview.json", nil)
		chExpectErr(t, err, "")
		if strings.Contains(err.Error(), "CDN firewall (Cloudflare) refused this network") != c.blocked ||
			!strings.HasPrefix(err.Error(), "Channel read unavailable: HTTP 403") {
			t.Fatalf("%s %v answered %q", c.base, c.headers, err)
		}
	}
}

func TestChannelReaderRejectsCyclesIdentitySubstitutionAndCrossAuthority(t *testing.T) {
	a := newChArchive(t)
	ctx := context.Background()
	a.create(t, "alpha")
	request := a.allocate(t, "alpha", strings.Repeat("a", 40), "1.2.3")
	manifest, record := a.seedManifest(t, request)
	reader := a.reader(t, "example/icebath-agent")
	substituted := chCopy(t, manifest)
	substituted["request"].(map[string]any)["identity"].(map[string]any)["appId"] = "ai.other.identity"
	head := record["head"].(map[string]any)
	raw := a.put(t, head["manifestKey"].(string), substituted)
	sum := sha256.Sum256(raw)
	head["sha256"] = hex.EncodeToString(sum[:])
	a.put(t, "releases/channels/alpha.json", record)
	_, err := reader.Resolve(ctx, "alpha")
	chExpectErr(t, err, "identity")
	record["repository"] = "other/icebath-agent"
	a.put(t, "releases/channels/alpha.json", record)
	_, err = reader.Resolve(ctx, "alpha")
	chExpectErr(t, err, "authority")
	record["repository"] = "example/icebath-agent"
	record["state"], record["destination"], record["minimumVersion"] = "retired", "alpha", "1.0.0"
	record["lastHead"], record["destinationHead"], record["receiverProtocol"] = head, head, 1
	record["receiver"] = map[string]any{"kind": "discontinued"}
	a.put(t, "releases/channels/alpha.json", record)
	_, err = reader.Resolve(ctx, "alpha")
	chExpectErr(t, err, "cycle")
}

func TestChannelLegacyBootstrapUsesRealArchiveKeys(t *testing.T) {
	a := newChArchive(t)
	ctx := context.Background()
	preview := a.create(t, "temporary")
	request := a.allocate(t, "temporary", strings.Repeat("a", 40), "2.0.0")
	manifest, _ := a.seedManifest(t, request)
	reader := a.reader(t, "example/icebath-agent")
	for _, c := range []struct{ name, policy, tag, build, windows string }{
		{"stable", "stable-release", "v2.0.0", strings.Repeat("a", 32), "2.0.0.0"},
		{"canary", "canary-release", "v2.1.0+canary.20260913T000100Z", strings.Repeat("b", 32), "2.1.0.10"},
	} {
		legacy := chCopy(t, manifest)
		req := legacy["request"].(map[string]any)
		req["channel"], req["buildId"], req["releaseTag"] = c.name, c.build, c.tag
		req["version"], req["windowsVersion"] = c.tag[1:], c.windows
		pkg := legacy["packages"].([]any)[0].(map[string]any)
		pkg["version"] = c.tag[1:]
		pkg["artifact"].(map[string]any)["key"] = "releases/tag/" + c.tag + "/actual.zip"
		pkg["feed"].(map[string]any)["key"] = "releases/tag/" + c.tag + "/stable-mac.yml"
		body := chCanon(t, legacy)
		sum := sha256.Sum256(body)
		record := chCopy(t, preview)
		record["name"], record["policy"], record["nextSequence"] = c.name, c.policy, 2
		record["head"] = map[string]any{"buildId": c.build, "sequence": 1,
			"manifestKey": "releases/channel-builds/" + c.build + "/build.json", "sha256": hex.EncodeToString(sum[:])}
		a.put(t, "releases/channel-builds/"+c.build+"/request.json", req)
		a.putRaw("releases/channel-builds/"+c.build+"/build.json", body)
		a.put(t, "releases/channels/"+c.name+".json", record)
		res, err := reader.Resolve(ctx, c.name)
		if err != nil || !pyjson.Equal(res.Manifest, jv(t, legacy)) {
			t.Fatalf("%s resolve %v", c.name, err)
		}
		invalid := []string{"2.1.0.65536", "65536.1.0.0", "2.1.0.-1", "2.1.0.1.0", "2.1.0.x"}
		if c.policy == "stable-release" {
			invalid = append(invalid, "2.0.0.10")
		}
		for _, windows := range invalid {
			bad := chCopy(t, req)
			bad["windowsVersion"] = windows
			_, err := validateChannelRequest(jv(t, bad), nil, nil, c.policy)
			chExpectErr(t, err, "Windows version")
		}
	}
}

func TestChannelSequenceVersionsRemainMonotonicAtNativeRollover(t *testing.T) {
	quad := func(text string) [4]int64 {
		var out [4]int64
		for i, part := range strings.Split(text, ".") {
			n, _ := json.Number(part).Int64()
			out[i] = n
		}
		return out
	}
	for _, seq := range []int64{1, 65535, 65536, 1<<32 - 1} {
		version, windows, err := channelPackageVersions(seq)
		if err != nil {
			t.Fatal(err)
		}
		q := quad(windows)
		if version != fmt.Sprintf("0.0.%d", seq) || q[1]*65536+q[2] != seq || q[0] > 65535 || q[1] > 65535 || q[2] > 65535 || q[3] > 65535 {
			t.Fatalf("%d gave %s %s", seq, version, windows)
		}
		if seq > 1 {
			_, prev, _ := channelPackageVersions(seq - 1)
			p := quad(prev)
			if !(q[0] > p[0] || (q[0] == p[0] && (q[1] > p[1] || (q[1] == p[1] && q[2] > p[2])))) {
				t.Fatalf("%d quad %s not above %s", seq, windows, prev)
			}
		}
	}
}

func TestChannelSequenceExhaustionNeverWraps(t *testing.T) {
	for _, seq := range []any{int64(0), int64(-1), true, int64(1 << 32), "1"} {
		_, _, err := channelPackageVersions(seq)
		chExpectErr(t, err, "Invalid sequence")
	}
}

func TestChannelMalformedRecordAndUnqualifiedRetirementNeverResolve(t *testing.T) {
	a := newChArchive(t)
	ctx := context.Background()
	reader := a.reader(t, "example/icebath-agent")
	record := a.create(t, "preview")
	malformed := chCopy(t, record)
	delete(malformed, "head")
	a.put(t, "releases/channels/preview.json", malformed)
	_, err := reader.Resolve(ctx, "preview")
	chExpectErr(t, err, "Missing channel record fields")
	a.put(t, "releases/channels/preview.json", record)
	a.create(t, "destination")
	request := a.allocate(t, "destination", strings.Repeat("a", 40), "1.0.0")
	request["version"], request["windowsVersion"], request["releaseTag"] = "1.0.0", "1.0.0.0", "v1.0.0"
	_, target := a.seedManifest(t, request)
	target["policy"] = "stable-release"
	a.put(t, "releases/channels/destination.json", target)
	record["state"], record["destination"], record["minimumVersion"] = "retired", "destination", "2.0.0"
	record["lastHead"], record["destinationHead"], record["receiverProtocol"] = nil, target["head"], 1
	record["receiver"] = map[string]any{"kind": "discontinued"}
	a.put(t, "releases/channels/preview.json", record)
	_, err = reader.Resolve(ctx, "preview")
	chExpectErr(t, err, "minimum version")
}

func TestChannelMalformedWireTypesAreChannelErrors(t *testing.T) {
	for field, value := range map[string]any{"policy": []any{}, "state": map[string]any{}, "revision": true, "head": []any{}, "identity": nil} {
		record := map[string]any{"schema": 1, "name": "arbitrary", "repository": "example/icebath-agent", "policy": "preview",
			"state": "active", "revision": 1, "nextSequence": 1, "head": nil, "identity": chIdentity("arbitrary", strings.Repeat("a", 16))}
		record[field] = value
		_, err := validateChannelRecord(jv(t, record), nil, nil)
		chExpectErr(t, err, "")
	}
}

func chStableRequest(version, archiveRef string) map[string]any {
	request := map[string]any{"schema": 1, "buildId": strings.Repeat("a", 32), "channel": "stable", "sequence": 1,
		"repository": "example/icebath-agent", "commit": strings.Repeat("b", 40), "sourceVersion": version, "version": version,
		"windowsVersion": version + ".0", "releaseTag": "v" + version, "identity": chIdentity("stable", strings.Repeat("a", 16)),
		"bundleEnv": map[string]any{}, "publicBase": "https://releases.example"}
	if archiveRef != "" {
		request["archiveRef"] = archiveRef
	}
	return request
}

func chStableManifest(request map[string]any, archivePrefix string) (map[string]any, map[string]any) {
	identity := request["identity"].(map[string]any)
	manifest := map[string]any{"schema": 1, "receiverProtocol": 1, "request": request, "packages": []any{map[string]any{
		"platform": "darwin", "arch": "arm64", "variant": "bundled", "identity": identity["appId"], "version": request["version"],
		"teamId": "ABCDEFGHIJ", "artifact": map[string]any{"key": archivePrefix + "Icebath.dmg", "sha256": strings.Repeat("d", 64), "size": 100},
		"feed": map[string]any{"key": archivePrefix + "stable-mac.yml", "channel": "stable"}}}}
	record := map[string]any{"schema": 1, "name": request["channel"], "repository": request["repository"], "policy": "stable-release",
		"state": "active", "revision": 1, "nextSequence": 2, "identity": identity,
		"head": map[string]any{"buildId": request["buildId"], "sequence": request["sequence"],
			"manifestKey": "releases/channel-builds/" + request["buildId"].(string) + "/build.json", "sha256": strings.Repeat("a", 64)}}
	return manifest, record
}

func TestChannelArchiveRefNamesTheProtectedArchivePrefix(t *testing.T) {
	for _, c := range []struct{ ref, prefix string }{{"rc.2-v1.2.3", "releases/tag/rc.2-v1.2.3/"}, {"v1.2.3", "releases/tag/v1.2.3/"}} {
		request := chStableRequest("1.2.3", c.ref)
		if _, err := validateChannelRequest(jv(t, request), nil, nil, "stable-release"); err != nil {
			t.Fatalf("%s request %v", c.ref, err)
		}
		manifest, record := chStableManifest(request, c.prefix)
		got, err := validateChannelManifest(jv(t, manifest), jv(t, record).(*pyjson.Object), "https://releases.example")
		if err != nil || !pyjson.Equal(got, jv(t, manifest)) {
			t.Fatalf("%s manifest %v", c.ref, err)
		}
	}
}

func TestChannelStableManifestWithoutArchiveRefFailsClosedOutsideTheTagPrefix(t *testing.T) {
	manifest, record := chStableManifest(chStableRequest("1.2.3", ""), "releases/tag/rc.2-v1.2.3/")
	_, err := validateChannelManifest(jv(t, manifest), jv(t, record).(*pyjson.Object), "https://releases.example")
	chExpectErr(t, err, "namespace")
}

func TestChannelArchiveRefMustNameTheReleaseVersion(t *testing.T) {
	_, err := validateChannelRequest(jv(t, chStableRequest("1.2.3", "rc.2-v1.2.4")), nil, nil, "stable-release")
	chExpectErr(t, err, "Archive ref")
	for _, ref := range []string{"rc.1-v0.21.5", "rc.12-v1.0.0"} {
		if _, ok := parseAttemptRef(ref); !ok {
			t.Fatalf("%s is not an attempt ref", ref)
		}
	}
	for _, ref := range []string{"v0.21.5-rc", "v0.21.5-rc.1", "rc.01-v0.21.5", "rc.0-v0.21.5", "rc.1-v2026.9.21", "v0.21.5", "abandoned-rc.1-v0.21.5"} {
		_, err := validateChannelRequest(jv(t, chStableRequest("1.2.3", ref)), nil, nil, "stable-release")
		chExpectErr(t, err, "archive ref")
	}
}
