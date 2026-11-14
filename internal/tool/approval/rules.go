package approval

import (
	"regexp"
	"strings"
)

const (
	bt                = "`"
	sshSensitivePath  = `(?:~|\$home|\$\{home\})/\.ssh(?:/|$)`
	homeDirPrefixes   = `(?:~\/\.icebath/|(?:\$home|\$\{home\})/\.icebath/|(?:\$icebath_home|\$\{icebath_home\})/)`
	homeEnvPath       = homeDirPrefixes + `\.env\b`
	homeConfigPath    = homeDirPrefixes + `config\.yaml\b`
	projectEnvPath    = `(?:(?:/|\.{1,2}/)?(?:[^\s/"\'` + bt + `]+/)*\.env(?:\.[^/\s"\'` + bt + `]+)*)`
	projectConfigPath = `(?:(?:/|\.{1,2}/)?(?:[^\s/"\'` + bt + `]+/)*config\.yaml)`
	shellRCFiles      = `(?:~|\$home|\$\{home\})/\.` + `(?:bashrc|zshrc|profile|bash_profile|zprofile)\b`
	credentialFiles   = `(?:~|\$home|\$\{home\})/\.` + `(?:netrc|pgpass|npmrc|pypirc)\b`
	homePrefix        = `(?:~|\$home|\$\{home\})/`
	secretFile        = `(?:` + sshSensitivePath + `|` + homeEnvPath + `|` + credentialFiles + `|` + projectEnvPath + `(?![\w-])` +
		`|` + homePrefix + `\.(?:aws/credentials|git-credentials|config/gh/hosts\.yml|docker/config\.json|kube/config)\b` +
		`|/etc/(?:shadow|passwd)\b|\bid_(?:rsa|ed25519|ecdsa|dsa)\b)`
	secretVar  = `\$\{?\w*(?:key|token|secret|passwd|password|credential)\w*\}?`
	uploadFlag = `(?<!\S)(?:(?-i:-[a-zA-Z]*[dFT])|--data(?:-binary|-raw|-urlencode|-ascii)?|--form(?:-string)?` +
		`|--upload-file|--json|--post-(?:data|file)|--body-(?:data|file))`
	globalFlags           = `(?:-\S++(?:\s++(?!-\S)\S++)?\s++)*`
	containerGlobalFlags  = `(?:-\S++(?:\s(?!-\S)\S++)?\s++)*`
	systemConfigPath      = `(?:/etc/|/private/(?:etc|var|tmp|home)/)`
	sensitiveWriteTarget  = `(?:` + systemConfigPath + `|/dev/sd|` + sshSensitivePath + `|` + homeEnvPath + `|` + homeConfigPath + `|` + shellRCFiles + `|` + credentialFiles + `)`
	userSensitiveTarget   = `(?:` + sshSensitivePath + `|` + shellRCFiles + `|` + credentialFiles + `)`
	projectSensitiveWrite = `(?:` + projectEnvPath + `|` + projectConfigPath + `)`
	commandTail           = `(?:\s*(?:&&|\|\||;).*)?$`
	writeTargetBoundary   = `(?=[\s;&|<>"\']|$)`
	cmdPos                = `(?:^|[\n` + bt + `]|\$\()` + `\s*` + `(?:sudo\s+(?:-[^\s]+\s+)*)?` + `(?:env\s+(?:\w+=\S*\s+)*)?` +
		`(?:(?:exec|nohup|setsid|time)\s+)*` + `\s*`
	hardlineSystemDirs = `/home|/home/\*|/root|/root/\*|/etc|/etc/\*|/usr|/usr/\*|` +
		`/var|/var/\*|/bin|/bin/\*|/sbin|/sbin/\*|/boot|/boot/\*|/lib|/lib/\*`
	rmFlagPrefix  = cmdPos + `rm\s+(-[^\s]*\s+)*`
	pkgOpts       = `(?:-[^\s]+(?:\s+[^-\s][^\s]*)?\s+)*`
	shellNamesAlt = "bash|sh|zsh|ksh|dash"

	invisibleUnicodeDesc  = "invisible or bidirectional Unicode control character (possible obfuscation)"
	gatewaySpliceDesc     = "stop/restart icebath gateway via shell-spliced verb (kills running agents)"
	uploadSecretDesc      = "upload a secret or credential file via curl/wget (possible exfiltration)"
	pipeCredentialDesc    = "pipe a credential file into a curl/wget upload (possible exfiltration)"
	findDynamicDesc       = "find dynamic shell word may expand to destructive flag"
	programDynamicDesc    = "dynamic shell word may expand to arbitrary program execution flag"
	redirectBlockDevDesc  = "redirect to raw block device"
	forkBombDesc          = "fork bomb"
	gatewayOutsideSystemd = "start gateway outside systemd (use 'systemctl --user restart icebath-gateway')"
)

func pyU(hex string) string { return `\` + "u" + hex }

var (
	emojiRangeSrc = `\U0001F000-\U0001FAFF` + pyU("2600") + "-" + pyU("27BF")
	invisibleSrc  = "[" + pyU("200b") + pyU("200c") + pyU("200e") + pyU("200f") + pyU("202a") + "-" + pyU("202e") +
		pyU("2060") + "-" + pyU("2064") + pyU("2066") + "-" + pyU("2069") + pyU("feff") + pyU("180e") +
		`\U000e0000-\U000e007f]` +
		"|(?<![" + emojiRangeSrc + pyU("fe0f") + "])" + pyU("200d") + "|" + pyU("200d") + "(?![" + emojiRangeSrc + "])"
)

func hardlineRmPath(pathAlt string) string {
	return `(?:["\'](?:` + pathAlt + `)["\']|(?:` + pathAlt + `)` + `(?:\s|$|[)` + bt + `;|&])` + `)`
}

type rule struct {
	desc   string
	legacy string
	m      matcher
	code   func(s *subject) bool
	masked bool
}

func (r *rule) hit(s *subject) bool {
	if r.code != nil {
		return r.code(s)
	}
	if _, isVM := r.m.(*machine); isVM {
		return r.m.MatchString(s.low)
	}
	return r.m.MatchString(s.wide)
}

type subject struct {
	low  string
	wide string
	toks []token
	tokd bool
}

func newSubject(low string) *subject {
	return &subject{low: low, wide: wide(low)}
}

func (s *subject) tokens() []token {
	if !s.tokd {
		s.toks = tokenize(s.low)
		s.tokd = true
	}
	return s.toks
}

func wideRule(desc, src string) rule {
	return rule{desc: desc, legacy: legacyKey(src), m: wideRe(src, true)}
}

func vmRule(desc, src string, lookarounds ...string) rule {
	return rule{desc: desc, legacy: legacyKey(src), m: compileMachine(src, lookarounds...)}
}

func codeRule(desc, src string, code func(s *subject) bool) rule {
	return rule{desc: desc, legacy: legacyKey(src), code: code}
}

func maskedRule(r rule) rule {
	r.masked = true
	return r
}

var hardlineRules = []rule{
	wideRule("recursive delete of root filesystem", rmFlagPrefix+hardlineRmPath(`/(?:(?:\.\.?)?/)*(?:\.\.?)?\**|/ \*`)),
	wideRule("recursive delete of system directory", rmFlagPrefix+hardlineRmPath(hardlineSystemDirs)),
	wideRule("recursive delete of home directory", rmFlagPrefix+hardlineRmPath(`(?:~|\$\{?HOME\}?)(?:/?|/\*)?`)),
	wideRule("format filesystem (mkfs)", cmdPos+`mkfs(\.[a-z0-9]+)?\b`),
	wideRule("dd to raw block device", cmdPos+`dd\b[^\n]*\bof=/dev/(sd|nvme|hd|mmcblk|vd|xvd)[a-z0-9]*`),
	maskedRule(wideRule(redirectBlockDevDesc, `>\s*/dev/(sd|nvme|hd|mmcblk|vd|xvd)[a-z0-9]*\b`)),
	maskedRule(wideRule(forkBombDesc, `:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)),
	wideRule("kill all processes", cmdPos+`kill\s+(-[^\s]+\s+)*-1\b`),
	wideRule("system shutdown/reboot", cmdPos+`(shutdown|reboot|halt|poweroff)\b`),
	wideRule("init 0/6 (shutdown/reboot)", cmdPos+`init\s+[06]\b`),
	wideRule("systemctl poweroff/reboot", cmdPos+`systemctl\s+(poweroff|reboot|halt|kexec)\b`),
	wideRule("telinit 0/6 (shutdown/reboot)", cmdPos+`telinit\s+[06]\b`),
}

var (
	deleteFromRe    = wideRe(`\bDELETE\s+FROM\b`, true)
	whereRe         = wideRe(`\bWHERE\b`, true)
	launchctlVerbRe = wideRe(`\blaunchctl\s+(?:stop|kickstart|bootout|unload|kill|disable|remove)\b`, true)
	launchctlNameRe = wideRe(`\b(?:icebath|ai\.icebath)\b`, true)
)

var dangerousRules = []rule{
	wideRule("delete in root path", `\brm\s+(-[^\s]*\s+)*/`),
	wideRule("recursive delete", `\brm\s+-[^\s]*r`),
	wideRule("recursive delete (long flag)", `\brm\s+--recursive\b`),
	vmRule("recursive delete (flags after operands)",
		`\brm\s+(?!--(?:\s|$))(?:(?!\s--(?:\s|$))[^\n"\';|&])*\s`+`(?:-[a-z]*r[a-z]*\b|--recursive\b)`,
		`(?!--(?:\s|$))`, ph(asNotDashDashEnd), `(?!\s--(?:\s|$))`, ph(asNotSpaceDashDashEnd)),
	wideRule("Windows cmd destructive delete", `\bcmd(?:\.exe)?\s+/(?:c|k)\s+.*\b(?:del|erase|rd|rmdir)\b`),
	wideRule("Windows PowerShell destructive delete", `\b(?:powershell|pwsh)(?:\.exe)?\b(?:\s+-\S+)*\s+(?:-(?:command|c)\s+)?["\']?(?:remove-item|rmdir|erase|del|rd|ri|rm)\b`),
	wideRule("PowerShell encoded command execution", `\b(?:powershell|pwsh)(?:\.exe)?\b.*\s-(?:encodedcommand|enc|e)\b`),
	wideRule("PowerShell destructive delete (Remove-Item)", `\bremove-item\b[^\n;|&]*\s-(?:recurse|force)\b`),
	wideRule("Windows destructive delete (recursive/quiet switch)", `\b(?:del|erase|rd|rmdir)\s+(?:/[a-z]\s+)*/[sq]\b`),
	wideRule("pipe remote content to PowerShell (iwr | iex)", `\b(?:iwr|invoke-webrequest|invoke-restmethod|irm|curl|wget)\b[^\n]*\|\s*(?:iex|invoke-expression)\b`),
	wideRule("execute remote content via Invoke-Expression", `\b(?:iex|invoke-expression)\s*\(\s*(?:iwr|invoke-webrequest|invoke-restmethod|irm)\b`),
	wideRule("force kill processes (taskkill /F)", `\btaskkill\b[^\n]*\s/f\b`),
	wideRule("force kill processes (Stop-Process -Force)", `\bstop-process\b[^\n]*\s-force\b`),
	wideRule("format filesystem (Format-Volume)", `\bformat-volume\b`),
	wideRule("wipe disk (Clear-Disk)", `\bclear-disk\b`),
	wideRule("disk partitioning (diskpart)", `\bdiskpart\b`),
	wideRule("format drive (format.com)", `\bformat(?:\.com)?\s+[a-z]:`),
	wideRule("wipe free space (cipher /w)", `\bcipher\s+/w\b`),
	wideRule("grant Everyone access (icacls)", `\bicacls\b[^\n]*\s/grant\b[^\n]*\b(?:everyone|todos|jeder|tout\s+le\s+monde|\*s-1-1-0)\b`),
	wideRule("reset ACLs recursively (icacls /reset)", `\bicacls\b[^\n]*\s/reset\b`),
	wideRule("delete volume shadow copies (vssadmin)", `\bvssadmin\b[^\n]*\bdelete\s+shadows\b`),
	wideRule("delete backups (wbadmin)", `\bwbadmin\b[^\n]*\bdelete\b`),
	wideRule("modify boot configuration (bcdedit /set)", `\bbcdedit\b[^\n]*\s/set\b`),
	wideRule("registry delete (reg delete)", `\breg(?:\.exe)?\s+delete\b`),
	wideRule("registry value delete (Remove-ItemProperty -Force)", `\bremove-itemproperty\b[^\n]*\s-force\b`),
	wideRule("force stop service (Stop-Service -Force)", `\bstop-service\b[^\n]*\s-force\b`),
	wideRule("stop/delete service (sc)", `\bsc(?:\.exe)?\s+(?:stop|delete)\b`),
	wideRule("access to SSH keys (Windows path)", `\busers[\\/][^\\/\s]+[\\/]\.ssh\b`),
	wideRule("access to icebath secrets (Windows path)", `\bappdata[\\/](?:local|roaming)[\\/]icebath[^\n]*\.env\b`),
	wideRule("world/other-writable permissions", `\bchmod\s+(-[^\s]*\s+)*(777|666|o\+[rwx]*w|a\+[rwx]*w)\b`),
	wideRule("recursive world/other-writable (long flag)", `\bchmod\s+--recursive\b.*(777|666|o\+[rwx]*w|a\+[rwx]*w)`),
	wideRule("recursive chown to root", `\bchown\s+(-[^\s]*)?R\s+root`),
	wideRule("recursive chown to root (long flag)", `\bchown\s+--recur[a-z]*\b.*root`),
	wideRule("format filesystem", cmdPos+`mkfs\b`),
	wideRule("disk copy", cmdPos+`dd\s+.*if=`),
	wideRule("write to block device", `>\s*/dev/sd`),
	wideRule("SQL DROP", `\bDROP\s+(TABLE|DATABASE)\b`),
	codeRule("SQL DELETE without WHERE", `\bDELETE\s+FROM\b(?![^\n]*\bWHERE\b)`, deleteWithoutWhere),
	wideRule("SQL TRUNCATE", `\bTRUNCATE\s+(TABLE)?\s*\w`),
	wideRule("overwrite system config", `>\s*`+systemConfigPath),
	wideRule("stop/restart system service", `\bsystemctl\s+(-[^\s]+\s+)*(stop|restart|disable|mask)\b`),
	wideRule("kill all processes", `\bkill\s+-9\s+-1\b`),
	wideRule("force kill processes", `\bpkill\s+-9\b`),
	wideRule("force kill processes (killall -KILL)", `\bkillall\s+(-[^\s]*\s+)*-(9|KILL|SIGKILL)\b`),
	wideRule("force kill processes (killall -s KILL)", `\bkillall\s+(-[^\s]*\s+)*-s\s+(KILL|SIGKILL|9)\b`),
	wideRule("kill processes by regex (killall -r)", `\bkillall\s+(-[^\s]*\s+)*-r\b`),
	wideRule(forkBombDesc, `:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
	wideRule("pipe remote content to shell", `\b(curl|wget)\b.*\|\s*(?:[/\w]*/)?(?:`+shellNamesAlt+`)(?:\s|$|-c)`),
	wideRule("execute remote script via process substitution", `\b(?:`+shellNamesAlt+`)\s+<\s*<?\s*\(\s*(curl|wget)\b`),
	wideRule("execute remote content via command substitution", `(?:\beval\b|\bsource\b|\.)\s*(?:\$\(\s*|`+bt+`\s*)(?:curl|wget)\b`),
	vmRule("cloud metadata endpoint access (instance credentials)",
		`(?<![\d.])(?:169\.254\.169\.254|100\.100\.100\.200)(?![\d.])`+
			`|(?<![\w.-])metadata\.google\.internal(?![\w.-])`+
			`|fd00:ec2::254`,
		`(?<![\d.])`, ph(asPrevNotDigitDot), `(?![\d.])`, ph(asNextNotDigitDot),
		`(?<![\w.-])`, ph(asPrevNotWordDotDash), `(?![\w.-])`, ph(asNextNotWordDotDash)),
	wideRule("pipe decoded content to shell (possible command obfuscation)", `\b(base64|base32|base16)\s+(?:-[dD]|--decode)\b.*\|\s*\b(?:`+shellNamesAlt+`)\b`),
	wideRule("pipe xxd-decoded content to shell (possible command obfuscation)", `\bxxd\s+-r\b.*\|\s*\b(?:`+shellNamesAlt+`)\b`),
	wideRule("pipe tr-transformed output to shell (possible command obfuscation)", `\becho\b[^|]*\|\s*\btr\b[^|]*\|\s*\b(?:`+shellNamesAlt+`)\b`),
	wideRule("pipe openssl-decoded content to shell (possible command obfuscation)", `\bopenssl\b.*\b(?:base64|enc)\b[^|]*\s+-[dD]\b[^|]*\|\s*\b(?:`+shellNamesAlt+`)\b`),
	vmRule(uploadSecretDesc,
		uploadFlag+`(?:[\s=]+|(?=[@"\'$]))(?:"[^"]*(?:`+secretVar+`|`+secretFile+`)|\'[^\']*`+secretFile+
			`|\$\([^)]*`+secretFile+`|\S*?(?:`+secretVar+`|`+secretFile+`))`,
		`(?<!\S)`, ph(asPrevSpace), `(?=[@"\'$])`, ph(asNextUploadQuote), `(?![\w-])`, ph(asNextNotWordDash)),
	vmRule(pipeCredentialDesc,
		secretFile+`[^|;&\n]*\|[^;&|\n]*\b(?:curl|wget)\b[^;&|\n]*`+uploadFlag,
		`(?<!\S)`, ph(asPrevSpace), `(?![\w-])`, ph(asNextNotWordDash)),
	vmRule(invisibleUnicodeDesc, invisibleSrc,
		"(?<!["+emojiRangeSrc+pyU("fe0f")+"])", ph(asPrevNotEmoji), "(?!["+emojiRangeSrc+"])", ph(asNextNotEmoji)),
	wideRule("overwrite system file via tee", `\btee\b.*["\']?`+sensitiveWriteTarget),
	wideRule("overwrite system file via redirection", `>>?\s*["\']?`+sensitiveWriteTarget),
	vmRule("overwrite project env/config via tee", `\btee\b.*["\']?`+projectSensitiveWrite+`["\']?`+writeTargetBoundary,
		writeTargetBoundary, ph(asNextTargetEnd)),
	vmRule("overwrite project env/config via redirection", `>>?\s*["\']?`+projectSensitiveWrite+`["\']?`+writeTargetBoundary,
		writeTargetBoundary, ph(asNextTargetEnd)),
	wideRule("xargs with rm", `\bxargs\s+.*\brm\b`),
	wideRule("find -exec/-execdir rm", `\bfind\b.*-exec(?:dir)?\s+(/\S*/)?rm\b`),
	maskedRule(vmRule(findDynamicDesc,
		cmdPos+`find\s[^;|&\n]*(?<!\S)-(?:\{[^}\s]*(?:delete|exec(?:dir)?)[^}\s]*\}|(?:del(?:ete?)?|exec(?:dir)?)[*?\[])`,
		`(?<!\S)`, ph(asPrevSpace))),
	wideRule("find -delete", `\bfind\b.*-delete\b`),
	maskedRule(vmRule(programDynamicDesc,
		`\b(?:rg|sort|ag|man)\b[^;|&\n]*(?<!\S)--(?:pre|hostname-bin|compress-program|pager|html)(?:\{|[*?\[])`,
		`(?<!\S)`, ph(asPrevSpace))),
	codeRule("stop/restart icebath gateway (kills running agents)", `\bicebath\s+`+globalFlags+`gateway\s+(stop|restart)\b`, gatewayLifecycleFlags),
	wideRule("icebath update (restarts gateway, kills running agents)", `\bicebath\s+update\b`),
	codeRule("docker with remote daemon redirect (-H/--host)", `\bdocker\s+`+containerGlobalFlags+`(?:-h|--host)[=\s]+\S+`,
		containerRule(dockerSeeds, valueTarget("-h", "--host"))),
	codeRule("docker with daemon redirect (--context: alternate daemon)", `\bdocker\s+`+containerGlobalFlags+`(?:-c|--context)[=\s]+\S+`,
		containerRule(dockerSeeds, valueTarget("-c", "--context"))),
	wideRule("docker context use (switches default daemon for future commands)", `\bdocker\s+context\s+use\b`),
	codeRule("podman with remote daemon redirect (--url/--connection/--identity)", `\bpodman\s+`+containerGlobalFlags+`(?:--url|--connection|--identity)[=\s]+\S+`,
		containerRule(podmanSeeds, valueTarget("--url", "--connection", "--identity"))),
	codeRule("podman remote mode (-r/--remote: remote daemon)", `\bpodman\s+`+containerGlobalFlags+`(?:-r\b|--remote\b)`,
		containerRule(podmanSeeds, wordTarget("-r", "--remote"))),
	wideRule("docker/podman daemon redirect via environment (DOCKER_HOST/CONTAINER_HOST)", `\b(?:docker_host|docker_context|container_host|container_connection)=\S+`),
	codeRule("docker compose restart/stop/kill/down (container lifecycle)", `\bdocker(?:-compose|\s+compose)\s+`+containerGlobalFlags+`(restart|stop|kill|down)\b`,
		containerRule(composeSeeds, wordTarget("restart", "stop", "kill", "down"))),
	codeRule("docker restart/stop/kill (container lifecycle)", `\bdocker\s+`+containerGlobalFlags+`(restart|stop|kill)\b`,
		containerRule(dockerSeeds, wordTarget("restart", "stop", "kill"))),
	wideRule(gatewayOutsideSystemd, `gateway\s+run\b.*(&\s*$|&\s*;|\bdisown\b|\bsetsid\b)`),
	wideRule(gatewayOutsideSystemd, `\bnohup\b.*gateway\s+run\b`),
	wideRule("kill icebath/gateway process (self-termination)", `\b(pkill|killall)\b.*\b(icebath|gateway|cli\.py)\b`),
	wideRule("kill process via pgrep/pidof expansion (self-termination)", `\bkill\b.*\$\(\s*(pgrep|pidof)\b`),
	wideRule("kill process via backtick pgrep/pidof expansion (self-termination)", `\bkill\b.*`+bt+`\s*(pgrep|pidof)\b`),
	codeRule("stop/restart icebath launchd service (kills running agents)",
		`\A(?=[\s\S]*\blaunchctl\s+(?:stop|kickstart|bootout|unload|kill|disable|remove)\b)(?=[\s\S]*\b(?:icebath|ai\.icebath)\b)`,
		func(s *subject) bool {
			return launchctlVerbRe.MatchString(s.wide) && launchctlNameRe.MatchString(s.wide)
		}),
	wideRule("copy/move file into system config path", `\b(cp|mv|install)\b.*\s`+systemConfigPath),
	wideRule("overwrite project env/config file", `\b(cp|mv|install)\b.*\s["\']?`+projectSensitiveWrite+`["\']?`+commandTail),
	wideRule("copy/move file into sensitive credential/SSH/shell-rc path", `\b(cp|mv|install)\b.*\s["\']?`+sensitiveWriteTarget+`[^\s"\']*["\']?`+commandTail),
	wideRule("in-place edit of sensitive credential/SSH/shell-rc path", `\bsed\s+-[^\s]*i.*(?:`+userSensitiveTarget+`)[^\s"\']*`),
	wideRule("in-place edit of sensitive credential/SSH/shell-rc path (long flag)", `\bsed\s+--in-place\b.*(?:`+userSensitiveTarget+`)[^\s"\']*`),
	wideRule("in-place edit of sensitive credential/SSH/shell-rc path (perl/ruby)", `\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:`+userSensitiveTarget+`)[^\s"\']*`),
	wideRule("in-place edit of system config", `\bsed\s+-[^\s]*i.*\s`+systemConfigPath),
	wideRule("in-place edit of system config (long flag)", `\bsed\s+--in-place\b.*\s`+systemConfigPath),
	wideRule("in-place edit of icebath config/env", `\bsed\s+-[^\s]*i.*(?:`+homeConfigPath+`|`+homeEnvPath+`)`),
	wideRule("in-place edit of icebath config/env (long flag)", `\bsed\s+--in-place\b.*(?:`+homeConfigPath+`|`+homeEnvPath+`)`),
	wideRule("in-place edit of icebath config/env (perl/ruby)", `\b(?:perl|ruby)\b.*(?:^|\s)-[^\s]*i\b.*(?:`+homeConfigPath+`|`+homeEnvPath+`)`),
	wideRule("shell execution via heredoc", `\b(?:`+shellNamesAlt+`)\s+<<`),
	wideRule("git reset --hard (destroys uncommitted changes)", `\bgit\s+reset\s+--h(?:a(?:r(?:d)?)?)?\b`),
	wideRule("git force push (rewrites remote history)", `\bgit\s+push\b.*--forc[a-z]*\b`),
	wideRule("git force push short flag (rewrites remote history)", `\bgit\s+push\b.*-f\b`),
	wideRule("git clean with force (deletes untracked files)", `\bgit\s+clean\s+-[^\s]*f`),
	wideRule("git branch force delete", `\bgit\s+branch\s+(?-i:-D)\b`),
	wideRule("git branch force delete (long flags)", `\bgit\s+branch\b[^;|&\n]*?(?:-d\b|--delete\b)[^;|&\n]*?(?:-f\b|--force\b)`),
	wideRule("git branch force delete (long flags, force-first)", `\bgit\s+branch\b[^;|&\n]*?(?:-f\b|--force\b)[^;|&\n]*?(?:-d\b|--delete\b)`),
	wideRule("chmod +x followed by immediate execution", `\bchmod\s+\+x\b.*[;&|]+\s*\./`),
	wideRule("sudo with privilege flag (stdin/askpass/shell/list)", `\bsudo\b[^;|&\n]*?\s+(?:-s\b|--st[a-z]*\b|-a\b|--a[a-z]*\b)`),
	wideRule("sudo with combined-flag privilege escalation", `\bsudo\b[^;|&\n]*?\s+-[a-z]*[sa][a-z]*\b`),
	wideRule("package manager uninstall", cmdPos+`npm\s+`+pkgOpts+`(?:uninstall|unlink|remove|rm|r|un)\b`),
	wideRule("package manager uninstall", cmdPos+`pnpm\s+`+pkgOpts+`(?:uninstall|remove|rm|un)\b`),
	wideRule("package manager uninstall", cmdPos+`yarn\s+`+pkgOpts+`(?:global\s+)?(?:uninstall|remove)\b`),
	wideRule("package manager uninstall", cmdPos+`pip(?:3)?\s+`+pkgOpts+`uninstall\b`),
	wideRule("package manager uninstall", cmdPos+`brew\s+`+pkgOpts+`(?:uninstall|remove|rm)\b`),
}

var contentRuleDescs = set(uploadSecretDesc, pipeCredentialDesc, invisibleUnicodeDesc)

var keyAliases = buildKeyAliases()

func buildKeyAliases() map[string][]string {
	sets := map[string][]string{}
	add := func(key string, members ...string) {
		for _, m := range members {
			if !contains(sets[key], m) {
				sets[key] = append(sets[key], m)
			}
		}
	}
	pairs := make([][2]string, 0, len(dangerousRules)+2)
	for _, r := range dangerousRules {
		pairs = append(pairs, [2]string{r.desc, r.legacy})
	}
	pairs = append(pairs,
		[2]string{scriptFlagDesc, `(python[23]?|perl|ruby|node)\s+-[ec]\s+`},
		[2]string{scriptHeredocDesc, `(python[23]?|perl|ruby|node)\s+<<`})
	for _, p := range pairs {
		add(p[0], p[0], p[1])
		add(p[1], p[1], p[0])
	}
	oldBranch := `git\s+branch\s+-D`
	add("git branch force delete", oldBranch)
	add(oldBranch, "git branch force delete")
	return sets
}

func deleteWithoutWhere(s *subject) bool {
	deletes := deleteFromRe.FindAllStringIndex(s.wide, -1)
	if len(deletes) == 0 {
		return false
	}
	wheres := whereRe.FindAllStringIndex(s.wide, -1)
	w, lineEnd := 0, -1
	for _, d := range deletes {
		e := d[1]
		for w < len(wheres) && wheres[w][0] < e {
			w++
		}
		if lineEnd < e {
			if k := strings.IndexByte(s.wide[e:], '\n'); k < 0 {
				lineEnd = len(s.wide)
			} else {
				lineEnd = e + k
			}
		}
		if w >= len(wheres) || wheres[w][0] >= lineEnd {
			return true
		}
	}
	return false
}

var sudoStdinRe = wideRe(`(?:^|[;&|`+bt+`\n]|&&|\|\||\$\()\s*sudo\s+-S\b`, true)

var verificationNameRe = regexp.MustCompile(`\Aicebath-(?:verify|ad-hoc)-[A-Za-z0-9_.-]+\z`)
