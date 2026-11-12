package containerinit

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Runs the container verb argv names by program name or first argument, false for an ordinary command line
func Dispatch(argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	args := argv[1:]
	switch filepath.Base(argv[0]) {
	case "init":
		return runInit(args), true
	case "s6-svscan":
		return runSvscan(args), true
	case "s6-svc":
		return runSvc(args), true
	case "s6-svstat":
		return runSvstat(args), true
	case "s6-svwait":
		return runSvwait(args), true
	case "s6-svscanctl":
		return runSvscanctl(args), true
	case "s6-setuidgid":
		return runSetuidgid(args), true
	case "with-contenv":
		return runWithContenv(args), true
	case "s6-log":
		return runS6Log(args), true
	case "tini":
		return runTini(args), true
	}
	if len(args) > 0 {
		switch args[0] {
		case "container-init":
			return runInit(args[1:]), true
		case "container-health":
			return runHealth(), true
		}
	}
	return 0, false
}

// Re-runs a root icebath call inside the image as the service account so volume files stay account owned
func DropRootInImage(argv []string) {
	if os.Getuid() != 0 || !isFile(provenanceFile) || truthy(os.Getenv("ICEBATH_DOCKER_EXEC_AS_ROOT")) {
		return
	}
	a, err := lookupAccount(accountName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "icebath-shim: account %s not found; refusing to silently run as root.\n", accountName)
		fmt.Fprintln(os.Stderr, "icebath-shim: re-run with --user icebath or set ICEBATH_DOCKER_EXEC_AS_ROOT=1.")
		os.Exit(126)
	}
	exe, err := os.Executable()
	if err == nil {
		err = a.apply()
	}
	if err == nil {
		err = syscall.Exec(exe, argv, withEnv(os.Environ(), "HOME", defaultHome))
	}
	fmt.Fprintf(os.Stderr, "icebath-shim: unable to drop to %s: %v\n", accountName, err)
	os.Exit(126)
}

// Copy of env with name set to value
func withEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return append(out, name+"="+value)
}

// Hands the container over to the supervisor under its s6-svscan name, the name the service manager detects
func runInit(args []string) int {
	svscan := filepath.Join(commandDir, "s6-svscan")
	err := syscall.Exec(svscan, append([]string{"s6-svscan"}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "init: fatal: unable to exec %s: %v\n", svscan, err)
	return 111
}

// Milliseconds from an s6-overlay grace variable, def when unset or invalid
func graceFrom(name string, def time.Duration) time.Duration {
	if ms, err := strconv.Atoi(os.Getenv(name)); err == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Dashboard service the image declares, started only when ICEBATH_DASHBOARD asks for it
func dashboardSpec(exe string) *nativeSpec {
	enabled := func() bool { return truthy(os.Getenv("ICEBATH_DASHBOARD")) }
	host := func() string { return envOr("ICEBATH_DASHBOARD_HOST", "0.0.0.0") }
	port := func() string { return envOr("ICEBATH_DASHBOARD_PORT", "9119") }
	return &nativeSpec{
		enabled: enabled,
		command: func() ([]string, *syscall.ProcAttr, error) {
			if truthy(os.Getenv("ICEBATH_DASHBOARD_INSECURE")) {
				fmt.Fprintln(os.Stderr, "[dashboard] ICEBATH_DASHBOARD_INSECURE no longer disables the auth gate.")
				fmt.Fprintln(os.Stderr, "[dashboard] A non-loopback dashboard requires an auth provider:")
				fmt.Fprintln(os.Stderr, "[dashboard]   set ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME + _PASSWORD (password)")
				fmt.Fprintln(os.Stderr, "[dashboard]   or ICEBATH_DASHBOARD_OAUTH_CLIENT_ID (OAuth).")
			}
			sys := &syscall.SysProcAttr{Setsid: true}
			if os.Getuid() == 0 {
				a, err := lookupAccount(accountName)
				if err != nil {
					return nil, nil, err
				}
				sys = a.credential()
				sys.Setsid = true
			}
			argv := []string{exe, "dashboard", "--host", host(), "--port", port(), "--no-open"}
			return argv, &syscall.ProcAttr{Dir: defaultHome, Env: withEnv(os.Environ(), "HOME", defaultHome), Sys: sys}, nil
		},
		finish: func(code, sig int) int {
			if enabled() {
				return 0
			}
			return 125
		},
		ready: func() bool {
			conn, err := net.DialTimeout("tcp", probeAddr(host(), port()), time.Second)
			if err != nil {
				return false
			}
			conn.Close()
			return true
		},
	}
}

// Main program of the container, bare icebath, a command found on PATH, or icebath with the arguments
func mainArgv(exe string, args []string) []string {
	if len(args) == 0 {
		return []string{exe}
	}
	if !strings.HasPrefix(args[0], "-") {
		if path, err := exec.LookPath(args[0]); err == nil {
			return append([]string{path}, args[1:]...)
		}
	}
	return append([]string{exe}, args...)
}

// Exit status the container reports for a main program wait status
func containerExit(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// Container init, bootstrapping the volume, supervising services, running the main program and reaping orphans
func runSvscan(args []string) int {
	pid1 := os.Getpid() == 1
	if !pid1 {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to become a subreaper: %v\n", err)
			return 111
		}
	}
	acct, err := lookupAccount(accountName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: %v\n", err)
		return 111
	}
	if uid := os.Getuid(); uid != 0 && uid != acct.uid {
		stage, wrapper := arbitraryUIDErrors(uid)
		fmt.Fprint(os.Stderr, stage)
		fmt.Fprint(os.Stderr, wrapper)
		return 1
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to resolve the icebath binary: %v\n", err)
		return 111
	}
	exe = productBinaryFor(exe)
	if err := saveContainerEnv(containerEnv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: warning: unable to save the container environment: %v\n", err)
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to create %s: %v\n", baseDir, err)
		return 111
	}
	if err := os.WriteFile(svscanPIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to write %s: %v\n", svscanPIDFile(), err)
		return 111
	}
	if err := runBootstrap(exe); err != nil {
		fmt.Fprintf(os.Stderr, "[stage2] Warning: setup stopped at a failed step: %v\n", err)
	}
	if acct, err = lookupAccount(accountName); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: %v\n", err)
		return 111
	}
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to create %s: %v\n", scanDir, err)
		return 111
	}
	r := newReaper()
	sv, err := newSupervisor(scanDir, r)
	if err == nil {
		err = sv.listen()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to control %s: %v\n", scanDir, err)
		return 111
	}
	os.Chown(scanDir, acct.uid, acct.gid)
	for _, entry := range []string{"control", "lock"} {
		if p := filepath.Join(scanDir, ".s6-svscan", entry); exists(p) {
			os.Chown(p, acct.uid, acct.gid)
		}
	}
	reconcile := exec.Command(exe, StageVerb, "reconcile")
	reconcile.Stdout, reconcile.Stderr = os.Stdout, os.Stderr
	if os.Getuid() == 0 {
		reconcile.SysProcAttr = acct.credential()
	}
	reconcile.Run()

	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH)
	dashDir := filepath.Join(scanDir, "dashboard")
	if err := os.MkdirAll(dashDir, 0o755); err == nil {
		os.WriteFile(filepath.Join(dashDir, "type"), []byte("longrun\n"), 0o644)
	}
	sv.mu.Lock()
	sv.adopt("dashboard", dashDir, dashboardSpec(exe))
	sv.mu.Unlock()
	sv.scan(false)

	mainDone := make(chan syscall.WaitStatus, 1)
	argv := mainArgv(exe, args)
	cwd, _ := os.Getwd()
	if orig := os.Getenv("ICEBATH_ORIG_CWD"); orig != "" {
		cwd = orig
	}
	sys := &syscall.SysProcAttr{Setpgid: true}
	if os.Getuid() == 0 {
		sys = acct.credential()
		sys.Setpgid = true
	}
	if _, err := unix.IoctlGetTermios(0, unix.TCGETS); err == nil {
		sys.Foreground, sys.Ctty = true, 0
	}
	mainPID, err := r.spawn(argv, &syscall.ProcAttr{Dir: cwd, Env: withEnv(os.Environ(), "HOME", defaultHome),
		Files: []uintptr{0, 1, 2}, Sys: sys}, func(ws syscall.WaitStatus) { mainDone <- ws })
	if err != nil {
		fmt.Fprintf(os.Stderr, "s6-svscan: fatal: unable to run %s: %v\n", argv[0], err)
		sv.stopAll(graceFrom("S6_SERVICES_GRACETIME", 3*time.Second))
		return 127
	}
	forward := func(sig syscall.Signal) {
		if syscall.Kill(-mainPID, sig) != nil {
			syscall.Kill(mainPID, sig)
		}
	}
	halted := sv.halt
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGCHLD {
				r.reap()
				continue
			}
			forward(sig.(syscall.Signal))
		case <-halted:
			halted = nil
			forward(syscall.SIGTERM)
		case ws := <-mainDone:
			code := containerExit(ws)
			signal.Stop(sigs)
			go func() {
				for range sigs {
				}
			}()
			shutdown(sv, r, pid1)
			return code
		}
	}
}

// Stops the services, then in PID 1 mode terminates and reaps every process left in the container
func shutdown(sv *supervisor, r *reaper, pid1 bool) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.reap()
			}
		}
	}()
	sv.stopAll(graceFrom("S6_SERVICES_GRACETIME", 3*time.Second))
	close(done)
	if !pid1 {
		return
	}
	syscall.Kill(-1, syscall.SIGTERM)
	syscall.Kill(-1, syscall.SIGCONT)
	deadline := time.Now().Add(graceFrom("S6_KILL_GRACETIME", 3*time.Second))
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			return
		}
		if pid <= 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	syscall.Kill(-1, syscall.SIGKILL)
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(-1, &ws, 0, nil); err != nil && !errors.Is(err, syscall.EINTR) {
			return
		}
	}
}
