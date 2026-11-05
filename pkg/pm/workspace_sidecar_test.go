package pm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const wsLazyRefusal = "lazy installs are disabled — run `icebath plugins install` and approve Node dependencies"

type wsAcquisition struct {
	name     string
	explicit bool
}

// Replaces package acquisition with a recorder answering through acquire
func wsAcquire(t *testing.T, acquire func(name string, opts EnsureOptions) (*Runner, error)) *[]wsAcquisition {
	t.Helper()
	calls := &[]wsAcquisition{}
	peSwap(t, &sidecarEnsure, func(ctx context.Context, name string, opts EnsureOptions) (*Runner, error) {
		*calls = append(*calls, wsAcquisition{name, opts.Explicit})
		return acquire(name, opts)
	})
	return calls
}

// Writes an executable shell script
func wsScript(t *testing.T, path, body string) {
	t.Helper()
	peWrite(t, path, "#!/bin/sh\n"+body)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Pins the process to a legacy C locale so no decoding can lean on a UTF-8 default
func wsLegacyLocale(t *testing.T) {
	t.Helper()
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")
	t.Setenv("PYTHONUTF8", "0")
}

func TestWorkspaceSidecarNoPackageJSONNeverAcquiresNPM(t *testing.T) {
	isolateHome(t)
	wsAcquire(t, func(string, EnsureOptions) (*Runner, error) {
		t.Fatal("a dependency-free plugin must not acquire npm")
		return nil, nil
	})
	if reason := InstallNodeSidecar(context.Background(), t.TempDir(), false); reason != "" {
		t.Fatalf("reason %q", reason)
	}
}

func TestWorkspaceSidecarAcquisitionReceivesConsentAndReportsRefusal(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			isolateHome(t)
			t.Setenv(EnvDisableLazyInstalls, "")
			dir := t.TempDir()
			peWrite(t, filepath.Join(dir, "package.json"), "{}")
			calls := wsAcquire(t, func(name string, opts EnsureOptions) (*Runner, error) {
				return nil, NewInstallError(name, "acquisition refused", "")
			})
			reason := InstallNodeSidecar(context.Background(), dir, explicit)
			if !strings.Contains(reason, "acquisition refused") || !reflect.DeepEqual(*calls, []wsAcquisition{{"npm", explicit}}) {
				t.Fatalf("reason %q calls %v", reason, *calls)
			}
		})
	}
}

func TestWorkspaceSidecarRealNPMUsesPairedNodeWithEmptyAmbientPath(t *testing.T) {
	npm, errNPM := exec.LookPath("npm")
	node, errNode := exec.LookPath("node")
	if errNPM != nil || errNode != nil {
		t.Fatalf("npm and node are required: %v %v", errNPM, errNode)
	}
	realHome := isolateHome(t)
	for _, executable := range []string{npm, node} {
		if pathIsRelativeTo(absoluteUnresolved(executable), realHome) {
			t.Fatalf("%s lives inside the icebath home", executable)
		}
	}
	resolved := resolvePath(npm)
	cli := resolved
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(resolved)), "lib", "npm*", "bin", "npm-cli.js")); len(matches) > 0 {
		cli = matches[0]
	}
	tmp := t.TempDir()
	npmBin, nodeBin := filepath.Join(tmp, "npm", "bin"), filepath.Join(tmp, "node", "bin")
	wsScript(t, filepath.Join(npmBin, "npm"), "exec node "+shellQuote(cli)+" \"$@\"\n")
	os.MkdirAll(nodeBin, 0o777)
	if err := os.Symlink(node, filepath.Join(nodeBin, "node")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("PATH", "")
	t.Setenv(EnvDisableLazyInstalls, "1")
	runner := &Runner{Name: "npm", Env: ComposeEnv([]EnvDiff{{"PATH": []string{nodeBin}}, {"PATH": []string{npmBin}}}, nil)}
	calls := wsAcquire(t, func(string, EnsureOptions) (*Runner, error) { return runner, nil })
	dependency, plugin := filepath.Join(tmp, "side-dep"), filepath.Join(tmp, "plugin")
	peWrite(t, filepath.Join(dependency, "package.json"), `{"name": "side-dep", "version": "1.0.0", "main": "index.js"}`)
	peWrite(t, filepath.Join(dependency, "index.js"), "module.exports = 'isolated';")
	peWrite(t, filepath.Join(plugin, "package.json"), `{"name": "plugin", "version": "1.0.0", "dependencies": {"side-dep": "file:../side-dep"}}`)
	ambient := pyEnviron()
	ctx := context.Background()
	if reason := InstallNodeSidecar(ctx, plugin, true); reason != "" {
		t.Fatalf("install %q", reason)
	}
	lock := peRead(t, filepath.Join(plugin, "package-lock.json"))
	stale := filepath.Join(plugin, "node_modules", "stale-file")
	peWrite(t, stale, "remove me")
	if reason := InstallNodeSidecar(ctx, plugin, true); reason != "" {
		t.Fatalf("ci %q", reason)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Fatal("npm ci kept an extraneous file")
	}
	if peRead(t, filepath.Join(plugin, "package-lock.json")) != lock {
		t.Fatal("npm ci rewrote the lock")
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := runner.Command(runCtx, "node", "-e", "console.log(require('side-dep'))")
	cmd.Dir = plugin
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "isolated" {
		t.Fatalf("node probe %q %v", out, err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "node_modules")); err == nil {
		t.Fatal("node_modules escaped the plugin")
	}
	if !reflect.DeepEqual(pyEnviron(), ambient) {
		t.Fatal("ambient environment mutated")
	}
	if !reflect.DeepEqual(*calls, []wsAcquisition{{"npm", true}, {"npm", true}}) {
		t.Fatalf("calls %v", *calls)
	}
	peWrite(t, filepath.Join(plugin, "package.json"), "{malformed")
	if reason := InstallNodeSidecar(ctx, plugin, true); !strings.Contains(reason, "exited") || !strings.Contains(reason, "JSON") {
		t.Fatalf("malformed %q", reason)
	}
}

func TestWorkspaceSidecarOnDemandInstallRespectsLazyRefusal(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	peWrite(t, filepath.Join(dir, "package.json"), "{}")
	t.Setenv(EnvDisableLazyInstalls, "1")
	marker := filepath.Join(dir, "ran")
	wsScript(t, filepath.Join(dir, "npm"), "touch "+shellQuote(marker)+"\n")
	wsAcquire(t, func(string, EnsureOptions) (*Runner, error) {
		return &Runner{Name: "npm", Env: map[string]string{"PATH": dir}}, nil
	})
	if reason := InstallNodeSidecar(context.Background(), dir, false); reason != wsLazyRefusal {
		t.Fatalf("reason %q", reason)
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("installed npm mutated the sidecar while on-demand installs are disabled")
	}
}

func TestWorkspaceEncodingUVFailureRetainsUTF8BuildDiagnostic(t *testing.T) {
	diagnostic := "🔍 cryptography: OpenSSL headers not found"
	for _, stage := range []string{"lock", "sync"} {
		for _, stream := range []string{"stdout", "stderr"} {
			for _, suffix := range []string{"", "\xff"} {
				t.Run(stage+"-"+stream+"-"+map[string]string{"": "utf8", "\xff": "invalid-byte"}[suffix], func(t *testing.T) {
					wsLegacyLocale(t)
					tmp := t.TempDir()
					raw := diagnostic + suffix + "\n"
					expected := diagnostic
					if suffix != "" {
						expected += "�"
					}
					core := filepath.Join(tmp, "core")
					peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"test-core\"\nversion=\"1\"\n")
					tools := filepath.Join(tmp, "tools")
					for _, s := range []string{"lock", "sync"} {
						out, errOut, code := raw, raw, "0"
						if s == stage {
							out, errOut, code = "", "", "17"
							if stream == "stdout" {
								out = raw
							} else {
								errOut = raw
							}
						}
						peWrite(t, filepath.Join(tools, s+".stdout"), out)
						peWrite(t, filepath.Join(tools, s+".stderr"), errOut)
						peWrite(t, filepath.Join(tools, s+".code"), code)
					}
					uv := filepath.Join(tools, "uv")
					wsScript(t, uv, "d=$(dirname \"$0\")\ncat \"$d/$1.stdout\"\ncat \"$d/$1.stderr\" >&2\nexit $(cat \"$d/$1.code\")\n")
					var results []uvResult
					peSwap(t, &uvCaptureRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
						r, err := runCaptured(ctx, command, cwd, env, timeout)
						results = append(results, r)
						return r, err
					})
					peSwap(t, &uvStreamRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration, write func(string)) (uvResult, error) {
						r, err := runStreaming(ctx, command, cwd, env, timeout, write)
						results = append(results, r)
						return r, err
					})
					e := &PythonEnvironment{UV: uv, Python: peHostPython(t), Destination: filepath.Join(tmp, "venv"), Cache: filepath.Join(tmp, "cache"), Env: pyEnviron()}
					err := LockAndSync(context.Background(), wsMembers(), []string{}, LockAndSyncOptions{Root: filepath.Join(tmp, "workspace"), Source: core, Environment: e})
					ie, exact := err.(*InstallError)
					if !exact || ie.Cause != "uv "+stage+" exited 17: "+expected {
						t.Fatalf("got %#v", err)
					}
					last := results[len(results)-1]
					got := last.Stdout
					if stream == "stderr" {
						got = last.Stderr
					}
					if got != expected+"\n" {
						t.Fatalf("decoded %s %q", stream, got)
					}
				})
			}
		}
	}
}

func TestWorkspaceEncodingNodeSidecarRetainsOutputAndExitStatus(t *testing.T) {
	diagnostic := "🔍 node-gyp: build toolchain unavailable"
	for _, installCmd := range []string{"ci", "install"} {
		for _, stream := range []string{"stdout", "stderr"} {
			for _, code := range []int{0, 17} {
				t.Run(fmt.Sprintf("%s-%s-%d", installCmd, stream, code), func(t *testing.T) {
					wsLegacyLocale(t)
					isolateHome(t)
					t.Setenv(EnvDisableLazyInstalls, "")
					t.Setenv("PM_SIDECAR_AMBIENT", "process")
					tmp := t.TempDir()
					peWrite(t, filepath.Join(tmp, "package.json"), "{}")
					if installCmd == "ci" {
						peWrite(t, filepath.Join(tmp, "package-lock.json"), "{}")
					}
					npmDir := filepath.Join(tmp, "pm-bin")
					redirect := ""
					if stream == "stderr" {
						redirect = " >&2"
					}
					npm := filepath.Join(npmDir, "npm")
					wsScript(t, npm, "d=${0%/*}\n{ printf '%s\\n' \"$0\" \"$@\"; pwd; printf '%s\\n' \"$PM_SIDECAR_RUNNER\" \"${PM_SIDECAR_AMBIENT-unset}\"; } > \"$d/seen\"\n"+
						"printf '"+wsOctal(diagnostic+"\xff\n")+"'"+redirect+"\nexit "+fmt.Sprint(code)+"\n")
					env := pyEnviron()
					delete(env, "PM_SIDECAR_AMBIENT")
					env["PATH"], env["PM_SIDECAR_RUNNER"] = npmDir, "runner"
					calls := wsAcquire(t, func(string, EnsureOptions) (*Runner, error) { return &Runner{Name: "npm", Env: env}, nil })
					reason := InstallNodeSidecar(context.Background(), tmp, false)
					if !reflect.DeepEqual(*calls, []wsAcquisition{{"npm", false}}) {
						t.Fatalf("calls %v", *calls)
					}
					seen := strings.Split(strings.TrimSuffix(peRead(t, filepath.Join(npmDir, "seen")), "\n"), "\n")
					want := []string{npm, installCmd, "--no-audit", "--no-fund", tmp, "runner", "unset"}
					if !reflect.DeepEqual(seen, want) {
						t.Fatalf("npm saw %q", seen)
					}
					if code == 0 && reason != "" {
						t.Fatalf("reason %q", reason)
					}
					if code != 0 && reason != fmt.Sprintf("npm %s exited %d: %s�", installCmd, code, diagnostic) {
						t.Fatalf("reason %q", reason)
					}
				})
			}
		}
	}
}

// Shell printf format writing every byte of s through an octal escape
func wsOctal(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&out, "\\%03o", s[i])
	}
	return out.String()
}
