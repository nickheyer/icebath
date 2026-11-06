package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func tier1Finding(check, severity, message string, line int) string {
	return fmt.Sprintf(`{"check_name": %q, "severity": %q, "message": %q, "file_path": "SKILL.md", "line_number": %d, "suggestion": "fix it"}`,
		check, severity, message, line)
}

// SkillEvaluator report JSON with a PII validator carrying findings and extra validator results appended
func tier1ReportJSON(t *testing.T, findings []string, extra ...string) *pyjson.Object {
	t.Helper()
	passed := fmt.Sprint(len(findings) == 0)
	results := []string{`{"validator": "PII Scan", "passed": ` + passed + `, "findings": [` + strings.Join(findings, ", ") + `]}`,
		`{"validator": "Unicode Smuggling Detection", "passed": true, "findings": []}`}
	v, err := pyjson.Loads(`{"overall_passed": ` + passed + `, "results": [` + strings.Join(append(results, extra...), ", ") + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func mustTier1(t *testing.T, raw *pyjson.Object) Tier1Report {
	t.Helper()
	r, err := parseTier1Report(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const incompleteSecurity = `{"validator": "Security Scan", "passed": false, "status": "incomplete", "findings": [%s]}`

func TestTier1ParseReport(t *testing.T) {
	r := mustTier1(t, tier1ReportJSON(t, nil))
	if !r.Available || !r.Passed || len(r.Findings) != 0 {
		t.Fatalf("clean %+v", r)
	}
	r = mustTier1(t, tier1ReportJSON(t, []string{tier1Finding("emails", "high", "Non-placeholder email address: git@github.com", 3)}))
	if len(r.Findings) != 1 || !slices.EqualFunc(r.AdvisoryFindings(), r.Findings, tier1Same) || len(r.SecretsFindings()) != 0 {
		t.Fatalf("emails %+v", r)
	}
	r = mustTier1(t, tier1ReportJSON(t, []string{tier1Finding("database_credentials", "critical", "msg", 3)}))
	if len(r.SecretsFindings()) != 1 || len(r.AdvisoryFindings()) != 0 {
		t.Fatalf("db creds %+v", r)
	}
	r = mustTier1(t, tier1ReportJSON(t, []string{tier1Finding("emails", "high", "msg", 3), `"not-a-dict"`}))
	if len(r.Findings) != 1 {
		t.Fatalf("malformed %+v", r)
	}
	r = mustTier1(t, tier1ReportJSON(t, nil, fmt.Sprintf(incompleteSecurity, "")))
	if !r.Passed || len(r.Findings) != 0 || !slices.Equal(r.IncompleteChecks, []string{"Security Scan"}) {
		t.Fatalf("incomplete %+v", r)
	}
	r = mustTier1(t, tier1ReportJSON(t, nil, fmt.Sprintf(incompleteSecurity, tier1Finding("hardcoded_secrets", "critical", "msg", 3))))
	if len(r.Findings) != 1 || !r.Findings[0].IsSecretsClass() || !slices.Equal(r.IncompleteChecks, []string{"Security Scan"}) || r.Passed {
		t.Fatalf("incomplete findings %+v", r)
	}
	bad, _ := pyjson.Loads(`{"results": ["x"]}`)
	if _, err := parseTier1Report(bad.(*pyjson.Object)); err == nil || err.Error() != "'str' object has no attribute 'get'" {
		t.Fatalf("non dict result: %v", err)
	}
}

func tier1Same(a, b Tier1Finding) bool {
	return a.Check == b.Check && a.Message == b.Message && a.Line.Cmp(b.Line) == 0
}

func patchTier1(t *testing.T, found bool, run func(ctx context.Context, argv []string, timeout time.Duration) (bool, error)) {
	prevLook, prevExec := tier1LookPath, tier1Exec
	t.Cleanup(func() { tier1LookPath, tier1Exec = prevLook, prevExec })
	tier1LookPath = func(string) (string, error) {
		if !found {
			return "", errors.New("not found")
		}
		return "/usr/bin/skillevaluator", nil
	}
	tier1Exec = run
}

func TestRunTier1Scan(t *testing.T) {
	dir := t.TempDir()
	patchTier1(t, false, nil)
	if r, err := RunTier1Scan(context.Background(), dir, ScanTimeoutSeconds); err != nil || r.Available || len(r.Findings) != 0 || r.Error != "scanner not on PATH" {
		t.Fatalf("missing %+v %v", r, err)
	}
	patchTier1(t, true, func(context.Context, []string, time.Duration) (bool, error) { return true, nil })
	if r, _ := RunTier1Scan(context.Background(), dir, 1); r.Available || r.Error != "scan timed out after 1s" {
		t.Fatalf("timeout %+v", r)
	}
	patchTier1(t, true, func(context.Context, []string, time.Duration) (bool, error) { return false, errors.New("boom") })
	if r, _ := RunTier1Scan(context.Background(), dir, ScanTimeoutSeconds); r.Available || r.Error != "scanner failed to launch: boom" {
		t.Fatalf("launch %+v", r)
	}
	patchTier1(t, true, func(context.Context, []string, time.Duration) (bool, error) { return false, nil })
	if r, _ := RunTier1Scan(context.Background(), dir, ScanTimeoutSeconds); r.Available || r.Error != "scanner produced no JSON report" {
		t.Fatalf("no report %+v", r)
	}
	var argvSeen []string
	patchTier1(t, true, func(_ context.Context, argv []string, _ time.Duration) (bool, error) {
		argvSeen = argv
		outdir := argv[slices.Index(argv, "-o")+1]
		payload := `{"overall_passed": false, "results": [{"validator": "PII Scan", "passed": false, "findings": [` + tier1Finding("emails", "high", "msg", 3) + `]}]}`
		return false, os.WriteFile(filepath.Join(outdir, "skillevaluator-output-1.json"), []byte(payload), 0o644)
	})
	r, err := RunTier1Scan(context.Background(), dir, ScanTimeoutSeconds)
	if err != nil || !r.Available || r.Passed || len(r.Findings) != 1 || r.Findings[0].Check != "emails" {
		t.Fatalf("real %+v %v", r, err)
	}
	if !slices.Equal(argvSeen[:7], []string{"skillevaluator", "validate", dir, "--checks", Tier1Checks, "--no-dedup", "-r"}) {
		t.Fatalf("argv %v", argvSeen)
	}
	if _, err := os.Stat(argvSeen[len(argvSeen)-1]); !os.IsNotExist(err) {
		t.Fatal("temporary report dir left behind")
	}
}

func TestFormatTier1Report(t *testing.T) {
	if FormatTier1Report(Tier1Report{}, 10) != "" {
		t.Fatal("unavailable report printed")
	}
	text := FormatTier1Report(mustTier1(t, tier1ReportJSON(t, []string{
		tier1Finding("database_credentials", "critical", "Database connection string with credentials", 3),
		tier1Finding("emails", "high", "Non-placeholder email address: a@b.com", 8)})), 10)
	for _, want := range []string{"[SECRETS]", "SKILL.md:3", "SKILL.md:8"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	text = FormatTier1Report(mustTier1(t, tier1ReportJSON(t, nil, fmt.Sprintf(incompleteSecurity, ""))), 10)
	if !strings.Contains(text, "Security Scan") {
		t.Fatalf("incomplete %q", text)
	}
	var findings []string
	for i := 1; i < 15; i++ {
		findings = append(findings, tier1Finding("emails", "high", fmt.Sprintf("m%d", i), i))
	}
	if text = FormatTier1Report(mustTier1(t, tier1ReportJSON(t, findings)), 5); !strings.Contains(text, "9 more") {
		t.Fatalf("limit %q", text)
	}
}

func TestTier1AdvisoryConfigGate(t *testing.T) {
	for body, want := range map[string]bool{
		"skills:\n  tier1_advisory: false\n":   false,
		"skills:\n  tier1_advisory: 'false'\n": false,
		"skills:\n  tier1_advisory: ' Off '\n": false,
		"skills:\n  tier1_advisory: null\n":    false,
		"skills:\n  tier1_advisory: 'yes'\n":   true,
		"skills: [1]\n":                        true,
		"skills: {\n":                          true,
	} {
		h := t.TempDir()
		writeTestFile(t, filepath.Join(h, "config.yaml"), body)
		if got := NewScope(h, nil).Tier1AdvisoryEnabled(); got != want {
			t.Errorf("%q: got %v want %v", body, got, want)
		}
	}
}
