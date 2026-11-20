package containerinit

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Settings in force when an action directive of the s6-log script appeared
type logAction struct {
	kind      byte
	dir       *logDir
	isoStamp  bool
	taiStamp  bool
	selectors []logSelector
}

type logSelector struct {
	keep bool
	re   *regexp.Regexp
}

// Rotated log directory with its current file, archive count and size cap
type logDir struct {
	path     string
	keep     int
	maxSize  int64
	current  *os.File
	size     int64
	lockFile *os.File
}

// TAI64N label of t, the name an archived log file carries
func tai64n(t time.Time) string {
	return fmt.Sprintf("@%016x%08x", uint64(1)<<62+uint64(t.Unix())+37, t.Nanosecond())
}

// Locks the directory and opens its current file for appending
func openLogDir(path string, keep int, maxSize int64) (*logDir, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(path, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("unable to lock %s: %w", path, err)
	}
	d := &logDir{path: path, keep: keep, maxSize: maxSize, lockFile: lock}
	return d, d.open()
}

func (d *logDir) open() error {
	f, err := os.OpenFile(filepath.Join(d.path, "current"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	d.current, d.size = f, info.Size()
	return nil
}

// Archives current under its TAI64N name and drops the oldest archives past the keep count
func (d *logDir) rotate() error {
	if d.size == 0 {
		return nil
	}
	d.current.Close()
	cur := filepath.Join(d.path, "current")
	os.Chmod(cur, 0o744)
	if err := os.Rename(cur, filepath.Join(d.path, tai64n(time.Now())+".s")); err != nil {
		return err
	}
	if err := d.open(); err != nil {
		return err
	}
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return err
	}
	var archives []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "@") {
			archives = append(archives, e.Name())
		}
	}
	sort.Strings(archives)
	for len(archives) > d.keep {
		os.Remove(filepath.Join(d.path, archives[0]))
		archives = archives[1:]
	}
	return nil
}

func (d *logDir) write(line string) error {
	if d.maxSize > 0 && d.size > 0 && d.size+int64(len(line)) > d.maxSize {
		if err := d.rotate(); err != nil {
			return err
		}
	}
	n, err := d.current.WriteString(line)
	d.size += int64(n)
	return err
}

// Parses the s6-log script into its actions
func parseLogScript(args []string) ([]*logAction, error) {
	keep, maxSize := 10, int64(99999)
	iso, tai := false, false
	var sel []logSelector
	var actions []*logAction
	for _, a := range args {
		switch {
		case a == "":
			continue
		case a[0] == 'n':
			n, err := strconv.Atoi(a[1:])
			if err != nil {
				return nil, fmt.Errorf("invalid directive: %s", a)
			}
			keep = n
		case a[0] == 's':
			n, err := strconv.ParseInt(a[1:], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid directive: %s", a)
			}
			maxSize = n
		case a == "T":
			iso = true
		case a == "t":
			tai = true
		case a[0] == '+' || a[0] == '-':
			re, err := regexp.Compile(a[1:])
			if err != nil {
				return nil, fmt.Errorf("invalid directive: %s", a)
			}
			sel = append(sel, logSelector{keep: a[0] == '+', re: re})
		case a == "1" || a == "2":
			actions = append(actions, &logAction{kind: a[0], isoStamp: iso, taiStamp: tai, selectors: sel})
		case a[0] == '/' || a[0] == '.':
			d, err := openLogDir(a, keep, maxSize)
			if err != nil {
				return nil, err
			}
			actions = append(actions, &logAction{kind: 'd', dir: d, isoStamp: iso, taiStamp: tai, selectors: sel})
		default:
			return nil, fmt.Errorf("unrecognized directive: %s", a)
		}
	}
	return actions, nil
}

// Whether the selection directives of an action keep the line, every line kept when there are none
func (a *logAction) selects(line string) bool {
	keep := true
	for i, s := range a.selectors {
		if i == 0 && s.keep {
			keep = false
		}
		if s.re.MatchString(line) {
			keep = s.keep
		}
	}
	return keep
}

// Applies every action to one line, prefixing the timestamps its settings ask for
func applyLogLine(actions []*logAction, line string, now time.Time) error {
	for _, a := range actions {
		if !a.selects(line) {
			continue
		}
		out := line
		if a.isoStamp {
			out = now.UTC().Format("2006-01-02 15:04:05.000000000") + " " + out
		}
		if a.taiStamp {
			out = tai64n(now) + " " + out
		}
		out += "\n"
		var err error
		switch a.kind {
		case '1':
			_, err = os.Stdout.WriteString(out)
		case '2':
			_, err = os.Stderr.WriteString(out)
		default:
			err = a.dir.write(out)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Reads lines from stdin into the script's actions until end of file, rotating on SIGALRM
func runS6Log(args []string) int {
	actions, err := parseLogScript(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-log: fatal: %v\n", err)
		return 100
	}
	var mu sync.Mutex
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGALRM, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			mu.Lock()
			if sig == syscall.SIGALRM {
				for _, a := range actions {
					if a.dir != nil {
						a.dir.rotate()
					}
				}
				mu.Unlock()
				continue
			}
			for _, a := range actions {
				if a.dir != nil {
					a.dir.current.Close()
				}
			}
			os.Exit(0)
		}
	}()
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			mu.Lock()
			werr := applyLogLine(actions, strings.TrimSuffix(line, "\n"), time.Now())
			mu.Unlock()
			if werr != nil {
				fmt.Fprintf(os.Stderr, "s6-log: warning: %v\n", werr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "s6-log: fatal: unable to read stdin: %v\n", err)
			return 111
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range actions {
		if a.dir != nil {
			a.dir.current.Close()
			os.Chmod(filepath.Join(a.dir.path, "current"), 0o744)
		}
	}
	return 0
}
