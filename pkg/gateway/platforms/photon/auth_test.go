package photon

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

type fakeResponse struct {
	status  int
	body    any
	headers http.Header
	text    string
}

func (f *fakeResponse) StatusCode() int {
	if f.status == 0 {
		return 200
	}
	return f.status
}

func (f *fakeResponse) JSON() (any, error) {
	if f.body == nil {
		return pyjson.NewObject(), nil
	}
	return f.body, nil
}

func (f *fakeResponse) Text() string        { return f.text }
func (f *fakeResponse) Header() http.Header { return f.headers }

func (f *fakeResponse) RaiseForStatus() error {
	if f.StatusCode() >= 400 {
		return runtimeError("HTTP %d", f.StatusCode())
	}
	return nil
}

func js(t *testing.T, text string) any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("fixture %s: %v", text, err)
	}
	return v
}

type getCall struct {
	url     string
	headers map[string]string
	timeout time.Duration
}

type postCall struct {
	url     string
	body    any
	headers map[string]string
}

func patchGet(t *testing.T, fn func(c getCall) (photonResponse, error)) {
	prev := httpGet
	httpGet = func(_ context.Context, rawURL string, headers map[string]string, timeout time.Duration) (photonResponse, error) {
		return fn(getCall{rawURL, headers, timeout})
	}
	t.Cleanup(func() { httpGet = prev })
}

func patchPost(t *testing.T, fn func(c postCall) (photonResponse, error)) {
	prev := httpPost
	httpPost = func(_ context.Context, rawURL string, body any, headers map[string]string, _ time.Duration) (photonResponse, error) {
		return fn(postCall{rawURL, body, headers})
	}
	t.Cleanup(func() { httpPost = prev })
}

func patchPersist(t *testing.T) {
	prev := persistRuntimeEnv
	persistRuntimeEnv = func(context.Context, string, string) {}
	t.Cleanup(func() { persistRuntimeEnv = prev })
}

var photonEnv = []string{"PHOTON_PROJECT_ID", "PHOTON_PROJECT_SECRET", "PHOTON_DASHBOARD_PROJECT_ID", "PHOTON_SPECTRUM_HOST",
	"PHOTON_ALLOWED_USERS", "PHOTON_HOME_CHANNEL", "PHOTON_DASHBOARD_HOST"}

func clearPhotonEnv(t *testing.T) {
	for _, k := range photonEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func tmpIcebathHome(t *testing.T) string {
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	clearPhotonEnv(t)
	return h
}

func usersBody(t *testing.T, rows string) photonResponse {
	return &fakeResponse{body: js(t, `{"succeed": true, "data": {"users": [`+rows+`]}}`)}
}

func objGet(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

func TestStoreAndLoadPhotonToken(t *testing.T) {
	ctx := context.Background()
	h := tmpIcebathHome(t)
	if err := StorePhotonToken(ctx, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadPhotonToken(ctx); err != nil || tok != "abc123def456" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	data := pyjson.Plain(js(t, string(raw))).(map[string]any)
	entry := data["credential_pool"].(map[string]any)["photon"].([]any)[0].(map[string]any)
	if entry["access_token"] != "abc123def456" {
		t.Fatalf("auth.json = %s", raw)
	}
}

func TestSaveAuthNeverWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits only")
	}
	h := tmpIcebathHome(t)
	if err := StorePhotonToken(context.Background(), "secret-token"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(h, "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestStoreProjectCredentialsRoundTrip(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	patchPersist(t)
	if err := StoreProjectCredentials(ctx, "sp-123", "secret-key", "dash-456", "Hermes Agent"); err != nil {
		t.Fatal(err)
	}
	clearPhotonEnv(t)
	sid, secret, err := LoadProjectCredentials(ctx)
	if err != nil || sid != "sp-123" || secret != "secret-key" {
		t.Fatalf("credentials = %q %q %v", sid, secret, err)
	}
	if id, err := LoadDashboardProjectID(ctx); err != nil || id != "sp-123" {
		t.Fatalf("dashboard id = %q, %v", id, err)
	}
}

func TestLoadUserNumbersFallsBackToHomeChannel(t *testing.T) {
	ctx := context.Background()
	h := tmpIcebathHome(t)
	if err := config.SaveEnvValueContext(ctx, h, "PHOTON_HOME_CHANNEL", "+15551234567"); err != nil {
		t.Fatal(err)
	}
	phone, assigned, err := LoadUserNumbers(ctx)
	if err != nil || phone != "+15551234567" || assigned != "" {
		t.Fatalf("numbers = %q %q %v", phone, assigned, err)
	}
}

func TestRefreshUserNumbersReadsExistingAssignment(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	if err := StoreUserNumbers(ctx, "+15551234567", "", "", nil); err != nil {
		t.Fatal(err)
	}
	patchGet(t, func(c getCall) (photonResponse, error) {
		if c.headers["Authorization"] != "Basic "+base64.StdEncoding.EncodeToString([]byte("sp:secret")) {
			t.Fatalf("headers = %v", c.headers)
		}
		if !strings.HasSuffix(c.url, "/projects/sp/users/") {
			t.Fatalf("url = %s", c.url)
		}
		return usersBody(t, `{"id": "user-uuid", "phoneNumber": "+1 (555) 123-4567", "assignedPhoneNumber": "+16282679185"}`), nil
	})
	phone, assigned, err := RefreshUserNumbers(ctx, "sp", "secret")
	if err != nil || phone != "+15551234567" || assigned != "+16282679185" {
		t.Fatalf("refresh = %q %q %v", phone, assigned, err)
	}
	if p, a, err := LoadUserNumbers(ctx); err != nil || p != "+15551234567" || a != "+16282679185" {
		t.Fatalf("stored = %q %q %v", p, a, err)
	}
}

func TestLoadProjectCredentialsEnvOverride(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	patchPersist(t)
	if err := StoreProjectCredentials(ctx, "from-file", "secret-file", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHOTON_PROJECT_ID", "from-env")
	t.Setenv("PHOTON_PROJECT_SECRET", "secret-env")
	if sid, secret, err := LoadProjectCredentials(ctx); err != nil || sid != "from-env" || secret != "secret-env" {
		t.Fatalf("credentials = %q %q %v", sid, secret, err)
	}
}

func TestRequestDeviceCodeUsesPhotonCLI(t *testing.T) {
	var captured postCall
	patchPost(t, func(c postCall) (photonResponse, error) {
		captured = c
		return &fakeResponse{body: js(t, `{"device_code": "dev-code-xyz", "user_code": "ABCD-1234",
			"verification_uri": "https://app.photon.codes/device",
			"verification_uri_complete": "https://app.photon.codes/device?code=ABCD-1234", "expires_in": 600, "interval": 5}`)}, nil
	})
	code, err := RequestDeviceCode(context.Background(), DefaultClientID, DefaultScope)
	if err != nil || code.DeviceCode != "dev-code-xyz" || code.UserCode != "ABCD-1234" || code.ExpiresIn != 600 || code.Interval != 5 {
		t.Fatalf("code = %+v, %v", code, err)
	}
	body := captured.body.(*pyjson.Object)
	if !strings.Contains(captured.url, "/api/auth/device/code") || objGet(body, "client_id") != "photon-cli" || objGet(body, "scope") != "openid profile email" {
		t.Fatalf("request = %s %v", captured.url, body)
	}
}

func deviceCodeFixture() DeviceCode {
	return DeviceCode{DeviceCode: "d", UserCode: "u", VerificationURI: "https://x", ExpiresIn: 10, Interval: 0}
}

func TestPollForTokenBodyAccessToken(t *testing.T) {
	patchPost(t, func(postCall) (photonResponse, error) {
		return &fakeResponse{body: js(t, `{"access_token": "tok-body"}`)}, nil
	})
	zero := 0
	if tok, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, 2, &zero, nil); err != nil || tok != "tok-body" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func TestListProjectsUnwrapsList(t *testing.T) {
	patchGet(t, func(getCall) (photonResponse, error) {
		return &fakeResponse{body: js(t, `[{"id": "p1", "name": "Hermes Agent"}]`)}, nil
	})
	projects, err := ListProjects(context.Background(), "tok")
	if err != nil || objGet(projects[0].(*pyjson.Object), "id") != "p1" {
		t.Fatalf("projects = %v, %v", projects, err)
	}
}

func TestFindProjectByNameCaseInsensitive(t *testing.T) {
	patchGet(t, func(getCall) (photonResponse, error) {
		return &fakeResponse{body: js(t, `{"data": [{"id": "p1", "name": "Other"}, {"id": "p2", "name": "hermes agent"}]}`)}, nil
	})
	proj, err := FindProjectByName(context.Background(), "tok", "Hermes Agent")
	if err != nil || proj == nil || objGet(proj, "id") != "p2" {
		t.Fatalf("project = %v, %v", proj, err)
	}
}

func TestCreateProjectOmitsSpectrumFlag(t *testing.T) {
	var captured postCall
	patchPost(t, func(c postCall) (photonResponse, error) {
		captured = c
		return &fakeResponse{body: js(t, `{"success": true, "id": "new-proj"}`)}, nil
	})
	data, err := CreateProject(context.Background(), "tok", "Hermes Agent", DefaultLocation)
	if err != nil || objGet(data, "id") != "new-proj" {
		t.Fatalf("project = %v, %v", data, err)
	}
	body := captured.body.(*pyjson.Object)
	if _, has := body.Get("spectrum"); has || objGet(body, "name") != "Hermes Agent" || objGet(body, "location") != "United States" {
		t.Fatalf("body = %v", body)
	}
	if captured.headers["Authorization"] != "Bearer tok" || !strings.HasSuffix(captured.url, "/api/projects") {
		t.Fatalf("request = %s %v", captured.url, captured.headers)
	}
}

func TestRegenerateProjectSecret(t *testing.T) {
	patchPost(t, func(c postCall) (photonResponse, error) {
		if !strings.HasSuffix(c.url, "/regenerate-secret") {
			t.Fatalf("url = %s", c.url)
		}
		return &fakeResponse{body: js(t, `{"success": true, "projectSecret": "rotated"}`)}, nil
	})
	if secret, err := RegenerateProjectSecret(context.Background(), "tok", "p"); err != nil || secret != "rotated" {
		t.Fatalf("secret = %q, %v", secret, err)
	}
}

func TestRegisterUserIfAbsentDedup(t *testing.T) {
	posted := 0
	patchGet(t, func(getCall) (photonResponse, error) {
		return usersBody(t, `{"id": "u1", "phoneNumber": "+1 (555) 123-4567", "assignedPhoneNumber": "+16282679185"}`), nil
	})
	patchPost(t, func(postCall) (photonResponse, error) {
		posted++
		return &fakeResponse{body: js(t, `{"success": true, "user": {}}`)}, nil
	})
	user, created, err := RegisterUserIfAbsent(context.Background(), "proj", "secret", "+15551234567", UserFields{})
	if err != nil || created || objGet(user, "id") != "u1" || posted != 0 {
		t.Fatalf("user = %v created %v posted %d, %v", user, created, posted, err)
	}
	if line := UserAssignedLine(user); line != "+16282679185" {
		t.Fatalf("assigned = %q", line)
	}
}

func TestUserAssignedLine(t *testing.T) {
	for raw, want := range map[string]string{`{"assignedPhoneNumber": "+16282679185"}`: "+16282679185", `{"phoneNumber": "+15551234567"}`: "",
		`{"assignedPhoneNumber": ""}`: "", `{}`: ""} {
		if got := UserAssignedLine(js(t, raw).(*pyjson.Object)); got != want {
			t.Fatalf("%s = %q", raw, got)
		}
	}
	if UserAssignedLine(nil) != "" {
		t.Fatal("None user has no line")
	}
}

func TestWaitForOptedInUserPollsUntilFlagLands(t *testing.T) {
	calls := 0
	patchGet(t, func(getCall) (photonResponse, error) {
		calls++
		if calls == 1 {
			return nil, runtimeError("transient")
		}
		meta := `{}`
		if calls >= 3 {
			meta = `{"opt_in": true}`
		}
		return usersBody(t, `{"id": "u1", "phoneNumber": "+15551234567", "assignedPhoneNumber": "+16282679185", "meta": `+meta+`}`), nil
	})
	var waits []int
	user, err := WaitForOptedInUser(context.Background(), "proj", "secret", "+15551234567", 50, 5,
		func(n int) { waits = append(waits, n) }, func(float64) {}, nil)
	if err != nil || user == nil || !UserOptedIn(user) || calls != 3 || len(waits) != 2 || waits[0] != 1 || waits[1] != 2 {
		t.Fatalf("user = %v calls %d waits %v, %v", user, calls, waits, err)
	}
	patchGet(t, func(getCall) (photonResponse, error) {
		return usersBody(t, `{"id": "u1", "phoneNumber": "+15551234567", "meta": {}}`), nil
	})
	if user, err := WaitForOptedInUser(context.Background(), "proj", "secret", "+15551234567", 15, 5, nil, func(float64) {}, nil); err != nil || user != nil {
		t.Fatalf("never opted in = %v, %v", user, err)
	}
}

func TestWaitForOptedInUserWallClockBound(t *testing.T) {
	now, calls := 0.0, 0
	patchGet(t, func(getCall) (photonResponse, error) {
		calls++
		now += 31
		return usersBody(t, `{"id": "u1", "phoneNumber": "+15551234567", "meta": {}}`), nil
	})
	user, err := WaitForOptedInUser(context.Background(), "proj", "secret", "+15551234567", 60, 5, nil,
		func(s float64) { now += s }, func() float64 { return now })
	if err != nil || user != nil || calls != 2 || now > 60+31 {
		t.Fatalf("user = %v calls %d now %v, %v", user, calls, now, err)
	}
}

func TestRegisterUserIfAbsentReusesOptedInDuplicate(t *testing.T) {
	patchGet(t, func(getCall) (photonResponse, error) {
		return usersBody(t, `{"id": "stale", "phoneNumber": "+15551234567", "meta": {}},
			{"id": "live", "phoneNumber": "+1 (555) 123-4567", "meta": {"opt_in": true}}`), nil
	})
	patchPost(t, func(postCall) (photonResponse, error) {
		t.Fatal("created a duplicate")
		return nil, nil
	})
	user, created, err := RegisterUserIfAbsent(context.Background(), "proj", "secret", "+15551234567", UserFields{})
	if err != nil || created || objGet(user, "id") != "live" {
		t.Fatalf("user = %v created %v, %v", user, created, err)
	}
}

func TestPrintRoutingStatusUsesShortTimeout(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	if err := StoreUserNumbers(ctx, "+15551234567", "+16282679185", "", nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHOTON_PROJECT_ID", "proj")
	t.Setenv("PHOTON_PROJECT_SECRET", "secret")
	var seen []time.Duration
	patchGet(t, func(c getCall) (photonResponse, error) {
		seen = append(seen, c.timeout)
		return nil, &requestError{class: "ReadTimeout"}
	})
	var lines []string
	if err := PrintRoutingStatus(ctx, func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || seen[0] > 5*time.Second {
		t.Fatalf("timeouts = %v", seen)
	}
	if len(lines) != 1 || lines[0] != "  routing             : ? could not verify (ReadTimeout)" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestPrintRoutingStatusReportsTheLiveRow(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	if err := StoreUserNumbers(ctx, "+15551234567", "+16282679185", "", nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHOTON_PROJECT_ID", "proj")
	t.Setenv("PHOTON_PROJECT_SECRET", "secret")
	for meta, want := range map[string]string{
		`{"opt_in": true}`: "  routing             : ✓ opted in (verified live)",
		`{}`:               "  routing             : ✗ not opted in — text +16282679185 once from +15551234567, then re-check",
	} {
		patchGet(t, func(getCall) (photonResponse, error) {
			return usersBody(t, `{"id": "u1", "phoneNumber": "+15551234567", "meta": `+meta+`}`), nil
		})
		var lines []string
		if err := PrintRoutingStatus(ctx, func(s string) { lines = append(lines, s) }); err != nil || len(lines) != 1 || lines[0] != want {
			t.Fatalf("meta %s lines = %q, %v", meta, lines, err)
		}
	}
	patchGet(t, func(getCall) (photonResponse, error) { return nil, &pyError{class: "AttributeError", msg: "boom"} })
	if err := PrintRoutingStatus(ctx, func(string) {}); err == nil {
		t.Fatal("an exception outside the probe classes propagates")
	}
}

func TestGetIMessageLineReturnsExisting(t *testing.T) {
	patchGet(t, func(getCall) (photonResponse, error) {
		return &fakeResponse{body: js(t, `[{"id": "l1", "platform": "imessage", "phoneNumber": "+15559999999", "status": "active"}]`)}, nil
	})
	line, err := GetIMessageLine(context.Background(), "tok", "proj", true)
	if o, ok := line.(*pyjson.Object); err != nil || !ok || objGet(o, "phoneNumber") != "+15559999999" {
		t.Fatalf("line = %v, %v", line, err)
	}
}

func TestGetIMessageLineProvisionsWhenAbsent(t *testing.T) {
	patchGet(t, func(getCall) (photonResponse, error) {
		return &fakeResponse{body: js(t, `{"data": {"lines": [{"platform": "sms"}]}}`)}, nil
	})
	if line, err := GetIMessageLine(context.Background(), "tok", "proj", false); err != nil || line != nil {
		t.Fatalf("no create = %v, %v", line, err)
	}
	var posted postCall
	patchPost(t, func(c postCall) (photonResponse, error) {
		posted = c
		return &fakeResponse{body: js(t, `{"line": {"platform": "imessage", "phoneNumber": "+1555"}}`)}, nil
	})
	line, err := GetIMessageLine(context.Background(), "tok", "proj", true)
	if o, ok := line.(*pyjson.Object); err != nil || !ok || objGet(o, "phoneNumber") != "+1555" || !strings.HasSuffix(posted.url, "/api/projects/proj/lines") {
		t.Fatalf("provisioned = %v, %v", line, err)
	}
	patchPost(t, func(postCall) (photonResponse, error) { return &fakeResponse{status: 500}, nil })
	if line, err := GetIMessageLine(context.Background(), "tok", "proj", true); err != nil || line != nil {
		t.Fatalf("failed provisioning = %v, %v", line, err)
	}
}

func TestCredentialSummaryNoSecretLeak(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	patchPersist(t)
	if err := StorePhotonToken(ctx, "token-aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := StoreProjectCredentials(ctx, "sp-uuid", "secret-bbbbbbbbbbb", "dash-uuid", ""); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := PrintCredentialSummary(ctx, func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	blob := strings.Join(lines, "\n")
	if strings.Contains(blob, "token-aaaa") || strings.Contains(blob, "secret-bbbb") || !strings.Contains(blob, "sp-uuid") {
		t.Fatalf("summary = %s", blob)
	}
	if !strings.Contains(blob, "  my number           : ✗ missing (run `icebath photon setup --phone ...`)") {
		t.Fatalf("summary = %s", blob)
	}
}

func TestDeviceResponseCandidatesCoversKnownShapes(t *testing.T) {
	body := js(t, `{"access_token": "tok-snake", "accessToken": "tok-camel", "data": {"access_token": "tok-data"}}`).(*pyjson.Object)
	bySource := map[string]string{}
	for _, c := range deviceResponseTokenCandidates(body, http.Header{"set-auth-token": {"Bearer tok-header"}}) {
		bySource[c.Source] = c.Token
	}
	want := map[string]string{"access_token": "tok-snake", "accessToken": "tok-camel", "data.access_token": "tok-data", "set-auth-token": "tok-header"}
	for k, v := range want {
		if bySource[k] != v {
			t.Fatalf("candidates = %v", bySource)
		}
	}
	dup := js(t, `{"access_token": " same ", "session": {"access_token": "bearer same"}, "data": "flat"}`).(*pyjson.Object)
	if got := deviceResponseTokenCandidates(dup, nil); len(got) != 1 || got[0].Token != "same" {
		t.Fatalf("dedup = %v", got)
	}
}

func TestValidatePhotonTokenRejectsUnrecognizedSession(t *testing.T) {
	patchGet(t, func(c getCall) (photonResponse, error) {
		if strings.HasSuffix(c.url, "/api/auth/get-session") {
			return &fakeResponse{body: js(t, `{}`)}, nil
		}
		return &fakeResponse{body: js(t, `[]`)}, nil
	})
	var de *PhotonDashboardAuthError
	if _, err := ValidatePhotonToken(context.Background(), "some-token"); !errors.As(err, &de) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginDeviceFlowValidatesBeforePersisting(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	patchPost(t, func(c postCall) (photonResponse, error) {
		if strings.HasSuffix(c.url, "/api/auth/device/code") {
			return &fakeResponse{body: js(t, `{"device_code": "dev", "user_code": "AAAA", "verification_uri": "https://app.photon.codes/device",
				"verification_uri_complete": null, "expires_in": 600, "interval": 0}`)}, nil
		}
		return &fakeResponse{body: js(t, `{"access_token": "good-token"}`)}, nil
	})
	patchGet(t, func(c getCall) (photonResponse, error) {
		if strings.HasSuffix(c.url, "/api/auth/get-session") {
			return &fakeResponse{body: js(t, `{"user": {"id": "u1"}}`)}, nil
		}
		return &fakeResponse{body: js(t, `[]`)}, nil
	})
	var slept []int
	prev := sleepFn
	sleepFn = func(_ context.Context, s int) error { slept = append(slept, s); return nil }
	t.Cleanup(func() { sleepFn = prev })
	token, err := LoginDeviceFlow(ctx, DefaultClientID, false, nil)
	if err != nil || token != "good-token" || len(slept) != 1 || slept[0] != DefaultPollInterval {
		t.Fatalf("token = %q slept %v, %v", token, slept, err)
	}
	if stored, err := LoadPhotonToken(ctx); err != nil || stored != "good-token" {
		t.Fatalf("stored = %q, %v", stored, err)
	}
}

func TestLoginDeviceFlowNamesEveryRejectedSource(t *testing.T) {
	tmpIcebathHome(t)
	patchPost(t, func(c postCall) (photonResponse, error) {
		if strings.HasSuffix(c.url, "/api/auth/device/code") {
			return &fakeResponse{body: js(t, `{"device_code": "dev", "user_code": "AAAA", "verification_uri": "https://x", "interval": 1}`)}, nil
		}
		return &fakeResponse{body: js(t, `{"access_token": "bad-token"}`)}, nil
	})
	patchGet(t, func(getCall) (photonResponse, error) { return &fakeResponse{status: 401}, nil })
	prev := sleepFn
	sleepFn = func(context.Context, int) error { return nil }
	t.Cleanup(func() { sleepFn = prev })
	var shown DeviceCode
	_, err := LoginDeviceFlow(context.Background(), DefaultClientID, false, func(c DeviceCode) { shown = c; panic("display failed") })
	want := "Photon issued a device token, but the dashboard session lookup rejected it. Device login returned no project-valid dashboard token (tried: poll)."
	var de *PhotonDashboardAuthError
	if !errors.As(err, &de) || err.Error() != want || shown.UserCode != "AAAA" || shown.ExpiresIn != DefaultPollTimeout {
		t.Fatalf("err = %v shown %+v", err, shown)
	}
	if tok, _ := LoadPhotonToken(context.Background()); tok != "" {
		t.Fatalf("rejected token persisted: %q", tok)
	}
}

func TestPollForTokenFollowsTheDeviceGrantStatuses(t *testing.T) {
	var replies []photonResponse
	patchPost(t, func(postCall) (photonResponse, error) {
		r := replies[0]
		replies = replies[1:]
		return r, nil
	})
	var slept []int
	prev := sleepFn
	sleepFn = func(_ context.Context, s int) error { slept = append(slept, s); return nil }
	t.Cleanup(func() { sleepFn = prev })
	pending := 0
	replies = []photonResponse{
		&fakeResponse{status: 400, body: js(t, `{"error": "authorization_pending"}`)},
		&fakeResponse{status: 400, body: js(t, `{"message": "slow_down"}`)},
		&fakeResponse{status: 429},
		&fakeResponse{status: 503, text: "busy"},
		&fakeResponse{headers: http.Header{"Set-Auth-Token": {"Bearer from-header"}}},
	}
	one := 1
	tok, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, 60, &one, func() { pending++; panic("ignored") })
	if err != nil || tok != "from-header" || pending != 3 || len(slept) != 5 || slept[1] != 1 || slept[2] != 6 || slept[3] != 16 || slept[4] != 16 {
		t.Fatalf("token = %q pending %d slept %v, %v", tok, pending, slept, err)
	}
	for body, want := range map[string]string{
		`{"error": "access_denied"}`: "Photon login failed: access_denied",
		`{"error": "expired_token"}`: "Photon login failed: expired_token",
		`{"error": "invalid_grant"}`: "Photon device token error: invalid_grant",
	} {
		replies = []photonResponse{&fakeResponse{status: 400, body: js(t, body)}}
		if _, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, 60, &one, nil); err == nil || err.Error() != want {
			t.Fatalf("%s = %v", body, err)
		}
	}
	replies = []photonResponse{&fakeResponse{status: 400, text: "raw"}}
	if _, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, 60, &one, nil); err == nil || err.Error() != "Photon device token error: raw" {
		t.Fatalf("raw text = %v", err)
	}
	replies = []photonResponse{&fakeResponse{body: js(t, `{"token_type": "bearer"}`)}}
	if _, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, 60, &one, nil); err == nil || !strings.Contains(err.Error(), "no token candidate") {
		t.Fatalf("no candidate = %v", err)
	}
	replies = nil
	if _, err := PollForToken(context.Background(), deviceCodeFixture(), DefaultClientID, -1, &one, nil); err == nil || err.Error() != "Photon device login timed out" {
		t.Fatalf("timeout = %v", err)
	}
}

func TestClearPhotonTokenEmptiesEveryShape(t *testing.T) {
	ctx := context.Background()
	h := tmpIcebathHome(t)
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte("\ufeff"+`{"credential_pool": {"photon": [{"token": "pool"}]}, "providers": {"photon": {"access_token": "legacy"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadPhotonToken(ctx); err != nil || tok != "pool" {
		t.Fatalf("pool token = %q, %v", tok, err)
	}
	if err := ClearPhotonToken(ctx); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadPhotonToken(ctx); err != nil || tok != "" {
		t.Fatalf("cleared token = %q, %v", tok, err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(`{"providers": {"photon": {"access_token": "legacy"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := LoadPhotonToken(ctx); tok != "legacy" {
		t.Fatalf("legacy token = %q", tok)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadPhotonToken(ctx); err != nil || tok != "" {
		t.Fatalf("corrupt file = %q, %v", tok, err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(`[1]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPhotonToken(ctx); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("list file = %v", err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte{'{', 0xff, '}'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPhotonToken(ctx); err == nil || err.Error() != "'utf-8' codec can't decode byte 0xff in position 1: invalid start byte" {
		t.Fatalf("bad bytes = %v", err)
	}
}

func TestCheckPhotonTokenValidTreatsTransientFailuresAsValid(t *testing.T) {
	ctx := context.Background()
	if ok, err := CheckPhotonTokenValid(ctx, ""); ok || err != nil {
		t.Fatal("empty token is stale")
	}
	patchGet(t, func(getCall) (photonResponse, error) { return &fakeResponse{status: 403}, nil })
	if ok, _ := CheckPhotonTokenValid(ctx, "tok"); ok {
		t.Fatal("a 403 is a definitive rejection")
	}
	patchGet(t, func(getCall) (photonResponse, error) { return nil, &requestError{class: "ConnectError", msg: "down"} })
	if ok, _ := CheckPhotonTokenValid(ctx, "tok"); !ok {
		t.Fatal("a network failure keeps the token")
	}
}

func TestOperatorPhoneFromASoleAllowedUser(t *testing.T) {
	ctx := context.Background()
	tmpIcebathHome(t)
	t.Setenv("PHOTON_ALLOWED_USERS", "bob,+1555-000-1111")
	if phone, _, err := LoadUserNumbers(ctx); err != nil || phone != "+15550001111" {
		t.Fatalf("phone = %q, %v", phone, err)
	}
	t.Setenv("PHOTON_ALLOWED_USERS", "+15550001111 +15550002222")
	if phone, _, _ := LoadUserNumbers(ctx); phone != "" {
		t.Fatalf("two candidates = %q", phone)
	}
}

func TestCreateUserValidatesAndUnwraps(t *testing.T) {
	ctx := context.Background()
	if _, err := CreateUser(ctx, "p", "s", "5551234567", UserFields{}, false); err == nil || err.Error() != "phone_number must be E.164 (e.g. +15551234567); got '5551234567'" {
		t.Fatalf("E.164 = %v", err)
	}
	var posted postCall
	patchPost(t, func(c postCall) (photonResponse, error) {
		posted = c
		return &fakeResponse{body: js(t, `{"data": {"id": "u9"}}`)}, nil
	})
	user, err := CreateUser(ctx, "p", "s", "+15551234567", UserFields{FirstName: "Ada"}, true)
	body := posted.body.(*pyjson.Object)
	if err != nil || objGet(user, "id") != "u9" || objGet(body, "type") != "shared" || objGet(body, "firstName") != "Ada" || objGet(body, "lastName") != nil {
		t.Fatalf("user = %v body %v, %v", user, body, err)
	}
	patchPost(t, func(postCall) (photonResponse, error) {
		return &fakeResponse{status: 422, body: js(t, `{"message": "bad phone"}`)}, nil
	})
	if _, err := CreateUser(ctx, "p", "s", "+15551234567", UserFields{}, false); err == nil || err.Error() != "Photon create-user failed: HTTP 422: bad phone" {
		t.Fatalf("status = %v", err)
	}
}

func TestCreateProjectRejectsFailureShapes(t *testing.T) {
	for body, want := range map[string]string{
		`[1]`:                                 "Photon create-project returned an unexpected response",
		`[]`:                                  "Photon create-project did not return a project id",
		`{"error": "quota"}`:                  "Photon create-project failed: quota",
		`{"succeed": false}`:                  "Photon create-project failed: {'succeed': False}",
		`{"data": {"name": "x"}}`:             "Photon create-project did not return a project id",
		`{"succeed": false, "message": "no"}`: "Photon create-project failed: no",
	} {
		patchPost(t, func(postCall) (photonResponse, error) { return &fakeResponse{body: js(t, body)}, nil })
		if _, err := CreateProject(context.Background(), "tok", DefaultProjectName, DefaultLocation); err == nil || err.Error() != want {
			t.Fatalf("%s = %v", body, err)
		}
	}
}

func TestLiveRequestSpeaksTheWireProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects/p/regenerate-secret":
			buf := make([]byte, 64)
			n, _ := r.Body.Read(buf)
			if string(buf[:n]) != "{}" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"projectSecret": "fresh"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("PHOTON_DASHBOARD_HOST", srv.URL+"/")
	ctx := context.Background()
	if secret, err := RegenerateProjectSecret(ctx, "tok", "p"); err != nil || secret != "fresh" {
		t.Fatalf("secret = %q, %v", secret, err)
	}
	_, err := ListProjects(ctx, "tok")
	if e, ok := err.(*pyError); !ok || e.class != "HTTPStatusError" || !strings.HasPrefix(err.Error(), "Client error '404 Not Found' for url '"+srv.URL+"/api/projects'") {
		t.Fatalf("404 = %v", err)
	}
	t.Setenv("PHOTON_DASHBOARD_HOST", "app.photon.codes")
	if _, err := ListProjects(ctx, "tok"); err == nil || err.Error() != "Request URL is missing an 'http://' or 'https://' protocol." {
		t.Fatalf("schemeless = %v", err)
	}
}

func TestAuthJSONPathFollowsProfileOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "default"))
	t.Setenv("HOME", filepath.Join(root, "user-home"))
	profile := filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := authJSONPath(home.WithOverride(context.Background(), profile)); !strings.HasPrefix(got, profile) {
		t.Fatalf("photon resolved %q outside the active profile", got)
	}
}

func multiplexOn(t *testing.T) {
	provider.SetMultiplexActive(false)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
}

func TestScopedSecretHelperScopedReadWinsOverEnviron(t *testing.T) {
	t.Setenv("PHOTON_PROJECT_SECRET", "default-profile-value")
	multiplexOn(t)
	ctx := provider.WithSecretScope(context.Background(), map[string]string{"PHOTON_PROJECT_SECRET": "secondary-profile-value"}, "")
	if v, err := getScopedSecret(ctx, "PHOTON_PROJECT_SECRET", "", false); err != nil || v != "secondary-profile-value" {
		t.Fatalf("scoped = %q, %v", v, err)
	}
}

func TestScopedSecretHelperScopedMissReturnsDefaultNoEnvironBorrow(t *testing.T) {
	t.Setenv("PHOTON_PROJECT_SECRET", "default-profile-value")
	multiplexOn(t)
	ctx := provider.WithSecretScope(context.Background(), map[string]string{"SOME_OTHER_KEY": "x"}, "")
	for _, def := range []string{"", "sentinel"} {
		if v, err := getScopedSecret(ctx, "PHOTON_PROJECT_SECRET", def, false); err != nil || v != def {
			t.Fatalf("miss with default %q = %q, %v", def, v, err)
		}
	}
}

func TestScopedSecretHelperUnscopedUnderMultiplexFallsBackToEnviron(t *testing.T) {
	t.Setenv("PHOTON_PROJECT_SECRET", "default-profile-own-value")
	multiplexOn(t)
	ctx := context.Background()
	if provider.CurrentSecretScope(ctx) != nil {
		t.Fatal("no scope installed")
	}
	if v, err := getScopedSecret(ctx, "PHOTON_PROJECT_SECRET", "", false); err != nil || v != "default-profile-own-value" {
		t.Fatalf("unscoped = %q, %v", v, err)
	}
	_ = os.Unsetenv("PHOTON_PROJECT_SECRET")
	if v, _ := getScopedSecret(ctx, "PHOTON_PROJECT_SECRET", "fallback-default", false); v != "fallback-default" {
		t.Fatalf("unset = %q", v)
	}
}

func TestScopedSecretHelperSingleProfileLegacyEnvironRead(t *testing.T) {
	provider.SetMultiplexActive(false)
	t.Setenv("PHOTON_PROJECT_SECRET", "legacy-env-value")
	if v, err := getScopedSecret(context.Background(), "PHOTON_PROJECT_SECRET", "", false); err != nil || v != "legacy-env-value" {
		t.Fatalf("legacy = %q, %v", v, err)
	}
	_ = os.Unsetenv("PHOTON_PROJECT_SECRET")
}

func TestAuthMultiplexScopedMissDoesNotLeakDefaultProjectID(t *testing.T) {
	tmpIcebathHome(t)
	t.Setenv("PHOTON_PROJECT_ID", "default-project-id")
	t.Setenv("PHOTON_PROJECT_SECRET", "default-project-secret")
	multiplexOn(t)
	ctx := provider.WithSecretScope(context.Background(), map[string]string{"SOMETHING_ELSE": "x"}, "")
	if sid, secret, err := LoadProjectCredentials(ctx); err != nil || sid != "" || secret != "" {
		t.Fatalf("credentials = %q %q, %v", sid, secret, err)
	}
}
