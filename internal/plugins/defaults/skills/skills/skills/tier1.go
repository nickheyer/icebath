package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// The SkillEvaluator command looked up on the path
	evaluator = "skillevaluator"
	// Its checks that need no key and give the same answer each time, run at install
	tier1Checks = "pii,unicode,lint,license,security"
	// Longest one run may take
	tier1Timeout = 120 * time.Second
)

// Checks whose findings may be a real credential
var secretChecks = map[string]bool{"database_credentials": true, "hardcoded_secrets": true, "jwt_tokens": true, "webhook_urls": true,
	"aws_identifiers": true, "github_tokens": true, "private_keys": true}

// One SkillEvaluator finding, as much of it as an install shows
type tier1Finding struct {
	check, validator, severity, message, file string
	line                                      int64
}

func (f tier1Finding) secret() bool { return secretChecks[f.check] }

// File and line of the finding, the file alone without a line, ? without either
func (f tier1Finding) location() string {
	switch {
	case f.file != "" && f.line != 0:
		return f.file + ":" + strconv.FormatInt(f.line, 10)
	case f.file != "":
		return f.file
	}
	return "?"
}

// What one SkillEvaluator run found
type tier1Report struct {
	findings []tier1Finding
	// Validators that did not finish, which give no opinion
	incomplete []string
}

// Findings that may be a real credential first, then the rest
func (r tier1Report) ordered() []tier1Finding {
	secrets := slices.DeleteFunc(slices.Clone(r.findings), func(f tier1Finding) bool { return !f.secret() })
	return append(secrets, slices.DeleteFunc(slices.Clone(r.findings), tier1Finding.secret)...)
}

// Runs SkillEvaluator's quick checks over a skill folder; false with why when it is not installed or gives no report, an error only for a
// report that is not UTF-8
func tier1(ctx context.Context, dir string) (tier1Report, bool, string, error) {
	program, err := exec.LookPath(evaluator)
	if err != nil {
		return tier1Report{}, false, "scanner not on PATH", nil
	}
	out, err := os.MkdirTemp("", "se-tier1-")
	if err != nil {
		return tier1Report{}, false, "", err
	}
	defer os.RemoveAll(out)
	run, cancel := context.WithTimeout(ctx, tier1Timeout)
	defer cancel()
	cmd := exec.CommandContext(run, program, "validate", dir, "--checks", tier1Checks, "--no-dedup", "-r", "json", "-o", out)
	cmd.WaitDelay = 5 * time.Second
	proc.HideWindow(cmd)
	err = cmd.Run()
	if run.Err() != nil {
		return tier1Report{}, false, fmt.Sprintf("scan timed out after %ds", int(tier1Timeout.Seconds())), nil
	}
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		return tier1Report{}, false, fmt.Sprintf("scanner failed to launch: %v", err), nil
	}
	reports, _ := filepath.Glob(filepath.Join(out, "skillevaluator-output-*.json"))
	if len(reports) == 0 {
		return tier1Report{}, false, "scanner produced no JSON report", nil
	}
	slices.Sort(reports)
	data, err := os.ReadFile(reports[len(reports)-1])
	if err != nil {
		return tier1Report{}, false, fmt.Sprintf("unparseable report: %v", err), nil
	}
	data = bytes.TrimPrefix(data, []byte("\uFEFF"))
	if !utf8.Valid(data) {
		return tier1Report{}, false, "", errors.New("the SkillEvaluator report is not UTF-8")
	}
	var doc struct {
		Results []struct {
			Validator *string `json:"validator"`
			Status    any     `json:"status"`
			Passed    *bool   `json:"passed"`
			Findings  []any   `json:"findings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return tier1Report{}, false, fmt.Sprintf("unparseable report: %v", err), nil
	}
	var report tier1Report
	for _, result := range doc.Results {
		validator := "unknown"
		if result.Validator != nil {
			validator = *result.Validator
		}
		if strings.ToLower(fmt.Sprint(result.Status)) == "incomplete" {
			report.incomplete = append(report.incomplete, validator)
		}
		for _, raw := range result.Findings {
			f, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text := func(key string) string {
				if v, ok := f[key]; ok && v != nil {
					return fmt.Sprint(v)
				}
				return ""
			}
			line, _ := f["line_number"].(float64)
			severity := strings.ToLower(text("severity"))
			if _, ok := f["severity"]; !ok || f["severity"] == nil {
				severity = "info"
			}
			report.findings = append(report.findings, tier1Finding{check: text("check_name"), validator: validator, severity: severity,
				message: strs.Head(text("message"), 200), file: text("file_path"), line: int64(line)})
		}
	}
	return report, true, "", nil
}

// The report as the user reads it, at most limit findings, those that may be credentials first
func (r tier1Report) text(limit int) string {
	var lines []string
	if len(r.findings) == 0 {
		if len(r.incomplete) > 0 {
			lines = append(lines, "The quick advice check found nothing in the checks that ran.")
		} else {
			lines = append(lines, "The quick advice check found nothing.")
		}
	} else {
		lines = append(lines, fmt.Sprintf("The quick advice check found %s. They are only advice, so check them before you rely on this skill.", strs.Count(len(r.findings), "thing")))
		shown := r.ordered()
		for _, f := range head(shown, limit) {
			label := strings.ToUpper(f.severity[:min(1, len(f.severity))]) + f.severity[min(1, len(f.severity)):] + " risk"
			if f.secret() {
				label = "Possible secret"
			}
			lines = append(lines, fmt.Sprintf("  %s at %s: %s", label, f.location(), f.message))
		}
		if len(shown) > limit {
			lines = append(lines, fmt.Sprintf("  And %s more.", strs.Number(float64(len(shown)-limit))))
		}
	}
	if len(r.incomplete) > 0 {
		lines = append(lines, fmt.Sprintf("  These checks did not run, so they say nothing: %s.", strings.Join(r.incomplete, ", ")))
	}
	return strings.Join(lines, "\n")
}

// Says the advisory SkillEvaluator report of a quarantined skill when skills.tier1_advisory is on; it never blocks the install
func (r *run) advise(ctx context.Context, dir string) {
	if !r.cfg.GetBool(skillsconfig.Tier1Advisory) {
		return
	}
	report, available, _, err := tier1(ctx, dir)
	if err != nil || !available {
		return
	}
	text := report.text(10)
	if len(report.findings) == 0 {
		r.line(Dim, text)
		return
	}
	tone := Warn
	secrets := slices.ContainsFunc(report.findings, tier1Finding.secret)
	if secrets {
		tone = Bad
	}
	r.line(Heading, "Quick advice check")
	r.line(tone, text)
	if secrets {
		r.line(Bad, "✗ The lines above may hold secrets. Check them before you use this skill.")
	}
}
