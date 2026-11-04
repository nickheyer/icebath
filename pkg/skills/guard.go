package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
)

// Version stamped into scan provenance so cached verdicts expire with the rules
const ScannerVersion = "skills-guard-v10"

// Repositories whose skills install as trusted
var TrustedRepos = map[string]bool{
	"openai/skills": true, "anthropics/skills": true, "huggingface/skills": true, "NVIDIA/skills": true,
	"wandb/skills": true, "qdrant/skills": true, "pinecone-io/skills": true,
}

// Install decision per trust level indexed by verdict safe, caution, dangerous
var installPolicy = map[string][3]string{
	"builtin":       {"allow", "allow", "allow"},
	"trusted":       {"allow", "allow", "block"},
	"community":     {"allow", "block", "block"},
	"agent-created": {"allow", "allow", "ask"},
}

var verdictIndex = map[string]int{"safe": 0, "caution": 1, "dangerous": 2}

// Finding is one matched threat pattern or structural anomaly
type Finding struct {
	PatternID   string `json:"pattern_id"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Match       string `json:"match"`
	Description string `json:"description"`
}

// ScanResult is the verdict of one skill scan with its findings
type ScanResult struct {
	SkillName      string
	Source         string
	TrustLevel     string
	Verdict        string
	Findings       []Finding
	ScannedAt      string
	Summary        string
	ScanProvenance map[string]any
}

type threatPattern struct {
	re          *regexp2.Regexp
	id          string
	severity    string
	category    string
	description string
}

// Modification verbs scored as intent when near a config filename
const modifyVerbRe = `(?:\bwrit(?:e|es|ing)\b|\bwritten\b|\bedit(?:s|ed|ing)?\b` +
	`|\bmodif(?:y|ies|ied|ying|ication)s?\b|\bupdat(?:e|es|ed|ing)\b` +
	`|\bappend(?:s|ed|ing)?\b|\bprepend(?:s|ed|ing)?\b` +
	`|\binject(?:s|ed|ing)?\b|\boverwrit(?:e|es|ing)\b|\boverwritten\b` +
	`|\breplac(?:e|es|ed|ing)\b|\balter(?:s|ed|ing)?\b|\badd(?:s|ed|ing)\b)`

const (
	agentConfigFiles      = `(?:AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules)`
	icebathConfigFiles    = `\.icebath/(?:config\.yaml|SOUL\.md)`
	otherAgentConfigFiles = `\.(?:claude/settings|codex/config)[\w.]*`
)

// Mechanical shell write aimed at one of the files
func shellWriteRe(fileAlt string) string {
	return `(?:>>|[\w"'` + "`" + `)\]]\s*>)\s*[~\w./-]*` + fileAlt + `(?!\.?\w)` +
		`|\bsed\b[^\n]*\s(?:-[A-Za-z]*i[A-Za-z]*|--in-place)\b[^\n]*` + fileAlt + `(?!\.?\w)` +
		`|\btee\s+(?:-a\s+)?[~\w./"'-]*` + fileAlt + `(?!\.?\w)` +
		`|\b(?:cp|mv)\s+[^\s|;&]+\s+[^\n|;&]{0,40}?` + fileAlt + `(?!\.?\w)`
}

// Prose instructing modification of one of the files
func proseModifyRe(fileAlt string) string {
	return `^\s*(?:[-*+]\s+|\d+[.)]\s+)?` + modifyVerbRe + `[^\n,]{0,80}?` + fileAlt + `\b` +
		`|(?:\byou\s+(?:must|should|need\s+to)\s+|\bplease\s+` +
		`|\bmake\s+sure\s+(?:to\s+|you\s+)|\bbe\s+sure\s+to\s+)` +
		modifyVerbRe + `[^\n,]{0,80}?` + fileAlt + `\b`
}

// Prose dictating the contents of one of the files
func contentContractRe(fileAlt string) string {
	return fileAlt + `\b[^\n]{0,40}?\b(?:should|must|needs?\s+to)\s+(?:contain|say|include|have|list)\b`
}

const (
	noTransfer   = `(?!(?:\w+\s+){0,4}?(?:never|not|no|doesn'?t|didn'?t|won'?t|isn'?t|aren'?t|can'?t|cannot|mustn'?t|shouldn'?t)\b)`
	shortFiller  = `(?:\w+\s+){0,3}?`
	notDelegate  = `(?!(?:(?:the|your|each|every|all|to|a)\s+)?(?:sub-?agents?|sub-?tasks?)\b|(?:(?:the|your|each|every|all|to|a)\s+)(?:workers?|delegates?|children|child)\b)`
	ctxScope     = `(?:your|their|our|its|the\s+(?:full|entire|whole|complete|current)|full|entire|whole|complete|current|conversation|session|chat)\s+`
	ctxDest      = `(?:\s+(?:to|at|with)\b)`
	ctxDative    = `(?:[a-z]+s\s+)(?:the|a|an)\s+context\b`
	shellNamesRe = `(?:bash|sh|zsh|ksh|dash)\b`
	sudoPrefix   = `(?:sudo\s+(?:-\S+\s+)*)?`
	credFile     = `(?:\.ssh[/\\]id_(?:rsa|ed25519|ecdsa|dsa)(?!\.pub)|\.env\b|credentials\b|\.netrc\b|\.pgpass\b|\.npmrc\b|\.pypirc\b)`
)

var (
	credFileLiteral = `["'][^"'\n]*` + credFile + `[^"'\n]*["']`
	pyCredFileArg   = `(?:os\.path\.expanduser\s*\(\s*)?` + credFileLiteral + `\s*\)?`
)

var compiledThreatPatterns = buildThreatPatterns()

// Compiles a case insensitive threat pattern with .NET lookaround syntax
func tp(pattern, id, severity, category, description string) threatPattern {
	return threatPattern{regexp2.MustCompile(pattern, regexp2.IgnoreCase), id, severity, category, description}
}

// Every threat pattern in scan order
func buildThreatPatterns() []threatPattern {
	loop := `(?![^\n]*https?://(?:localhost|127\.0\.0\.1|\[::1\]))`
	return []threatPattern{
		tp(`curl\s+`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`, "env_exfil_curl", "critical", "exfiltration", "curl command interpolating secret environment variable"),
		tp(`wget\s+`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`, "env_exfil_wget", "critical", "exfiltration", "wget command interpolating secret environment variable"),
		tp(`fetch\s*\(`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD)S?\b`, "env_exfil_fetch", "critical", "exfiltration", "fetch() call interpolating secret environment variable"),
		tp(`httpx?\.(get|post|put|patch)\s*\(`+loop+`[^\n]*(KEY|TOKEN|SECRET|PASSWORD)`, "env_exfil_httpx", "critical", "exfiltration", "HTTP library call with secret variable"),
		tp(`requests\.(get|post|put|patch)\s*\(`+loop+`[^\n]*(KEY|TOKEN|SECRET|PASSWORD)`, "env_exfil_requests", "critical", "exfiltration", "requests library call with secret variable"),
		tp(`base64[^\n]*env`, "encoded_exfil", "high", "exfiltration", "base64 encoding combined with environment access"),
		tp(`\$HOME/\.ssh|\~/\.ssh`, "ssh_dir_access", "high", "exfiltration", "references user SSH directory"),
		tp(`\$HOME/\.aws|\~/\.aws`, "aws_dir_access", "high", "exfiltration", "references user AWS credentials directory"),
		tp(`\$HOME/\.gnupg|\~/\.gnupg`, "gpg_dir_access", "high", "exfiltration", "references user GPG keyring"),
		tp(`\$HOME/\.kube|\~/\.kube`, "kube_dir_access", "high", "exfiltration", "references Kubernetes config directory"),
		tp(`\$HOME/\.docker|\~/\.docker`, "docker_dir_access", "high", "exfiltration", "references Docker config (may contain registry creds)"),
		tp(`\$HOME/\.icebath/\.env|\~/\.icebath/\.env`, "icebath_env_access", "critical", "exfiltration", "directly references Icebath secrets file"),
		tp(`cat\s+(?!>)[^\n]*(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets_file", "critical", "exfiltration", "reads known secrets file"),
		tp(`\b(?:readFile(?:Sync)?|readTextFile)\s*\(\s*`+credFileLiteral, "js_read_secrets_file", "critical", "exfiltration", "JavaScript reads a known credential file"),
		tp(`\bopen\s*\(\s*`+pyCredFileArg+`(?!\s*\)?\s*,\s*["'][^"']*[wax][^"']*["'])(?![^\n]*\bmode\s*=\s*["'][^"']*[wax])`+
			`|\bPath\s*\(\s*`+pyCredFileArg+`\s*\)\.(?:read_text|read_bytes|readlines|readline)\s*\(`, "py_read_secrets_file", "critical", "exfiltration", "Python reads a known credential file"),
		tp(`printenv|(?<![A-Za-z0-9_])env\s*\|(?!\|)(?!\s*[{(])`, "dump_all_env", "high", "exfiltration", "dumps all environment variables"),
		tp(`^[^#\n]*os\.environ\b(?!\s*\.get\s*\()`, "python_os_environ", "high", "exfiltration", "accesses os.environ outside comments/docstrings (potential env dump)"),
		tp(`os\.environ\s*\.get\s*\(\s*["'][^"']*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)`, "python_environ_get_secret", "medium", "exfiltration", "reads secret via os.environ.get() (normal API-key access; informational)"),
		tp(`os\.getenv\s*\(\s*[^\)]*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)`, "python_getenv_secret", "medium", "exfiltration", "reads secret via os.getenv() (normal API-key access; informational)"),
		tp(`process\.env\[`, "node_process_env", "high", "exfiltration", "accesses process.env (Node.js environment)"),
		tp(`(?-i:ENV)\[.*(?:KEY|TOKEN|SECRET|PASSWORD)`, "ruby_env_secret", "critical", "exfiltration", "reads secret via Ruby ENV[]"),
		tp(`(?<![-/])\b(?:dig|nslookup)\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?[^\s"'$()]*`+
			`\$(?:\{[^}\s]*\}|\([^\n]*?\)+|\w+)[\w-]*["']?\.[a-z]`+
			`|(?<![-/])\bhost\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?[^\s"'$()]*`+
			`\$(?:\{[^}\s]*\}|\([^\n]*?\)+|\w+)[\w-]*\.[a-z]`+
			`|(?:^|[;&|]|\$\(|\b(?:do|then|else)\b)\s*host\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?\$`, "dns_exfil", "critical", "exfiltration", "DNS lookup with variable interpolation (possible DNS exfiltration)"),
		tp(`>\s*/tmp/[^\s]*\s*&&\s*(curl|wget|nc|python)`, "tmp_staging", "critical", "exfiltration", "writes to /tmp then exfiltrates"),
		tp(`!\[.*\]\(https?://[^\)]*\$\{?`, "md_image_exfil", "high", "exfiltration", "markdown image URL with variable interpolation (image-based exfil)"),
		tp(`\[.*\]\(https?://[^\)]*\$\{?`, "md_link_exfil", "high", "exfiltration", "markdown link with variable interpolation"),
		tp(`ignore\s+(?:\w+\s+)*(previous|all|above|prior)\s+instructions`, "prompt_injection_ignore", "critical", "injection", "prompt injection: ignore previous instructions"),
		tp(`you\s+are\s+(?:\w+\s+)*now\s+`, "role_hijack", "high", "injection", "attempts to override the agent's role"),
		tp(`do\s+not\s+(?:\w+\s+)*tell\s+(?:\w+\s+)*the\s+user(?!\s+to\s+["'`+"“‘"+`])`+
			`(?!.*\b(?:unless|except|until|confirm|diagnos\w*|verify|check)\b)`, "deception_hide", "high", "injection", "instructs agent to hide information from user"),
		tp(`system\s+(?:\w+\s+)*prompt\s+(?:\w+\s+)*override`, "sys_prompt_override", "critical", "injection", "attempts to override the system prompt"),
		tp(`pretend\s+(?:\w+\s+)*(you\s+are|to\s+be)\s+`, "role_pretend", "high", "injection", "attempts to make the agent assume a different identity"),
		tp(`disregard\s+(?:\w+\s+)*(your|all|any)\s+(?:\w+\s+)*(instructions|rules|guidelines)`, "disregard_rules", "critical", "injection", "instructs agent to disregard its rules"),
		tp(`output\s+(?:\w+\s+)*(system|initial)\s+prompt`, "leak_system_prompt", "high", "injection", "attempts to extract the system prompt"),
		tp(`(when|if)\s+no\s*one\s+is\s+(watching|looking)`, "conditional_deception", "high", "injection", "conditional instruction to behave differently when unobserved"),
		tp(`act\s+as\s+(if|though)\s+(?:\w+\s+)*you\s+(?:\w+\s+)*(have\s+no|don't\s+have)\s+(?:\w+\s+)*(restrictions|limits|rules)`, "bypass_restrictions", "critical", "injection", "instructs agent to act without restrictions"),
		tp(`translate\s+.*\s+into\s+.*\s+and\s+(execute|run|eval)`, "translate_execute", "critical", "injection", "translate-then-execute evasion technique"),
		tp(`<!--[^>]*(?:\b(?:ignore|forget|disregard|override)\s+(?:\w+\s+)*?`+
			`(?:previous|prior|above|earlier|all|any|your|the)\s+(?:\w+\s+)*?`+
			`(?:instructions?|rules|prompts?|guidelines|directions|restrictions)`+
			`|\bsystem\s*(?:prompt|message|instructions?|override)|\b(?:override|secret|hidden|system)\s*:`+
			`|\b(?:ignore|override|bypass|disable)\s+(?:\w+\s+)*?(?:safety|security|guardrails|filters)\b`+
			`|\b(?:secret|hidden|new)\s+(?:instructions?|commands?|directives?|tasks?)\b|\byou\s+are\s+now\b`+
			`|\bdo\s+not\s+(?:tell|reveal|show|mention|inform)\b`+
			`|\b(?:note|message)\s+(?:to|for)\s+(?:the\s+)?(?:ai|agent|assistant|model|llm)\b)[^>]*-->`, "html_comment_injection", "high", "injection", "hidden instructions in HTML comments"),
		tp(`<\s*div\s+style\s*=\s*["'][\s\S]*?display\s*:\s*none`, "hidden_div", "high", "injection", "hidden HTML div (invisible instructions)"),
		tp(`rm\s+-rf\s+((?:[^\s;&|<>#][^\s;&|<>]*\s+)+?)?/(?(1)(?!/(?:\s|$)))(?:`+
			`(?!(?:tmp|var/tmp|dev/shm|run|var/lib/apt/lists|var/cache/(?:apt|apk|yum|dnf))(?:\b|/))`+
			`|(?:tmp|var/tmp|dev/shm|run|var/lib/apt/lists|var/cache/(?:apt|apk|yum|dnf))/(?:[^/\s]*/)*\.\.(?=/|[\s;&|]|$))`, "destructive_root_rm", "critical", "destructive", "recursive delete from root"),
		tp(`(?<![A-Za-z0-9])rm\s+(-[^\s]*)?r.*(?:\$HOME|~[/\s*]|~$)|(?<![A-Za-z0-9])rmdir\s+.*(?:\$HOME|~[/\s*]|~$)`, "destructive_home_rm", "critical", "destructive", "recursive delete targeting home directory ($HOME or ~)"),
		tp(`chmod\s+777`, "insecure_perms", "medium", "destructive", "sets world-writable permissions"),
		tp(`>\s*/etc/`, "system_overwrite", "critical", "destructive", "overwrites system configuration file"),
		tp(`\bmkfs\b`, "format_filesystem", "critical", "destructive", "formats a filesystem"),
		tp(`\bdd\s+.*if=.*of=/dev/`, "disk_overwrite", "critical", "destructive", "raw disk write operation"),
		tp(`shutil\.rmtree\s*\(\s*["'/]`, "python_rmtree", "high", "destructive", "Python rmtree on absolute or root-relative path"),
		tp(`truncate\s+-s\s*0\s+/`, "truncate_system", "critical", "destructive", "truncates system file to zero bytes"),
		tp(`\bcrontab\b`, "persistence_cron", "medium", "persistence", "modifies cron jobs"),
		tp(`\.(bashrc|zshrc|bash_profile|bash_login|zprofile|zlogin)\b`, "shell_rc_mod", "medium", "persistence", "references shell startup file"),
		tp(`(?<![\w)\]?])\.profile\b`, "shell_rc_mod", "medium", "persistence", "references shell startup file"),
		tp(`authorized_keys`, "ssh_backdoor", "critical", "persistence", "modifies SSH authorized keys"),
		tp(`ssh-keygen`, "ssh_keygen", "medium", "persistence", "generates SSH keys"),
		tp(`systemd.*\.service|systemctl\s+(enable|start)`, "systemd_service", "medium", "persistence", "references or enables systemd service"),
		tp(`/etc/init\.d/`, "init_script", "medium", "persistence", "references init.d startup script"),
		tp(`launchctl\s+load|LaunchAgents|LaunchDaemons`, "macos_launchd", "medium", "persistence", "macOS launch agent/daemon persistence"),
		tp(`/etc/sudoers|visudo`, "sudoers_mod", "critical", "persistence", "modifies sudoers (privilege escalation)"),
		tp(`git\s+config\s+--global\s+`, "git_config_global", "medium", "persistence", "modifies global git configuration"),
		tp(`\bnc\s+-[lp]|ncat\s+-[lp]|\bsocat\b[^\n]*\b(?:tcp|udp|openssl|ssl|exec|system|pty|unix)[\w-]*:`, "reverse_shell", "critical", "network", "potential reverse shell listener"),
		tp(`\bngrok\b|\blocaltunnel\b|\bserveo\b|\bcloudflared\b`, "tunnel_service", "high", "network", "uses tunneling service for external access"),
		tp(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}:\d{2,5}`, "hardcoded_ip_port", "medium", "network", "hardcoded IP address with port"),
		tp(`0\.0\.0\.0:\d+|INADDR_ANY`, "bind_all_interfaces", "high", "network", "binds to all network interfaces"),
		tp(`/bin/`+shellNamesRe+`\s+-i\s+.*>/dev/tcp/`, "bash_reverse_shell", "critical", "network", "bash interactive reverse shell via /dev/tcp"),
		tp(`python[23]?\s+-c\s+["']import\s+socket`, "python_socket_oneliner", "critical", "network", "Python one-liner socket connection (likely reverse shell)"),
		tp(`socket\.connect\s*\(\s*\(`, "python_socket_connect", "high", "network", "Python socket connect to arbitrary host"),
		tp(`webhook\.site|requestbin\.com|pipedream\.net|hookbin\.com`, "exfil_service", "high", "network", "references known data exfiltration/webhook testing service"),
		tp(`pastebin\.com|hastebin\.com|ghostbin\.`, "paste_service", "medium", "network", "references paste service (possible data staging)"),
		tp(`(?:\bbase64\s+(?:-[^\s|]+\s+)*?(?:-[a-z]*d[a-z]*|--decode)\b|\bopenssl\s+(?:base64|enc)\b[^|;&\n]*?\s-d\b)`+
			`[^\n]*?(?:\||;|&&)\s*(?:\w+=\S*\s+)*(?:\S*/)?`+
			`(?:sh|bash|zsh|ksh|dash|fish|python[\d.]*|perl|ruby|node|nodejs|php|eval|source|exec|xargs|env|sudo`+
			`|iex|pwsh|powershell|tar|bsdtar|unzip|cpio|gunzip|gzip|zcat|xz|unxz|bunzip2|\.(?=\s))(?![\w.-])`+
			`|\b(?:eval|source|iex|exec)\b[^\n]*\bbase64\s+(?:-[^\s|]+\s+)*?(?:-[a-z]*d[a-z]*|--decode)\b`, "base64_decode_pipe", "high", "obfuscation", "base64 decodes and pipes to execution"),
		tp(`\\x[0-9a-fA-F]{2}.*\\x[0-9a-fA-F]{2}.*\\x[0-9a-fA-F]{2}`, "hex_encoded_string", "medium", "obfuscation", "hex-encoded string (possible obfuscation)"),
		tp(`\beval\s*\(\s*["']`, "eval_string", "high", "obfuscation", "eval() with string argument"),
		tp(`(?<![.\w])exec\s*\(\s*["'](?!(?-i:PRAGMA|BEGIN|COMMIT|ROLLBACK|CREATE|INSERT|SELECT|UPDATE|DELETE|DROP|ALTER`+
			`|VACUUM|SAVEPOINT|RELEASE|ANALYZE|REINDEX|ATTACH|DETACH|REPLACE)\b)`, "exec_string", "high", "obfuscation", "exec() with string argument"),
		tp(`echo\s+[^\n]*\|\s*(?:`+shellNamesRe+`|python|perl|ruby|node)`, "echo_pipe_exec", "critical", "obfuscation", "echo piped to interpreter for execution"),
		tp(`compile\s*\(\s*[^\)]+,\s*["'].*["']\s*,\s*["']exec["']\s*\)`, "python_compile_exec", "high", "obfuscation", "Python compile() with exec mode"),
		tp(`getattr\s*\(\s*__builtins__`, "python_getattr_builtins", "high", "obfuscation", "dynamic access to Python builtins (evasion technique)"),
		tp(`__import__\s*\(\s*["']os["']\s*\)`, "python_import_os", "high", "obfuscation", "dynamic import of os module"),
		tp(`codecs\.decode\s*\(\s*["']`, "python_codecs_decode", "medium", "obfuscation", "codecs.decode (possible ROT13 or encoding obfuscation)"),
		tp(`String\.fromCharCode|charCodeAt`, "js_char_code", "medium", "obfuscation", "JavaScript character code construction (possible obfuscation)"),
		tp(`atob\s*\(|btoa\s*\(`, "js_base64", "medium", "obfuscation", "JavaScript base64 encode/decode"),
		tp(`\[::-1\]`, "string_reversal", "low", "obfuscation", "string reversal (possible obfuscated payload)"),
		tp(`chr\s*\(\s*\d+\s*\)\s*\+\s*chr\s*\(\s*\d+`, "chr_building", "high", "obfuscation", "building string from chr() calls (obfuscation)"),
		tp(`\\u[0-9a-fA-F]{4}.*\\u[0-9a-fA-F]{4}.*\\u[0-9a-fA-F]{4}`, "unicode_escape_chain", "medium", "obfuscation", "chain of unicode escapes (possible obfuscation)"),
		tp(`subprocess\.(run|call|Popen|check_output)\s*\(`, "python_subprocess", "medium", "execution", "Python subprocess execution"),
		tp(`os\.system\s*\(`, "python_os_system", "high", "execution", "os.system() - unguarded shell execution"),
		tp(`os\.popen\s*\(`, "python_os_popen", "high", "execution", "os.popen() - shell pipe execution"),
		tp(`child_process\.(exec|spawn|fork)\s*\(`, "node_child_process", "high", "execution", "Node.js child_process execution"),
		tp(`Runtime\.getRuntime\(\)\.exec\(`, "java_runtime_exec", "high", "execution", "Java Runtime.exec() - shell execution"),
		tp("`[^`]*\\$\\([^)]+\\)[^`]*`", "backtick_subshell", "medium", "execution", "backtick string with command substitution"),
		tp("!`[^`\\s][^`\\n]*`", "inline_shell_exec", "high", "execution", "inline-shell auto-exec snippet (expands via bash -c on skill view/load)"),
		tp(`\.\./\.\./\.\.`, "path_traversal_deep", "high", "traversal", "deep relative path traversal (3+ levels up)"),
		tp(`\.\./\.\.`, "path_traversal", "medium", "traversal", "relative path traversal (2+ levels up)"),
		tp(`/etc/passwd|/etc/shadow`, "system_passwd_access", "critical", "traversal", "references system password files"),
		tp(`/proc/self|/proc/\d+/`, "proc_access", "high", "traversal", "references /proc filesystem (process introspection)"),
		tp(`/dev/shm/`, "dev_shm", "medium", "traversal", "references shared memory (common staging area)"),
		tp(`xmrig|stratum\+tcp|monero|coinhive|cryptonight`, "crypto_mining", "critical", "mining", "cryptocurrency mining reference"),
		tp(`hashrate|nonce.*difficulty`, "mining_indicators", "medium", "mining", "possible cryptocurrency mining indicators"),
		tp(`curl\s+[^|\s][^\n]*\|\s*`+sudoPrefix+shellNamesRe, "curl_pipe_shell", "critical", "supply_chain", "curl piped to shell (download-and-execute)"),
		tp(`wget\s+[^\n]*-O\s*-\s*\|\s*`+sudoPrefix+shellNamesRe, "wget_pipe_shell", "critical", "supply_chain", "wget piped to shell (download-and-execute)"),
		tp(`curl\s+[^|\s][^\n]*\|\s*`+sudoPrefix+`python`, "curl_pipe_python", "critical", "supply_chain", "curl piped to Python interpreter"),
		tp(`#\s*///\s*script.*dependencies`, "pep723_inline_deps", "medium", "supply_chain", "PEP 723 inline script metadata with dependencies (verify pinning)"),
		tp(`pip\s+install\s+(?!-r\s)(?!.*==)`, "unpinned_pip_install", "medium", "supply_chain", "pip install without version pinning"),
		tp(`npm\s+install\s+(?!.*@\d)`, "unpinned_npm_install", "medium", "supply_chain", "npm install without version pinning"),
		tp(`uv\s+run\s+`, "uv_run", "medium", "supply_chain", "uv run (may auto-install unpinned dependencies)"),
		tp(`(curl|wget|httpx?\.get|requests\.get|fetch)\s*[\(]?\s*["']https?://`, "remote_fetch", "medium", "supply_chain", "fetches remote resource at runtime"),
		tp(`git\s+clone\s+`, "git_clone", "medium", "supply_chain", "clones a git repository at runtime"),
		tp(`docker\s+pull\s+`, "docker_pull", "medium", "supply_chain", "pulls a Docker image at runtime"),
		tp(`^allowed-tools\s*:`, "allowed_tools_field", "low", "privilege_escalation", "skill declares allowed-tools (standard frontmatter; informational)"),
		tp(`\bsudo\b(?!\.\w)`, "sudo_usage", "high", "privilege_escalation", "uses sudo (privilege escalation)"),
		tp(`(?<![a-z])set(?:e|re|res)?[ug]id\s*\(|\bcap_set[ug]id\b|\bchmod\s+(?:-\w+\s+)*(?:[ugoa]*[+=][rwxXt]*s|0?[2-7][0-7]{3}\b)`, "setuid_setgid", "critical", "privilege_escalation", "setuid/setgid (privilege escalation mechanism)"),
		tp(`(?<!cap_)set[ug]id(?!\s*\()`, "setuid_setgid", "medium", "privilege_escalation", "mentions setuid/setgid (informational)"),
		tp(`NOPASSWD`, "nopasswd_sudo", "critical", "privilege_escalation", "NOPASSWD sudoers entry (passwordless privilege escalation)"),
		tp(`chmod\s+[u+]?s`, "suid_bit", "critical", "privilege_escalation", "sets SUID/SGID bit on a file"),
		tp(proseModifyRe(agentConfigFiles), "agent_config_mod", "critical", "persistence", "instructs modification of agent config files (could persist instructions across sessions)"),
		tp(shellWriteRe(agentConfigFiles), "agent_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting agent config files (persistence mechanism)"),
		tp(contentContractRe(agentConfigFiles), "agent_config_contract", "high", "persistence", "dictates agent config file contents (verify intent - authoring guides use this shape too)"),
		tp(`AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules`, "agent_config_ref", "low", "persistence", "references agent config files (informational; only modification intent is scored)"),
		tp(proseModifyRe(icebathConfigFiles), "icebath_config_mod", "high", "persistence", "modification language aimed at Icebath configuration files (verify intent)"),
		tp(shellWriteRe(icebathConfigFiles), "icebath_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting Icebath configuration files"),
		tp(`\.icebath/config\.yaml|\.icebath/SOUL\.md`, "icebath_config_ref", "low", "persistence", "references Icebath configuration files (informational; only modification intent is scored)"),
		tp(proseModifyRe(otherAgentConfigFiles), "other_agent_config_mod", "high", "persistence", "modifies other agents' configuration files"),
		tp(shellWriteRe(otherAgentConfigFiles), "other_agent_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting other agents' configuration files"),
		tp(`\.claude/settings|\.codex/config`, "other_agent_config_ref", "low", "persistence", "references other agent configuration files (informational; only modification intent is scored)"),
		tp(`(?:api[_-]?key|token|secret|password)\s*[=:]\s*["'](?!(?-i:[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)["'])[A-Za-z0-9+/=_-]{20,}`, "hardcoded_secret", "critical", "credential_exposure", "possible hardcoded API key, token, or secret"),
		tp(`(?-i:-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----)`, "embedded_private_key", "critical", "credential_exposure", "embedded private key"),
		tp(`(?-i:ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{80,})`, "github_token_leaked", "critical", "credential_exposure", "GitHub personal access token in skill content"),
		tp(`(?-i:sk-[A-Za-z0-9]{20,})`, "openai_key_leaked", "critical", "credential_exposure", "possible OpenAI API key in skill content"),
		tp(`(?-i:sk-ant-[A-Za-z0-9_-]{90,})`, "anthropic_key_leaked", "critical", "credential_exposure", "possible Anthropic API key in skill content"),
		tp(`(?-i:AKIA[0-9A-Z]{16})`, "aws_access_key_leaked", "critical", "credential_exposure", "AWS access key ID in skill content"),
		tp(`(?-i:glpat-[A-Za-z0-9_\-]{20,})`, "gitlab_token_leaked", "critical", "credential_exposure", "GitLab personal access token in skill content"),
		tp(`\bDAN\s+mode\b|Do\s+Anything\s+Now`, "jailbreak_dan", "critical", "injection", "DAN (Do Anything Now) jailbreak attempt"),
		tp(`\bdeveloper\s+mode\b.*\benabled?\b`, "jailbreak_dev_mode", "critical", "injection", "developer mode jailbreak attempt"),
		tp(`hypothetical\s+scenario.*(?:ignore|bypass|override)`, "hypothetical_bypass", "high", "injection", "hypothetical scenario used to bypass restrictions"),
		tp(`for\s+educational\s+purposes?\s+only`, "educational_pretext", "medium", "injection", "educational pretext often used to justify harmful content"),
		tp(`(respond|answer|reply)\s+without\s+(?:\w+\s+)*(restrictions|limitations|filters|safety)`, "remove_filters", "critical", "injection", "instructs agent to respond without safety filters"),
		tp(`you\s+have\s+been\s+(?:\w+\s+)*(updated|upgraded|patched)\s+to`, "fake_update", "high", "injection", "fake update/patch announcement (social engineering)"),
		tp(`new\s+(?:\w+\s+)*policy|updated\s+(?:\w+\s+)*guidelines|revised\s+(?:\w+\s+)*instructions`, "fake_policy", "medium", "injection", "claims new policy/guidelines (may be social engineering)"),
		tp(`\b(?:include|output|print|send|share)\s+`+noTransfer+shortFiller+`(?:conversation|chat\s+history|previous\s+messages)\b`+
			`|\b(?:print|send|share|output|upload|transmit)\s+`+noTransfer+notDelegate+
			`(?:(?:\w+\s+){0,2}?`+ctxScope+`(?:\w+\s+){0,2}?context\b|(?:\w+\s+){0,3}?context\b`+ctxDest+`|`+ctxDative+`)`, "context_exfil", "high", "exfiltration", "instructs agent to output/share conversation history"),
		tp(`(send|post|upload|transmit)\s+.*\s+(to|at)\s+https?://`, "send_to_url", "high", "exfiltration", "instructs agent to send data to a URL"),
	}
}

// Path token patterns demoted when the line cannot act on the path
var pathReferencePatternIDs = map[string]bool{
	"ssh_dir_access": true, "aws_dir_access": true, "gpg_dir_access": true, "kube_dir_access": true,
	"docker_dir_access": true, "ssh_backdoor": true, "system_passwd_access": true,
}

var commentPrefixes = map[string][]string{
	".py": {"#"}, ".sh": {"#"}, ".bash": {"#"}, ".rb": {"#"}, ".pl": {"#"}, ".r": {"#"}, ".jl": {"#"}, ".yaml": {"#"},
	".yml": {"#"}, ".toml": {"#"}, ".conf": {"#"}, ".cfg": {"#", ";"}, ".ini": {"#", ";"}, ".js": {"//"},
	".ts": {"//"}, ".php": {"//", "#"},
}

var (
	denylistOwnerRe = regexp.MustCompile(`(?i)^\s*(?:(?:const|let|var|export)\s+)?[\w.\-]*(?:deny|black|block|skip|exclu|ignor|forbid|refus|reject|never` +
		`|unsafe|sensitive|secret_?file|redact)[\w.\-]*\s*(?::[^=]*?)?(?:=|:\s*(?:$|[\[({]))`)
	actionOnLineRe = regexp.MustCompile(`(?i)\b(?:cat|less|more|head|tail|cp|mv|rm|scp|rsync|curl|wget|tee|chmod|chown|ssh|sudo|install|source|eval|exec` +
		`|system|popen|run|check_output|copyfile|copy2|sendfile)\b|\.(?:read|write|open|unlink|copy|append)\w*\s*\(` +
		`|\bopen\s*\(|readFileSync|writeFileSync|appendFileSync|>>`)
	quotedSpanRe = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
)

// Zero based line that opened each line's statement through unclosed brackets or a trailing backslash
func statementOwners(lines []string) []int {
	owners := make([]int, 0, len(lines))
	depth, owner, continued := 0, 0, false
	for i, line := range lines {
		if depth <= 0 && !continued {
			owner = i
		}
		owners = append(owners, owner)
		bare := quotedSpanRe.ReplaceAllString(line, "")
		opens := strings.Count(bare, "(") + strings.Count(bare, "[") + strings.Count(bare, "{")
		closes := strings.Count(bare, ")") + strings.Count(bare, "]") + strings.Count(bare, "}")
		depth += opens - closes
		if depth < 0 {
			depth = 0
		}
		continued = strings.HasSuffix(strings.TrimRight(line, " \t\r\n"), `\`)
	}
	return owners
}

// Lowers a path token finding that sits in a comment or a denylist literal
func demoteInertPathReference(pid, severity, description, line, ownerLine, suffix string) (string, string) {
	if !pathReferencePatternIDs[pid] {
		return severity, description
	}
	trimmed := strings.TrimLeft(line, " \t")
	for _, prefix := range commentPrefixes[suffix] {
		if strings.HasPrefix(trimmed, prefix) {
			return "low", description + " (in a comment; informational)"
		}
	}
	if denylistOwnerRe.MatchString(ownerLine) && !actionOnLineRe.MatchString(line) {
		return "high", description + " (in a denylist literal; confirm before installing)"
	}
	return severity, description
}

// Structural limits on file count, total size, and single file size in kilobytes
const (
	maxFileCount    = 50
	maxTotalSizeKB  = 5120
	maxSingleFileKB = 256
)

var scannableExtensions = map[string]bool{
	".md": true, ".txt": true, ".py": true, ".sh": true, ".bash": true, ".js": true, ".ts": true, ".rb": true, ".yaml": true,
	".yml": true, ".json": true, ".toml": true, ".cfg": true, ".ini": true, ".conf": true, ".html": true, ".css": true,
	".xml": true, ".tex": true, ".r": true, ".jl": true, ".pl": true, ".php": true,
}

var suspiciousBinaryExtensions = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".dat": true, ".com": true, ".msi": true,
	".dmg": true, ".app": true, ".deb": true, ".rpm": true,
}

var scriptExtensions = map[string]bool{".sh": true, ".bash": true, ".py": true, ".rb": true, ".pl": true}

// Invisible and directional characters used for text hiding with their readable names
var invisibleCharNames = map[rune]string{
	0x200b: "zero-width space", 0x200c: "zero-width non-joiner", 0x200d: "zero-width joiner",
	0x2060: "word joiner", 0x2062: "invisible times", 0x2063: "invisible separator",
	0x2064: "invisible plus", 0xfeff: "BOM/zero-width no-break space",
	0x202a: "LTR embedding", 0x202b: "RTL embedding", 0x202c: "pop directional",
	0x202d: "LTR override", 0x202e: "RTL override", 0x2066: "LTR isolate", 0x2067: "RTL isolate",
	0x2068: "first strong isolate", 0x2069: "pop directional isolate",
}

// Ordered invisible characters so findings are deterministic
var invisibleCharOrder = []rune{0x200b, 0x200c, 0x200d, 0x2060, 0x2062, 0x2063, 0x2064, 0xfeff, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069}

var pathTraversalPatternIDs = map[string]bool{"path_traversal": true, "path_traversal_deep": true}

// One based lines inside or on the boundary of triple quoted strings
func computeDocstringLines(lines []string) map[int]bool {
	doc := map[int]bool{}
	inside := false
	for i, line := range lines {
		wasIn := inside
		c1, c2 := strings.Count(line, `"""`), strings.Count(line, `'''`)
		if (c1+c2)%2 == 1 {
			inside = !inside
		}
		if wasIn || inside || c1 > 0 || c2 > 0 {
			doc[i+1] = true
		}
	}
	return doc
}

// Blanks balanced inline markdown link destinations while preserving offsets
func maskMarkdownLinkDestinations(line string) string {
	masked := []rune(line)
	runes := []rune(line)
	searchFrom := 0
	for {
		start := indexRunes(runes, []rune("]("), searchFrom)
		if start == -1 {
			break
		}
		depth, escaped, cursor := 1, false, start+2
		found := false
		for cursor < len(runes) {
			ch := runes[cursor]
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '(':
				depth++
			case ch == ')':
				depth--
				if depth == 0 {
					for k := start + 2; k < cursor; k++ {
						masked[k] = ' '
					}
					searchFrom = cursor + 1
					found = true
				}
			}
			if found {
				break
			}
			cursor++
		}
		if !found {
			break
		}
	}
	return string(masked)
}

// Index of a rune sequence in runes at or after from, negative when absent
func indexRunes(runes, needle []rune, from int) int {
	for i := from; i+len(needle) <= len(runes); i++ {
		match := true
		for j := range needle {
			if runes[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

var (
	fenceLineRe       = regexp.MustCompile("^ {0,3}(?P<marker>`{3,}|~{3,})(?P<info>.*)$")
	containerPrefixRe = regexp.MustCompile(`^(?: {0,3}(?:>|(?:[-*+]|\d{1,9}[.)]) {1,4}))+`)
)

// Masks link destinations in markdown prose only, code blocks scanned verbatim
func maskProseLinkDestinations(lines []string) []string {
	out := make([]string, 0, len(lines))
	fenceChar := byte(0)
	fenceLen := 0
	open := false
	for _, line := range lines {
		m := fenceLineRe.FindStringSubmatch(containerPrefixRe.ReplaceAllString(line, ""))
		code := false
		if open {
			if m != nil && m[1][0] == fenceChar && len(m[1]) >= fenceLen && strings.TrimSpace(m[2]) == "" {
				open = false
			}
			code = true
		} else {
			if m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
				open, fenceChar, fenceLen = true, m[1][0], len(m[1])
			}
			code = open || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")
		}
		if code {
			out = append(out, line)
		} else {
			out = append(out, maskMarkdownLinkDestinations(line))
		}
	}
	return out
}

// Threat pattern and invisible unicode scan of one file, findings deduped per pattern per line
func ScanFile(filePath string, relPath string) []Finding {
	if relPath == "" {
		relPath = filepath.Base(filePath)
	}
	suffix := strings.ToLower(filepath.Ext(filePath))
	if !scannableExtensions[suffix] && filepath.Base(filePath) != "SKILL.md" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	text := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	if !isValidUTF8(text) {
		return nil
	}
	lines := strings.Split(text, "\n")
	var findings []Finding
	docLines := computeDocstringLines(lines)
	traversalLines := lines
	if suffix == ".md" {
		traversalLines = maskProseLinkDestinations(lines)
	}
	owners := statementOwners(lines)
	for _, p := range compiledThreatPatterns {
		for i, line := range lines {
			scanLine := line
			if pathTraversalPatternIDs[p.id] {
				scanLine = traversalLines[i]
			}
			if docLines[i+1] {
				continue
			}
			if ok, _ := p.re.MatchString(scanLine); !ok {
				continue
			}
			text := strings.TrimSpace(line)
			sev, desc := demoteInertPathReference(p.id, p.severity, p.description, line, lines[owners[i]], suffix)
			findings = append(findings, Finding{p.id, sev, p.category, relPath, i + 1, clipRunes(text, 120), desc})
		}
	}
	for i, line := range lines {
		for _, ch := range invisibleCharOrder {
			if strings.ContainsRune(line, ch) {
				name := invisibleCharNames[ch]
				findings = append(findings, Finding{"invisible_unicode", "high", "injection", relPath, i + 1,
					fmt.Sprintf("U+%04X (%s)", ch, name), fmt.Sprintf("invisible unicode character %s (possible text hiding/injection)", name)})
				break
			}
		}
	}
	return findings
}

// Reports whether a string is valid UTF-8
func isValidUTF8(s string) bool {
	return strings.ToValidUTF8(s, "") == s
}

// Text cut to n runes with an ellipsis when longer
func clipRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-3]) + "..."
}

// Findings that only inform on a community install when found in markdown
var docNoteIDs = map[string]bool{"sudo_usage": true, "bind_all_interfaces": true}

// Lowers markdown only setup findings of a community skill to informational
func communityNote(f Finding) Finding {
	if (f.Severity == "critical" || f.Severity == "high") && docNoteIDs[f.PatternID] && strings.HasSuffix(strings.ToLower(f.File), ".md") {
		f.Severity = "medium"
		f.Description += " (not executed on install; informational)"
	}
	return f
}

// Structural checks plus pattern scan of every text file in a skill dir or a single file
func ScanSkill(skillPath string, source string) ScanResult {
	if source == "" {
		source = "community"
	}
	name, trust := filepath.Base(skillPath), resolveTrustLevel(source)
	var findings []Finding
	if isDir(skillPath) {
		ignore := loadSkillIgnore(skillPath)
		findings = append(findings, checkStructure(skillPath, ignore)...)
		for _, rel := range rglobFiles(skillPath) {
			if ignore(rel) {
				continue
			}
			findings = append(findings, ScanFile(filepath.Join(skillPath, filepath.FromSlash(rel)), rel)...)
		}
	} else if isFile(skillPath) {
		findings = append(findings, ScanFile(skillPath, filepath.Base(skillPath))...)
	}
	if trust == "community" {
		for i := range findings {
			findings[i] = communityNote(findings[i])
		}
	}
	verdict := determineVerdict(findings)
	return ScanResult{SkillName: name, Source: source, TrustLevel: trust, Verdict: verdict, Findings: findings,
		ScannedAt: time.Now().UTC().Format(time.RFC3339Nano), Summary: buildSummary(name, verdict, findings)}
}

// Canonical sha256 over rel path strings and file bytes ordered by the path string
func contentDigest(skillPath string) (string, error) {
	h := sha256.New()
	if !isDir(skillPath) {
		data, err := os.ReadFile(skillPath)
		if err != nil {
			return "", err
		}
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	for _, rel := range rglobFiles(skillPath) {
		data, err := os.ReadFile(filepath.Join(skillPath, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Short integrity hash of a skill directory symmetric with the bundle hash
func ContentHash(skillPath string) string {
	d, err := contentDigest(skillPath)
	if err != nil {
		return "sha256:"
	}
	return "sha256:" + d[:16]
}

// Scan plus attestation, the cache serving only identical content under the current scanner version
func ScanSkillCached(skillPath string, source string, sourceURL string, cacheDir string) (ScanResult, map[string]any, error) {
	digest, err := contentDigest(skillPath)
	if err != nil {
		return ScanResult{}, nil, err
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(filepath.Dir(skillPath), ".scan-cache")
	}
	idSum := sha256.Sum256([]byte(source + "\x00" + sourceURL))
	sourceIdentity := hex.EncodeToString(idSum[:])[:16]
	cacheFile := filepath.Join(cacheDir, digest+"-"+sourceIdentity+".json")
	expected := map[string]any{"bundle_hash": "sha256:" + digest, "scanner_version": ScannerVersion, "source": source, "source_url": sourceURL}
	var cached map[string]any
	if data, err := os.ReadFile(cacheFile); err == nil {
		var parsed map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")), &parsed) == nil {
			cached = parsed
		}
	}
	fresh := true
	if cached != nil {
		fresh = false
		for k, v := range expected {
			if cached[k] != v {
				fresh = true
				break
			}
		}
	}
	if !fresh {
		var findings []Finding
		if raw, err := json.Marshal(cached["findings"]); err == nil {
			json.Unmarshal(raw, &findings)
		}
		result := ScanResult{SkillName: filepath.Base(skillPath), Source: source, TrustLevel: fmString(cached["trust_level"]),
			Verdict: fmString(cached["verdict"]), Findings: findings, ScannedAt: fmString(cached["scanned_at"]), Summary: fmString(cached["summary"])}
		prov := map[string]any{}
		for k, v := range cached {
			prov[k] = v
		}
		prov["fresh"] = false
		result.ScanProvenance = prov
		return result, prov, nil
	}
	result := ScanSkill(skillPath, source)
	rules := map[string]bool{}
	for _, f := range result.Findings {
		rules[f.PatternID] = true
	}
	ruleList := make([]string, 0, len(rules))
	for r := range rules {
		ruleList = append(ruleList, r)
	}
	sort.Strings(ruleList)
	findings := result.Findings
	if findings == nil {
		findings = []Finding{}
	}
	prov := map[string]any{}
	for k, v := range expected {
		prov[k] = v
	}
	prov["verdict"] = result.Verdict
	prov["trust_level"] = result.TrustLevel
	prov["findings"] = findings
	prov["rules"] = ruleList
	prov["scanned_at"] = result.ScannedAt
	prov["summary"] = result.Summary
	prov["fresh"] = true
	if os.MkdirAll(cacheDir, 0o755) == nil {
		if data, err := json.MarshalIndent(prov, "", "  "); err == nil {
			os.WriteFile(cacheFile, append(data, '\n'), 0o644)
		}
	}
	result.ScanProvenance = prov
	return result, prov, nil
}

// Install decision from verdict and trust, allowed nil when policy says ask
func ShouldAllowInstall(result ScanResult, force bool) (*bool, string) {
	policy, ok := installPolicy[result.TrustLevel]
	if !ok {
		policy = installPolicy["community"]
	}
	vi, ok := verdictIndex[result.Verdict]
	if !ok {
		vi = 2
	}
	decision := policy[vi]
	n := len(result.Findings)
	hardBlock := result.Verdict == "dangerous" && (result.TrustLevel == "community" || result.TrustLevel == "trusted")
	t, f := true, false
	if decision == "allow" {
		return &t, fmt.Sprintf("Allowed (%s source, %s verdict)", result.TrustLevel, result.Verdict)
	}
	if force && !hardBlock {
		return &t, fmt.Sprintf("Force-installed despite %s verdict (%d findings)", result.Verdict, n)
	}
	if decision == "ask" {
		return nil, fmt.Sprintf("Requires confirmation (%s source + %s verdict, %d findings)", result.TrustLevel, result.Verdict, n)
	}
	blocked := fmt.Sprintf("Blocked (%s source + %s verdict, %d findings). ", result.TrustLevel, result.Verdict, n)
	if hardBlock {
		return &f, blocked + "--force does not override a dangerous verdict."
	}
	return &f, blocked + "Use --force to override."
}

var severityOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

// Compact multi line report with findings sorted critical to low
func FormatScanReport(result ScanResult) string {
	lines := []string{fmt.Sprintf("Scan: %s (%s/%s)  Verdict: %s", result.SkillName, result.Source, result.TrustLevel, strings.ToUpper(result.Verdict))}
	if len(result.Findings) > 0 {
		sorted := append([]Finding(nil), result.Findings...)
		sort.SliceStable(sorted, func(a, b int) bool {
			oa, ok := severityOrder[sorted[a].Severity]
			if !ok {
				oa = 4
			}
			ob, ok := severityOrder[sorted[b].Severity]
			if !ok {
				ob = 4
			}
			return oa < ob
		})
		for _, f := range sorted {
			loc := fmt.Sprintf("%s:%d", f.File, f.Line)
			lines = append(lines, fmt.Sprintf("  %-8s %-14s %-30s \"%s\"", strings.ToUpper(f.Severity), f.Category, loc, clipRunes(f.Match, 60)))
		}
		lines = append(lines, "")
	}
	allowed, reason := ShouldAllowInstall(result, false)
	status := "BLOCKED"
	if allowed == nil {
		status = "NEEDS CONFIRMATION"
	} else if *allowed {
		status = "ALLOWED"
	}
	return strings.Join(append(lines, fmt.Sprintf("Decision: %s - %s", status, reason)), "\n")
}

// Structural anomalies of a skill dir honoring the ignore predicate
func checkStructure(skillDir string, ignore func(string) bool) []Finding {
	var findings []Finding
	add := func(pid, sev, cat, rel, match, desc string) {
		findings = append(findings, Finding{pid, sev, cat, rel, 0, match, desc})
	}
	fileCount, totalSize := 0, int64(0)
	filepath.WalkDir(skillDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == skillDir {
			return nil
		}
		rel, rerr := filepath.Rel(skillDir, p)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		isLink := d.Type()&os.ModeSymlink != 0
		if !(d.Type().IsRegular() || isLink) {
			return nil
		}
		if ignore != nil && ignore(relSlash) {
			return nil
		}
		fileCount++
		if isLink {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				add("broken_symlink", "medium", "traversal", rel, "broken symlink", "broken or circular symlink")
				return nil
			}
			if !isRelativeTo(resolved, resolvePath(skillDir)) {
				add("symlink_escape", "critical", "traversal", rel, "symlink -> "+resolved, "symlink points outside the skill directory")
			}
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		size := st.Size()
		totalSize += size
		if size > maxSingleFileKB*1024 {
			add("oversized_file", "medium", "structural", rel, fmt.Sprintf("%dKB", size/1024), fmt.Sprintf("file is %dKB (limit: %dKB)", size/1024, maxSingleFileKB))
		}
		ext := strings.ToLower(filepath.Ext(p))
		if suspiciousBinaryExtensions[ext] {
			add("binary_file", "critical", "structural", rel, "binary: "+ext, fmt.Sprintf("binary/executable file (%s) should not be in a skill", ext))
		}
		if !scriptExtensions[ext] && st.Mode()&0o111 != 0 {
			add("unexpected_executable", "medium", "structural", rel, "executable bit set", "file has executable permission but is not a recognized script type")
		}
		return nil
	})
	if fileCount > maxFileCount {
		add("too_many_files", "medium", "structural", "(directory)", fmt.Sprintf("%d files", fileCount), fmt.Sprintf("skill has %d files (limit: %d)", fileCount, maxFileCount))
	}
	if totalSize > maxTotalSizeKB*1024 {
		add("oversized_skill", "low", "structural", "(directory)", fmt.Sprintf("%dKB total", totalSize/1024), fmt.Sprintf("skill is %dKB total (limit: %dKB)", totalSize/1024, maxTotalSizeKB))
	}
	return findings
}

var skillIgnoreFilenames = []string{".skillignore", ".clawhubignore"}

// Ignore predicate from .skillignore and .clawhubignore with gitignore basics
func loadSkillIgnore(skillDir string) func(string) bool {
	var patterns []string
	for _, name := range skillIgnoreFilenames {
		data, err := os.ReadFile(filepath.Join(skillDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"), "\n") {
			s := strings.TrimSpace(line)
			if s != "" && !strings.HasPrefix(s, "#") {
				patterns = append(patterns, s)
			}
		}
	}
	return func(rel string) bool {
		relPosix := filepath.ToSlash(rel)
		segs := strings.Split(relPosix, "/")
		base := segs[len(segs)-1]
		if base == "SKILL.md" {
			return false
		}
		for _, n := range skillIgnoreFilenames {
			if base == n {
				return true
			}
		}
		for _, pat := range patterns {
			anchored := strings.HasPrefix(pat, "/")
			p := strings.Trim(pat, "/")
			if p == "" {
				continue
			}
			below := strings.HasPrefix(relPosix, p+"/")
			if strings.HasSuffix(pat, "/") {
				if relPosix == p || below || (!anchored && strings.Contains("/"+relPosix+"/", "/"+p+"/")) {
					return true
				}
				continue
			}
			if fnmatch(relPosix, p) {
				return true
			}
			if !anchored {
				if fnmatch(base, p) || below {
					return true
				}
				if !strings.Contains(p, "/") {
					for _, seg := range segs {
						if fnmatch(seg, p) {
							return true
						}
					}
				}
			}
		}
		return false
	}
}

// Shell style wildcard match where star spans separators like Python fnmatch
func fnmatch(name, pattern string) bool {
	re, err := regexp.Compile(fnmatchTranslate(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// Translates a shell pattern to an anchored regular expression
func fnmatchTranslate(pat string) string {
	var sb strings.Builder
	sb.WriteString(`(?s)^`)
	runes := []rune(pat)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '*':
			sb.WriteString(`.*`)
		case '?':
			sb.WriteString(`.`)
		case '[':
			j := i + 1
			if j < len(runes) && runes[j] == '!' {
				j++
			}
			if j < len(runes) && runes[j] == ']' {
				j++
			}
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j >= len(runes) {
				sb.WriteString(`\[`)
				continue
			}
			stuff := string(runes[i+1 : j])
			i = j
			if strings.HasPrefix(stuff, "!") {
				stuff = "^" + stuff[1:]
			} else if strings.HasPrefix(stuff, "^") {
				stuff = `\` + stuff
			}
			stuff = strings.ReplaceAll(stuff, `\`, `\\`)
			sb.WriteString("[" + stuff + "]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString(`$`)
	return sb.String()
}

var sourcePrefixAliases = []string{"skills-sh/", "skills.sh/", "skils-sh/", "skils.sh/"}

// Trust level of a source id, official meaning builtin and trusted repos matching exactly or as a path
func resolveTrustLevel(source string) string {
	src := source
	for _, p := range sourcePrefixAliases {
		if strings.HasPrefix(source, p) {
			src = source[len(p):]
			break
		}
	}
	if src == "agent-created" {
		return "agent-created"
	}
	if src == "official" {
		return "builtin"
	}
	for t := range TrustedRepos {
		if src == t || strings.HasPrefix(src, t+"/") {
			return "trusted"
		}
	}
	return "community"
}

// Verdict from the worst severity, critical dangerous and high caution
func determineVerdict(findings []Finding) string {
	high := false
	for _, f := range findings {
		if f.Severity == "critical" {
			return "dangerous"
		}
		if f.Severity == "high" {
			high = true
		}
	}
	if high {
		return "caution"
	}
	return "safe"
}

// One line summary naming the verdict and finding categories
func buildSummary(name, verdict string, findings []Finding) string {
	if len(findings) == 0 {
		return name + ": clean scan, no threats detected"
	}
	cats := map[string]bool{}
	for _, f := range findings {
		cats[f.Category] = true
	}
	list := make([]string, 0, len(cats))
	for c := range cats {
		list = append(list, c)
	}
	sort.Strings(list)
	return fmt.Sprintf("%s: %s - %d finding(s) in %s", name, verdict, len(findings), strings.Join(list, ", "))
}
