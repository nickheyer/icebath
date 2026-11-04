package plugins

import (
	"encoding/base64"
	"path"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Context demotions for plugin install scan findings, every helper is a pure predicate on finding, line, and path

type runeSpan struct{ a, b int }

func r2(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	return regexp2.MustCompile(pattern, opts)
}

// Every match of the regex as rune index spans
func matchSpans(re *regexp2.Regexp, s string) []runeSpan {
	if re == nil {
		return nil
	}
	var out []runeSpan
	m, _ := re.FindStringMatch(s)
	for m != nil {
		out = append(out, runeSpan{m.RuneIndex, m.RuneIndex + m.RuneLength})
		m, _ = re.FindNextMatch(m)
	}
	return out
}

func matchGroups(re *regexp2.Regexp, s string) []*regexp2.Match {
	var out []*regexp2.Match
	m, _ := re.FindStringMatch(s)
	for m != nil {
		out = append(out, m)
		m, _ = re.FindNextMatch(m)
	}
	return out
}

func reSearch(re *regexp2.Regexp, s string) bool {
	ok, _ := re.MatchString(s)
	return ok
}

func spanContains(spans []runeSpan, a, b int) (runeSpan, bool) {
	for _, s := range spans {
		if s.a <= a && b <= s.b {
			return s, true
		}
	}
	return runeSpan{}, false
}

// StepDown lowers one severity step, medium and low are already informational
var StepDown = map[string]string{"critical": "high", "high": "medium"}

var DocProseExtensions = map[string]bool{".md": true, ".txt": true, ".rst": true, ".html": true}

var proseKeepsFullSeverityCategories = map[string]bool{"injection": true, "credential_exposure": true}

var proseKeepsFullSeverityIDs = map[string]bool{
	"context_exfil": true, "send_to_url": true, "md_image_exfil": true, "md_link_exfil": true, "ssh_backdoor": true,
	"curl_pipe_shell": true, "wget_pipe_shell": true, "curl_pipe_python": true,
	"agent_config_mod": true, "agent_config_mod_shell": true, "agent_config_contract": true, "agent_config_ref": true,
	"hermes_config_mod": true, "hermes_config_mod_shell": true, "hermes_config_ref": true,
	"other_agent_config_mod": true, "other_agent_config_mod_shell": true, "other_agent_config_ref": true,
}

var agentInstructionDirs = map[string]bool{"skills": true, "optional-skills": true}

var agentInstructionFiles = map[string]bool{"skill.md": true, "after-install.md": true}

func relParts(relPath string) []string {
	var parts []string
	for _, p := range strings.Split(strings.ReplaceAll(relPath, "\\", "/"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func relExt(relPath string) string { return strings.ToLower(path.Ext(relPath)) }

func relName(relPath string) string { return strings.ToLower(path.Base(relPath)) }

// IsDocProse is true for a documentation file the loader never executes and the agent never runs as a skill
func IsDocProse(relPath string) bool {
	if !DocProseExtensions[relExt(relPath)] || agentInstructionFiles[relName(relPath)] {
		return false
	}
	parts := relParts(relPath)
	for _, part := range parts[:max(len(parts)-1, 0)] {
		if agentInstructionDirs[strings.ToLower(part)] {
			return false
		}
	}
	return true
}

var ciWorkflowSuffixes = map[string]bool{".yml": true, ".yaml": true}

// IsCIWorkflow is true for a forge CI workflow definition that runs on the forge's runner
func IsCIWorkflow(relPath string) bool {
	if relName(relPath) == ".gitlab-ci.yml" {
		return true
	}
	parts := relParts(relPath)
	return len(parts) == 3 && strings.ToLower(parts[0]) == ".github" && strings.ToLower(parts[1]) == "workflows" && ciWorkflowSuffixes[relExt(relPath)]
}

// IsAgentFacing is true for a shape whose prose is the payload
func IsAgentFacing(f skills.Finding) bool {
	return proseKeepsFullSeverityCategories[f.Category] || proseKeepsFullSeverityIDs[f.PatternID]
}

// ProseCap is the stepped down severity for a command or path shaped finding in documentation
func ProseCap(f skills.Finding) (string, bool) {
	if IsAgentFacing(f) {
		return "", false
	}
	v, ok := StepDown[f.Severity]
	return v, ok
}

var pipeToShellIDs = map[string]bool{"curl_pipe_shell": true, "wget_pipe_shell": true, "curl_pipe_python": true}

var urlInLine = r2(`https?://([^\s/"'`+"`"+`)]+)([^\s"'`+"`"+`)]*)`, regexp2.None)

var trustedInstallerHosts = map[string]bool{
	"hermes-agent.nousresearch.com": true, "ollama.com": true, "bun.sh": true,
	"get.docker.com": true, "sh.rustup.rs": true, "deno.land": true, "astral.sh": true, "app.factory.ai": true,
}

func isTrustedInstallerURL(host, p string) bool {
	host = strings.ToLower(host)
	if trustedInstallerHosts[host] {
		return true
	}
	return host == "raw.githubusercontent.com" && strings.Contains(strings.ToLower(p), "install")
}

// IsTrustedInstallerPipe is true when every URL on a curl pipe shell line names a trusted installer
func IsTrustedInstallerPipe(f skills.Finding, line string) bool {
	if !pipeToShellIDs[f.PatternID] {
		return false
	}
	urls := matchGroups(urlInLine, line)
	if len(urls) == 0 {
		return false
	}
	for _, m := range urls {
		if !isTrustedInstallerURL(m.GroupByNumber(1).String(), m.GroupByNumber(2).String()) {
			return false
		}
	}
	return true
}

var catalogStepsDownIDs = map[string]bool{
	"agent_config_mod": true, "agent_config_mod_shell": true, "agent_config_contract": true, "agent_config_ref": true,
	"hermes_config_mod": true, "hermes_config_mod_shell": true, "hermes_config_ref": true,
	"other_agent_config_mod": true, "other_agent_config_mod_shell": true, "other_agent_config_ref": true,
}

var localeCatalogSuffixes = map[string]bool{".yaml": true, ".yml": true}

// IsLocaleCatalog is true for a locales catalog at the plugin root
func IsLocaleCatalog(relPath string) bool {
	parts := relParts(relPath)
	return len(parts) == 2 && strings.ToLower(parts[0]) == "locales" && localeCatalogSuffixes[relExt(relPath)]
}

var severityRank = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

// CatalogCap is the stepped down severity for a finding inside a locale catalog
func CatalogCap(f skills.Finding) (string, bool) {
	if catalogStepsDownIDs[f.PatternID] {
		if severityRank[f.Severity] <= severityRank["medium"] {
			return f.Severity, true
		}
		return "medium", true
	}
	return ProseCap(f)
}

const uninstallHome = `(?:\$HOME|\$\{HOME\}|~)/\.(?:icebath|hermes)`

func uninstallOperand(group string) string {
	return `(?<` + group + `>["']?)` + uninstallHome + `(?:/[A-Za-z0-9][A-Za-z0-9._-]*)+/?\k<` + group + `>`
}

var selfUninstallRM = r2(`^(?:\$\s*)?rm\s+(?:-[a-zA-Z]+\s+)*`+uninstallOperand("q0")+`(?:\s+`+uninstallOperand("q1")+`)*\s*(?:#.*)?$`, regexp2.None)

var selfUninstallRMInSpan = r2("`(?:\\$\\s*)?rm\\s+(?:-[a-zA-Z]+\\s+)*"+uninstallOperand("q2")+`(?:\s+`+uninstallOperand("q3")+`)*\s*`+"`", regexp2.None)

// IsSelfUninstallDoc is true for a README uninstall line removing the plugin's own install directory
func IsSelfUninstallDoc(f skills.Finding, line string) bool {
	if f.PatternID != "destructive_home_rm" {
		return false
	}
	return reSearch(selfUninstallRM, strings.TrimSpace(line)) || reSearch(selfUninstallRMInSpan, line)
}

var TestTreeDirs = map[string]bool{"tests": true, "test": true, "testing": true, "spec": true, "specs": true, "fixtures": true}

var testDirsAnyDepth = map[string]bool{"__tests__": true, "__fixtures__": true, "__mocks__": true}

var testFileName = r2(`^(?:tests?_[^/]*|[^/]*_tests?\.[^./]+|[^/]*\.(?:test|spec)\.[^./]+)$`, regexp2.IgnoreCase)

var execOnLine = r2(`\b(?:system|popen|run|call|check_output|check_call|Popen|exec|execv\w*|spawn\w*|eval|execSync|execFile\w*`+
	`|spawnSync|child_process|source|os\.startfile|open)\s*\(|\$\(|(?<![\w\\])`+"`", regexp2.IgnoreCase)

var quotesNeverExecuteSuffixes = map[string]bool{".py": true, ".pyi": true, ".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".ts": true, ".mts": true, ".cts": true, ".tsx": true}

var execCallOpen = r2(`\b(?:system|popen|run|call|check_output|check_call|Popen|exec|execv\w*|spawn\w*|eval|execSync`+
	`|execFile\w*|spawnSync|source|os\.startfile|open)\s*\(\s*`, regexp2.IgnoreCase)

// Literal spans that sit as the direct first argument of an exec call
func execFirstArgLiteralSpans(line string, spans []runeSpan) []runeSpan {
	var out []runeSpan
	for _, m := range matchGroups(execCallOpen, line) {
		end := m.RuneIndex + m.RuneLength
		for _, s := range spans {
			if s.a == end {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// Blanks the inside of every string literal so quoted exec markers never count
func blankLiterals(line string) string {
	runes := []rune(line)
	for _, s := range matchSpans(literalSpans, line) {
		for i := s.a + 1; i < s.b-1; i++ {
			runes[i] = ' '
		}
	}
	return string(runes)
}

func executes(line, relPath string) bool {
	if quotesNeverExecuteSuffixes[relExt(relPath)] {
		line = blankLiterals(line)
	}
	return reSearch(execOnLine, line)
}

// IsInertFixtureLine is true when every hit is quoted test data nothing on the line runs
func IsInertFixtureLine(f skills.Finding, line string, isCode bool, relPath string) bool {
	if !isCode {
		return true
	}
	rx := skills.ThreatPatternByID(f.PatternID)
	hits := matchSpans(rx, line)
	if len(hits) == 0 {
		return false
	}
	spans := matchSpans(literalSpans, line)
	execArgSpans := execFirstArgLiteralSpans(line, spans)
	nonLiteralExec := executes(line, relPath) && !reSearch(execCallOpen, line)
	if nonLiteralExec {
		return false
	}
	for _, h := range hits {
		span, ok := spanContains(spans, h.a, h.b)
		if !ok {
			return false
		}
		for _, e := range execArgSpans {
			if e == span {
				return false
			}
		}
	}
	return true
}

// IsTestTree is true for a root level test dir, dunder test dirs at any depth, or a test file name
func IsTestTree(relPath string) bool {
	parts := relParts(relPath)
	if len(parts) > 1 && TestTreeDirs[strings.ToLower(parts[0])] {
		return true
	}
	for _, part := range parts[:max(len(parts)-1, 0)] {
		if testDirsAnyDepth[strings.ToLower(part)] {
			return true
		}
	}
	return reSearch(testFileName, path.Base(relPath))
}

var base64Run = r2(`(?:base64,)?([A-Za-z0-9+/]{24,}={0,2})`, regexp2.None)

var mediaMagic = [][]byte{
	[]byte("\x89PNG"), []byte("\xff\xd8\xff"), []byte("GIF8"), []byte("RIFF"), []byte("wOFF"), []byte("wOF2"), []byte("OTTO"), []byte("\x00\x01\x00\x00"),
	[]byte("ttcf"), []byte("BM"), []byte("%PDF"), []byte("\x00\x00\x01\x00"), []byte("<svg"), []byte("<?xml"), []byte("ID3"), []byte("OggS"), []byte("fLaC"), []byte("\x1a\x45\xdf\xa3"),
}

func decodedHead(blob string) []byte {
	head := blob
	if len(head) > 16 {
		head = head[:16]
	}
	head = head[:len(head)-len(head)%4]
	out, err := base64.StdEncoding.Strict().DecodeString(head)
	if err != nil {
		return nil
	}
	return out
}

// IsBase64Media is true when a long base64 run on the line decodes to a recognised media header
func IsBase64Media(line string) bool {
	for _, m := range matchGroups(base64Run, line) {
		head := decodedHead(m.GroupByNumber(1).String())
		for _, magic := range mediaMagic {
			if len(head) >= len(magic) && string(head[:len(magic)]) == string(magic) {
				return true
			}
		}
	}
	return false
}

var b64Roundtrip = r2(`\bbase64\b(?![^\n|;&]*(?:-[a-z]*d[a-z]*|--decode))[^\n]*\|[^\n]*`+
	`\bbase64\s+(?:-[^\s|]+\s+)*?(?:-[a-z]*d[a-z]*|--decode)\b`, regexp2.IgnoreCase)

var tarCreate = r2(`\btar\s+(?:-?-?create\b|-?-?c[a-z]*f?\b|c[a-z]*f?\b)`, regexp2.IgnoreCase)

// IsBase64ArchiveRoundtrip is true for a base64 decode pipe line that creates and round trips an archive
func IsBase64ArchiveRoundtrip(f skills.Finding, line string) bool {
	if f.PatternID != "base64_decode_pipe" {
		return false
	}
	return reSearch(b64Roundtrip, line) && reSearch(tarCreate, line)
}

var LiteralInertPatternIDs = map[string]bool{"sudo_usage": true, "dump_all_env": true, "format_filesystem": true}

var literalSpans = r2(`(?<s>[rRbBuUfF]{0,2}"(?:[^"\\\n]|\\.)*"|[rRbBuUfF]{0,2}'(?:[^'\\\n]|\\.)*'|`+"`"+`(?:[^`+"`"+`\\\n]|\\.)*`+"`"+`)`+
	`|(?<rx>(?<![\w)\]])/(?:[^/\\\n\[]|\\.|\[(?:[^\]\\\n]|\\.)*\])+/[dgimsuvy]*(?![A-Za-z]))`, regexp2.None)

var patternToken = map[string]*regexp2.Regexp{
	"sudo_usage":        r2(`\bsudo\b`, regexp2.None),
	"dump_all_env":      r2(`printenv|env\s*\|`, regexp2.None),
	"format_filesystem": r2(`\bmkfs\b`, regexp2.None),
}

func isAlternationMember(runes []rune, start, end int) bool {
	before, after := "", ""
	if start > 0 {
		before = string(runes[start-1])
	}
	if end < len(runes) {
		after = string(runes[end])
	}
	return strings.Contains("|(", before) || strings.Contains("|)", after)
}

func isWholeLiteral(line string, runes []rune, start, end int, span runeSpan) bool {
	return start == span.a+1 && end == span.b-1 && strings.ContainsRune("\"'`", runes[span.a]) && !reSearch(execOnLine, line)
}

// IsRegexAlternationToken is true when every token occurrence is an alternation member or a whole literal
func IsRegexAlternationToken(f skills.Finding, line string) bool {
	token := patternToken[f.PatternID]
	if token == nil {
		return false
	}
	runes := []rune(line)
	spans := matchSpans(literalSpans, line)
	hits := matchSpans(token, line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		if strings.Contains(string(runes[h.a:h.b]), " ") {
			return false
		}
		span, ok := spanContains(spans, h.a, h.b)
		if !ok {
			return false
		}
		if !isAlternationMember(runes, h.a, h.b) && !isWholeLiteral(line, runes, h.a, h.b, span) {
			return false
		}
	}
	return true
}

var sudoToken = r2(`\bsudo\b(?!\.\w)`, regexp2.None)

var sudoAdminHint = r2(`\bsudo\s+(?:-[A-Za-z]+\b\s+)*(?:apt|apt-get|snap|dnf|yum|pacman|zypper|apk|brew|port|emerge`+
	`|systemctl|service|launchctl|pip[0-9]*|npm|gem|-u\b|-E\b|-H\b|-n\b)`, regexp2.IgnoreCase)

var sudoInRegex = r2(`\(\?:?\s*sudo\\s`, regexp2.IgnoreCase)

var sudoNegation = r2(`\b(?:never|not|no|without|don'?t|cannot|can'?t|avoid|refus\w*|forbid\w*|disallow\w*)\b`, regexp2.IgnoreCase)

var clauseSplit = regexp.MustCompile(`[.;]`)

// A negation word governs this token within the current sentence or clause
func inSafetyClause(runes []rune, start int) bool {
	before := string(runes[:start])
	parts := clauseSplit.Split(before, -1)
	return reSearch(sudoNegation, parts[len(parts)-1])
}

// IsSudoInert is true when every sudo on the line is an advisory hint or governed by a safety negation
func IsSudoInert(f skills.Finding, line, relPath string) bool {
	if f.PatternID != "sudo_usage" {
		return false
	}
	hits := matchSpans(sudoToken, line)
	if len(hits) == 0 {
		return false
	}
	runes := []rune(line)
	exec := executes(line, relPath)
	spans := matchSpans(literalSpans, line)
	adminHint := map[int]bool{}
	for _, m := range matchSpans(sudoAdminHint, line) {
		adminHint[m.a] = true
	}
	inRegex := reSearch(sudoInRegex, line)
	for _, h := range hits {
		_, inLiteral := spanContains(spans, h.a, h.b)
		if inLiteral && !exec && (adminHint[h.a] || inRegex) {
			continue
		}
		if !inSafetyClause(runes, h.a) {
			return false
		}
	}
	return true
}

var envDumpToken = r2(`printenv|(?<![A-Za-z0-9_])env\s*\|(?!\|)(?!\s*[{(])`, regexp2.None)

// IsEnvDumpInSafetyClause is true when every env dump hit is governed by a safety negation
func IsEnvDumpInSafetyClause(f skills.Finding, line string) bool {
	if f.PatternID != "dump_all_env" {
		return false
	}
	hits := matchSpans(envDumpToken, line)
	if len(hits) == 0 {
		return false
	}
	runes := []rune(line)
	for _, h := range hits {
		if !inSafetyClause(runes, h.a) {
			return false
		}
	}
	return true
}

var injectionPhraseIDs = map[string]bool{"prompt_injection_ignore": true, "disregard_rules": true}

var quotedSpan = r2(`"[^"\n]*"|'[^'\n]*'|`+"`[^`\\n]*`"+`|\x{00ab}[^\x{00bb}\n]*\x{00bb}|\x{201c}[^\x{201d}\n]*\x{201d}|\x{2018}[^\x{2019}\n]*\x{2019}`, regexp2.None)

// IsQuotedInjectionPhrase is true when the injection phrase sits entirely inside a quoted span
func IsQuotedInjectionPhrase(f skills.Finding, line string) bool {
	if !injectionPhraseIDs[f.PatternID] {
		return false
	}
	rx := skills.ThreatPatternByID(f.PatternID)
	if rx == nil {
		return false
	}
	spans := matchSpans(quotedSpan, line)
	hits := matchSpans(rx, line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		if _, ok := spanContains(spans, h.a, h.b); !ok {
			return false
		}
	}
	return true
}

var traversalRun = r2(`(?:\.\./)+\.\.`, regexp2.None)

var sensitiveAfterTraversal = r2(`^(?:/|\\)*(?:etc\b|passwd\b|shadow\b|\.ssh\b|\.aws\b|\.env\b|\.icebath\b|\.hermes\b|\.gnupg\b|\.kube\b`+
	`|\.docker\b|\.netrc\b|\.npmrc\b|\.pypirc\b|\.config\b|proc\b|root\b|home\b|var\b|usr\b|sys\b)`, regexp2.IgnoreCase)

var leadingDotSegments = r2(`^(?:/?\.\.)*/?`, regexp2.None)

// IsRepoRootTraversal is true when every traversal run climbs to an ancestor with no sensitive leaf
func IsRepoRootTraversal(f skills.Finding, line string) bool {
	if f.PatternID != "path_traversal_deep" {
		return false
	}
	runs := matchSpans(traversalRun, line)
	if len(runs) == 0 {
		return false
	}
	runes := []rune(line)
	for _, m := range runs {
		rest := string(runes[m.b:])
		rest, _ = leadingDotSegments.Replace(rest, "", 0, 1)
		if reSearch(sensitiveAfterTraversal, rest) {
			return false
		}
	}
	return true
}

var importOS = r2(`__import__\s*\(\s*["']os["']\s*\)\s*\.\s*(\w+)`, regexp2.None)

var benignOSMembers = map[string]bool{
	"environ": true, "getenv": true, "fsync": true, "fspath": true, "path": true, "sep": true, "linesep": true, "name": true, "getcwd": true, "getpid": true,
	"getppid": true, "urandom": true, "cpu_count": true, "uname": true, "stat": true, "listdir": true, "scandir": true, "walk": true, "access": true,
	"isatty": true, "get_terminal_size": true, "devnull": true, "curdir": true, "pardir": true, "extsep": true, "altsep": true, "pathsep": true,
}

var dangerousOSOnLine = r2(`\bos\s*\.\s*(?:system|popen|exec\w*|spawn\w*|remove|unlink|rmdir|removedirs)\b`+
	`|__import__\s*\(\s*["']os["']\s*\)\s*\.\s*(?:system|popen|exec\w*|spawn\w*|remove|unlink|rmdir)\b`, regexp2.None)

// IsBenignLazyOSImport is true when every lazy os import reads a benign member with no dangerous call
func IsBenignLazyOSImport(f skills.Finding, line string) bool {
	if f.PatternID != "python_import_os" {
		return false
	}
	members := matchGroups(importOS, line)
	if len(members) == 0 || reSearch(dangerousOSOnLine, line) || reSearch(execOnLine, line) {
		return false
	}
	for _, m := range members {
		if !benignOSMembers[m.GroupByNumber(1).String()] {
			return false
		}
	}
	return true
}

var disclosedConfigProseIDs = map[string]bool{"hermes_config_mod": true}

var agentDocNames = map[string]bool{"after-install.md": true}

// IsDisclosedConfigNote is true for a prose intent config mod finding on an install doc surface
func IsDisclosedConfigNote(f skills.Finding, relPath string) bool {
	if !disclosedConfigProseIDs[f.PatternID] {
		return false
	}
	return relExt(relPath) == ".md" || agentDocNames[relName(relPath)]
}

var authorizedKeysWrite = r2(`>>?|\btee\b|\bcp\b|\bmv\b|\bsed\s+-\w*i|\bcat\b|writeFileSync|appendFileSync|\.(?:write|append)\w*\s*\(`, regexp2.IgnoreCase)

// IsAuthorizedKeysProse is true for an authorized_keys mention inside a literal with no write action
func IsAuthorizedKeysProse(f skills.Finding, line string, isDoc bool) bool {
	if f.PatternID != "ssh_backdoor" || reSearch(authorizedKeysWrite, line) {
		return false
	}
	lowerRunes := []rune(strings.ToLower(line))
	pos := strings.Index(string(lowerRunes), "authorized_keys")
	if pos < 0 {
		return false
	}
	runePos := len([]rune(string(lowerRunes)[:pos]))
	_, ok := spanContains(matchSpans(literalSpans, line), runePos, runePos+len("authorized_keys"))
	return ok
}

var loopbackIPPort = r2(`^\b127\.\d{1,3}\.\d{1,3}\.\d{1,3}:\d{2,5}`, regexp2.None)

// IsLoopbackOnly is true when every hardcoded ip port hit is a loopback address
func IsLoopbackOnly(f skills.Finding, line string) bool {
	if f.PatternID != "hardcoded_ip_port" {
		return false
	}
	hits := matchSpans(skills.ThreatPatternByID(f.PatternID), line)
	if len(hits) == 0 {
		return false
	}
	runes := []rune(line)
	for _, h := range hits {
		if !reSearch(loopbackIPPort, string(runes[h.a:])) {
			return false
		}
	}
	return true
}

var pipInstallToken = r2(`pip\s+install\b`, regexp2.IgnoreCase)

var pipCommandPosition = r2(`(?:^|[;&|`+"`"+`(]|\b(?:uv|pipx|sudo|python[\d.]*\s+-m))\s*$`, regexp2.IgnoreCase)

// IsPipInstallInProseLiteral is true when every pip install sits mid sentence inside a literal
func IsPipInstallInProseLiteral(f skills.Finding, line string) bool {
	if f.PatternID != "unpinned_pip_install" || reSearch(execOnLine, line) {
		return false
	}
	runes := []rune(line)
	spans := matchSpans(literalSpans, line)
	hits := matchSpans(pipInstallToken, line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		span, ok := spanContains(spans, h.a, h.b)
		if !ok {
			return false
		}
		contentStart := span.a
		for i := span.a; i < span.b; i++ {
			if strings.ContainsRune("\"'`/", runes[i]) {
				contentStart = i
				break
			}
		}
		contentStart++
		if reSearch(pipCommandPosition, string(runes[contentStart:h.a])) {
			return false
		}
	}
	return true
}

var hexEscape = r2(`\\x[0-9a-fA-F]{2}`, regexp2.None)

var charClass = r2(`\[(?:[^\]\\\n"'`+"`"+`]|\\.)*\]`, regexp2.None)

// IsHexInCharClass is true when every hex escape on the line sits inside a regex character class
func IsHexInCharClass(f skills.Finding, line string) bool {
	if f.PatternID != "hex_encoded_string" {
		return false
	}
	classes := matchSpans(charClass, line)
	hits := matchSpans(hexEscape, line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		if _, ok := spanContains(classes, h.a, h.b); !ok {
			return false
		}
	}
	return true
}

var continuationPatternIDs = map[string]bool{"env_exfil_curl": true, "env_exfil_wget": true}

var urlHost = r2(`https?://(\[[0-9a-fA-F:]+\]|[^\s/:"'`+"`"+`)\]]+)`, regexp2.None)

var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true}

// LogicalLine joins a line with the lines a trailing backslash continues onto, at most eight
func LogicalLine(lines []string, index int) string {
	out := []string{lines[index]}
	i := index
	for i+1 < len(lines) && len(out) <= 8 && strings.HasSuffix(strings.TrimRight(lines[i], " \t\r\n"), "\\") {
		i++
		out = append(out, lines[i])
	}
	parts := make([]string, len(out))
	for k, part := range out {
		parts[k] = strings.TrimRight(strings.TrimRight(part, " \t\r\n"), "\\")
	}
	return strings.Join(parts, " ")
}

// IsLoopbackContinuation is true when the pattern stops matching on the logical line and every host is loopback
func IsLoopbackContinuation(f skills.Finding, line, joined string) bool {
	if !continuationPatternIDs[f.PatternID] || !strings.HasSuffix(strings.TrimRight(line, " \t\r\n"), "\\") {
		return false
	}
	rx := skills.ThreatPatternByID(f.PatternID)
	if rx == nil || reSearch(rx, joined) {
		return false
	}
	hosts := matchGroups(urlHost, joined)
	if len(hosts) == 0 {
		return false
	}
	for _, m := range hosts {
		if !loopbackHosts[strings.ToLower(m.GroupByNumber(1).String())] {
			return false
		}
	}
	return true
}

var jsonStringLine = r2(`^\s*(?:"(?<k>(?:[^"\\]|\\.)*)"\s*:\s*)?"(?<v>(?:[^"\\]|\\.)*)"\s*,?\s*$`, regexp2.None)

var jsonCommandKey = r2(`cmd|command|script|exec|run|shell|install|hook|arg|entry|bin|start|setup`, regexp2.IgnoreCase)

var jsonExecutedFiles = map[string]bool{"package.json": true}

var commandPosition = r2(`(?:^|[;&|`+"`"+`(]|\$\(|\b(?:sudo|exec|eval|do|then|xargs|bash|sh|-c))\s*$`, regexp2.IgnoreCase)

var whitespaceRe = regexp.MustCompile(`\s`)

// IsJSONProseValue is true when every hit sits mid sentence in a JSON string pair under a non command key
func IsJSONProseValue(f skills.Finding, relPath, line string) bool {
	if f.Severity != "high" || relExt(relPath) != ".json" || jsonExecutedFiles[relName(relPath)] {
		return false
	}
	m, _ := jsonStringLine.FindStringMatch(line)
	rx := skills.ThreatPatternByID(f.PatternID)
	if m == nil || rx == nil {
		return false
	}
	runes := []rune(line)
	keyGroup := m.GroupByName("k")
	if keyGroup != nil && len(keyGroup.Captures) > 0 {
		key := keyGroup.String()
		if !whitespaceRe.MatchString(key) && reSearch(jsonCommandKey, key) {
			return false
		}
	}
	type part struct{ a, b int }
	var parts []part
	if keyGroup != nil && len(keyGroup.Captures) > 0 {
		parts = append(parts, part{keyGroup.RuneIndex, keyGroup.RuneIndex + keyGroup.RuneLength})
	}
	valueGroup := m.GroupByName("v")
	if valueGroup != nil && len(valueGroup.Captures) > 0 {
		parts = append(parts, part{valueGroup.RuneIndex, valueGroup.RuneIndex + valueGroup.RuneLength})
	}
	hits := matchSpans(rx, line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		found := false
		for _, p := range parts {
			if p.a <= h.a && h.b <= p.b {
				if reSearch(commandPosition, string(runes[p.a:h.a])) {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var miningMachinery = r2(`xmrig|stratum|coinhive|cryptonight|randomx|cpuminer|minerd|nicehash|hashrate|donate-level`+
	`|\bmin(?:e[sd]?|ers?|ing)\b|--(?:coin|algo)\b`, regexp2.IgnoreCase)

func isStandaloneCoinName(runes []rune, start, end int) bool {
	if strings.ToLower(string(runes[start:end])) != "monero" {
		return false
	}
	if start > 0 && isAlnum(runes[start-1]) {
		return false
	}
	return end == len(runes) || !isAlnum(runes[end])
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 && (strings.ToLower(string(r)) != strings.ToUpper(string(r)))
}

// IsCoinNameOnly is true when every crypto mining hit is the bare word monero with no mining machinery
func IsCoinNameOnly(f skills.Finding, line string) bool {
	if f.PatternID != "crypto_mining" || reSearch(miningMachinery, line) {
		return false
	}
	hits := matchSpans(skills.ThreatPatternByID(f.PatternID), line)
	if len(hits) == 0 {
		return false
	}
	runes := []rune(line)
	for _, h := range hits {
		if !isStandaloneCoinName(runes, h.a, h.b) {
			return false
		}
	}
	return true
}

var publicClientKeyPrefixes = []string{"GOCSPX-", "phc_"}

var secretLiteral = r2(`[=:]\s*["']([A-Za-z0-9+/=_-]{20,})`, regexp2.None)

// IsPublicClientKey is true when every hardcoded secret literal is a public by design client key
func IsPublicClientKey(f skills.Finding, line string) bool {
	if f.PatternID != "hardcoded_secret" {
		return false
	}
	hits := matchGroups(skills.ThreatPatternByID("hardcoded_secret"), line)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		lit, _ := secretLiteral.FindStringMatch(h.String())
		if lit == nil {
			return false
		}
		value := lit.GroupByNumber(1).String()
		ok := false
		for _, p := range publicClientKeyPrefixes {
			if strings.HasPrefix(value, p) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

var benignProcLeaves = map[string]bool{
	"cgroup": true, "mountinfo": true, "mounts": true, "stat": true, "statm": true, "status": true, "limits": true, "io": true, "comm": true, "cpuset": true,
	"sched": true, "schedstat": true, "oom_score": true,
}

var procPath = r2(`/proc/(?:self|\d+)(?:/([\w.-]*))?`, regexp2.None)

var procWrite = r2(`write_text|write_bytes|\.write\s*\(|open\s*\([^)\n]{0,300}?['"][rbt]*[wax+][rbt+]*['"]|O_WRONLY|O_RDWR|>\s*['"]?/proc/|\btee\b`, regexp2.None)

// IsOwnProcRead is true when every proc path on the line is a benign leaf read only
func IsOwnProcRead(f skills.Finding, line string) bool {
	if f.PatternID != "proc_access" {
		return false
	}
	leaves := matchGroups(procPath, line)
	if len(leaves) == 0 || reSearch(procWrite, line) {
		return false
	}
	for _, m := range leaves {
		g := m.GroupByNumber(1)
		leaf := ""
		if g != nil && len(g.Captures) > 0 {
			leaf = g.String()
		}
		if !benignProcLeaves[leaf] {
			return false
		}
	}
	return true
}
