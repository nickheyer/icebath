package plugins

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Skills guard engine applied to plugin install and update with plugin sized exemptions

const PluginScannerVersion = "plugin-guard-v12"

var excludedDirs = map[string]bool{
	".git": true, "__pycache__": true, "node_modules": true, ".venv": true, "venv": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true, ".tox": true,
}

var codeFileExtensions = map[string]bool{".py": true, ".js": true, ".ts": true, ".sh": true, ".bash": true, ".rb": true, ".pl": true, ".php": true}

var commentPrefixesByExtension = map[string]string{
	".py": "#", ".sh": "#", ".bash": "#", ".rb": "#", ".pl": "#", ".r": "#", ".jl": "#",
	".js": "//", ".ts": "//", ".php": "//",
}

var commentSeverityCap = map[string]string{"critical": "high", "high": "medium"}

var changelogFilenames = map[string]bool{"changelog.md": true}

var codeExemptPatternIDs = map[string]bool{
	"python_environ_get_secret": true, "python_getenv_secret": true, "python_os_environ": true, "node_process_env": true,
	"ruby_env_secret": true, "env_exfil_httpx": true, "env_exfil_requests": true, "env_exfil_fetch": true,
	"env_exfil_curl": true, "env_exfil_wget": true,
	"context_exfil": true, "send_to_url": true, "fake_policy": true,
	"agent_config_mod": true, "agent_config_contract": true, "encoded_exfil": true,
}

var severityRemap = map[string]string{"binary_file": "high", "hermes_env_access": "medium", "curl_pipe_shell": "high"}

var jsCapabilityRemap = map[string]string{"dns_exfil": "high", "ssh_backdoor": "high"}

var docProseDemotions = map[string]string{"agent_config_mod": "high", "hardcoded_secret": "high"}

var mainGuardDemotions = map[string]string{"hardcoded_secret": "high"}

const (
	maxPluginFileCount    = 400
	maxPluginTotalSizeKB  = 10 * 1024
	maxPluginSingleFileKB = 1024
)

var bytecodeSuffixes = []string{".pyc", ".pyo"}

type walkEntry struct {
	path string
	rel  string
	info fs.DirEntry
}

// Every non excluded entry under the plugin dir with its slash joined relative path
func walkPlugin(pluginDir string) []walkEntry {
	var out []walkEntry
	filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == pluginDir {
			return nil
		}
		rel, relErr := filepath.Rel(pluginDir, p)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, part := range parts {
			if excludedDirs[part] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		out = append(out, walkEntry{path: p, rel: strings.Join(parts, "/"), info: d})
		return nil
	})
	return out
}

func finding(patternID, severity, category, file, match, description string) skills.Finding {
	return skills.Finding{PatternID: patternID, Severity: severity, Category: category, File: file, Line: 0, Match: match, Description: description}
}

// Whether an if test is the conventional module self test guard comparing __name__ with "__main__"
func isMainGuard(n *pyast.Node) bool {
	test := n.Child("test")
	ops, comps := test.Children("ops"), test.Children("comparators")
	if !test.Is("Compare") || len(ops) != 1 || !ops[0].Is("Eq") || len(comps) != 1 {
		return false
	}
	isName := func(e *pyast.Node) bool { id, _ := e.Str("id"); return e.Is("Name") && id == "__name__" }
	isMain := func(e *pyast.Node) bool {
		v, ok := e.Get("value").(string)
		return e.Is("Constant") && ok && v == "__main__"
	}
	left, right := test.Child("left"), comps[0]
	return isName(left) && isMain(right) || isName(right) && isMain(left)
}

// Lines executed only by a module self test guard, none for unreadable, non UTF-8 or invalid Python
func mainGuardBodyLines(filePath string) map[int]bool {
	out := map[int]bool{}
	data, err := os.ReadFile(filePath)
	if err != nil || !utf8.Valid(data) {
		return out
	}
	tree, err := pyast.Parse(strings.TrimPrefix(string(data), "\ufeff"))
	if err != nil {
		if le, ok := err.(*pyast.LimitError); ok {
			panic(le)
		}
		return out
	}
	for _, n := range pyast.Walk(tree) {
		if !n.Is("If") || !isMainGuard(n) {
			continue
		}
		for _, st := range n.Children("body") {
			for l := st.Lineno; l <= st.EndLineno; l++ {
				out[l] = true
			}
		}
	}
	return out
}

var severityRankTable = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

// Lowers severity to the cap, never raises it
func atMost(severity, cap string) string {
	if severityRankTable[severity] > severityRankTable[cap] {
		return cap
	}
	return severity
}

// A whole line comment steps down once, twice for command or path shapes
func commentSeverity(f skills.Finding) string {
	sev := f.Severity
	if v, ok := commentSeverityCap[sev]; ok {
		sev = v
	}
	if IsAgentFacing(f) {
		return sev
	}
	if v, ok := StepDown[sev]; ok {
		return v
	}
	return sev
}

func fileLines(filePath string) []string {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n")
}

func stepDown(sev string) string {
	if v, ok := StepDown[sev]; ok {
		return v
	}
	return sev
}

// Severity after the inert context demotions from the broadest context to the narrowest
func contextSeverity(f skills.Finding, relPath, line, joined string, docProse, isCode, localeCatalog bool) string {
	sev := f.Severity
	if docProse {
		var capped string
		var ok bool
		if localeCatalog {
			capped, ok = CatalogCap(f)
		} else {
			capped, ok = ProseCap(f)
		}
		if ok {
			sev = capped
		}
		if IsSelfUninstallDoc(f, line) {
			sev = atMost(sev, "medium")
		}
	} else if IsJSONProseValue(f, relPath, line) {
		if capped, ok := ProseCap(f); ok {
			sev = capped
		}
	}
	if IsTestTree(relPath) {
		inert := f.Category == "credential_exposure" || IsInertFixtureLine(f, line, isCode, relPath)
		if inert {
			sev = atMost(sev, "medium")
		} else {
			sev = stepDown(sev)
		}
	}
	if isCode && IsRegexAlternationToken(f, line) {
		sev = stepDown(sev)
	}
	return inertContextCap(f, relPath, line, joined, isCode, docProse, sev)
}

// Narrow inert context demotions that only ever lower
func inertContextCap(f skills.Finding, relPath, line, joined string, isCode, docProse bool, sev string) string {
	if f.PatternID == "encoded_exfil" && IsBase64Media(line) {
		return "low"
	}
	toLow := IsLoopbackOnly(f, line) || IsLoopbackContinuation(f, line, joined) || IsHexInCharClass(f, line) ||
		(isCode && IsPipInstallInProseLiteral(f, line)) || IsOwnProcRead(f, joined)
	if toLow {
		return "low"
	}
	toMedium := IsBase64ArchiveRoundtrip(f, line) || IsCoinNameOnly(f, line) || IsTrustedInstallerPipe(f, line) ||
		IsSudoInert(f, line, relPath) || IsEnvDumpInSafetyClause(f, line) || IsRepoRootTraversal(f, line) ||
		IsBenignLazyOSImport(f, line) || IsDisclosedConfigNote(f, relPath) || IsAuthorizedKeysProse(f, line, docProse)
	if toMedium {
		return atMost(sev, "medium")
	}
	if IsPublicClientKey(f, line) {
		return atMost(sev, "high")
	}
	return sev
}

// A whole line code comment or a changelog entry describes threats instead of executing them
func isDefensiveDocumentation(f skills.Finding, relPath string) bool {
	if changelogFilenames[relName(relPath)] {
		return true
	}
	prefix, ok := commentPrefixesByExtension[relExt(relPath)]
	if !ok || f.Match == "" {
		return false
	}
	stripped := strings.TrimLeft(f.Match, " \t")
	if !strings.HasPrefix(stripped, prefix) {
		return false
	}
	if prefix == "#" && (strings.HasPrefix(stripped, "#!") || strings.HasPrefix(stripped, "#:")) {
		return false
	}
	return true
}

// Applies plugin specific exemptions and severity remaps to raw findings
func filterFindings(findings []skills.Finding, relPath, filePath string) []skills.Finding {
	ext := relExt(relPath)
	isCode := codeFileExtensions[ext]
	mainGuardLines := map[int]bool{}
	if strings.ToLower(filepath.Ext(filePath)) == ".py" {
		mainGuardLines = mainGuardBodyLines(filePath)
	}
	isJS := ext == ".js" || ext == ".ts"
	isCI := IsCIWorkflow(relPath)
	docProse := IsDocProse(relPath) || isCI
	localeCatalog := IsLocaleCatalog(relPath)
	var lines []string
	if len(findings) > 0 {
		lines = fileLines(filePath)
	}
	var out []skills.Finding
	for _, f := range findings {
		if isCode && codeExemptPatternIDs[f.PatternID] {
			continue
		}
		if isJS {
			if v, ok := jsCapabilityRemap[f.PatternID]; ok {
				f.Severity = v
			} else if v, ok := severityRemap[f.PatternID]; ok {
				f.Severity = v
			}
		} else if v, ok := severityRemap[f.PatternID]; ok {
			f.Severity = v
		}
		if docProse {
			if v, ok := docProseDemotions[f.PatternID]; ok {
				f.Severity = v
			}
		}
		line, joined := f.Match, f.Match
		if f.Line > 0 && f.Line <= len(lines) {
			line = lines[f.Line-1]
			joined = LogicalLine(lines, f.Line-1)
		}
		f.Severity = contextSeverity(f, relPath, line, joined, docProse || localeCatalog, isCode, localeCatalog)
		if isDefensiveDocumentation(f, relPath) {
			f.Severity = commentSeverity(f)
			if IsQuotedInjectionPhrase(f, line) {
				f.Severity = atMost(f.Severity, "medium")
			}
		}
		if v, ok := mainGuardDemotions[f.PatternID]; ok && f.Severity == "critical" && mainGuardLines[f.Line] {
			f.Severity = v
		}
		out = append(out, f)
	}
	return out
}

// Describes the critical findings that made a plugin install dangerous
func dangerousFindingsSummary(findings []skills.Finding) string {
	critical := 0
	ids := map[string]bool{}
	for _, f := range findings {
		if f.Severity == "critical" {
			critical++
			ids[f.PatternID] = true
		}
	}
	names := ""
	if len(ids) > 0 {
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		names = " (" + strings.Join(sorted, ", ") + ")"
	}
	return fmt.Sprintf("%d critical of %d findings%s", critical, len(findings), names)
}

// Bytecode the checkout tracks, empty without a checkout or git
func trackedBytecode(pluginDir string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "-C", pluginDir, "ls-files", "-z", "--", "*.pyc", "*.pyo")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return map[string]bool{}
	}
	result := map[string]bool{}
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			result[string(p)] = true
		}
	}
	return result
}

func hasBytecodeSuffix(rel string) bool {
	for _, s := range bytecodeSuffixes {
		if strings.HasSuffix(rel, s) {
			return true
		}
	}
	return false
}

// Tracked bytecode anywhere plus loose bytecode shipped in the tree
func bytecodeFindings(pluginDir string) []skills.Finding {
	names := map[string]bool{}
	for _, e := range walkPlugin(pluginDir) {
		if hasBytecodeSuffix(e.rel) && e.info.Type().IsRegular() {
			names[e.rel] = true
		}
	}
	for rel := range trackedBytecode(pluginDir) {
		names[rel] = true
	}
	sorted := make([]string, 0, len(names))
	for rel := range names {
		sorted = append(sorted, rel)
	}
	sort.Strings(sorted)
	var out []skills.Finding
	for _, rel := range sorted {
		out = append(out, finding("bytecode_file", severityRemap["binary_file"], "structural", rel, "committed bytecode",
			"committed Python bytecode runs in place of the scanned source (catalog rule 17)"))
	}
	return out
}

// Structural checks sized for plugin repositories
func checkPluginStructure(pluginDir string) []skills.Finding {
	findings := bytecodeFindings(pluginDir)
	fileCount, totalSize := 0, int64(0)
	resolvedRoot, err := filepath.EvalSymlinks(pluginDir)
	if err != nil {
		resolvedRoot = pluginDir
	}
	for _, e := range walkPlugin(pluginDir) {
		if e.info.Type()&fs.ModeSymlink != 0 {
			fileCount++
			resolved, err := filepath.EvalSymlinks(e.path)
			if err != nil {
				findings = append(findings, finding("broken_symlink", "medium", "traversal", e.rel, "broken symlink", "broken or circular symlink"))
				continue
			}
			if rel, err := filepath.Rel(resolvedRoot, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				findings = append(findings, finding("symlink_escape", "critical", "traversal", e.rel, "symlink -> "+resolved, "symlink points outside the plugin directory"))
			}
			continue
		}
		if !e.info.Type().IsRegular() {
			continue
		}
		fileCount++
		info, err := e.info.Info()
		if err != nil {
			continue
		}
		size := info.Size()
		totalSize += size
		if size > maxPluginSingleFileKB*1024 {
			findings = append(findings, finding("oversized_file", "medium", "structural", e.rel, fmt.Sprintf("%dKB", size/1024),
				fmt.Sprintf("file is %dKB (limit: %dKB)", size/1024, maxPluginSingleFileKB)))
		}
		ext := strings.ToLower(filepath.Ext(e.path))
		if skills.IsSuspiciousBinaryExtension(ext) {
			findings = append(findings, finding("binary_file", severityRemap["binary_file"], "structural", e.rel, "binary: "+ext,
				fmt.Sprintf("binary/executable file (%s) bundled in plugin (cannot be scanned)", ext)))
		}
	}
	if fileCount > maxPluginFileCount {
		findings = append(findings, finding("too_many_files", "medium", "structural", "(directory)", fmt.Sprintf("%d files", fileCount),
			fmt.Sprintf("plugin has %d files (limit: %d)", fileCount, maxPluginFileCount)))
	}
	if totalSize > maxPluginTotalSizeKB*1024 {
		findings = append(findings, finding("oversized_bundle", "medium", "structural", "(directory)", fmt.Sprintf("%dKB", totalSize/1024),
			fmt.Sprintf("plugin is %dKB total (limit: %dKB)", totalSize/1024, maxPluginTotalSizeKB)))
	}
	return findings
}

// ScanPlugin scans a plugin directory with every external plugin at community trust
func ScanPlugin(pluginDir, source string) skills.ScanResult {
	var all []skills.Finding
	if st, err := os.Stat(pluginDir); err == nil && st.IsDir() {
		all = append(all, checkPluginStructure(pluginDir)...)
		entries := walkPlugin(pluginDir)
		sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
		for _, e := range entries {
			if e.info.Type().IsRegular() {
				all = append(all, filterFindings(skills.ScanFile(e.path, e.rel), e.rel, e.path)...)
			}
		}
	}
	verdict := skills.DetermineVerdict(all)
	name := filepath.Base(pluginDir)
	summary := name + ": clean scan, no threats detected"
	if len(all) > 0 {
		cats := map[string]bool{}
		for _, f := range all {
			cats[f.Category] = true
		}
		sorted := make([]string, 0, len(cats))
		for c := range cats {
			sorted = append(sorted, c)
		}
		sort.Strings(sorted)
		summary = fmt.Sprintf("%s: %s — %d finding(s) in %s", name, verdict, len(all), strings.Join(sorted, ", "))
	}
	if source == "" {
		source = name
	}
	return skills.ScanResult{
		SkillName: name, Source: source, TrustLevel: "community", Verdict: verdict, Findings: all,
		ScannedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00"), Summary: summary,
		ScanProvenance: map[string]any{"scanner_version": PluginScannerVersion, "verdict": verdict, "source": source},
	}
}

// ShouldAllowPluginInstall maps a verdict to allowed, nil for confirmation, and the reason
func ShouldAllowPluginInstall(result skills.ScanResult, force bool) (*bool, string) {
	n := len(result.Findings)
	t, f := true, false
	switch result.Verdict {
	case "safe":
		return &t, "Allowed (clean scan)"
	case "caution":
		if force {
			return &t, fmt.Sprintf("Force-installed despite caution verdict (%d findings)", n)
		}
		return nil, fmt.Sprintf("Requires confirmation (caution verdict, %d findings)", n)
	}
	return &f, fmt.Sprintf("Blocked (dangerous verdict, %s). --force does not override a dangerous verdict.", dangerousFindingsSummary(result.Findings))
}

// FormatPluginScanReport renders the compact scan report the CLI shows
func FormatPluginScanReport(result skills.ScanResult) string {
	return skills.FormatScanReport(result)
}
