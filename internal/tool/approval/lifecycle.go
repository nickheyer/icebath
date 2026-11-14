package approval

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/shell"
)

const HostImageKillRejection = "Blocked: this command kills every process whose image/name matches the icebath " +
	"executable, which is the process hosting this gateway (and this command). " +
	"Stop only the process you own instead: process(action=\"kill\", session_id=\"proc_…\") " +
	"for a background job icebath started, or kill/taskkill by its explicit PID."

var (
	gatewayLifecycleRe = wideRe(
		`(?:(?:^|[^/\w.\-])icebath(?:\.(?:exe|cmd|bat|com|ps1))?\s+gateway\s+(?:restart|stop|uninstall)\b)`+
			`|(?:launchctl\s+(?:kickstart|unload|load|stop|restart|submit|bootstrap|bootout|remove|disable)\b[^\n]*\bicebath[.\-]?gateway)`+
			`|(?:systemctl\s+(?:-\S+\s+)*(?:restart|stop|start)\b[^\n]*\bicebath[.\-]?gateway)`+
			`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bicebath\b[^\n]*\bgateway)`+
			`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bgateway\b[^\n]*\bicebath)`, true)
	lineContinuationRe   = regexp.MustCompile(`\\\r?\n[ \t]*`)
	argvPunctuationRe    = regexp.MustCompile(`[\[\],]+`)
	launchctlLifecycleRe = wideRe(`\blaunchctl\s+(?:kickstart|unload|load|stop|restart|bootout|kill|disable|remove)\b`, true)
	gatewayLabelRe       = wideRe(`\bicebath[.\-]?gateway\b`, true)
	killVerbRe           = wideRe(`\b(?:kill|taskkill|stop-process)\b`, true)
	gatewayCmdlineRe     = wideRe(`\bicebath\b[^\n]*\bgateway\b|\bgateway\b[^\n]*\bicebath\b`, true)
	segmentLexer         = shell.Lexer{Comments: true, Operators: ";&|()"}
	nameKillers          = set("pkill", "killall", "taskkill", "stop-process")
	nameEnumerators      = set("pgrep", "pidof", "get-process")
	killerValueOptions   = map[string]map[string]bool{
		"pkill": set("-g", "--pgroup", "-G", "--group", "-P", "--parent", "-s", "--session", "-t", "--terminal",
			"-u", "--euid", "-U", "--uid", "-F", "--pidfile", "--ns", "--nslist", "-d", "--delimiter",
			"--signal", "-O", "--older", "-r", "--runstates", "--cgroup", "-A", "--ignore-ancestors"),
		"killall": set("-s", "--signal", "-u", "--user", "-o", "--older-than", "-y", "--younger-than",
			"-n", "--ns", "-Z", "--context"),
		"pidof": set("-o", "--omit-pid", "-S", "--separator"),
	}
	transparentPrefixes = set("sudo", "doas", "env", "nohup", "setsid", "nice", "ionice", "stdbuf",
		"timeout", "exec", "command", "builtin", "eatmydata",
		"pkexec", "su", "runuser", "setpriv", "systemd-run", "nsenter", "unshare")
	transparentValueOptions = map[string]map[string]bool{
		"sudo":    set("-u", "-g", "-U", "-C", "-p", "-r", "-t", "-T", "--user", "--group", "--prompt"),
		"doas":    set("-u", "-C"),
		"env":     set("-u", "--unset", "-S", "--split-string", "-C", "--chdir"),
		"nice":    set("-n", "--adjustment"),
		"ionice":  set("-c", "-n", "-p", "--class", "--classdata"),
		"stdbuf":  set("-i", "-o", "-e", "--input", "--output", "--error"),
		"timeout": set("-s", "-k", "--signal", "--kill-after"),
		"pkexec":  set("--user"),
		"su":      set("-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"runuser": set("-u", "--user", "-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"setpriv": set("--reuid", "--regid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set",
			"--selinux-label", "--apparmor-profile"),
		"systemd-run": set("-u", "--unit", "-p", "--property", "-E", "--setenv", "--slice",
			"--description", "--uid", "--gid", "--on-calendar", "--service-type"),
		"nsenter": set("-t", "--target", "-S", "--setuid", "-G", "--setgid", "-r", "--root", "-w", "--wd"),
		"unshare": set("--map-user", "--map-group", "--setgroups", "-R", "--root", "-w", "--wd"),
	}
)

func (d *Detector) GatewayLifecycle(text string) bool {
	if text == "" {
		return false
	}
	text = StripInertHeredocs(text)
	normalized := lineContinuationRe.ReplaceAllString(text, " ")
	if gatewayLifecycleRe.MatchString(wide(normalized)) {
		return true
	}
	if named, ok := profileFlagLifecycle(normalized); ok && named != "" {
		named = strings.Trim(pyStrip(named), "\"'")
		if d.profile != "" && casefold(pyStrip(named)) == casefold(pyStrip(d.profile)) {
			return true
		}
	}
	for _, segment := range commandSegments(normalized) {
		joined := strings.Join(segment, " ")
		if joined != "" && gatewayLifecycleRe.MatchString(wide(joined)) {
			return true
		}
		if stripped := argvPunctuationRe.ReplaceAllString(joined, " "); stripped != joined && gatewayLifecycleRe.MatchString(wide(stripped)) {
			return true
		}
	}
	w := wide(normalized)
	return (launchctlLifecycleRe.MatchString(w) && gatewayLabelRe.MatchString(w)) || d.HostImageKill(normalized)
}

type profileHit struct {
	named string
	ok    bool
}

func profileFlagLifecycle(text string) (string, bool) {
	toks := tokenize(text)
	n := len(toks)
	target := func(k int) bool {
		if k >= n || k+1 >= n || !foldEqual(toks[k].text, "gateway") {
			return false
		}
		_, restart := foldPrefix(toks[k+1].text, "restart")
		_, stop := foldPrefix(toks[k+1].text, "stop")
		return restart || stop
	}
	reach0, reach1 := make([]bool, n+2), make([]bool, n+2)
	for k := n - 1; k >= 0; k-- {
		flag := isFlagToken(toks[k].text)
		reach0[k] = target(k) || (flag && reach1[k+1])
		reach1[k] = target(k) || (flag && reach1[k+1]) || (!flag && reach0[k+1])
	}
	selector := func(i int) profileHit {
		t := toks[i].text
		if p, ok := foldPrefix(t, "--profile="); ok && p < len(t) && reach0[i+1] {
			return profileHit{t[p:], true}
		}
		if (foldEqual(t, "-p") || foldEqual(t, "--profile")) && i+1 < n && reach0[i+2] {
			return profileHit{toks[i+1].text, true}
		}
		return profileHit{}
	}
	res := make([]profileHit, n+2)
	for i := n - 1; i >= 0; i-- {
		var r profileHit
		if isFlagToken(toks[i].text) {
			if i+1 < n && !isFlagToken(toks[i+1].text) && res[i+2].ok {
				r = res[i+2]
			} else if res[i+1].ok {
				r = res[i+1]
			}
		}
		if !r.ok {
			r = selector(i)
		}
		res[i] = r
	}
	for k := 0; k+1 < n; k++ {
		if _, ok := foldSuffix(toks[k].text, "icebath"); ok && res[k+1].ok {
			return res[k+1].named, true
		}
	}
	return "", false
}

func splitLogicalLines(text string) []string {
	var lines []string
	var cur strings.Builder
	inSingle, inDouble, escape := false, false, false
	started := false
	for _, ch := range text {
		switch {
		case escape:
			escape = false
		case ch == '\\':
			escape = true
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case ch == '\n' && !inSingle && !inDouble:
			lines = append(lines, cur.String())
			cur.Reset()
			started = false
			continue
		}
		cur.WriteRune(ch)
		started = true
	}
	if started {
		lines = append(lines, cur.String())
	}
	return lines
}

func pySplitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, w := runeAt(s, i)
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			start = i + w
		case '\r':
			lines = append(lines, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				w = 2
			}
			start = i + w
		}
		i += w
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitSegments(tokens []string) [][]string {
	var out [][]string
	var seg []string
	for _, t := range tokens {
		if t != "" && strings.Trim(t, ";&|()") == "" {
			if len(seg) > 0 {
				out = append(out, seg)
				seg = nil
			}
			continue
		}
		seg = append(seg, t)
	}
	if len(seg) > 0 {
		out = append(out, seg)
	}
	return out
}

func commandSegments(command string) [][]string {
	var out [][]string
	for _, line := range splitLogicalLines(strings.ReplaceAll(command, "\\\n", "")) {
		tokens, err := segmentLexer.Split(line)
		if err != nil {
			for _, physical := range pySplitLines(line) {
				if t, err := segmentLexer.Split(physical); err == nil {
					out = append(out, splitSegments(t)...)
				}
			}
			continue
		}
		out = append(out, splitSegments(tokens)...)
	}
	return out
}

func executableName(token string) string {
	if name := pathName(token); name != "" {
		return name
	}
	return token
}

func peelTransparentPrefixes(segment []string, index int) int {
	for range 8 {
		if index >= len(segment) {
			return index
		}
		name := executableName(segment[index])
		if !transparentPrefixes[name] {
			return index
		}
		valueOptions := transparentValueOptions[name]
		index++
		for index < len(segment) {
			token := segment[index]
			if token == "--" {
				index++
				break
			}
			if valueOptions[token] {
				index += 2
				continue
			}
			if strings.HasPrefix(token, "-") || envAssignmentPrefix(token) {
				index++
				continue
			}
			break
		}
		if name == "timeout" && index < len(segment) && !strings.HasPrefix(segment[index], "-") {
			index++
		}
	}
	return index
}

func executedCommandIndex(segment []string) int {
	index := -1
	for i, t := range segment {
		if !envAssignmentPrefix(t) {
			index = i
			break
		}
	}
	if index < 0 {
		return -1
	}
	index = peelTransparentPrefixes(segment, index)
	if index < len(segment) {
		return index
	}
	return -1
}

func (d *Detector) isHostImage(value string, substring bool) bool {
	name := strings.TrimSuffix(pyLower(strings.Trim(pyStrip(value), "\"'")), ".exe")
	if name == "" {
		return false
	}
	if strings.HasSuffix(name, "*") {
		prefix := strings.TrimRight(name, "*")
		return strings.HasPrefix("icebath", prefix) || strings.HasPrefix(d.image, prefix)
	}
	if name == "icebath" || name == strings.TrimSuffix(d.image, ".exe") {
		return true
	}
	return substring && runeCount(name) >= 2 && strings.HasPrefix("icebath", name)
}

func ereTail(head string) int {
	lastBad := -1
	for i := 0; i < len(head); i++ {
		if head[i] == '\n' && i != len(head)-1 {
			lastBad = i
		}
	}
	for i := lastBad + 1; i < len(head); i++ {
		if strings.IndexByte(`.*+?\[](){}|`, head[i]) >= 0 {
			return i
		}
	}
	return -1
}

func wildcardOnly(rest string) bool {
	for _, r := range rest {
		if !strings.ContainsRune(".*+?$", r) && !pySpace(r) {
			return false
		}
	}
	return true
}

func (d *Detector) patternReachesHost(pattern string, fullCmdline, exact bool) bool {
	core := strings.TrimLeft(strings.Trim(pyStrip(pattern), "\"'"), "^")
	core = strings.TrimSuffix(core, "$")
	head, rest := core, ""
	if fullCmdline {
		head, rest, _ = strings.Cut(core, " ")
	}
	if !d.isHostImage(head, !exact) {
		if m := ereTail(head); m >= 0 {
			rest = head[m:] + " " + rest
			head = head[:m]
		}
	}
	if !d.isHostImage(head, !exact) {
		return fullCmdline && gatewayCmdlineRe.MatchString(wide(core))
	}
	return pyStrip(rest) == "" || wildcardOnly(rest) || strings.Contains(pyLower(rest), "icebath")
}

func imageNameFilter(value string) (string, bool) {
	i := skipSpace(value, 0)
	if i < len(value) && (value[i] == '\'' || value[i] == '"') {
		i++
	}
	n, ok := foldPrefix(value[i:], "imagename")
	if !ok {
		return "", false
	}
	i += n
	j := skipSpace(value, i)
	if j == i {
		return "", false
	}
	n, ok = foldPrefix(value[j:], "eq")
	if !ok {
		return "", false
	}
	j += n
	k := skipSpace(value, j)
	if k == j {
		return "", false
	}
	end := k
	for end < len(value) && !spaceAt(value, end) {
		end += runeWidth(value, end)
	}
	if end == k {
		return "", false
	}
	return value[k:end], true
}

func (d *Detector) killerTargetsHost(name string, args []string) bool {
	switch name {
	case "pkill", "pgrep", "killall", "pidof":
		hasAny := func(opts ...string) bool {
			for _, a := range args {
				if contains(opts, a) {
					return true
				}
			}
			return false
		}
		exact := name == "pidof" || ((name == "killall") != hasAny("-r", "--regexp", "-x", "--exact"))
		full := (name == "pkill" || name == "pgrep") && hasAny("-f", "--full")
		valueOptions := killerValueOptions[name]
		if name == "pgrep" {
			valueOptions = killerValueOptions["pkill"]
		}
		var operands []string
		for position := 0; position < len(args); {
			token := args[position]
			if token == "--" {
				operands = append(operands, args[position+1:]...)
				break
			}
			if valueOptions[token] {
				position += 2
				continue
			}
			if !strings.HasPrefix(token, "-") {
				operands = append(operands, token)
			}
			position++
		}
		for _, op := range operands {
			if d.patternReachesHost(op, full, exact) {
				return true
			}
		}
		return false
	case "taskkill":
		for position := 0; position+1 < len(args); position++ {
			option := strings.TrimLeft(pyLower(args[position]), "/-")
			value := args[position+1]
			if option == "im" && d.isHostImage(value, false) {
				return true
			}
			if option == "fi" {
				if image, ok := imageNameFilter(value); ok && d.isHostImage(image, false) {
					return true
				}
			}
		}
		return false
	}
	var values []string
	for position, token := range args {
		option, inline, _ := strings.Cut(token, ":")
		switch pyLower(option) {
		case "-name", "-processname", "-n":
			switch {
			case inline != "":
				values = append(values, inline)
			case position+1 < len(args):
				values = append(values, args[position+1])
			default:
				values = append(values, "")
			}
			continue
		}
		if name == "get-process" && !strings.HasPrefix(token, "-") && (position == 0 || !strings.HasPrefix(args[position-1], "-")) {
			values = append(values, token)
		}
	}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if d.isHostImage(part, false) {
				return true
			}
		}
	}
	return false
}

func killerName(token string) string {
	return strings.TrimSuffix(pyLower(executableName(token)), ".exe")
}

func (d *Detector) segmentNamesHost(tokens []string, killers map[string]bool) bool {
	var candidates []int
	if index := executedCommandIndex(tokens); index >= 0 {
		candidates = append(candidates, index)
	}
	for i, t := range tokens {
		if killers[killerName(t)] {
			candidates = append(candidates, i)
		}
	}
	for _, position := range candidates {
		name := killerName(tokens[position])
		if killers[name] && d.killerTargetsHost(name, tokens[position+1:]) {
			return true
		}
	}
	return false
}

func (d *Detector) HostImageKill(text string) bool {
	normalized := lineContinuationRe.ReplaceAllString(text, " ")
	killVerb := killVerbRe.MatchString(wide(normalized))
	for _, segment := range commandSegments(normalized) {
		joined := strings.Join(segment, " ")
		stripped := argvPunctuationRe.ReplaceAllString(joined, " ")
		sets := [][]string{segment}
		if stripped != joined {
			sets = append(sets, pyFields(stripped))
		}
		for _, tokens := range sets {
			if d.segmentNamesHost(tokens, nameKillers) {
				return true
			}
			if killVerb && d.segmentNamesHost(tokens, nameEnumerators) {
				return true
			}
		}
	}
	return false
}
