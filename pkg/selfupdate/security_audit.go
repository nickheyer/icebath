package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	osvBatchURL       = "https://api.osv.dev/v1/querybatch"
	osvVulnURL        = "https://api.osv.dev/v1/vulns/%s"
	osvBatchMax       = 1000
	auditHTTPTimeout  = 20 * time.Second
	detailParallelism = 8
)

// Severity tiers for the fail on gate, unknown below low so it never blocks
var severityOrder = map[string]int{"UNKNOWN": 0, "LOW": 1, "MODERATE": 2, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}

// One name, version and OSV ecosystem found on disk with its origin
type auditComponent struct {
	Name, Version, Ecosystem, Source string
}

// One OSV advisory with its tier, summary and fixed versions
type auditVulnerability struct {
	OSVID         string
	Severity      string
	Summary       string
	FixedVersions []string
}

type auditFinding struct {
	Component auditComponent
	Vuln      auditVulnerability
}

// Network error an OSV call raised, the urllib URLError, TimeoutError or ConnectionError
type auditHTTPError struct{ err error }

func (e *auditHTTPError) Error() string { return e.err.Error() }

// Audit seams over the OSV transport, the venv scan and the error stream, replaced by hermes tests
var (
	auditHTTPJSON              = osvHTTPJSON
	auditVenvPins              = venvDistributionPins
	auditStderr      io.Writer = pysys.Stderr
	auditVersionInfo           = updaterecord.GetVersionInfo
)

// Distributions of this install's committed environment, the import path of the running agent
func venvDistributionPins(ctx context.Context) ([]pm.DistributionPin, error) {
	venv, err := pm.CommittedVenv(ctx, pm.RepoRoot())
	if err != nil || venv == "" {
		return []pm.DistributionPin{}, err
	}
	return pm.DistributionPins(venv)
}

// Every distribution installed in the agent environment, one per lowered name and version
func discoverVenv(ctx context.Context) ([]auditComponent, error) {
	pins, err := auditVenvPins(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[[2]string]bool{}
	out := []auditComponent{}
	for _, pin := range pins {
		name, version := "", ""
		if pin.Name != nil {
			name = pystr.Strip(*pin.Name)
		}
		if pin.Version != nil {
			version = pystr.Strip(*pin.Version)
		}
		if strings.ReplaceAll(strings.ToLower(name), "_", "-") == "icebath-agent" && version == "0.0.0" {
			info, err := auditVersionInfo()
			if err != nil {
				return nil, err
			}
			version = info.BaseVersion
			if version == "unknown" {
				continue
			}
		}
		key := [2]string{strings.ToLower(name), version}
		if name != "" && version != "" && !seen[key] {
			seen[key] = true
			out = append(out, auditComponent{Name: name, Version: version, Ecosystem: "PyPI", Source: "venv"})
		}
	}
	return out, nil
}

var auditReqLine = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]+\])?\s*==\s*([A-Za-z0-9._+!-]+)\s*(?:;.*)?$`)

// Name and version of every spec that is an exact pin
func matchPins(specs []string) [][2]string {
	out := [][2]string{}
	for _, spec := range specs {
		if m := auditReqLine.FindStringSubmatch(spec); m != nil {
			out = append(out, [2]string{m[1], m[2]})
		}
	}
	return out
}

// Exact pins of a requirements file, comments and options skipped
func parseRequirements(text string) [][2]string {
	var specs []string
	for _, raw := range pystr.Splitlines(text, false) {
		line := pystr.Strip(raw)
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "-") {
			specs = append(specs, line)
		}
	}
	return matchPins(specs)
}

// Exact pins of a pyproject dependencies list and its optional groups, none when the file does not parse
func parsePyprojectPins(text string) [][2]string {
	doc, err := pytoml.Loads(text)
	if err != nil {
		return [][2]string{}
	}
	project, _ := doc["project"].(map[string]any)
	if !pyrepr.Truthy(doc["project"]) {
		project = map[string]any{}
	}
	if project == nil {
		return [][2]string{}
	}
	groups := []any{project["dependencies"]}
	if optional, ok := project["optional-dependencies"].(map[string]any); ok {
		keys := make([]string, 0, len(optional))
		for key := range optional {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			groups = append(groups, optional[key])
		}
	}
	var specs []string
	for _, group := range groups {
		if list, ok := group.([]any); ok {
			for _, item := range list {
				specs = append(specs, pyrepr.Str(item))
			}
		}
	}
	return matchPins(specs)
}

// Python pins plugins declare under the home plugins directory
func discoverPlugins(home string) []auditComponent {
	pluginsDir := filepath.Join(home, "plugins")
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return []auditComponent{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := []auditComponent{}
	files := []struct {
		name  string
		parse func(string) [][2]string
	}{{"requirements.txt", parseRequirements}, {"requirements-dev.txt", parseRequirements}, {"pyproject.toml", parsePyprojectPins}}
	for _, entry := range entries {
		dir := filepath.Join(pluginsDir, entry.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		for _, file := range files {
			path := filepath.Join(dir, file.name)
			var pins [][2]string
			if isRegular(path) {
				raw, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				pins = file.parse(pystr.DecodeUTF8SigReplace(raw))
			}
			for _, pin := range pins {
				out = append(out, auditComponent{Name: pin[0], Version: pin[1], Ecosystem: "PyPI", Source: "plugin:" + entry.Name()})
			}
		}
	}
	return out
}

var (
	auditNpxPkg = regexp.MustCompile(`^(@[A-Za-z0-9._-]+/[A-Za-z0-9._-]+|[A-Za-z0-9._-]+)@([A-Za-z0-9._+-]+)$`)
	auditUvxPkg = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)==([A-Za-z0-9._+!-]+)$`)
)

// Component of an MCP server command that pins an auditable version, false otherwise
func extractMCPComponent(serverName, command string, args []string) (auditComponent, bool) {
	cmd := strings.ToLower(pystr.Strip(command))
	var pattern *regexp.Regexp
	ecosystem := ""
	switch {
	case strings.HasSuffix(cmd, "npx"):
		pattern, ecosystem = auditNpxPkg, "npm"
	case strings.HasSuffix(cmd, "uvx"):
		pattern, ecosystem = auditUvxPkg, "PyPI"
	}
	ref, found := "", false
	for _, token := range args {
		if !strings.HasPrefix(token, "-") {
			ref, found = token, true
			break
		}
	}
	if pattern == nil || !found {
		return auditComponent{}, false
	}
	m := pattern.FindStringSubmatch(ref)
	if m == nil {
		return auditComponent{}, false
	}
	return auditComponent{Name: m[1], Version: m[2], Ecosystem: ecosystem, Source: "mcp:" + serverName}, true
}

// Pinned MCP server packages of the config
func discoverMCP(ctx context.Context) ([]auditComponent, error) {
	servers, err := mcp.GetMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []auditComponent{}
	for _, name := range names {
		cfg, ok := servers[name].(map[string]any)
		if !ok {
			continue
		}
		rawArgs := cfg["args"]
		if !pyrepr.Truthy(rawArgs) {
			rawArgs = []any{}
		}
		list, ok := rawArgs.([]any)
		if !ok {
			continue
		}
		args := make([]string, 0, len(list))
		for _, a := range list {
			args = append(args, pyrepr.Str(a))
		}
		command := ""
		if pyrepr.Truthy(cfg["command"]) {
			command = pyrepr.Str(cfg["command"])
		}
		if c, ok := extractMCPComponent(name, command, args); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// Decoded JSON of an OSV GET, or a POST of payload, a network failure as auditHTTPError
func osvHTTPJSON(ctx context.Context, url string, payload any) (any, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if payload != nil {
		text, err := pyjson.Dumps(payload)
		if err != nil {
			return nil, err
		}
		method, body = http.MethodPost, bytes.NewReader([]byte(text))
	}
	ctx, cancel := context.WithTimeout(ctx, auditHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, &auditHTTPError{err: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &auditHTTPError{err: fmt.Errorf("<urlopen error %s>", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &auditHTTPError{err: fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &auditHTTPError{err: err}
	}
	if !utf8.Valid(raw) {
		return nil, &pm.PyError{Class: "UnicodeDecodeError", Message: "'utf-8' codec can't decode the OSV response"}
	}
	return pyjson.Loads(string(raw))
}

// Advisory ids per component that has any, from batched OSV queries
func osvQueryBatch(ctx context.Context, components []auditComponent) ([]auditComponent, map[auditComponent][]string, error) {
	order := []auditComponent{}
	findings := map[auditComponent][]string{}
	for start := 0; start < len(components); start += osvBatchMax {
		chunk := components[start:min(len(components), start+osvBatchMax)]
		queries := make([]any, 0, len(chunk))
		for _, c := range chunk {
			queries = append(queries, pyjson.ObjectOf("package", pyjson.ObjectOf("name", c.Name, "ecosystem", c.Ecosystem), "version", c.Version))
		}
		resp, err := auditHTTPJSON(ctx, osvBatchURL, pyjson.ObjectOf("queries", queries))
		if err != nil {
			var network *auditHTTPError
			if errors.As(err, &network) {
				return nil, nil, runtimeErr("OSV batch query failed: " + network.Error())
			}
			return nil, nil, err
		}
		results, _ := rowValue(resp, "results").([]any)
		for i, result := range results {
			if i >= len(chunk) {
				break
			}
			vulns, _ := rowValue(result, "vulns").([]any)
			var ids []string
			for _, v := range vulns {
				if id := rowValue(v, "id"); pyrepr.Truthy(id) {
					ids = append(ids, pyrepr.Str(id))
				}
			}
			if len(ids) > 0 {
				if _, seen := findings[chunk[i]]; !seen {
					order = append(order, chunk[i])
				}
				findings[chunk[i]] = ids
			}
		}
	}
	return order, findings, nil
}

// Severity tier of an OSV record from the GHSA bucket, then the per affected ecosystem bucket
func osvSeverityFromRecord(record any) string {
	candidates := []any{rowValue(rowValue(record, "database_specific"), "severity")}
	affected, _ := rowValue(record, "affected").([]any)
	for _, entry := range affected {
		candidates = append(candidates, rowValue(rowValue(entry, "ecosystem_specific"), "severity"))
	}
	for _, sev := range candidates {
		if text, ok := sev.(string); ok {
			tier := strings.ToUpper(pystr.Strip(text))
			if _, known := severityOrder[tier]; known {
				return tier
			}
		}
	}
	return "UNKNOWN"
}

// Fixed versions named across an OSV record's ranges, deduplicated in order
func osvFixedVersions(record any) []string {
	out := []string{}
	seen := map[string]bool{}
	affected, _ := rowValue(record, "affected").([]any)
	for _, entry := range affected {
		ranges, _ := rowValue(entry, "ranges").([]any)
		for _, rng := range ranges {
			events, _ := rowValue(rng, "events").([]any)
			for _, event := range events {
				if value, present, _ := rowLookup(event, "fixed"); present {
					text := pyrepr.Str(value)
					if !seen[text] {
						seen[text] = true
						out = append(out, text)
					}
				}
			}
		}
	}
	return out
}

// Summary, severity and fixed versions of each unique advisory, fetched in parallel
func osvFetchDetails(ctx context.Context, ids []string) (map[string]auditVulnerability, error) {
	uniqueSet := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			uniqueSet[id] = true
		}
	}
	unique := make([]string, 0, len(uniqueSet))
	for id := range uniqueSet {
		unique = append(unique, id)
	}
	sort.Strings(unique)
	out := map[string]auditVulnerability{}
	if len(unique) == 0 {
		return out, nil
	}
	results := make([]auditVulnerability, len(unique))
	errs := make([]error, len(unique))
	sem := make(chan struct{}, detailParallelism)
	var wg sync.WaitGroup
	for i, vid := range unique {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, vid string) {
			defer wg.Done()
			defer func() { <-sem }()
			rec, err := auditHTTPJSON(ctx, fmt.Sprintf(osvVulnURL, vid), nil)
			if err != nil {
				var network *auditHTTPError
				if errors.As(err, &network) {
					results[i] = auditVulnerability{OSVID: vid, Severity: "UNKNOWN", FixedVersions: []string{}}
					return
				}
				errs[i] = err
				return
			}
			summary := ""
			if v := rowValue(rec, "summary"); pyrepr.Truthy(v) {
				summary = pystr.Strip(pyrepr.Str(v))
			}
			results[i] = auditVulnerability{OSVID: vid, Severity: osvSeverityFromRecord(rec), Summary: summary, FixedVersions: osvFixedVersions(rec)}
		}(i, vid)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, err
		}
		out[results[i].OSVID] = results[i]
	}
	return out, nil
}

// Every scannable component across the enabled sources
func discoverComponents(ctx context.Context, home string, skipVenv, skipPlugins, skipMCP bool) ([]auditComponent, error) {
	var out []auditComponent
	if !skipVenv {
		found, err := discoverVenv(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if !skipPlugins {
		out = append(out, discoverPlugins(home)...)
	}
	if !skipMCP {
		found, err := discoverMCP(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// Findings of the components against OSV, most severe first
func runAudit(ctx context.Context, components []auditComponent) ([]auditFinding, error) {
	if len(components) == 0 {
		return []auditFinding{}, nil
	}
	order, raw, err := osvQueryBatch(ctx, components)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []auditFinding{}, nil
	}
	var ids []string
	for _, comp := range order {
		ids = append(ids, raw[comp]...)
	}
	details, err := osvFetchDetails(ctx, ids)
	if err != nil {
		return nil, err
	}
	findings := []auditFinding{}
	for _, comp := range order {
		for _, vid := range raw[comp] {
			vuln, ok := details[vid]
			if !ok {
				vuln = auditVulnerability{OSVID: vid, Severity: "UNKNOWN", FixedVersions: []string{}}
			}
			findings = append(findings, auditFinding{Component: comp, Vuln: vuln})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if sa, sb := severityOrder[a.Vuln.Severity], severityOrder[b.Vuln.Severity]; sa != sb {
			return sa > sb
		}
		if a.Component.Source != b.Component.Source {
			return a.Component.Source < b.Component.Source
		}
		if an, bn := strings.ToLower(a.Component.Name), strings.ToLower(b.Component.Name); an != bn {
			return an < bn
		}
		return a.Vuln.OSVID < b.Vuln.OSVID
	})
	return findings, nil
}

// Human report of findings grouped by source
func renderHuman(findings []auditFinding, total int) string {
	if len(findings) == 0 {
		return fmt.Sprintf("No known vulnerabilities found across %d component(s).", total)
	}
	lines := []string{fmt.Sprintf("Found %d known vulnerability finding(s) across %d component(s):", len(findings), total), ""}
	lastSource, haveLast := "", false
	for _, f := range findings {
		c, v := f.Component, f.Vuln
		if !haveLast || c.Source != lastSource {
			lines = append(lines, "["+c.Source+"]")
			lastSource, haveLast = c.Source, true
		}
		lines = append(lines, fmt.Sprintf("  %s  %s==%s  %s", pystr.Ljust(v.Severity, 8), c.Name, c.Version, v.OSVID))
		if v.Summary != "" {
			summary := v.Summary
			if r := []rune(summary); len(r) > 100 {
				summary = string(r[:97]) + "..."
			}
			lines = append(lines, "           "+summary)
		}
		if len(v.FixedVersions) > 0 {
			lines = append(lines, "           fixed in: "+strings.Join(v.FixedVersions[:min(3, len(v.FixedVersions))], ", "))
		}
	}
	return strings.Join(lines, "\n")
}

// Indented JSON report of findings
func renderJSON(findings []auditFinding, total int) (string, error) {
	rows := make([]any, 0, len(findings))
	for _, f := range findings {
		fixed := make([]any, 0, len(f.Vuln.FixedVersions))
		for _, v := range f.Vuln.FixedVersions {
			fixed = append(fixed, v)
		}
		rows = append(rows, pyjson.ObjectOf("package", f.Component.Name, "version", f.Component.Version,
			"ecosystem", f.Component.Ecosystem, "source", f.Component.Source, "vuln_id", f.Vuln.OSVID,
			"severity", f.Vuln.Severity, "summary", f.Vuln.Summary, "fixed_versions", fixed))
	}
	return pyjson.Dumps(pyjson.ObjectOf("total_components_scanned", total, "finding_count", len(findings), "findings", rows), pyjson.Indent(2))
}

// Audits the venv, plugin and MCP pins against OSV, exit 1 when a finding meets the fail on tier
func CmdSecurityAudit(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	outputJSON := pyrepr.Truthy(ns["json"])
	failOn := "critical"
	if pyrepr.Truthy(ns["fail_on"]) {
		failOn = pyrepr.Str(ns["fail_on"])
	}
	failOn = strings.ToUpper(failOn)
	threshold, known := severityOrder[failOn]
	if !known {
		fmt.Fprintf(auditStderr, "unknown --fail-on value: %s (choose from: low, moderate, high, critical)\n", strings.ToLower(failOn))
		return 2, nil
	}
	components, err := discoverComponents(ctx, h, pyrepr.Truthy(ns["skip_venv"]), pyrepr.Truthy(ns["skip_plugins"]), pyrepr.Truthy(ns["skip_mcp"]))
	if err != nil {
		return 0, err
	}
	total := len(components)
	if total == 0 {
		if outputJSON {
			fmt.Fprintln(w, `{"total_components_scanned": 0, "finding_count": 0, "findings": []}`)
		} else {
			fmt.Fprintln(w, "No components discovered (everything skipped, or empty environment).")
		}
		return 0, nil
	}
	findings, err := runAudit(ctx, components)
	if err != nil {
		var pyErr *pm.PyError
		if errors.As(err, &pyErr) && pyErr.Class == "RuntimeError" {
			fmt.Fprintf(auditStderr, "audit failed: %s\n", pyErr.Message)
			return 2, nil
		}
		return 0, err
	}
	if outputJSON {
		text, err := renderJSON(findings, total)
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(w, text)
	} else {
		fmt.Fprintln(w, renderHuman(findings, total))
	}
	for _, f := range findings {
		if severityOrder[f.Vuln.Severity] >= threshold {
			return 1, nil
		}
	}
	return 0, nil
}
