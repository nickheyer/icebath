package termlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Environment variable carrying the address space limit and command of a limited exec, as JSON
const limitEnv = "TERMLAB_LIMIT"

// Address space limit of builds, the 12000000 KiB of ulimit -v the project builds under
const BuildLimit = 12000000 * 1024

// Soft heap limit of the Go toolchain during builds
const BuildMemLimit = "4GiB"

// Address space cap and argv the helper process execs into
type limited struct {
	Bytes uint64
	Argv  []string
}

// Lowers the address space limit of this process, then replaces it with the command so every child inherits the limit
func execLimited(raw string) error {
	var l limited
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return err
	}
	if len(l.Argv) == 0 {
		return errors.New("no command")
	}
	if err := unix.Setrlimit(unix.RLIMIT_AS, &unix.Rlimit{Cur: l.Bytes, Max: l.Bytes}); err != nil {
		return fmt.Errorf("setrlimit: %w", err)
	}
	path, err := exec.LookPath(l.Argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, l.Argv, without(os.Environ(), limitEnv, DriverEnv))
}

// Command running argv under the address space limit, through this test binary as the limiting helper
func Limited(bytes uint64, argv ...string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(limited{Bytes: bytes, Argv: argv})
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self)
	cmd.Env = append(without(os.Environ(), limitEnv, DriverEnv), limitEnv+"="+string(raw))
	return cmd, nil
}

var built struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// Environment variable naming a prebuilt icebath binary the tests run instead of building one
const BinaryEnv = "TERMLAB_ICEBATH"

// Builds cmd/icebath once per test binary under the project memory limits, or takes the prebuilt binary named
func Icebath() (string, error) {
	built.once.Do(func() {
		if path := os.Getenv(BinaryEnv); path != "" {
			abs, err := filepath.Abs(path)
			if err != nil {
				built.err = err
				return
			}
			st, err := os.Stat(abs)
			if err != nil {
				built.err = fmt.Errorf("%s: %w", BinaryEnv, err)
				return
			}
			if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
				built.err = fmt.Errorf("%s names %s, which is not an executable file", BinaryEnv, abs)
				return
			}
			built.path = abs
			return
		}
		dir, err := os.MkdirTemp("", "termlab-bin-")
		if err != nil {
			built.err = err
			return
		}
		built.dir = dir
		out := filepath.Join(dir, "icebath")
		cmd, err := Limited(BuildLimit, "go", "build", "-tags", "goolm", "-o", out, "../../cmd/icebath")
		if err != nil {
			built.err = err
			return
		}
		cmd.Env = append(cmd.Env, "GOMEMLIMIT="+BuildMemLimit)
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Run(); err != nil {
			built.err = fmt.Errorf("go build ../../cmd/icebath: %w: %s", err, bytes.TrimSpace(log.Bytes()))
			return
		}
		built.path = out
	})
	return built.path, built.err
}

// Removes the directory the icebath build went into
func RemoveBuild() error {
	if built.dir == "" {
		return nil
	}
	return os.RemoveAll(built.dir)
}

// Writes an icebath home with a loopback provider and the settings given
func WriteConfig(dir string, settings map[string]string) error {
	arena := map[string]any{}
	cfg := map[string]any{
		"model":     map[string]any{"provider": "custom", "default": "termlab-model", "base_url": "http://127.0.0.1:9/v1", "api_key": "termlab-loopback"},
		"display":   map[string]any{"arena": arena},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
		"updates":   map[string]any{"check": false},
	}
	for key, value := range settings {
		if name, ok := strings.CutPrefix(key, "arena."); ok {
			arena[name] = value
			continue
		}
		node := cfg
		parts := strings.Split(key, ".")
		for _, part := range parts[:len(parts)-1] {
			next, ok := node[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[part] = next
			}
			node = next
		}
		node[parts[len(parts)-1]] = value
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=termlab-loopback\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), raw, 0o600)
}
