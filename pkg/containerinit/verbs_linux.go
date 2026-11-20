package containerinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Writes command letters into a control fifo, failing when no supervisor reads it
func writeControl(path, letters string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return errors.New("supervisor not listening")
		}
		return err
	}
	defer f.Close()
	_, err = f.WriteString(letters)
	return err
}

// True when the supervisor of dir has its control fifo open
func supervised(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "supervise", "control"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Sends the letters of every flag to each service directory named after them
func runSvc(args []string) int {
	letters, dirs := "", []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			dirs = append(dirs, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				if !strings.ContainsRune("udoOxtrhiq12akpcy", c) {
					fmt.Fprintf(os.Stderr, "s6-svc: usage: s6-svc [ -udoOxtrhiq12akpcy ] servicedir...\n")
					return 100
				}
				letters += string(c)
			}
			continue
		}
		dirs = append(dirs, a)
	}
	if len(dirs) == 0 {
		fmt.Fprintf(os.Stderr, "s6-svc: usage: s6-svc [ -udoOxtrhiq12akpcy ] servicedir...\n")
		return 100
	}
	code := 0
	for _, d := range dirs {
		if err := writeControl(filepath.Join(d, "supervise", "control"), letters); err != nil {
			fmt.Fprintf(os.Stderr, "s6-svc: fatal: unable to control %s: %v\n", d, err)
			code = 111
		}
	}
	return code
}

// One svstat field rendered the way s6-svstat -o prints it
func statField(st svcStatus, field string, now time.Time) (string, bool) {
	since := time.Unix(0, st.Since)
	readySince := time.Unix(0, st.ReadySince)
	switch field {
	case "up":
		return strconv.FormatBool(st.Up), true
	case "wantedup":
		return strconv.FormatBool(st.WantUp), true
	case "normallyup":
		return strconv.FormatBool(st.NormallyUp), true
	case "ready":
		return strconv.FormatBool(st.Ready), true
	case "paused":
		return strconv.FormatBool(st.Paused), true
	case "pid":
		if !st.Up {
			return "-1", true
		}
		return strconv.Itoa(st.Pid), true
	case "pgid":
		if !st.Up {
			return "-1", true
		}
		return strconv.Itoa(st.Pgid), true
	case "exitcode":
		if st.Up {
			return "-1", true
		}
		return strconv.Itoa(st.ExitCode), true
	case "signal":
		if st.Up || st.Signal == 0 {
			return "NA", true
		}
		return "SIG" + strings.ToUpper(strings.TrimPrefix(syscall.Signal(st.Signal).String(), "signal ")), true
	case "signum":
		if st.Up || st.Signal == 0 {
			return "-1", true
		}
		return strconv.Itoa(st.Signal), true
	case "updownsince":
		return since.UTC().Format("2006-01-02 15:04:05.000000000"), true
	case "readysince":
		return readySince.UTC().Format("2006-01-02 15:04:05.000000000"), true
	case "updownfor":
		return strconv.Itoa(int(now.Sub(since).Seconds())), true
	case "readyfor":
		return strconv.Itoa(int(now.Sub(readySince).Seconds())), true
	}
	return "", false
}

// Prints the state of one service, the human line or the -o field values
func runSvstat(args []string) int {
	fields := []string{}
	var dir string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			fields = strings.Split(args[i+1], ",")
			i++
		case strings.HasPrefix(args[i], "-o") && len(args[i]) > 2:
			fields = strings.Split(args[i][2:], ",")
		case !strings.HasPrefix(args[i], "-"):
			dir = args[i]
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "s6-svstat: usage: s6-svstat [ -o fields ] servicedir")
		return 100
	}
	if !supervised(dir) {
		fmt.Fprintf(os.Stderr, "s6-svstat: fatal: unable to read status for %s: supervisor not listening\n", dir)
		return 1
	}
	st, err := readStatus(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svstat: fatal: unable to read status for %s: %v\n", dir, err)
		return 111
	}
	now := time.Now()
	if len(fields) > 0 {
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			v, ok := statField(st, f, now)
			if !ok {
				fmt.Fprintf(os.Stderr, "s6-svstat: fatal: invalid field: %s\n", f)
				return 100
			}
			out = append(out, v)
		}
		fmt.Println(strings.Join(out, " "))
		return 0
	}
	secs := int(now.Sub(time.Unix(0, st.Since)).Seconds())
	var line string
	if st.Up {
		line = fmt.Sprintf("up (pid %d) %d seconds", st.Pid, secs)
		if !st.NormallyUp {
			line += ", normally down"
		}
		if !st.WantUp {
			line += ", want down"
		}
		if st.Ready {
			line += fmt.Sprintf(", ready %d seconds", int(now.Sub(time.Unix(0, st.ReadySince)).Seconds()))
		}
	} else {
		if st.Signal != 0 {
			line = fmt.Sprintf("down (signal %s) %d seconds", statFieldMust(st, "signal", now), secs)
		} else {
			line = fmt.Sprintf("down (exitcode %d) %d seconds", st.ExitCode, secs)
		}
		if st.NormallyUp {
			line += ", normally up"
		}
		if st.WantUp {
			line += ", want up"
		}
	}
	fmt.Println(line)
	return 0
}

func statFieldMust(st svcStatus, field string, now time.Time) string {
	v, _ := statField(st, field, now)
	return v
}

// Waits until every or any service reaches the wanted state, exit 99 when the deadline passes
func runSvwait(args []string) int {
	mode, anyOf, timeout := byte('u'), false, time.Duration(0)
	dirs := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" && i+1 < len(args):
			ms, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "s6-svwait: usage: s6-svwait [ -U | -u | -d | -D | -r | -R ] [ -a | -o ] [ -t timeout ] servicedir...")
				return 100
			}
			timeout = time.Duration(ms) * time.Millisecond
			i++
		case strings.HasPrefix(a, "-t") && len(a) > 2:
			ms, err := strconv.Atoi(a[2:])
			if err != nil {
				return 100
			}
			timeout = time.Duration(ms) * time.Millisecond
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, c := range a[1:] {
				switch c {
				case 'u', 'U', 'd', 'D', 'r', 'R':
					mode = byte(c)
				case 'o':
					anyOf = true
				case 'a':
					anyOf = false
				default:
					fmt.Fprintln(os.Stderr, "s6-svwait: usage: s6-svwait [ -U | -u | -d | -D | -r | -R ] [ -a | -o ] [ -t timeout ] servicedir...")
					return 100
				}
			}
		default:
			dirs = append(dirs, a)
		}
	}
	if len(dirs) == 0 {
		return 100
	}
	met := func(st svcStatus) bool {
		switch mode {
		case 'u':
			return st.Up
		case 'U', 'r', 'R':
			return st.Up && st.Ready
		case 'd':
			return !st.Up
		}
		return !st.Up && !st.Finishing
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		done, count := false, 0
		for _, d := range dirs {
			if !supervised(d) {
				fmt.Fprintf(os.Stderr, "s6-svwait: fatal: unable to subscribe to events for %s: supervisor not listening\n", d)
				return 102
			}
			st, err := readStatus(d)
			if err == nil && met(st) {
				count++
			}
		}
		if anyOf {
			done = count > 0
		} else {
			done = count == len(dirs)
		}
		if done {
			return 0
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "s6-svwait: fatal: timed out")
			return 99
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Sends scan directory letters, a to rescan, n to prune, the halt letters to stop the container
func runSvscanctl(args []string) int {
	letters, dirs := "", []string{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				if !strings.ContainsRune("ahnNtsqpi0678zbr", c) {
					fmt.Fprintln(os.Stderr, "s6-svscanctl: usage: s6-svscanctl [ -zabhitqnN ] svscandir")
					return 100
				}
				letters += string(c)
			}
			continue
		}
		dirs = append(dirs, a)
	}
	if len(dirs) != 1 {
		fmt.Fprintln(os.Stderr, "s6-svscanctl: usage: s6-svscanctl [ -zabhitqnN ] svscandir")
		return 100
	}
	if err := writeControl(svscanControl(dirs[0]), letters); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscanctl: fatal: unable to control %s: %v\n", dirs[0], err)
		return 111
	}
	return 0
}

// Strips the tini flags a legacy entrypoint passes and hands the command to the init
func runTini(args []string) int {
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if a == "-p" || a == "-e" {
			args = args[1:]
			if len(args) > 0 {
				args = args[1:]
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			args = args[1:]
			continue
		}
		break
	}
	target := initTarget
	if v := os.Getenv("ICEBATH_TINI_SHIM_TARGET"); v != "" {
		target = v
	}
	if len(args) > 0 && (args[0] == "/init" || args[0] == target) {
		return execProgram("tini", args, os.Environ())
	}
	return execProgram("tini", append([]string{target}, args...), os.Environ())
}

// Exit zero when the supervisor is alive and every wanted service is up and ready
func runHealth() int {
	raw, err := os.ReadFile(svscanPIDFile())
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy: the container init is not running")
		return 1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err == nil {
		if kerr := syscall.Kill(pid, 0); kerr != nil && !errors.Is(kerr, syscall.EPERM) {
			err = kerr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy: the container init is not running")
		return 1
	}
	entries, err := os.ReadDir(scanDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: cannot read %s: %v\n", scanDir, err)
		return 1
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(scanDir, e.Name())
		st, err := readStatus(dir)
		if err != nil {
			continue
		}
		if st.WantUp && (!st.Up || !st.Ready) {
			fmt.Fprintf(os.Stderr, "unhealthy: %s is wanted up but is not up and ready\n", e.Name())
			return 1
		}
	}
	return 0
}
