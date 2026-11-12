package agent

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	logCallLineRE = regexp.MustCompile(`^(\S+ \S+) INFO \[(\S+)\] agent\.conversation_loop: API call #(\d+): model=(\S+) provider=\S+ ` +
		`in=(\d+) out=(\d+) total=\d+ latency=([\d.]+)s cache=(\d+)/(\d+)`)
	logCallWriteRE    = regexp.MustCompile(` write=(\d+)`)
	logCallIDRE       = regexp.MustCompile(` id=(\S+)`)
	logCallUpstreamRE = regexp.MustCompile(` upstream=(.+?)(?: [a-z_]+=|$)`)
)

// ParseAPICallLogs reads the per call usage lines of the given sessions from agent log files, unreadable files skipped
func ParseAPICallLogs(paths []string, sessionIDs map[string]bool) []map[string]any {
	calls := []map[string]any{}
	for _, path := range paths {
		fh, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := bufio.NewReader(fh)
		for {
			raw, readErr := reader.ReadString('\n')
			if raw != "" {
				if rec := parseAPICallLogLine(strings.TrimRight(strings.ToValidUTF8(raw, "�"), "\r\n"), sessionIDs); rec != nil {
					calls = append(calls, rec)
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = fh.Close()
	}
	return calls
}

// One API call record of a matching log line, nil for any other line or session
func parseAPICallLogLine(line string, sessionIDs map[string]bool) map[string]any {
	m := logCallLineRE.FindStringSubmatch(line)
	if m == nil || !sessionIDs[m[2]] {
		return nil
	}
	n, _ := strconv.Atoi(m[3])
	inp, _ := strconv.Atoi(m[5])
	out, _ := strconv.Atoi(m[6])
	lat, _ := strconv.ParseFloat(m[7], 64)
	hit, _ := strconv.Atoi(m[8])
	rec := map[string]any{"ts": m[1], "sid": m[2], "n": n, "model": m[4], "inp": inp, "out": out, "lat": lat, "hit": hit}
	if mm := logCallWriteRE.FindStringSubmatch(line); mm != nil {
		write, _ := strconv.Atoi(mm[1])
		rec["write"] = write
	}
	if mm := logCallIDRE.FindStringSubmatch(line); mm != nil {
		rec["id"] = mm[1]
	}
	if mm := logCallUpstreamRE.FindStringSubmatch(line); mm != nil {
		rec["upstream"] = mm[1]
	}
	return rec
}
