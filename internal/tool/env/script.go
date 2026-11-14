package env

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/shell"
)

const snapTmpSuffix = ".tmp.XXXXXXXXXX"

const snapTmp = `"$__icebath_snap_tmp"`

var shellEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidShellEnvName(name string) bool {
	return shellEnvName.MatchString(name)
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:n]
}

func cwdMarker(sessionID string) string {
	return "__ICEBATH_CWD_" + sessionID + "__"
}

func pipeMarker(sessionID string) string {
	return "__ICEBATH_PIPE_" + sessionID + "__"
}

func pipeMarkerPrintf(marker string) string {
	return `printf '\n` + marker + `%s` + marker + `\n' "${__icebath_ps[*]-}"`
}

func evalCapturingPipestatus(escaped string) string {
	return "eval '" + escaped + "\n\n__icebath_rc=$? __icebath_ps=(\"${PIPESTATUS[@]}\"); (exit $__icebath_rc) && :'"
}

func canCapturePipestatus(command string) bool {
	return !strings.Contains(command, "<<") && !strings.HasSuffix(command, "\\")
}

func cwdMarkerPrintf(marker string) string {
	return `printf '\n` + marker + `%s` + marker + `\n' "$(pwd -P)"`
}

func exportDumpExcludingSessionVars(tmpPath string, excluded []string) string {
	safe := map[string]bool{}
	for _, name := range excluded {
		if name != "" {
			safe[name] = true
		}
	}
	names := make([]string, 0, len(safe))
	for name := range safe {
		names = append(names, name)
	}
	sort.Strings(names)
	var extra strings.Builder
	for _, name := range names {
		extra.WriteString(" ")
		extra.WriteString(shell.Quote(name))
	}
	return "{ ( unset ${!ICEBATH_SESSION_*} ${!ICEBATH_CRON_AUTO_DELIVER_*} " +
		"AI_AGENT ICEBATH_AGENT " +
		"ICEBATH_DELEGATED_CHILD_CONTEXT ICEBATH_CRON_SESSION " +
		"${!ICEBATH_RPC_*} ICEBATH_KERNEL_DIR " +
		"ICEBATH_UI_SESSION_ID" + extra.String() + " 2>/dev/null; " +
		"export -p; ) || true; } " +
		"> " + tmpPath
}

type scriptPaths struct {
	quotedCwd       string
	quotedSnap      string
	snapTmpTemplate string
	cwdMarker       string
}

func snapshotBootstrapScript(p scriptPaths, excluded []string) string {
	return "umask 077\n" +
		"__icebath_snap_tmp=$(mktemp " + p.snapTmpTemplate + ") || exit 1\n" +
		exportDumpExcludingSessionVars(snapTmp, excluded) + "\n" +
		"__icebath_fns=$(declare -F | awk '{print $3}' | grep -vE '^_[^_]') || true\n" +
		"[ -n \"$__icebath_fns\" ] && declare -f $__icebath_fns >> " + snapTmp + " 2>/dev/null || true\n" +
		"alias -p >> " + snapTmp + "\n" +
		"echo 'shopt -s expand_aliases' >> " + snapTmp + "\n" +
		"echo 'set +e' >> " + snapTmp + "\n" +
		"echo 'set +u' >> " + snapTmp + "\n" +
		"mv -f " + snapTmp + " " + p.quotedSnap + " || rm -f " + snapTmp + "\n" +
		"builtin cd -- " + p.quotedCwd + " 2>/dev/null || true\n" +
		cwdMarkerPrintf(p.cwdMarker) + "\n"
}

func passthroughSaveRestore(names []string) (save, restore []string) {
	for _, name := range names {
		marker := "_ICEBATH_RUNTIME_PASSTHROUGH_" + name
		present, value := marker+"_PRESENT", marker+"_VALUE"
		save = append(save, present+"=${"+name+"+x}", value+"=${"+name+"-}")
		restore = append(restore,
			`if [ "$`+present+`" = x ]; then export `+name+`="$`+value+`"; else unset `+name+`; fi`,
			"unset "+present+" "+value)
	}
	return save, restore
}

type wrapOptions struct {
	paths         scriptPaths
	passthrough   []string
	snapshotReady bool
	pipeMarker    string
	persistEnv    bool
}

func wrapCommandScript(command string, o wrapOptions) string {
	escaped := strings.ReplaceAll(command, "'", `'\''`)
	capture := o.pipeMarker != "" && canCapturePipestatus(command)
	save, restore := passthroughSaveRestore(o.passthrough)
	parts := append([]string{}, save...)
	if o.snapshotReady {
		parts = append(parts, "source "+o.paths.quotedSnap+" >/dev/null 2>&1 || true")
	}
	parts = append(parts, restore...)
	run := "eval '" + escaped + "'"
	if capture {
		run = evalCapturingPipestatus(escaped)
	}
	parts = append(parts,
		`export AI_AGENT="${AI_AGENT:-icebath}" ICEBATH_AGENT="${ICEBATH_AGENT:-true}"`,
		`export GIT_PAGER="${GIT_PAGER:-cat}" PAGER="${PAGER:-cat}"`,
		"builtin cd -- "+o.paths.quotedCwd+" || exit 126",
		run,
		"__icebath_ec=$?",
		"umask 077")
	if o.snapshotReady && o.persistEnv {
		parts = append(parts,
			"__icebath_snap_tmp=$(mktemp "+o.paths.snapTmpTemplate+") && "+
				"{ "+exportDumpExcludingSessionVars(snapTmp, o.passthrough)+" "+
				"&& mv -f "+snapTmp+" "+o.paths.quotedSnap+"; } "+
				"2>/dev/null || rm -f "+snapTmp+" 2>/dev/null || true")
	}
	if capture {
		parts = append(parts, pipeMarkerPrintf(o.pipeMarker))
	}
	parts = append(parts, cwdMarkerPrintf(o.paths.cwdMarker), "exit $__icebath_ec")
	return strings.Join(parts, "\n")
}

func splitMarker(output, marker string) (string, string, bool) {
	last := strings.LastIndex(output, marker)
	if last == -1 {
		return "", "", false
	}
	start := last
	for n := 0; n < 4096 && start > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(output[:start])
		start -= size
	}
	rel := strings.LastIndex(output[start:last], marker)
	if rel == -1 {
		return "", "", false
	}
	first := start + rel
	cwd := strings.TrimSpace(output[first+len(marker) : last])
	lineStart := strings.LastIndex(output[:first], "\n")
	if lineStart == -1 {
		lineStart = first
	}
	lineEnd := len(output)
	if idx := strings.Index(output[last+len(marker):], "\n"); idx != -1 {
		lineEnd = last + len(marker) + idx + 1
	}
	return cwd, output[:lineStart] + output[lineEnd:], true
}

func quoteCwdForCd(cwd string) string {
	switch {
	case cwd == "~":
		return cwd
	case cwd == "~/":
		return "$HOME"
	case strings.HasPrefix(cwd, "~/"):
		return "$HOME/" + shell.Quote(cwd[2:])
	}
	return shell.Quote(cwd)
}

func embedStdinHeredoc(command, stdin string) string {
	delimiter := "ICEBATH_STDIN_" + randomHex(12)
	return "{\n" + command + "\n} << '" + delimiter + "' < <(IFS= read -r -d '' s || :; printf '%s' \"${s%?}\")\n" +
		stdin + "\n" + delimiter
}

func stagedStdinPath(tempDir string) string {
	dir := strings.TrimRight(tempDir, "/")
	if dir == "" {
		dir = "/"
	}
	return dir + "/.icebath-stdin-" + randomHex(32)
}

func redirectStdinFromFile(command, path string) string {
	quoted := shell.Quote(path)
	return "exec 0< " + quoted + " || exit $?\nrm -f -- " + quoted + " || exit $?\n" + command
}
