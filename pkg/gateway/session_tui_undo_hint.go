package gateway

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Epoch seconds a message stamp names, a positive finite number or an ISO date, false for anything else
func undoEpoch(value any) (float64, bool) {
	number := math.NaN()
	switch v := value.(type) {
	case float64:
		number = v
	case int64:
		number = float64(v)
	case int:
		number = float64(v)
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0, false
		}
		parsed, err := fromISOFormat(text)
		if err != nil {
			return 0, false
		}
		return pydatetime.Timestamp(parsed.T), true
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0, false
	}
	return number, true
}

// Start of an undone span, the undone user message's stamp or the newest kept message's when that is later, false when neither is known
func UndoSpanStart(target any, kept []map[string]any) (float64, bool) {
	start, known := undoEpoch(target)
	for _, message := range kept {
		if message == nil || state.IsCompactionSummaryMessage(message) {
			continue
		}
		if when, stamped := undoEpoch(message["timestamp"]); stamped && (!known || when > start) {
			start, known = when, true
		}
	}
	return start, known
}

// Files that differ from the checkpoint an undone span started from, none when no checkpoint was taken in the span
func UndoChangedFiles(mgr *tools.CheckpointManager, cwd string, since float64) []string {
	if mgr == nil || !mgr.Enabled {
		return nil
	}
	type snapshot struct {
		hash string
		at   float64
	}
	var snapshots []snapshot
	for _, entry := range mgr.ListCheckpoints(cwd) {
		taken, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil || entry.Hash == "" {
			continue
		}
		snapshots = append(snapshots, snapshot{entry.Hash, float64(taken.Unix())})
	}
	inSpan := false
	for _, s := range snapshots {
		inSpan = inSpan || s.at >= math.Floor(since)
	}
	if !inSpan {
		return nil
	}
	base := snapshots[len(snapshots)-1].hash
	for _, s := range snapshots {
		if s.at <= since {
			base = s.hash
			break
		}
	}
	result := mgr.Diff(cwd, base)
	if !pyrepr.Truthy(result["success"]) || result["diff"] == nil {
		return nil
	}
	return patchPaths(pyrepr.Str(result["diff"]))
}

// Wire hint of an undone span that started at a known time, nil when the span changed no file
func UndoHint(mgr *tools.CheckpointManager, cwd string, since float64, known bool) *tuigateway.UndoRollbackHint {
	if !known {
		return nil
	}
	files := UndoChangedFiles(mgr, cwd, since)
	if len(files) == 0 {
		return nil
	}
	return &tuigateway.UndoRollbackHint{Files: files}
}

// Paths the file headers of a git patch name, both sides of a rename, sorted without repeats
func patchPaths(patch string) []string {
	seen := map[string]bool{}
	lines := strings.Split(patch, "\n")
	for i, line := range lines {
		sides, isHeader := strings.CutPrefix(line, "diff --git ")
		if !isHeader {
			continue
		}
		var from, to string
		renamed, copied := false, false
		for _, extended := range lines[i+1:] {
			if strings.HasPrefix(extended, "diff --git ") || strings.HasPrefix(extended, "@@") || strings.HasPrefix(extended, "--- ") ||
				strings.HasPrefix(extended, "Binary files ") || extended == "GIT binary patch" {
				break
			}
			if name, has := strings.CutPrefix(extended, "rename from "); has {
				from, renamed = gitPathText(name), true
			}
			if name, has := strings.CutPrefix(extended, "rename to "); has {
				to, renamed = gitPathText(name), true
			}
			if name, has := strings.CutPrefix(extended, "copy to "); has {
				to, copied = gitPathText(name), true
			}
		}
		switch {
		case renamed:
			seen[from], seen[to] = true, true
		case copied:
			seen[to] = true
		default:
			if before, after, split := patchSides(sides); split {
				seen[before], seen[after] = true, true
			}
		}
	}
	delete(seen, "")
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Path of an extended patch header line, unquoted when git quoted it
func gitPathText(name string) string {
	if text, used, closed := unquoteGitPath(name); closed && used == len(name) {
		return text
	}
	return name
}

// Old and new path of a patch file header, false when the unquoted sides differ and so cannot be told apart
func patchSides(sides string) (string, string, bool) {
	trim := func(before, after string) (string, string, bool) {
		return strings.TrimPrefix(before, "a/"), strings.TrimPrefix(after, "b/"), true
	}
	if strings.HasPrefix(sides, `"`) {
		before, used, closed := unquoteGitPath(sides)
		if !closed || !strings.HasPrefix(sides[used:], " ") {
			return "", "", false
		}
		after := sides[used+1:]
		if strings.HasPrefix(after, `"`) {
			text, taken, closed := unquoteGitPath(after)
			if !closed || taken != len(after) {
				return "", "", false
			}
			after = text
		}
		return trim(before, after)
	}
	if at := strings.Index(sides, ` "`); at >= 0 {
		after, used, closed := unquoteGitPath(sides[at+1:])
		if !closed || used != len(sides)-at-1 {
			return "", "", false
		}
		return trim(sides[:at], after)
	}
	if doubled := len(sides) - len("a/ b/"); doubled >= 0 && doubled%2 == 0 {
		if path := sides[2 : 2+doubled/2]; sides == "a/"+path+" b/"+path {
			return path, path, true
		}
	}
	return "", "", false
}

// Text of the C quoted path a string starts with and the bytes it took, false when the string does not start with a closed quote
func unquoteGitPath(s string) (string, int, bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", 0, false
	}
	escapes := map[byte]byte{'a': 7, 'b': 8, 'f': 12, 'n': 10, 'r': 13, 't': 9, 'v': 11, '\\': '\\', '"': '"'}
	var out []byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return string(out), i + 1, true
		}
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		if plain, known := escapes[s[i]]; known {
			out = append(out, plain)
			continue
		}
		if i+2 >= len(s) {
			break
		}
		value := 0
		for _, digit := range []byte(s[i : i+3]) {
			if digit < '0' || digit > '7' {
				return "", 0, false
			}
			value = value*8 + int(digit-'0')
		}
		out = append(out, byte(value))
		i += 2
	}
	return "", 0, false
}
