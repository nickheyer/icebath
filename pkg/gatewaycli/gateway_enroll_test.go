package gatewaycli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Default root with one secondary profile, the root pinned as the default root and the multiplex env cleared
func enrollTopology(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "")
	os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
	return root
}

func writeEnrollConfig(t *testing.T, root, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Warning outcome and output with the active home set to activeHome
func runEnrollWarning(activeHome string) (bool, string) {
	var buf bytes.Buffer
	fired := warnIfSecondaryMultiplexProfile(home.WithOverride(context.Background(), activeHome), &buf)
	return fired, buf.String()
}

func TestFiresForSecondaryWhenDefaultRootHasMultiplexOn(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "gateway:\n  multiplex_profiles: true\n")
	if fired, _ := runEnrollWarning(filepath.Join(root, "profiles", "alice")); !fired {
		t.Fatal("warning did not fire")
	}
}

func TestTheRetiredOptOutStillWarnsASecondary(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "gateway:\n  multiplex_profiles: false\n")
	if fired, _ := runEnrollWarning(filepath.Join(root, "profiles", "alice")); !fired {
		t.Fatal("warning did not fire")
	}
}

func TestSilentForDefaultProfileEvenWithMultiplexOn(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "gateway:\n  multiplex_profiles: true\n")
	if fired, _ := runEnrollWarning(root); fired {
		t.Fatal("warning fired for the default profile")
	}
}

func TestEnvOverrideForcesMultiplexOnWithoutConfigFlag(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "{}\n")
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "true")
	if fired, _ := runEnrollWarning(filepath.Join(root, "profiles", "alice")); !fired {
		t.Fatal("warning did not fire")
	}
}

func TestTheEnvOverrideIsRetiredToo(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "gateway:\n  multiplex_profiles: true\n")
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "false")
	if fired, _ := runEnrollWarning(filepath.Join(root, "profiles", "alice")); !fired {
		t.Fatal("warning did not fire")
	}
}

func TestSilentForUnrelatedDirNamedProfiles(t *testing.T) {
	root := enrollTopology(t)
	writeEnrollConfig(t, root, "gateway:\n  multiplex_profiles: true\n")
	other := filepath.Join(filepath.Dir(root), "elsewhere", "profiles", "x")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if fired, _ := runEnrollWarning(other); fired {
		t.Fatal("warning fired for an unrelated profiles directory")
	}
}

// Loopback connector and ambient IdP for one enroll run against a fresh profile
func enrollConnector(t *testing.T, status int, reply string) (string, string, *string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "no-managed"))
	for _, k := range []string{"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ENROLL_TOKEN", "GATEWAY_RELAY_IDP_CLIENT_ID", "GATEWAY_RELAY_IDP_CLIENT_SECRET", "GATEWAY_RELAY_IDP_SCOPE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	posted := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/idp" {
			_, _ = io.WriteString(w, "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2ln")
			return
		}
		raw, _ := io.ReadAll(r.Body)
		*posted = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization") + " " + string(raw)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", srv.URL+"/idp")
	return h, srv.URL, posted
}

func TestEnrollRedeemsTokenAndPersistsRelayCredentials(t *testing.T) {
	h, base, posted := enrollConnector(t, 200, `{"secret": "s3", "deliveryKey": "dk", "tenant": "acme", "gatewayId": "gw-x"}`)
	var out bytes.Buffer
	ns := map[string]any{"token": "tok", "connector_url": "ws" + strings.TrimPrefix(base, "http") + "/relay/", "gateway_id": "gw-in", "wake_url": nil}
	code, err := CmdGatewayEnroll(context.Background(), ns, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, out.String())
	}
	want := `POST /relay/enroll Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2ln {"enrollmentToken": "tok", "gatewayId": "gw-in"}`
	if *posted != want {
		t.Fatalf("posted %q", *posted)
	}
	env, _ := os.ReadFile(filepath.Join(h, ".env"))
	for _, line := range []string{"GATEWAY_RELAY_ID=gw-x", "GATEWAY_RELAY_SECRET=s3", "GATEWAY_RELAY_DELIVERY_KEY=dk", "GATEWAY_RELAY_URL=ws" + strings.TrimPrefix(base, "http") + "/relay"} {
		if !strings.Contains(string(env), line) {
			t.Fatalf(".env lacks %q:\n%s", line, env)
		}
	}
	text := out.String()
	for _, part := range []string{`✓ Enrolled gateway "gw-x" for tenant acme`, "GATEWAY_RELAY_SECRET=<hidden>", "GATEWAY_RELAY_DELIVERY_KEY=<hidden>", "Restart the gateway to pick up the new env."} {
		if !strings.Contains(text, part) {
			t.Fatalf("output lacks %q:\n%s", part, text)
		}
	}
}

func TestEnrollConnectorRefusalsBecomeEnrollmentFailures(t *testing.T) {
	cases := []struct {
		status int
		reply  string
		want   string
	}{
		{401, `{}`, "✗ Enrollment failed: Connector rejected the caller identity (401)."},
		{403, `{"error": "token already used"}`, "✗ Enrollment failed: token already used"},
		{403, `[]`, "✗ Enrollment failed: Enrollment token invalid, expired, already used, or tenant mismatch (403)."},
		{500, `{"error": "boom"}`, "✗ Enrollment failed: Connector returned HTTP 500: boom"},
		{200, `{"tenant": "acme"}`, "✗ Enrollment failed: Connector returned an unexpected response (no secret)."},
	}
	for _, tc := range cases {
		_, base, _ := enrollConnector(t, tc.status, tc.reply)
		var out bytes.Buffer
		code, err := CmdGatewayEnroll(context.Background(), map[string]any{"token": "tok", "connector_url": base}, &out)
		if err != nil || code != 1 || !strings.HasPrefix(out.String(), tc.want) {
			t.Fatalf("%d %s: code=%d err=%v out=%q", tc.status, tc.reply, code, err, out.String())
		}
	}
}

func TestEnrollRefusesWithoutTokenOrConnector(t *testing.T) {
	enrollConnector(t, 200, `{}`)
	var out bytes.Buffer
	if code, _ := CmdGatewayEnroll(context.Background(), map[string]any{"token": nil}, &out); code != 1 || !strings.HasPrefix(out.String(), "✗ No enrollment token.") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	out.Reset()
	if code, _ := CmdGatewayEnroll(context.Background(), map[string]any{"token": "tok"}, &out); code != 1 || !strings.HasPrefix(out.String(), "✗ No connector URL.") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}
