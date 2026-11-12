package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Known log files by the name icebath logs takes
var logFiles = logging.LogFiles

var (
	logTSRE         = pyre.MustCompile(`(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2})`)
	logLevelRE      = pyre.MustCompile(`\s(DEBUG|INFO|WARNING|ERROR|CRITICAL)\s`)
	logLoggerNameRE = pyre.MustCompile(`\s(?:DEBUG|INFO|WARNING|ERROR|CRITICAL)(?:\s+\[.*?\])?\s+(\S+):`)
	logSinceRE      = pyre.MustCompile(`^(\d+)\s*([smhd])$`)
	logLevelOrder   = map[string]int{"DEBUG": 0, "INFO": 1, "WARNING": 2, "ERROR": 3, "CRITICAL": 4}
)

// Arguments of icebath logs for one log file
type TailLogOptions struct {
	Name, Level, Session, Since, Component string
	Lines                                  int
	Follow                                 bool
}

// Where a named log lives without checking it exists, false for an unknown name
func logFilePath(ctx context.Context, name string) (string, bool) {
	return logging.LogFilePath(ctx, name)
}

// Groups of the first match of a Python pattern, nil without one
func pyreFind(re *regexp2.Regexp, s string) []string {
	m, err := re.FindStringMatch(s)
	if err != nil || m == nil {
		return nil
	}
	groups := m.Groups()
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.String()
	}
	return out
}

// Naive wall clock cutoff of a relative time like 1h, 30m or 2d in UTC, false when unparseable
func parseLogSince(since string) (time.Time, bool, error) {
	m := pyreFind(logSinceRE, pystr.Lower(pystr.Strip(since)))
	if m == nil {
		return time.Time{}, false, nil
	}
	n, _ := pystr.IntText(m[1])
	unit := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400}[m[2]]
	total := new(big.Int).Mul(n, big.NewInt(unit))
	days := new(big.Int).Div(total, big.NewInt(86400))
	if days.Cmp(big.NewInt(math.MaxInt32)) > 0 {
		return time.Time{}, false, &pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C int"}
	}
	if days.Int64() > 999999999 {
		return time.Time{}, false, &pyint.Error{Kind: "OverflowError", Msg: fmt.Sprintf("days=%d; must have magnitude <= 999999999", days.Int64())}
	}
	now := naiveLocalNow()
	if total.Int64() > now.Unix()-time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		return time.Time{}, false, &pyint.Error{Kind: "OverflowError", Msg: "date value out of range"}
	}
	return time.Unix(now.Unix()-total.Int64(), int64(now.Nanosecond())).UTC(), true, nil
}

// Local wall clock of now at microsecond precision as a naive time in UTC, as datetime.now gives it
func naiveLocalNow() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond()/1000*1000, time.UTC)
}

// Naive timestamp in UTC at the first stamp of a line, false without one or when strptime refuses it
func parseLogLineTimestamp(line string) (time.Time, bool) {
	m := pyreFind(logTSRE, line)
	if m == nil {
		return time.Time{}, false
	}
	t, _, err := pydatetime.Strptime(strings.ReplaceAll(m[1], "T", " "), "%Y-%m-%d %H:%M:%S")
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Level word of a line, empty without one
func extractLogLevel(line string) string {
	if m := pyreFind(logLevelRE, line); m != nil {
		return m[1]
	}
	return ""
}

// Logger name after the level and optional session tag, false without one
func extractLoggerName(line string) (string, bool) {
	if m := pyreFind(logLoggerNameRE, line); m != nil {
		return m[1], true
	}
	return "", false
}

// Whether the logger of a line starts with one of the component prefixes
func lineMatchesComponent(line string, prefixes []string) bool {
	name, ok := extractLoggerName(line)
	if !ok {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Active filters of one icebath logs run, nil fields being inactive
type logFilters struct {
	minLevel          *string
	sessionFilter     *string
	since             *time.Time
	componentPrefixes []string
	hasComponent      bool
}

// Whether a line passes every active filter, a line without stamp or level passing those
func matchesLogFilters(line string, f logFilters) bool {
	if f.since != nil {
		if ts, ok := parseLogLineTimestamp(line); ok && ts.Before(*f.since) {
			return false
		}
	}
	if f.minLevel != nil {
		if level := extractLogLevel(line); level != "" && logLevelOrder[level] < logLevelOrder[*f.minLevel] {
			return false
		}
	}
	if f.sessionFilter != nil && !strings.Contains(line, *f.sessionFilter) {
		return false
	}
	return !f.hasComponent || lineMatchesComponent(line, f.componentPrefixes)
}

// Stateful filter over consecutive lines, an unstamped line taking the verdict of the record above
type logLineFilter struct {
	filters logFilters
	carry   *bool
}

func (k *logLineFilter) keep(line string) bool {
	if _, ok := parseLogLineTimestamp(line); ok {
		v := matchesLogFilters(line, k.filters)
		k.carry = &v
	} else if k.carry == nil {
		timed := k.filters.since != nil || k.filters.minLevel != nil
		return !timed && matchesLogFilters(line, k.filters)
	}
	return *k.carry
}

// Lines of text split after each universal newline the way readlines keeps them
func readlinesKeep(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []string
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+1])
		text = text[i+1:]
	}
	return out
}

// Every line of a log file read as utf-8-sig with replacement
func readAllLogLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return readlinesKeep(pystr.DecodeUTF8SigReplace(raw)), nil
}

// Last n entries of a slice, the Python negative slice
func lastN(lines []string, n int) []string {
	if n <= 0 {
		if n == 0 {
			return lines
		}
		if -n >= len(lines) {
			return nil
		}
		return lines[-n:]
	}
	if n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}

// Last n lines, files over one MiB read in growing chunks from the end
func readLastNLogLines(path string, n int) ([]string, error) {
	lines, err := func() ([]string, error) {
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		size := st.Size()
		if size == 0 {
			return []string{}, nil
		}
		if size <= 1_048_576 {
			all, err := readAllLogLines(path)
			if err != nil {
				return nil, err
			}
			return lastN(all, n), nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		chunkSize := int64(8192)
		var parts [][]byte
		pos := size
		for pos > 0 && len(parts) <= n+1 {
			readSize := min(chunkSize, pos)
			pos -= readSize
			buf := make([]byte, readSize)
			if _, err := f.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			chunk := bytes.Split(buf, []byte("\n"))
			if len(parts) > 0 {
				parts[0] = append(append([]byte{}, chunk[len(chunk)-1]...), parts[0]...)
				parts = append(append([][]byte{}, chunk[:len(chunk)-1]...), parts...)
			} else {
				parts = chunk
			}
			chunkSize = min(chunkSize*2, 65536)
		}
		var decoded []string
		for _, raw := range parts {
			if len(bytes.Trim(raw, " \t\n\r\x0b\x0c")) > 0 {
				decoded = append(decoded, pystr.DecodeUTF8Replace(raw)+"\n")
			}
		}
		return lastN(decoded, n), nil
	}()
	if err != nil {
		all, err := readAllLogLines(path)
		if err != nil {
			return nil, err
		}
		return lastN(all, n), nil
	}
	return lines, nil
}

// Last n matching lines, over reading so enough survive the filters
func readLogTail(path string, n int, hasFilters bool, f logFilters) ([]string, error) {
	if !hasFilters {
		return readLastNLogLines(path, n)
	}
	raw, err := readLastNLogLines(path, max(n*20, 2000))
	if err != nil {
		return nil, err
	}
	keep := &logLineFilter{filters: f}
	var out []string
	for _, l := range raw {
		if keep.keep(l) {
			out = append(out, l)
		}
	}
	return lastN(out, n), nil
}

// Polls a log file for new content and prints matching lines until interrupted
func followLog(ctx context.Context, w io.Writer, path string, f logFilters) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	keep := &logLineFilter{filters: f}
	var pending []byte
	buf := make([]byte, 65536)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := pystr.DecodeUTF8Replace(pending[:i+1])
				pending = pending[i+1:]
				if keep.keep(line) {
					fmt.Fprint(w, line)
				}
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case <-interrupt:
			fmt.Fprintln(w, "\n--- stopped ---")
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Reads and shows log lines with the level, session, since and component filters, following on request
func TailLog(ctx context.Context, w io.Writer, o TailLogOptions) (int, error) {
	filename, ok := logFiles[o.Name]
	if !ok {
		names := make([]string, 0, len(logFiles))
		for k := range logFiles {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "Unknown log: %s. Available: %s\n", pyrepr.Repr(o.Name), strings.Join(names, ", "))
		return 1, nil
	}
	logPath, _ := logFilePath(ctx, o.Name)
	if _, err := os.Stat(logPath); err != nil {
		fmt.Fprintf(w, "Log file not found: %s\n", logPath)
		fmt.Fprintln(w, "(Logs are created when Icebath runs — try 'icebath chat' first)")
		return 1, nil
	}
	var f logFilters
	if o.Since != "" {
		cutoff, ok, err := parseLogSince(o.Since)
		if err != nil {
			return 1, err
		}
		if !ok {
			fmt.Fprintf(w, "Invalid --since value: %s. Use format like '1h', '30m', '2d'.\n", pyrepr.Repr(o.Since))
			return 1, nil
		}
		f.since = &cutoff
	}
	minLevel := ""
	if o.Level != "" {
		minLevel = pystr.Upper(o.Level)
		if _, ok := logLevelOrder[minLevel]; !ok {
			fmt.Fprintf(w, "Invalid --level: %s. Use DEBUG, INFO, WARNING, ERROR, or CRITICAL.\n", pyrepr.Repr(o.Level))
			return 1, nil
		}
		f.minLevel = &minLevel
	}
	if o.Component != "" {
		prefixes, ok := logging.ComponentPrefixes[pystr.Lower(o.Component)]
		if !ok {
			names := make([]string, 0, len(logging.ComponentPrefixes))
			for k := range logging.ComponentPrefixes {
				names = append(names, k)
			}
			sort.Strings(names)
			fmt.Fprintf(w, "Unknown component: %s. Available: %s\n", pyrepr.Repr(o.Component), strings.Join(names, ", "))
			return 1, nil
		}
		f.componentPrefixes, f.hasComponent = prefixes, true
	}
	if o.Session != "" {
		session := o.Session
		f.sessionFilter = &session
	}
	hasFilters := f.minLevel != nil || f.sessionFilter != nil || f.since != nil || f.hasComponent
	lines, err := readLogTail(logPath, o.Lines, hasFilters, f)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintf(w, "Permission denied: %s\n", logPath)
			return 1, nil
		}
		return 1, err
	}
	var parts []string
	for _, p := range [][2]string{{"level>", minLevel}, {"session", o.Session}, {"component", o.Component}, {"since", o.Since}} {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+p[1])
		}
	}
	filterDesc := ""
	if len(parts) > 0 {
		filterDesc = " [" + strings.Join(parts, ", ") + "]"
	}
	mode := fmt.Sprintf("last %d", o.Lines)
	if o.Follow {
		mode = "Ctrl+C to stop"
	}
	fmt.Fprintf(w, "--- %s/logs/%s%s (%s) ---\n", home.Display(home.Dir(ctx)), filename, filterDesc, mode)
	for _, line := range lines {
		fmt.Fprint(w, line)
	}
	if !o.Follow {
		return 0, nil
	}
	return 0, followLog(ctx, w, logPath, f)
}

// Human size of a log file in B, KB or MB
func logSizeLabel(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(size)/(1024*1024))
}

// Relative age of a modification time, the date past a day
func logAgeLabel(mtime time.Time) string {
	age := time.Since(mtime).Seconds()
	switch {
	case age < 60:
		return "just now"
	case age < 3600:
		return fmt.Sprintf("%dm ago", int(age/60))
	case age < 86400:
		return fmt.Sprintf("%dh ago", int(age/3600))
	}
	return mtime.Local().Format("2006-01-02")
}

// Prints the log files with sizes and ages, plus the root home update logs under a profile
func ListLogs(ctx context.Context, w io.Writer) error {
	logDir := filepath.Join(home.Dir(ctx), "logs")
	var rootLogs []string
	for _, name := range []string{"handoff", "update"} {
		path, _ := logFilePath(ctx, name)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && filepath.Dir(path) != logDir {
			rootLogs = append(rootLogs, path)
		}
	}
	display := home.Display(home.Dir(ctx))
	_, dirErr := os.Stat(logDir)
	if dirErr != nil && len(rootLogs) == 0 {
		fmt.Fprintf(w, "No logs directory at %s/logs/\n", display)
		return nil
	}
	fmt.Fprintf(w, "Log files in %s/logs/:\n\n", display)
	var entries []string
	if dirErr == nil {
		dirEntries, err := os.ReadDir(logDir)
		if err != nil {
			return err
		}
		for _, e := range dirEntries {
			entries = append(entries, filepath.Join(logDir, e.Name()))
		}
		sort.Strings(entries)
	}
	found := false
	for _, entry := range append(entries, rootLogs...) {
		st, err := os.Stat(entry)
		if err != nil || !st.Mode().IsRegular() || pypath.PathSuffix(entry) != ".log" {
			continue
		}
		label := filepath.Base(entry)
		if filepath.Dir(entry) != logDir {
			label += " (root)"
		}
		fmt.Fprintf(w, "  %s %8s   %s\n", padRight(label, 25), logSizeLabel(st.Size()), logAgeLabel(st.ModTime()))
		found = true
	}
	if !found {
		fmt.Fprintln(w, "  (no log files yet — run 'icebath chat' to generate logs)")
	}
	return nil
}
