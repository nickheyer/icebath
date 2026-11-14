package selfupdate

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// OSV transport answering the batch with ids per component name and each detail from a record table
func saFakeOSV(t *testing.T, ids map[string][]string, records map[string]*pyjson.Object) {
	vsSwap(t, &auditHTTPJSON, func(_ context.Context, url string, payload any) (any, error) {
		if payload == nil {
			id := url[strings.LastIndex(url, "/")+1:]
			return records[id], nil
		}
		queries := rowValue(payload, "queries").([]any)
		results := []any{}
		for _, q := range queries {
			vulns := []any{}
			for _, id := range ids[pyjsonText(rowValue(rowValue(q, "package"), "name"))] {
				vulns = append(vulns, pyjson.ObjectOf("id", id))
			}
			results = append(results, pyjson.ObjectOf("vulns", vulns))
		}
		return pyjson.ObjectOf("results", results), nil
	})
}

func pyjsonText(v any) string { s, _ := v.(string); return s }

func saWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRequirementsParserExtractsPinnedVersions(t *testing.T) {
	got := parseRequirements("requests==2.20.0\nflask==2.0.1\n")
	if !reflect.DeepEqual(got, [][2]string{{"requests", "2.20.0"}, {"flask", "2.0.1"}}) {
		t.Fatal(got)
	}
}

func TestRequirementsParserSkipsCommentsAndOptions(t *testing.T) {
	got := parseRequirements("# comment\n-r other.txt\n--index-url https://x\nflask==2.0.1\n")
	if !reflect.DeepEqual(got, [][2]string{{"flask", "2.0.1"}}) {
		t.Fatal(got)
	}
}

func TestMCPComponentExtractionNpxScopedPinned(t *testing.T) {
	comp, ok := extractMCPComponent("fs", "npx", []string{"-y", "@modelcontextprotocol/server-filesystem@0.5.0"})
	want := auditComponent{Name: "@modelcontextprotocol/server-filesystem", Version: "0.5.0", Ecosystem: "npm", Source: "mcp:fs"}
	if !ok || comp != want {
		t.Fatal(comp, ok)
	}
}

func TestMCPComponentExtractionEmptyArgs(t *testing.T) {
	if _, ok := extractMCPComponent("x", "npx", nil); ok {
		t.Fatal("an unpinned launcher produced a component")
	}
}

func TestPluginDiscoveryReadsRequirementsTxt(t *testing.T) {
	tmp := t.TempDir()
	saWrite(t, filepath.Join(tmp, "plugins", "myplugin", "requirements.txt"), "requests==2.20.0\n")
	components := discoverPlugins(tmp)
	if len(components) != 1 || components[0].Name != "requests" || components[0].Source != "plugin:myplugin" {
		t.Fatal(components)
	}
}

func TestPluginDiscoverySkipsWhenNoPluginsDir(t *testing.T) {
	if got := discoverPlugins(t.TempDir()); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestSeverityExtractionDatabaseSpecificSeverity(t *testing.T) {
	rec := pyjson.ObjectOf("database_specific", pyjson.ObjectOf("severity", "HIGH"))
	if got := osvSeverityFromRecord(rec); got != "HIGH" {
		t.Fatal(got)
	}
}

func TestSeverityExtractionFixedVersionsExtractedAndDeduped(t *testing.T) {
	rec := pyjson.ObjectOf("affected", []any{
		pyjson.ObjectOf("ranges", []any{pyjson.ObjectOf("events", []any{pyjson.ObjectOf("introduced", "0"), pyjson.ObjectOf("fixed", "2.0.0")})}),
		pyjson.ObjectOf("ranges", []any{pyjson.ObjectOf("events", []any{pyjson.ObjectOf("fixed", "2.0.0"), pyjson.ObjectOf("fixed", "1.9.5")})}),
	})
	if got := osvFixedVersions(rec); !reflect.DeepEqual(got, []string{"2.0.0", "1.9.5"}) {
		t.Fatal(got)
	}
}

// Venv components when the agent and requests both carry the 0.0.0 source version
func saVersions(t *testing.T, base string) map[string]string {
	t.Helper()
	text := func(s string) *string { return &s }
	vsSwap(t, &auditVenvPins, func(context.Context) ([]pm.DistributionPin, error) {
		return []pm.DistributionPin{{Name: text("icebath_agent"), Version: text("0.0.0")}, {Name: text("requests"), Version: text("0.0.0")}}, nil
	})
	vsSwap(t, &auditVersionInfo, func() (updaterecord.VersionInfo, error) {
		return updaterecord.VersionInfo{BaseVersion: base}, nil
	})
	comps, err := discoverVenv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range comps {
		out[c.Name] = c.Version
	}
	return out
}

func TestVenvDiscoveryPlaceholderAgentVersionResolvesToRunningRelease(t *testing.T) {
	versions := saVersions(t, "0.21.5")
	if versions["icebath_agent"] != "0.21.5" || versions["requests"] != "0.0.0" {
		t.Fatal(versions)
	}
}

func TestVenvDiscoveryUnknownReleaseSkipsPlaceholder(t *testing.T) {
	if versions := saVersions(t, "unknown"); !reflect.DeepEqual(versions, map[string]string{"requests": "0.0.0"}) {
		t.Fatal(versions)
	}
}

func TestRunAuditFindingsSortedBySeverityDesc(t *testing.T) {
	tmp := t.TempDir()
	saWrite(t, filepath.Join(tmp, "plugins", "p", "requirements.txt"), "alpha==1.0.0\nbeta==2.0.0\n")
	saFakeOSV(t, map[string][]string{"alpha": {"LOW-1"}, "beta": {"CRIT-1"}}, map[string]*pyjson.Object{
		"LOW-1":  pyjson.ObjectOf("database_specific", pyjson.ObjectOf("severity", "LOW"), "summary", "low"),
		"CRIT-1": pyjson.ObjectOf("database_specific", pyjson.ObjectOf("severity", "CRITICAL"), "summary", "crit"),
	})
	comps, err := discoverComponents(context.Background(), tmp, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := runAudit(context.Background(), comps)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 || findings[0].Vuln.OSVID != "CRIT-1" || findings[1].Vuln.OSVID != "LOW-1" {
		t.Fatal(findings)
	}
}

func TestExitCodesUnknownFailOnValueExitsTwo(t *testing.T) {
	var errw bytes.Buffer
	vsSwap[io.Writer](t, &auditStderr, &errw)
	ns := map[string]any{"skip_venv": true, "skip_plugins": true, "skip_mcp": true, "json": false, "fail_on": "garbage"}
	code, err := CmdSecurityAudit(context.Background(), t.TempDir(), ns, &bytes.Buffer{})
	if err != nil || code != 2 || !strings.Contains(strings.ToLower(errw.String()), "fail-on") {
		t.Fatal(code, err, errw.String())
	}
}

func TestExitCodesJSONOutputShape(t *testing.T) {
	text := func(s string) *string { return &s }
	vsSwap(t, &auditVenvPins, func(context.Context) ([]pm.DistributionPin, error) {
		return []pm.DistributionPin{{Name: text("pkg"), Version: text("1.0")}}, nil
	})
	saFakeOSV(t, map[string][]string{"pkg": {"X-1"}}, map[string]*pyjson.Object{
		"X-1": pyjson.ObjectOf("database_specific", pyjson.ObjectOf("severity", "HIGH"), "summary", "bad",
			"affected", []any{pyjson.ObjectOf("ranges", []any{pyjson.ObjectOf("events", []any{pyjson.ObjectOf("fixed", "1.1")})})}),
	})
	var out bytes.Buffer
	ns := map[string]any{"skip_venv": false, "skip_plugins": true, "skip_mcp": true, "json": true, "fail_on": "critical"}
	if _, err := CmdSecurityAudit(context.Background(), t.TempDir(), ns, &out); err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.Loads(out.String())
	if err != nil {
		t.Fatal(err, out.String())
	}
	finding := rowValue(decoded, "findings").([]any)[0]
	if !pyjson.Equal(rowValue(decoded, "finding_count"), 1) || rowValue(finding, "severity") != "HIGH" ||
		!reflect.DeepEqual(rowValue(finding, "fixed_versions"), []any{"1.1"}) {
		t.Fatal(out.String())
	}
}
