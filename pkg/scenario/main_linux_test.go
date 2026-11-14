package scenario

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	modulePath       = "github.com/nickheyer/icebath"
	isolatedEnv      = "ICEBATH_SCENARIO_ISOLATED"
	runTagEnv        = "ICEBATH_SCENARIO_RUN"
	exercisedEnv     = "ICEBATH_SCENARIO_EXERCISED"
	exercisedDefault = "/home/nick/code/icebath/dev/ledger2/exercised.tsv"
)

// Built binary, its coverage directory and the module root shared by every scenario of one run
var suite struct {
	bin      string
	coverDir string
	root     string
	runTag   string
}

// Runs the scenarios in a copy of this binary that owns a private loopback only network
func TestMain(m *testing.M) {
	if os.Getenv(isolatedEnv) == "" {
		os.Exit(supervise())
	}
	os.Exit(runIsolated(m))
}

// Starts the isolated copy, waits for it and kills every process of the run it left alive
func supervise() int {
	runtime.LockOSThread()
	tag := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Args[0] = os.Args[0]
	cmd.Env = append(os.Environ(), isolatedEnv+"=1", runTagEnv+"="+tag)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		Pdeathsig:   syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "scenario: starting the isolated run needs unprivileged user and network namespaces: %v\n", err)
		return 2
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range signals {
			cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	signal.Stop(signals)
	killTagged(runTagEnv+"="+tag, 0)
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "scenario: isolated run ended abnormally: %v\n", err)
	return 2
}

// Builds the instrumented binary, runs the selected scenarios and writes the exercised table after a whole suite
func runIsolated(m *testing.M) int {
	flag.Parse()
	if err := loopbackUp(); err != nil {
		fmt.Fprintf(os.Stderr, "scenario: bringing the private loopback up: %v\n", err)
		return 2
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		return 2
	}
	work, err := os.MkdirTemp("", "icebath-scenario-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		return 2
	}
	defer os.RemoveAll(work)
	suite.root, suite.runTag = root, os.Getenv(runTagEnv)
	suite.bin = filepath.Join(work, "bin", "icebath")
	suite.coverDir = filepath.Join(work, "cover")
	for _, dir := range []string{filepath.Dir(suite.bin), suite.coverDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "scenario:", err)
			return 2
		}
	}
	built := time.Now()
	build := exec.Command("go", "build", "-tags=goolm", "-cover", "-coverpkg="+modulePath+"/...", "-o", suite.bin, "./cmd/icebath")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "scenario: building cmd/icebath with coverage failed: %v\n%s\n", err, bytes.TrimSpace(out))
		return 2
	}
	code := m.Run()
	if !wholeSuite() {
		return code
	}
	out := os.Getenv(exercisedEnv)
	if out == "" {
		out = exercisedDefault
	}
	if err := writeExercised(root, suite.coverDir, work, out, built); err != nil {
		fmt.Fprintf(os.Stderr, "scenario: writing %s: %v\n", out, err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

// Reports whether this run selected every scenario, the only run whose coverage describes the suite
func wholeSuite() bool {
	run, skip, list := flag.Lookup("test.run"), flag.Lookup("test.skip"), flag.Lookup("test.list")
	return (run.Value.String() == "" || run.Value.String() == "Scenario") && skip.Value.String() == "" && list.Value.String() == ""
}

// Sets the loopback interface of the private network up and drops the capability that allowed it
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
}

// Lists live processes whose environment carries the marker, zombies and this process left out
func tagged(marker string) []int {
	needle := []byte(marker + "\x00")
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !bytes.Contains(append(env, 0), needle) {
			continue
		}
		if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil {
			if i := bytes.LastIndexByte(stat, ')'); i > 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
				continue
			}
		}
		pids = append(pids, pid)
	}
	return pids
}

// Asks marked processes to end, waits out the grace period, kills the rest and returns how many it killed
func killTagged(marker string, grace time.Duration) int {
	for _, pid := range tagged(marker) {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && len(tagged(marker)) > 0; {
		time.Sleep(100 * time.Millisecond)
	}
	killed := 0
	for attempt := 0; attempt < 20; attempt++ {
		pids := tagged(marker)
		if len(pids) == 0 {
			break
		}
		if attempt == 0 {
			killed = len(pids)
		}
		for _, pid := range pids {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return killed
}
