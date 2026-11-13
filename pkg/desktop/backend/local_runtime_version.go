package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	localRuntimeVersionTTL = 5 * time.Second
	versionProbeMaxBuffer  = 64 * 1024
)

var localRuntimeVersionLine = regexp.MustCompile(`(?m)^\s*Icebath Agent\s+([^\s(]+)`)

// Client version a launcher prints for --version, without a leading v, empty when absent
func ParseLocalRuntimeVersion(output string) string {
	m := localRuntimeVersionLine.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	return strings.TrimPrefix(m[1], "v")
}

// limitedBuffer fails a write once the captured output passes its cap
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("stdout maxBuffer length exceeded")
	}
	return b.Buffer.Write(p)
}

// Runs a launcher for its stdout within the probe timeout and output cap, hidden and through cmd.exe for scripts
var runVersionProbe = func(command string, args []string, cwd string, env map[string]string, timeout time.Duration, shell bool, maxBuffer int) (string, error) {
	cmd := spawnCommand(command, args, shell)
	cmd.Dir = cwd
	cmd.Env = envList(env)
	out := &limitedBuffer{max: maxBuffer}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
		return out.String(), nil
	case <-ctx.Done():
		terminateProcess(cmd.Process)
		<-done
		return "", errors.New("version probe timed out")
	}
}

var localRuntimeVersion struct {
	mu        sync.Mutex
	key       string
	version   string
	expiresAt time.Time
	cached    bool
	flightKey string
	flight    *Future[string]
}

// Version of the local installation from its launcher, cached briefly per root and home with one probe in flight
func ResolveLocalRuntimeVersion(root, icebathHome string, launcher func(root string) string, home HomeInputs) string {
	key := root + "\x00" + icebathHome
	c := &localRuntimeVersion
	c.mu.Lock()
	if c.cached && c.key == key && c.expiresAt.After(time.Now()) {
		version := c.version
		c.mu.Unlock()
		return version
	}
	if c.flight != nil && c.flightKey == key {
		flight := c.flight
		c.mu.Unlock()
		version, _ := flight.Wait()
		return version
	}
	flight := NewFuture(func() (string, error) {
		version := probeLocalRuntimeVersion(root, icebathHome, launcher, home)
		c.mu.Lock()
		c.key, c.version, c.expiresAt, c.cached = key, version, time.Now().Add(localRuntimeVersionTTL), true
		c.mu.Unlock()
		return version, nil
	})
	c.flightKey, c.flight = key, flight
	c.mu.Unlock()
	version, _ := flight.Wait()
	c.mu.Lock()
	if c.flight == flight {
		c.flight = nil
	}
	c.mu.Unlock()
	return version
}

// Runs the installation launcher with --version in the desktop backend env, empty on any failure
func probeLocalRuntimeVersion(root, icebathHome string, launcher func(root string) string, home HomeInputs) string {
	isWindows := runtime.GOOS == "windows"
	command := launcher(root)
	if command == "" {
		return ""
	}
	lower := strings.ToLower(command)
	shell := isWindows && (strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat"))
	current := ProcessEnv()
	env := make(map[string]string, len(current)+5)
	for k, v := range current {
		env[k] = v
	}
	for k, v := range BuildDesktopBackendEnv(current, electronPlatformName(), home) {
		env[k] = v
	}
	env["ICEBATH_HOME"] = icebathHome
	out, err := runVersionProbe(WindowsShellCommand(command, shell, isWindows), []string{"--version"}, root, env, ResolveProbeTimeout(os.Getenv), shell, versionProbeMaxBuffer)
	if err != nil {
		return ""
	}
	return ParseLocalRuntimeVersion(out)
}

// Version of this machine's install, the packaged stamp version for a shipped payload, else the launcher version
func ResolveDesktopClientVersion(packagedPayload bool, packagedVersion, root, icebathHome string, launcher func(root string) string, home HomeInputs) string {
	if packagedPayload {
		return packagedVersion
	}
	return ResolveLocalRuntimeVersion(root, icebathHome, launcher, home)
}
