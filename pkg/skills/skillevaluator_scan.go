package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	// ScannerBin is the SkillEvaluator executable looked up on PATH
	ScannerBin = "skillevaluator"
	// Tier1Checks are the keyless deterministic SkillEvaluator checks run at install time
	Tier1Checks = "pii,unicode,lint,license,security"
	// ScanTimeoutSeconds bounds one Tier 1 scan
	ScanTimeoutSeconds = 120
)

// SecretsClassChecks are the pii categories that indicate a possibly real credential
var SecretsClassChecks = map[string]bool{"database_credentials": true, "hardcoded_secrets": true, "jwt_tokens": true,
	"webhook_urls": true, "aws_identifiers": true, "github_tokens": true, "private_keys": true}

// Tier1Finding is one SkillEvaluator finding reduced to what install time shows
type Tier1Finding struct {
	Check, Validator, Severity, Message, File string
	Line                                      *big.Int
	Suggestion                                string
}

// Reports whether the finding names a possibly real credential
func (f Tier1Finding) IsSecretsClass() bool { return SecretsClassChecks[f.Check] }

// File and line of the finding, the file alone without a line, a question mark without either
func (f Tier1Finding) Location() string {
	if f.File != "" && f.Line != nil && f.Line.Sign() != 0 {
		return f.File + ":" + f.Line.String()
	}
	if f.File != "" {
		return f.File
	}
	return "?"
}

// Tier1Report is the install relevant reduction of one SkillEvaluator run
type Tier1Report struct {
	Available        bool
	Passed           bool
	Findings         []Tier1Finding
	IncompleteChecks []string
	Error            string
}

// Findings that are hygiene advice rather than possible credentials
func (r Tier1Report) AdvisoryFindings() []Tier1Finding {
	return slices.DeleteFunc(slices.Clone(r.Findings), Tier1Finding.IsSecretsClass)
}

// Findings that name a possibly real credential
func (r Tier1Report) SecretsFindings() []Tier1Finding {
	return slices.DeleteFunc(slices.Clone(r.Findings), func(f Tier1Finding) bool { return !f.IsSecretsClass() })
}

// skills.tier1_advisory with Python truthiness and false words, true when unset or unreadable
func (s *Scope) Tier1AdvisoryEnabled() (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	cfg := s.skillsCfg()
	if cfg == nil {
		return true
	}
	value, ok := cfg["tier1_advisory"]
	if !ok {
		return true
	}
	if text, isStr := value.(string); isStr {
		return !slices.Contains([]string{"false", "0", "no", "off"}, pystr.Lower(pystr.Strip(text)))
	}
	return pyrepr.Truthy(value)
}

func pyCut200(s string) string {
	n := 0
	for i := range s {
		if n == 200 {
			return s[:i]
		}
		n++
	}
	return s
}

func pyOr(v, def any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return def
}

// Findings and pass signal of a SkillEvaluator JSON report, incomplete validators kept out of the signal
func parseTier1Report(report *pyjson.Object) (out Tier1Report, err error) {
	defer hubRecover(&err)
	findings, incomplete, failed := []Tier1Finding{}, []string{}, false
	for _, res := range pyIterate(pyOr(pyGet(report, "results", []any{}), []any{})) {
		validator := pyrepr.Str(pyGet(res, "validator", "unknown"))
		if pystr.Lower(pyrepr.Str(pyGet(res, "status", ""))) == "incomplete" {
			incomplete = append(incomplete, validator)
		} else {
			failed = failed || !pyrepr.Truthy(pyGet(res, "passed", true))
		}
		for _, f := range pyIterate(pyOr(pyGet(res, "findings", []any{}), []any{})) {
			if _, isDict := f.(*pyjson.Object); !isDict {
				continue
			}
			line, lerr := pyint.Of(pyOr(pyGet(f, "line_number", nil), 0))
			if lerr != nil {
				hubRaise(lerr)
			}
			findings = append(findings, Tier1Finding{
				Check: pyrepr.Str(pyGet(f, "check_name", "")), Validator: validator,
				Severity: pystr.Lower(pyrepr.Str(pyGet(f, "severity", "info"))), Message: pyCut200(pyrepr.Str(pyGet(f, "message", ""))),
				File: pyrepr.Str(pyGet(f, "file_path", "")), Line: line, Suggestion: pyCut200(pyrepr.Str(pyGet(f, "suggestion", ""))),
			})
		}
	}
	return Tier1Report{Available: true, Passed: !failed && len(findings) == 0, Findings: findings, IncompleteChecks: incomplete}, nil
}

var tier1LookPath = exec.LookPath

// Runs the scanner with captured output, reporting a timeout or a launch failure but not its exit status
var tier1Exec = func(ctx context.Context, argv []string, timeout time.Duration) (bool, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	var sink strings.Builder
	cmd.Stdout, cmd.Stderr = &sink, &sink
	runErr := cmd.Run()
	if runCtx.Err() != nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return false, runErr
	}
	return false, nil
}

// SkillEvaluator Tier 1 over one skill dir, unavailable on every scanner failure, errors only for undecodable reports
func RunTier1Scan(ctx context.Context, skillDir string, timeout int) (Tier1Report, error) {
	unavailable := func(why string) (Tier1Report, error) { return Tier1Report{Passed: true, Error: why}, nil }
	if _, err := tier1LookPath(ScannerBin); err != nil {
		return unavailable("scanner not on PATH")
	}
	outdir, err := os.MkdirTemp("", "se-tier1-")
	if err != nil {
		return Tier1Report{}, err
	}
	defer os.RemoveAll(outdir)
	argv := []string{ScannerBin, "validate", skillDir, "--checks", Tier1Checks, "--no-dedup", "-r", "json", "-o", outdir}
	timedOut, launchErr := tier1Exec(ctx, argv, time.Duration(timeout)*time.Second)
	if timedOut {
		return unavailable("scan timed out after " + strconv.Itoa(timeout) + "s")
	}
	if launchErr != nil {
		return unavailable(fmt.Sprintf("scanner failed to launch: %v", launchErr))
	}
	reports, _ := filepath.Glob(filepath.Join(outdir, "skillevaluator-output-*.json"))
	if len(reports) == 0 {
		return unavailable("scanner produced no JSON report")
	}
	slices.Sort(reports)
	data, err := os.ReadFile(reports[len(reports)-1])
	if err != nil {
		return unavailable(fmt.Sprintf("unparseable report: %v", err))
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return Tier1Report{}, pystr.UTF8DecodeError(data, i)
		}
		i += size
	}
	parsed, err := pyjson.Loads(string(data))
	if err != nil {
		return unavailable(fmt.Sprintf("unparseable report: %v", err))
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return unavailable("unexpected report shape")
	}
	return parseTier1Report(obj)
}

// Console advisory summary of a Tier 1 report, empty when the scan was unavailable
func FormatTier1Report(report Tier1Report, limit int) string {
	if !report.Available {
		return ""
	}
	var lines []string
	if len(report.Findings) == 0 {
		if len(report.IncompleteChecks) > 0 {
			lines = append(lines, "SkillEvaluator Tier 1: no findings from completed checks.")
		} else {
			lines = append(lines, "SkillEvaluator Tier 1: no findings.")
		}
	} else {
		lines = append(lines, fmt.Sprintf("SkillEvaluator Tier 1 (advisory): %d finding(s) \xe2\x80\x94 informational, "+
			"verify before relying on this skill.", len(report.Findings)))
		shown := append(report.SecretsFindings(), report.AdvisoryFindings()...)
		for _, f := range pyHeadFindings(shown, limit) {
			label := pystr.Upper(f.Severity)
			if f.IsSecretsClass() {
				label = "SECRETS"
			}
			lines = append(lines, fmt.Sprintf("  [%s] %s \xe2\x80\x94 %s", label, f.Location(), f.Message))
		}
		if len(shown) > limit {
			lines = append(lines, fmt.Sprintf("  \xe2\x80\xa6 and %d more", len(shown)-limit))
		}
	}
	if len(report.IncompleteChecks) > 0 {
		lines = append(lines, fmt.Sprintf("  (not run: %s \xe2\x80\x94 no opinion from these checks)", strings.Join(report.IncompleteChecks, ", ")))
	}
	return strings.Join(lines, "\n")
}

// Python shown[:limit] including negative limits
func pyHeadFindings(s []Tier1Finding, limit int) []Tier1Finding {
	if limit < 0 {
		limit += len(s)
		if limit < 0 {
			limit = 0
		}
	}
	return s[:min(limit, len(s))]
}
