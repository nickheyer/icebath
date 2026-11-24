package openclaw

import (
	"os"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// One reported migration item, None source or destination held as nil
type ItemResult struct {
	Kind        string
	Source      *string
	Destination *string
	Status      string
	Reason      string
	Details     *jsonutil.Object
	Sensitive   bool
}

// Destination text, empty for None
func (i ItemResult) DestinationText() string { return derefOr(i.Destination, "") }

// JSON object of the item in dataclass field order
func (i ItemResult) JSON() *jsonutil.Object {
	o := jsonutil.NewObject()
	o.Set("kind", i.Kind)
	o.Set("source", optStr(i.Source))
	o.Set("destination", optStr(i.Destination))
	o.Set("status", i.Status)
	o.Set("reason", i.Reason)
	details := i.Details
	if details == nil {
		details = jsonutil.NewObject()
	}
	o.Set("details", details)
	o.Set("sensitive", i.Sensitive)
	return o
}

// Pointed to text or def for nil
func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// JSON value of an optional string, null for nil
func optStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Pointer to s, nil for the empty string Python treats as falsy
func strOrNone(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status counts in first seen order, the five standard statuses always present
type Summary struct {
	keys   []string
	counts map[string]int
}

// Summary seeded with zero counts of the standard statuses
func newSummary() *Summary {
	s := &Summary{counts: map[string]int{}}
	for _, k := range []string{StatusMigrated, StatusArchived, StatusSkipped, StatusConflict, StatusError} {
		s.add(k, 0)
	}
	return s
}

// Adds n to a status count, a new status appended
func (s *Summary) add(key string, n int) {
	if _, ok := s.counts[key]; !ok {
		s.keys = append(s.keys, key)
	}
	s.counts[key] += n
}

// Count of a status, zero when absent
func (s *Summary) Get(key string) int {
	if s == nil {
		return 0
	}
	return s.counts[key]
}

// JSON object of the counts
func (s *Summary) JSON() *jsonutil.Object {
	o := jsonutil.NewObject()
	for _, k := range s.keys {
		o.Set(k, int64(s.counts[k]))
	}
	return o
}

// Migration report the engine returns and persists
type Report struct {
	Timestamp         string
	Mode              string
	SourceRoot        string
	TargetRoot        string
	WorkspaceTarget   *string
	OutputDir         *string
	MigrateSecrets    bool
	Preset            *string
	SkillConflictMode string
	Selected          []string
	Summary           *Summary
	Items             []ItemResult
	Warnings          []string
	NextSteps         []string
}

// Output directory text, empty for None
func (r *Report) OutputDirText() string { return derefOr(r.OutputDir, "") }

// JSON object of the report in the key order report.json carries
func (r *Report) JSON() *jsonutil.Object {
	o := jsonutil.NewObject()
	o.Set("timestamp", r.Timestamp)
	o.Set("mode", r.Mode)
	o.Set("source_root", r.SourceRoot)
	o.Set("target_root", r.TargetRoot)
	o.Set("workspace_target", optStr(r.WorkspaceTarget))
	o.Set("output_dir", optStr(r.OutputDir))
	o.Set("migrate_secrets", r.MigrateSecrets)
	o.Set("preset", optStr(r.Preset))
	o.Set("skill_conflict_mode", r.SkillConflictMode)
	selection := jsonutil.NewObject()
	selection.Set("selected", stringsToAny(r.Selected))
	selection.Set("preset", optStr(r.Preset))
	selection.Set("skill_conflict_mode", r.SkillConflictMode)
	available := make([]any, 0, len(MigrationOptions))
	for _, opt := range MigrationOptions {
		available = append(available, jsonutil.ObjectOf("id", opt.ID, "label", opt.Label, "description", opt.Description))
	}
	selection.Set("available", available)
	presets := make([]any, 0, len(MigrationPresets))
	for _, p := range MigrationPresets {
		ids := append([]string(nil), p.Options...)
		sortStrings(ids)
		presets = append(presets, jsonutil.ObjectOf("id", p.ID, "selected", stringsToAny(ids)))
	}
	selection.Set("presets", presets)
	o.Set("selection", selection)
	o.Set("summary", r.Summary.JSON())
	items := make([]any, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, it.JSON())
	}
	o.Set("items", items)
	o.Set("warnings", stringsToAny(r.Warnings))
	o.Set("next_steps", stringsToAny(r.NextSteps))
	return o
}

// JSON list of strings
func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// JSON value of a YAML value, every mapping turned into an ordered object
func toJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := jsonutil.NewObject()
		for _, k := range config.MappingKeys(x) {
			o.Set(k, toJSON(x[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toJSON(e)
		}
		return out
	}
	return v
}

var secretKeyMarkers = []string{
	"accesstoken", "apikey", "authorization", "bearertoken", "clientsecret", "cookie", "credential", "password",
	"privatekey", "refreshtoken", "secret",
}

var secretValuePatterns = []*regexp2.Regexp{
	pyre.MustCompile(`\bBearer\s+[A-Za-z0-9._~+/=\-]+`),
	pyre.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}\b`),
	pyre.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{16,}\b`),
	pyre.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{8,}\b`),
	pyre.MustCompile(`\bAIza[0-9A-Za-z_\-]{12,}\b`),
}

// Key lowercased with everything outside a to z and 0 to 9 dropped
func normalizeSecretKey(key string) string {
	var b strings.Builder
	for _, r := range lower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Reports whether a key names a credential
func isSecretKey(key string) bool {
	n := normalizeSecretKey(key)
	if n == "token" || strings.HasSuffix(n, "token") || n == "auth" || n == "authorization" {
		return true
	}
	for _, marker := range secretKeyMarkers {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// String with every token shaped substring replaced by the redaction marker
func redactString(value string) string {
	for _, p := range secretValuePatterns {
		out, err := p.Replace(value, RedactedMigrationValue, -1, -1)
		if err != nil {
			raise(err)
		}
		value = out
	}
	return value
}

// Deep copy of a report value with credential keys and token shaped strings redacted
func RedactMigrationValue(value any) (out any, err error) {
	defer catch(&err)
	return redactInternal(value, map[any]bool{}), nil
}

// Redacts one value, a mapping met a second time collapsing to the marker
func redactInternal(value any, seen map[any]bool) any {
	switch v := value.(type) {
	case string:
		return redactString(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = redactInternal(e, seen)
		}
		return out
	case *jsonutil.Object:
		if v == nil {
			return nil
		}
		if seen[v] {
			return RedactedMigrationValue
		}
		seen[v] = true
		out := jsonutil.NewObject()
		for _, k := range v.Keys() {
			if isSecretKey(k) {
				out.Set(k, RedactedMigrationValue)
			} else {
				out.Set(k, redactInternal(v.Value(k), seen))
			}
		}
		return out
	case map[string]any:
		return redactInternal(toJSON(v), seen)
	}
	return value
}

// Writes the redacted report.json and summary.md into outputDir
func WriteReport(outputDir string, report *jsonutil.Object) (err error) {
	defer catch(&err)
	mkdirAll(outputDir)
	redactedValue, rerr := RedactMigrationValue(report)
	if rerr != nil {
		return rerr
	}
	redacted := redactedValue.(*jsonutil.Object)
	text, derr := jsonutil.Encode(redacted, jsonutil.Indent(2))
	if derr != nil {
		return derr
	}
	writeText(pathutil.Join(outputDir, "report.json"), text+"\n")
	grouped := map[string][]any{}
	items, _ := dindex(redacted, "items").([]any)
	for _, item := range items {
		status := pyStr(dindex(item, "status"))
		grouped[status] = append(grouped[status], item)
	}
	lines := []string{
		"# OpenClaw -> Icebath Migration Report",
		"",
		"- Timestamp: " + pyStr(dindex(redacted, "timestamp")),
		"- Mode: " + pyStr(dindex(redacted, "mode")),
		"- Source: `" + pyStr(dindex(redacted, "source_root")) + "`",
		"- Target: `" + pyStr(dindex(redacted, "target_root")) + "`",
		"",
		"## Summary",
		"",
	}
	for _, e := range ditems(dindex(redacted, "summary")) {
		lines = append(lines, "- "+e.Key+": "+pyStr(e.Val))
	}
	if warnings, _ := or(dget(redacted, "warnings", nil), []any{}).([]any); len(warnings) > 0 {
		lines = append(lines, "", "## Warnings", "")
		for _, w := range warnings {
			lines = append(lines, "- "+pyStr(w))
		}
	}
	lines = append(lines, "", "## What Was Not Fully Brought Over", "")
	var notBrought []any
	notBrought = append(notBrought, grouped[StatusSkipped]...)
	notBrought = append(notBrought, grouped[StatusConflict]...)
	notBrought = append(notBrought, grouped[StatusError]...)
	if len(notBrought) == 0 {
		lines = append(lines, "- Nothing. All discovered items were either migrated or archived.")
	}
	for _, item := range notBrought {
		source := pyStr(or(dindex(item, "source"), "(n/a)"))
		dest := pyStr(or(dindex(item, "destination"), "(n/a)"))
		reason := pyStr(or(dindex(item, "reason"), dindex(item, "status")))
		lines = append(lines, "- `"+source+"` -> `"+dest+"`: "+reason)
	}
	if steps, _ := or(dget(redacted, "next_steps", nil), []any{}).([]any); len(steps) > 0 {
		lines = append(lines, "", "## Next Steps", "")
		for _, s := range steps {
			lines = append(lines, "- "+pyStr(s))
		}
	}
	writeText(pathutil.Join(outputDir, "summary.md"), strings.Join(lines, "\n")+"\n")
	return nil
}

// Writes a JSON value with two space indentation and raw non-ASCII text plus a trailing newline
func writeJSONFile(path string, value any) {
	text, err := jsonutil.Encode(value, jsonutil.Indent(2))
	if err != nil {
		raise(err)
	}
	if err := os.WriteFile(path, []byte(text+"\n"), 0o666); err != nil {
		raise(err)
	}
}
