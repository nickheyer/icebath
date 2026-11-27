package skills

import (
	"github.com/dlclark/regexp2/v2"
)

// Version of the guard's rules, stamped on each scan it caches so a verdict expires with the rules that gave it
const scannerVersion = "skills-guard-v10"

// Repositories whose skills install as trusted
var trustedRepos = map[string]bool{
	"openai/skills": true, "anthropics/skills": true, "huggingface/skills": true, "NVIDIA/skills": true, "wandb/skills": true,
	"qdrant/skills": true, "pinecone-io/skills": true,
}

// What a verdict means for an install by the trust of its source: for safe, caution and dangerous
var policy = map[string][3]string{
	"builtin":       {"allow", "allow", "allow"},
	"trusted":       {"allow", "allow", "block"},
	"community":     {"allow", "block", "block"},
	"agent-created": {"allow", "allow", "ask"},
}

// One threat pattern of the guard
type threat struct {
	re          *regexp2.Regexp
	id          string
	severity    string
	category    string
	description string
}

// A case blind pattern, read with lookaround
func threatOf(pattern, id, severity, category, description string) threat {
	return threat{regexp2.MustCompile(pattern, regexp2.IgnoreCase), id, severity, category, description}
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
	credFileArg     = `(?:os\.path\.expanduser\s*\(\s*)?` + credFileLiteral + `\s*\)?`
)

// Every threat pattern in the order a scan tries them
var threats = func() []threat {
	loop := `(?![^\n]*https?://(?:localhost|127\.0\.0\.1|\[::1\]))`
	return []threat{
		threatOf(`curl\s+`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`, "env_exfil_curl", "critical", "exfiltration", "curl command interpolating secret environment variable"),
		threatOf(`wget\s+`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`, "env_exfil_wget", "critical", "exfiltration", "wget command interpolating secret environment variable"),
		threatOf(`fetch\s*\(`+loop+`[^\n]*\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD)S?\b`, "env_exfil_fetch", "critical", "exfiltration", "fetch() call interpolating secret environment variable"),
		threatOf(`httpx?\.(get|post|put|patch)\s*\(`+loop+`[^\n]*(KEY|TOKEN|SECRET|PASSWORD)`, "env_exfil_httpx", "critical", "exfiltration", "HTTP library call with secret variable"),
		threatOf(`requests\.(get|post|put|patch)\s*\(`+loop+`[^\n]*(KEY|TOKEN|SECRET|PASSWORD)`, "env_exfil_requests", "critical", "exfiltration", "requests library call with secret variable"),
		threatOf(`base64[^\n]*env`, "encoded_exfil", "high", "exfiltration", "base64 encoding combined with environment access"),
		threatOf(`\$HOME/\.ssh|\~/\.ssh`, "ssh_dir_access", "high", "exfiltration", "references user SSH directory"),
		threatOf(`\$HOME/\.aws|\~/\.aws`, "aws_dir_access", "high", "exfiltration", "references user AWS credentials directory"),
		threatOf(`\$HOME/\.gnupg|\~/\.gnupg`, "gpg_dir_access", "high", "exfiltration", "references user GPG keyring"),
		threatOf(`\$HOME/\.kube|\~/\.kube`, "kube_dir_access", "high", "exfiltration", "references Kubernetes config directory"),
		threatOf(`\$HOME/\.docker|\~/\.docker`, "docker_dir_access", "high", "exfiltration", "references Docker config (may contain registry creds)"),
		threatOf(`\$HOME/\.icebath/\.env|\~/\.icebath/\.env`, "icebath_env_access", "critical", "exfiltration", "directly references the icebath keys file"),
		threatOf(`cat\s+(?!>)[^\n]*(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets_file", "critical", "exfiltration", "reads known secrets file"),
		threatOf(`\b(?:readFile(?:Sync)?|readTextFile)\s*\(\s*`+credFileLiteral, "js_read_secrets_file", "critical", "exfiltration", "JavaScript reads a known credential file"),
		threatOf(`\bopen\s*\(\s*`+credFileArg+`(?!\s*\)?\s*,\s*["'][^"']*[wax][^"']*["'])(?![^\n]*\bmode\s*=\s*["'][^"']*[wax])`+
			`|\bPath\s*\(\s*`+credFileArg+`\s*\)\.(?:read_text|read_bytes|readlines|readline)\s*\(`, "py_read_secrets_file", "critical", "exfiltration", "Python reads a known credential file"),
		threatOf(`printenv|(?<![A-Za-z0-9_])env\s*\|(?!\|)(?!\s*[{(])`, "dump_all_env", "high", "exfiltration", "dumps all environment variables"),
		threatOf(`^[^#\n]*os\.environ\b(?!\s*\.get\s*\()`, "python_os_environ", "high", "exfiltration", "accesses os.environ outside comments/docstrings (potential env dump)"),
		threatOf(`os\.environ\s*\.get\s*\(\s*["'][^"']*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)`, "python_environ_get_secret", "medium", "exfiltration", "reads secret via os.environ.get() (normal API-key access; informational)"),
		threatOf(`os\.getenv\s*\(\s*[^\)]*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)`, "python_getenv_secret", "medium", "exfiltration", "reads secret via os.getenv() (normal API-key access; informational)"),
		threatOf(`process\.env\[`, "node_process_env", "high", "exfiltration", "accesses process.env (Node.js environment)"),
		threatOf(`(?-i:ENV)\[.*(?:KEY|TOKEN|SECRET|PASSWORD)`, "ruby_env_secret", "critical", "exfiltration", "reads secret via Ruby ENV[]"),
		threatOf(`(?<![-/])\b(?:dig|nslookup)\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?[^\s"'$()]*`+
			`\$(?:\{[^}\s]*\}|\([^\n]*?\)+|\w+)[\w-]*["']?\.[a-z]`+
			`|(?<![-/])\bhost\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?[^\s"'$()]*`+
			`\$(?:\{[^}\s]*\}|\([^\n]*?\)+|\w+)[\w-]*\.[a-z]`+
			`|(?:^|[;&|]|\$\(|\b(?:do|then|else)\b)\s*host\s+(?:[-+@]\S*(?:\s+[^\s$"'-][^\s$]*)?\s+)*["']?\$`, "dns_exfil", "critical", "exfiltration", "DNS lookup with variable interpolation (possible DNS exfiltration)"),
		threatOf(`>\s*/tmp/[^\s]*\s*&&\s*(curl|wget|nc|python)`, "tmp_staging", "critical", "exfiltration", "writes to /tmp then exfiltrates"),
		threatOf(`!\[.*\]\(https?://[^\)]*\$\{?`, "md_image_exfil", "high", "exfiltration", "markdown image URL with variable interpolation (image-based exfil)"),
		threatOf(`\[.*\]\(https?://[^\)]*\$\{?`, "md_link_exfil", "high", "exfiltration", "markdown link with variable interpolation"),
		threatOf(`ignore\s+(?:\w+\s+)*(previous|all|above|prior)\s+instructions`, "prompt_injection_ignore", "critical", "injection", "prompt injection: ignore previous instructions"),
		threatOf(`you\s+are\s+(?:\w+\s+)*now\s+`, "role_hijack", "high", "injection", "attempts to override the agent's role"),
		threatOf(`do\s+not\s+(?:\w+\s+)*tell\s+(?:\w+\s+)*the\s+user(?!\s+to\s+["'`+"“‘"+`])`+
			`(?!.*\b(?:unless|except|until|confirm|diagnos\w*|verify|check)\b)`, "deception_hide", "high", "injection", "instructs agent to hide information from user"),
		threatOf(`system\s+(?:\w+\s+)*prompt\s+(?:\w+\s+)*override`, "sys_prompt_override", "critical", "injection", "attempts to override the system prompt"),
		threatOf(`pretend\s+(?:\w+\s+)*(you\s+are|to\s+be)\s+`, "role_pretend", "high", "injection", "attempts to make the agent assume a different identity"),
		threatOf(`disregard\s+(?:\w+\s+)*(your|all|any)\s+(?:\w+\s+)*(instructions|rules|guidelines)`, "disregard_rules", "critical", "injection", "instructs agent to disregard its rules"),
		threatOf(`output\s+(?:\w+\s+)*(system|initial)\s+prompt`, "leak_system_prompt", "high", "injection", "attempts to extract the system prompt"),
		threatOf(`(when|if)\s+no\s*one\s+is\s+(watching|looking)`, "conditional_deception", "high", "injection", "conditional instruction to behave differently when unobserved"),
		threatOf(`act\s+as\s+(if|though)\s+(?:\w+\s+)*you\s+(?:\w+\s+)*(have\s+no|don't\s+have)\s+(?:\w+\s+)*(restrictions|limits|rules)`, "bypass_restrictions", "critical", "injection", "instructs agent to act without restrictions"),
		threatOf(`translate\s+.*\s+into\s+.*\s+and\s+(execute|run|eval)`, "translate_execute", "critical", "injection", "translate-then-execute evasion technique"),
		threatOf(`<!--[^>]*(?:\b(?:ignore|forget|disregard|override)\s+(?:\w+\s+)*?`+
			`(?:previous|prior|above|earlier|all|any|your|the)\s+(?:\w+\s+)*?`+
			`(?:instructions?|rules|prompts?|guidelines|directions|restrictions)`+
			`|\bsystem\s*(?:prompt|message|instructions?|override)|\b(?:override|secret|hidden|system)\s*:`+
			`|\b(?:ignore|override|bypass|disable)\s+(?:\w+\s+)*?(?:safety|security|guardrails|filters)\b`+
			`|\b(?:secret|hidden|new)\s+(?:instructions?|commands?|directives?|tasks?)\b|\byou\s+are\s+now\b`+
			`|\bdo\s+not\s+(?:tell|reveal|show|mention|inform)\b`+
			`|\b(?:note|message)\s+(?:to|for)\s+(?:the\s+)?(?:ai|agent|assistant|model|llm)\b)[^>]*-->`, "html_comment_injection", "high", "injection", "hidden instructions in HTML comments"),
		threatOf(`<\s*div\s+style\s*=\s*["'][\s\S]*?display\s*:\s*none`, "hidden_div", "high", "injection", "hidden HTML div (invisible instructions)"),
		threatOf(`rm\s+-rf\s+((?:[^\s;&|<>#][^\s;&|<>]*\s+)+?)?/(?(1)(?!/(?:\s|$)))(?:`+
			`(?!(?:tmp|var/tmp|dev/shm|run|var/lib/apt/lists|var/cache/(?:apt|apk|yum|dnf))(?:\b|/))`+
			`|(?:tmp|var/tmp|dev/shm|run|var/lib/apt/lists|var/cache/(?:apt|apk|yum|dnf))/(?:[^/\s]*/)*\.\.(?=/|[\s;&|]|$))`, "destructive_root_rm", "critical", "destructive", "recursive delete from root"),
		threatOf(`(?<![A-Za-z0-9])rm\s+(-[^\s]*)?r.*(?:\$HOME|~[/\s*]|~$)|(?<![A-Za-z0-9])rmdir\s+.*(?:\$HOME|~[/\s*]|~$)`, "destructive_home_rm", "critical", "destructive", "recursive delete targeting home directory ($HOME or ~)"),
		threatOf(`chmod\s+777`, "insecure_perms", "medium", "destructive", "sets world-writable permissions"),
		threatOf(`>\s*/etc/`, "system_overwrite", "critical", "destructive", "overwrites system configuration file"),
		threatOf(`\bmkfs\b`, "format_filesystem", "critical", "destructive", "formats a filesystem"),
		threatOf(`\bdd\s+.*if=.*of=/dev/`, "disk_overwrite", "critical", "destructive", "raw disk write operation"),
		threatOf(`shutil\.rmtree\s*\(\s*["'/]`, "python_rmtree", "high", "destructive", "Python rmtree on absolute or root-relative path"),
		threatOf(`truncate\s+-s\s*0\s+/`, "truncate_system", "critical", "destructive", "truncates system file to zero bytes"),
		threatOf(`\bcrontab\b`, "persistence_cron", "medium", "persistence", "modifies cron jobs"),
		threatOf(`\.(bashrc|zshrc|bash_profile|bash_login|zprofile|zlogin)\b`, "shell_rc_mod", "medium", "persistence", "references shell startup file"),
		threatOf(`(?<![\w)\]?])\.profile\b`, "shell_rc_mod", "medium", "persistence", "references shell startup file"),
		threatOf(`authorized_keys`, "ssh_backdoor", "critical", "persistence", "modifies SSH authorized keys"),
		threatOf(`ssh-keygen`, "ssh_keygen", "medium", "persistence", "generates SSH keys"),
		threatOf(`systemd.*\.service|systemctl\s+(enable|start)`, "systemd_service", "medium", "persistence", "references or enables systemd service"),
		threatOf(`/etc/init\.d/`, "init_script", "medium", "persistence", "references init.d startup script"),
		threatOf(`launchctl\s+load|LaunchAgents|LaunchDaemons`, "macos_launchd", "medium", "persistence", "macOS launch agent/daemon persistence"),
		threatOf(`/etc/sudoers|visudo`, "sudoers_mod", "critical", "persistence", "modifies sudoers (privilege escalation)"),
		threatOf(`git\s+config\s+--global\s+`, "git_config_global", "medium", "persistence", "modifies global git configuration"),
		threatOf(`\bnc\s+-[lp]|ncat\s+-[lp]|\bsocat\b[^\n]*\b(?:tcp|udp|openssl|ssl|exec|system|pty|unix)[\w-]*:`, "reverse_shell", "critical", "network", "potential reverse shell listener"),
		threatOf(`\bngrok\b|\blocaltunnel\b|\bserveo\b|\bcloudflared\b`, "tunnel_service", "high", "network", "uses tunneling service for external access"),
		threatOf(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}:\d{2,5}`, "hardcoded_ip_port", "medium", "network", "hardcoded IP address with port"),
		threatOf(`0\.0\.0\.0:\d+|INADDR_ANY`, "bind_all_interfaces", "high", "network", "binds to all network interfaces"),
		threatOf(`/bin/`+shellNamesRe+`\s+-i\s+.*>/dev/tcp/`, "bash_reverse_shell", "critical", "network", "bash interactive reverse shell via /dev/tcp"),
		threatOf(`python[23]?\s+-c\s+["']import\s+socket`, "python_socket_oneliner", "critical", "network", "Python one-liner socket connection (likely reverse shell)"),
		threatOf(`socket\.connect\s*\(\s*\(`, "python_socket_connect", "high", "network", "Python socket connect to arbitrary host"),
		threatOf(`webhook\.site|requestbin\.com|pipedream\.net|hookbin\.com`, "exfil_service", "high", "network", "references known data exfiltration/webhook testing service"),
		threatOf(`pastebin\.com|hastebin\.com|ghostbin\.`, "paste_service", "medium", "network", "references paste service (possible data staging)"),
		threatOf(`(?:\bbase64\s+(?:-[^\s|]+\s+)*?(?:-[a-z]*d[a-z]*|--decode)\b|\bopenssl\s+(?:base64|enc)\b[^|;&\n]*?\s-d\b)`+
			`[^\n]*?(?:\||;|&&)\s*(?:\w+=\S*\s+)*(?:\S*/)?`+
			`(?:sh|bash|zsh|ksh|dash|fish|python[\d.]*|perl|ruby|node|nodejs|php|eval|source|exec|xargs|env|sudo`+
			`|iex|pwsh|powershell|tar|bsdtar|unzip|cpio|gunzip|gzip|zcat|xz|unxz|bunzip2|\.(?=\s))(?![\w.-])`+
			`|\b(?:eval|source|iex|exec)\b[^\n]*\bbase64\s+(?:-[^\s|]+\s+)*?(?:-[a-z]*d[a-z]*|--decode)\b`, "base64_decode_pipe", "high", "obfuscation", "base64 decodes and pipes to execution"),
		threatOf(`\\x[0-9a-fA-F]{2}.*\\x[0-9a-fA-F]{2}.*\\x[0-9a-fA-F]{2}`, "hex_encoded_string", "medium", "obfuscation", "hex-encoded string (possible obfuscation)"),
		threatOf(`\beval\s*\(\s*["']`, "eval_string", "high", "obfuscation", "eval() with string argument"),
		threatOf(`(?<![.\w])exec\s*\(\s*["'](?!(?-i:PRAGMA|BEGIN|COMMIT|ROLLBACK|CREATE|INSERT|SELECT|UPDATE|DELETE|DROP|ALTER`+
			`|VACUUM|SAVEPOINT|RELEASE|ANALYZE|REINDEX|ATTACH|DETACH|REPLACE)\b)`, "exec_string", "high", "obfuscation", "exec() with string argument"),
		threatOf(`echo\s+[^\n]*\|\s*(?:`+shellNamesRe+`|python|perl|ruby|node)`, "echo_pipe_exec", "critical", "obfuscation", "echo piped to interpreter for execution"),
		threatOf(`compile\s*\(\s*[^\)]+,\s*["'].*["']\s*,\s*["']exec["']\s*\)`, "python_compile_exec", "high", "obfuscation", "Python compile() with exec mode"),
		threatOf(`getattr\s*\(\s*__builtins__`, "python_getattr_builtins", "high", "obfuscation", "dynamic access to Python builtins (evasion technique)"),
		threatOf(`__import__\s*\(\s*["']os["']\s*\)`, "python_import_os", "high", "obfuscation", "dynamic import of os module"),
		threatOf(`codecs\.decode\s*\(\s*["']`, "python_codecs_decode", "medium", "obfuscation", "codecs.decode (possible ROT13 or encoding obfuscation)"),
		threatOf(`String\.fromCharCode|charCodeAt`, "js_char_code", "medium", "obfuscation", "JavaScript character code construction (possible obfuscation)"),
		threatOf(`atob\s*\(|btoa\s*\(`, "js_base64", "medium", "obfuscation", "JavaScript base64 encode/decode"),
		threatOf(`\[::-1\]`, "string_reversal", "low", "obfuscation", "string reversal (possible obfuscated payload)"),
		threatOf(`chr\s*\(\s*\d+\s*\)\s*\+\s*chr\s*\(\s*\d+`, "chr_building", "high", "obfuscation", "building string from chr() calls (obfuscation)"),
		threatOf(`\\u[0-9a-fA-F]{4}.*\\u[0-9a-fA-F]{4}.*\\u[0-9a-fA-F]{4}`, "unicode_escape_chain", "medium", "obfuscation", "chain of unicode escapes (possible obfuscation)"),
		threatOf(`subprocess\.(run|call|Popen|check_output)\s*\(`, "python_subprocess", "medium", "execution", "Python subprocess execution"),
		threatOf(`os\.system\s*\(`, "python_os_system", "high", "execution", "os.system() - unguarded shell execution"),
		threatOf(`os\.popen\s*\(`, "python_os_popen", "high", "execution", "os.popen() - shell pipe execution"),
		threatOf(`child_process\.(exec|spawn|fork)\s*\(`, "node_child_process", "high", "execution", "Node.js child_process execution"),
		threatOf(`Runtime\.getRuntime\(\)\.exec\(`, "java_runtime_exec", "high", "execution", "Java Runtime.exec() - shell execution"),
		threatOf("`[^`]*\\$\\([^)]+\\)[^`]*`", "backtick_subshell", "medium", "execution", "backtick string with command substitution"),
		threatOf("!`[^`\\s][^`\\n]*`", "inline_shell_exec", "high", "execution", "inline-shell auto-exec snippet (expands via bash -c on skill view/load)"),
		threatOf(`\.\./\.\./\.\.`, "path_traversal_deep", "high", "traversal", "deep relative path traversal (3+ levels up)"),
		threatOf(`\.\./\.\.`, "path_traversal", "medium", "traversal", "relative path traversal (2+ levels up)"),
		threatOf(`/etc/passwd|/etc/shadow`, "system_passwd_access", "critical", "traversal", "references system password files"),
		threatOf(`/proc/self|/proc/\d+/`, "proc_access", "high", "traversal", "references /proc filesystem (process introspection)"),
		threatOf(`/dev/shm/`, "dev_shm", "medium", "traversal", "references shared memory (common staging area)"),
		threatOf(`xmrig|stratum\+tcp|monero|coinhive|cryptonight`, "crypto_mining", "critical", "mining", "cryptocurrency mining reference"),
		threatOf(`hashrate|nonce.*difficulty`, "mining_indicators", "medium", "mining", "possible cryptocurrency mining indicators"),
		threatOf(`curl\s+[^|\s][^\n]*\|\s*`+sudoPrefix+shellNamesRe, "curl_pipe_shell", "critical", "supply_chain", "curl piped to shell (download-and-execute)"),
		threatOf(`wget\s+[^\n]*-O\s*-\s*\|\s*`+sudoPrefix+shellNamesRe, "wget_pipe_shell", "critical", "supply_chain", "wget piped to shell (download-and-execute)"),
		threatOf(`curl\s+[^|\s][^\n]*\|\s*`+sudoPrefix+`python`, "curl_pipe_python", "critical", "supply_chain", "curl piped to Python interpreter"),
		threatOf(`#\s*///\s*script.*dependencies`, "pep723_inline_deps", "medium", "supply_chain", "PEP 723 inline script metadata with dependencies (verify pinning)"),
		threatOf(`pip\s+install\s+(?!-r\s)(?!.*==)`, "unpinned_pip_install", "medium", "supply_chain", "pip install without version pinning"),
		threatOf(`npm\s+install\s+(?!.*@\d)`, "unpinned_npm_install", "medium", "supply_chain", "npm install without version pinning"),
		threatOf(`uv\s+run\s+`, "uv_run", "medium", "supply_chain", "uv run (may auto-install unpinned dependencies)"),
		threatOf(`(curl|wget|httpx?\.get|requests\.get|fetch)\s*[\(]?\s*["']https?://`, "remote_fetch", "medium", "supply_chain", "fetches remote resource at runtime"),
		threatOf(`git\s+clone\s+`, "git_clone", "medium", "supply_chain", "clones a git repository at runtime"),
		threatOf(`docker\s+pull\s+`, "docker_pull", "medium", "supply_chain", "pulls a Docker image at runtime"),
		threatOf(`^allowed-tools\s*:`, "allowed_tools_field", "low", "privilege_escalation", "skill declares allowed-tools (standard frontmatter; informational)"),
		threatOf(`\bsudo\b(?!\.\w)`, "sudo_usage", "high", "privilege_escalation", "uses sudo (privilege escalation)"),
		threatOf(`(?<![a-z])set(?:e|re|res)?[ug]id\s*\(|\bcap_set[ug]id\b|\bchmod\s+(?:-\w+\s+)*(?:[ugoa]*[+=][rwxXt]*s|0?[2-7][0-7]{3}\b)`, "setuid_setgid", "critical", "privilege_escalation", "setuid/setgid (privilege escalation mechanism)"),
		threatOf(`(?<!cap_)set[ug]id(?!\s*\()`, "setuid_setgid", "medium", "privilege_escalation", "mentions setuid/setgid (informational)"),
		threatOf(`NOPASSWD`, "nopasswd_sudo", "critical", "privilege_escalation", "NOPASSWD sudoers entry (passwordless privilege escalation)"),
		threatOf(`chmod\s+[u+]?s`, "suid_bit", "critical", "privilege_escalation", "sets SUID/SGID bit on a file"),
		threatOf(proseModifyRe(agentConfigFiles), "agent_config_mod", "critical", "persistence", "instructs modification of agent config files (could persist instructions across sessions)"),
		threatOf(shellWriteRe(agentConfigFiles), "agent_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting agent config files (persistence mechanism)"),
		threatOf(contentContractRe(agentConfigFiles), "agent_config_contract", "high", "persistence", "dictates agent config file contents (verify intent - authoring guides use this shape too)"),
		threatOf(`AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules`, "agent_config_ref", "low", "persistence", "references agent config files (informational; only modification intent is scored)"),
		threatOf(proseModifyRe(icebathConfigFiles), "icebath_config_mod", "high", "persistence", "modification language aimed at icebath settings files (verify intent)"),
		threatOf(shellWriteRe(icebathConfigFiles), "icebath_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting icebath settings files"),
		threatOf(`\.icebath/config\.yaml|\.icebath/SOUL\.md`, "icebath_config_ref", "low", "persistence", "references icebath settings files, only for your information, because only an intent to change them counts"),
		threatOf(proseModifyRe(otherAgentConfigFiles), "other_agent_config_mod", "high", "persistence", "modifies other agents' configuration files"),
		threatOf(shellWriteRe(otherAgentConfigFiles), "other_agent_config_mod_shell", "critical", "persistence", "shell write (redirect/sed -i/tee/cp/mv) targeting other agents' configuration files"),
		threatOf(`\.claude/settings|\.codex/config`, "other_agent_config_ref", "low", "persistence", "references other agent configuration files (informational; only modification intent is scored)"),
		threatOf(`(?:api[_-]?key|token|secret|password)\s*[=:]\s*["'](?!(?-i:[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)["'])[A-Za-z0-9+/=_-]{20,}`, "hardcoded_secret", "critical", "credential_exposure", "possible hardcoded API key, token, or secret"),
		threatOf(`(?-i:-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----)`, "embedded_private_key", "critical", "credential_exposure", "embedded private key"),
		threatOf(`(?-i:ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{80,})`, "github_token_leaked", "critical", "credential_exposure", "GitHub personal access token in skill content"),
		threatOf(`(?-i:sk-[A-Za-z0-9]{20,})`, "openai_key_leaked", "critical", "credential_exposure", "possible OpenAI API key in skill content"),
		threatOf(`(?-i:sk-ant-[A-Za-z0-9_-]{90,})`, "anthropic_key_leaked", "critical", "credential_exposure", "possible Anthropic API key in skill content"),
		threatOf(`(?-i:AKIA[0-9A-Z]{16})`, "aws_access_key_leaked", "critical", "credential_exposure", "AWS access key ID in skill content"),
		threatOf(`(?-i:glpat-[A-Za-z0-9_\-]{20,})`, "gitlab_token_leaked", "critical", "credential_exposure", "GitLab personal access token in skill content"),
		threatOf(`\bDAN\s+mode\b|Do\s+Anything\s+Now`, "jailbreak_dan", "critical", "injection", "DAN (Do Anything Now) jailbreak attempt"),
		threatOf(`\bdeveloper\s+mode\b.*\benabled?\b`, "jailbreak_dev_mode", "critical", "injection", "developer mode jailbreak attempt"),
		threatOf(`hypothetical\s+scenario.*(?:ignore|bypass|override)`, "hypothetical_bypass", "high", "injection", "hypothetical scenario used to bypass restrictions"),
		threatOf(`for\s+educational\s+purposes?\s+only`, "educational_pretext", "medium", "injection", "educational pretext often used to justify harmful content"),
		threatOf(`(respond|answer|reply)\s+without\s+(?:\w+\s+)*(restrictions|limitations|filters|safety)`, "remove_filters", "critical", "injection", "instructs agent to respond without safety filters"),
		threatOf(`you\s+have\s+been\s+(?:\w+\s+)*(updated|upgraded|patched)\s+to`, "fake_update", "high", "injection", "fake update/patch announcement (social engineering)"),
		threatOf(`new\s+(?:\w+\s+)*policy|updated\s+(?:\w+\s+)*guidelines|revised\s+(?:\w+\s+)*instructions`, "fake_policy", "medium", "injection", "claims new policy/guidelines (may be social engineering)"),
		threatOf(`\b(?:include|output|print|send|share)\s+`+noTransfer+shortFiller+`(?:conversation|chat\s+history|previous\s+messages)\b`+
			`|\b(?:print|send|share|output|upload|transmit)\s+`+noTransfer+notDelegate+
			`(?:(?:\w+\s+){0,2}?`+ctxScope+`(?:\w+\s+){0,2}?context\b|(?:\w+\s+){0,3}?context\b`+ctxDest+`|`+ctxDative+`)`, "context_exfil", "high", "exfiltration", "instructs agent to output/share conversation history"),
		threatOf(`(send|post|upload|transmit)\s+.*\s+(to|at)\s+https?://`, "send_to_url", "high", "exfiltration", "instructs agent to send data to a URL"),
	}
}()
