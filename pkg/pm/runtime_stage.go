package pm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StageRuntimeOptions name the locked project, network mode, cache, validation imports and an optional wheelhouse
type StageRuntimeOptions struct {
	Project    string
	Offline    bool
	Cache      string
	Imports    string
	Wheelhouse string
}

// Builds a locked dependency environment at its final path from a scratch copy of the project and validates it
func StageRuntime(ctx context.Context, uv, python, destination string, opts StageRuntimeOptions) (string, error) {
	if opts.Project == "" {
		return "", errors.New("a runtime stage needs the locked project it builds")
	}
	imports := opts.Imports
	if imports == "" {
		imports = PMRuntimeImports
	}
	destination = absoluteUnresolved(destination)
	env, err := RuntimeEnvironment(ctx)
	if err != nil {
		return "", err
	}
	cache := opts.Cache
	if cache == "" {
		if cache, err = UvCacheDir(ctx); err != nil {
			return "", err
		}
	} else {
		cache = absoluteUnresolved(cache)
	}
	environment := &PythonEnvironment{UV: uv, Python: python, Destination: destination, Cache: cache, Env: env,
		Offline: opts.Offline || opts.Wheelhouse != "", Output: os.Stderr, NoConfig: true}
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return "", err
	}
	snapshot, err := os.MkdirTemp(filepath.Dir(destination), "pm-project-")
	if err != nil {
		return "", err
	}
	err = func() error {
		for _, name := range []string{"pyproject.toml", "uv.lock"} {
			if err := copyFileBytes(filepath.Join(opts.Project, name), filepath.Join(snapshot, name)); err != nil {
				return err
			}
		}
		if err := environment.Create(ctx); err != nil {
			return err
		}
		if opts.Wheelhouse == "" {
			return environment.Sync(ctx, snapshot, SyncOptions{Locked: true, NoDefaultGroups: true, NoInstallProject: true, Timeout: 600 * time.Second})
		}
		return environment.InstallWheelhouse(ctx, snapshot, opts.Wheelhouse, 600*time.Second)
	}()
	if rerr := os.RemoveAll(snapshot); err == nil && rerr != nil {
		err = rerr
	}
	if err != nil {
		return "", err
	}
	executable := environment.Executable()
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, executable, "-I", "-B", "-c", imports)
	cmd.Env = EnvList(env)
	cmd.WaitDelay = time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if checkCtx.Err() == context.DeadlineExceeded {
		return "", &uvTimeoutError{Argv: cmd.Args, Timeout: 30 * time.Second}
	}
	if code, err := exitStatus(runErr); err != nil {
		return "", err
	} else if code != 0 {
		if err := utf8Strict([]byte(stderr.String())); err != nil {
			return "", err
		}
		return "", NewInstallError("pm-runtime", "dependency validation failed: "+pyStrip(universalNewlines(stderr.String())), "")
	}
	return executable, nil
}

// Copies a file's bytes the way shutil copyfile does, without metadata
func copyFileBytes(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o666)
}
