//go:build !windows

package cron

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/provider"
)

func killScriptProcess(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

func TestRunJobScriptConfiguredInterpreterIsRefusedUnlessAPythonPath(t *testing.T) {
	py := hostPython(t)
	cases := []struct {
		name     string
		make     func(d string) string
		expected string
	}{
		{"bare-name", func(d string) string { return "python3" }, "absolute or ~-prefixed"},
		{"missing", func(d string) string { return filepath.Join(d, "missing", "python3") }, "not found"},
		{"directory", func(d string) string { return d }, "not a file"},
		{"not-executable", func(d string) string {
			if err := os.WriteFile(filepath.Join(d, "python3"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(d, "python3")
		}, "not executable"},
		{"bash", func(d string) string { return "/bin/bash" }, "must be a Python executable"},
		{"python-symlink-to-bash", func(d string) string {
			if err := os.Symlink("/bin/bash", filepath.Join(d, "python")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(d, "python")
		}, "must be a Python executable"},
		{"pythonw", func(d string) string {
			if err := os.Symlink(py, filepath.Join(d, "pythonw")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(d, "pythonw")
		}, "must be a Python executable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scriptStore(t)
			script := writeScript(t, s, "job.py", "print(\"ran\")\n")
			interp := c.make(t.TempDir())
			ok, out := runScript(t, s, script, &interp)
			if ok || !strings.Contains(out, c.expected) {
				t.Fatalf("got %v %q", ok, out)
			}
		})
	}
}

func TestConfiguredInterpreterKeepsDotDotAndReportsEveryRefusalAsPythonDoes(t *testing.T) {
	s := scriptStore(t)
	script := writeScript(t, s, "job.py", "print(\"ran\")\n")
	d := t.TempDir()
	for _, dir := range []string{filepath.Join(d, "x"), filepath.Join(d, "other", "deep")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "python3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("other", "deep"), filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostPython(t), filepath.Join(d, "other", "python3")); err != nil {
		t.Fatal(err)
	}
	refusals := []struct{ interpreter, want string }{
		{d + "//x/./../python3", "Interpreter is not executable: " + d + "/x/../python3"},
		{" " + d + "/x/../python3  ", "Interpreter is not executable: " + d + "/x/../python3"},
		{d + "/python3/python3", "Unable to resolve interpreter path " + pyrepr.StrRepr(d+"/python3/python3") + ": [Errno 20] Not a directory: " + pyrepr.StrRepr(d+"/python3/python3")},
		{"~no_such_user_zz/python3", "Unable to resolve interpreter path '~no_such_user_zz/python3': Could not determine home directory."},
		{d + "/nope/../python3", "Interpreter not found: " + d + "/nope/../python3"},
		{"./~/x", "Interpreter not found: ./~/x"},
		{"python3", "Interpreter must be an absolute or ~-prefixed path (got 'python3'). Bare names like 'python3' are not stable across PATH changes."},
		{d, "Interpreter path is not a file: " + d},
		{"/bin/bash", "Interpreter must be a Python executable (python, python3, python3.12, ...): /bin/bash"},
		{"/x\x00y", "Script execution failed: stat: embedded null character in path"},
	}
	for _, c := range refusals {
		interp := c.interpreter
		ok, out := runScript(t, s, script, &interp)
		if ok || out != c.want {
			t.Fatalf("%q got %v %q want %q", c.interpreter, ok, out, c.want)
		}
	}
	linked := d + "/link/../python3"
	python, refusal, err := resolveCronInterpreter(linked)
	if err != nil || refusal != "" || python != linked {
		t.Fatalf("got %q %q %v", python, refusal, err)
	}
	ok, out := runScript(t, s, script, &linked)
	if !ok || out != "ran" {
		t.Fatalf("link first interpreter %v %q", ok, out)
	}
}

func TestScriptPathContainmentSymlinkEscapeBlocked(t *testing.T) {
	s := scriptStore(t)
	outside := filepath.Join(t.TempDir(), "outside_evil.py")
	if err := os.WriteFile(outside, []byte("print(\"escaped\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Home(), "scripts", "sneaky.py")); err != nil {
		t.Fatal(err)
	}
	ok, out := runScript(t, s, "sneaky.py", nil)
	if ok || !blockedOrOutside(out) {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathFollowsLinksBeforeDotDot(t *testing.T) {
	s := scriptStore(t)
	scripts := filepath.Join(s.Home(), "scripts")
	writeScript(t, s, "x.py", "print(\"top\")\n")
	writeScript(t, s, filepath.Join("sub", "x.py"), "print(\"sub\")\n")
	if err := os.MkdirAll(filepath.Join(scripts, "sub", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("sub", "deeper"), filepath.Join(scripts, "link")); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"link/../x.py", scripts + "/link/../x.py", "./link//../x.py"} {
		ok, out := runScript(t, s, ref, nil)
		if !ok || out != "sub" {
			t.Fatalf("%q got %v %q", ref, ok, out)
		}
	}
	away := t.TempDir()
	if err := os.MkdirAll(filepath.Join(away, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(away, "evil.py"), []byte("print(\"escaped\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeScript(t, s, "evil.py", "print(\"inside\")\n")
	if err := os.Symlink(filepath.Join(away, "inner"), filepath.Join(scripts, "out")); err != nil {
		t.Fatal(err)
	}
	ok, out := runScript(t, s, "out/../evil.py", nil)
	resolvedAway, err := pypath.Resolve(away)
	if err != nil {
		t.Fatal(err)
	}
	if ok || out != "Blocked: script path resolves outside the scripts directory ("+scripts+"): 'out/../evil.py'" || strings.Contains(out, resolvedAway) {
		t.Fatalf("got %v %q", ok, out)
	}
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKilledScriptThatOutlivesBothReapWaitsRaisesTimeoutExpired(t *testing.T) {
	s, _ := newTestStore(t)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = cronScriptSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	unreaped := &cronScriptProcess{cmd: cmd, exited: make(chan struct{})}
	err := s.terminateCronScriptProcess(unreaped)
	var expired *pysubprocess.TimeoutExpired
	if !errors.As(err, &expired) || err.Error() != "Command '['sleep', '30']' timed out after 1.0 seconds" {
		t.Fatalf("got %v", err)
	}
	if state, waitErr := cmd.Process.Wait(); waitErr != nil || state.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("the group was not terminated %v %v", state, waitErr)
	}
	exited := &cronScriptProcess{cmd: cmd, exited: make(chan struct{})}
	close(exited.exited)
	if err := s.terminateCronScriptProcess(exited); err != nil {
		t.Fatal(err)
	}
}

func TestScriptTerminationReapsDescendants(t *testing.T) {
	for _, trigger := range []string{"cancel", "timeout"} {
		for _, topology := range []string{"detached", "stubborn-pipe"} {
			t.Run(trigger+"-"+topology, func(t *testing.T) {
				s := scriptStore(t)
				prev := scriptTimeoutOverride
				scriptTimeoutOverride = 60
				if trigger == "timeout" {
					scriptTimeoutOverride = 3
				}
				t.Cleanup(func() { scriptTimeoutOverride = prev })
				ready := filepath.Join(s.Home(), "child.pid")
				ignoreTerm, detached := "", "False"
				if topology == "stubborn-pipe" {
					ignoreTerm = "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
				} else {
					detached = "True"
				}
				child := "import os, signal, time; from pathlib import Path; " + ignoreTerm +
					"Path(" + pyrepr.StrRepr(ready+".tmp") + ").write_text(str(os.getpid())); " +
					"os.replace(" + pyrepr.StrRepr(ready+".tmp") + ", " + pyrepr.StrRepr(ready) + "); time.sleep(60)"
				script := writeScript(t, s, "blocking.py", "import subprocess, sys, time\n"+
					"subprocess.Popen([sys.executable, '-c', "+pyrepr.StrRepr(child)+"], start_new_session="+detached+")\n"+
					"time.sleep(60)\n")
				cancel := &latchEvent{}
				type result struct {
					ok  bool
					out string
					err error
				}
				results := make(chan result, 1)
				go func() {
					ok, out, err := s.runJobScript(context.Background(), script, s.Home(), cancel, nil)
					results <- result{ok, out, err}
				}()
				readPid := func() int {
					raw, err := os.ReadFile(ready)
					if err != nil {
						return 0
					}
					pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
					return pid
				}
				pid := 0
				var early *result
				defer func() {
					cancel.Set()
					if pid == 0 {
						pid = readPid()
					}
					if pid != 0 && scriptProcessLive(pid) {
						killScriptProcess(pid)
					}
				}()
				for deadline := time.Now().Add(10 * time.Second); pid == 0 && early == nil && time.Now().Before(deadline); {
					select {
					case got := <-results:
						early = &got
					case <-time.After(10 * time.Millisecond):
						pid = readPid()
					}
				}
				if pid == 0 || early != nil {
					t.Fatalf("the child never reported ready, early result %+v", early)
				}
				if !scriptProcessLive(pid) {
					t.Fatal("child must acknowledge readiness before termination")
				}
				if trigger == "cancel" {
					cancel.Set()
				}
				var got result
				select {
				case got = <-results:
				case <-time.After(15 * time.Second):
					t.Fatal("the script run did not return")
				}
				want := "timed out"
				if trigger == "cancel" {
					want = "cancelled"
				}
				if got.err != nil || got.ok || !strings.Contains(got.out, want) {
					t.Fatalf("result %+v", got)
				}
				for deadline := time.Now().Add(5 * time.Second); scriptProcessLive(pid) && time.Now().Before(deadline); {
					time.Sleep(50 * time.Millisecond)
				}
				if scriptProcessLive(pid) {
					t.Fatalf("descendant %d survived %s", pid, trigger)
				}
			})
		}
	}
}

func TestRunJobNoAgentUsesConfiguredInterpreter(t *testing.T) {
	s := scriptStore(t)
	resetDeliver(t)
	python := hostPython(t)
	wrapper := filepath.Join(s.Home(), "venv", "bin", "python3")
	writeFixtureFile(t, wrapper, "#!"+python+"\n"+
		"import os, sys\n"+
		"env = os.environ.copy()\n"+
		"env[\"CRON_WRAPPER_USED\"] = \"1\"\n"+
		"os.execve(sys.executable, [sys.executable, *sys.argv[1:]], env)\n")
	if err := os.Chmod(wrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	writeScript(t, s, "marker.py", "import os\nprint(os.environ.get(\"CRON_WRAPPER_USED\", \"0\"))\n")
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "marker.py", NoAgent: true, Deliver: strPtr("local"), Interpreter: wrapper})
	out, err := s.runJob(context.Background(), job, runJobOptions{})
	if err != nil || !out.Success || out.Error != nil || out.FinalResponse != "1" || !strings.Contains(out.Output, "1") {
		t.Fatalf("outcome %+v %v", out, err)
	}
}

const presenceProbe = "#!/usr/bin/env bash\n" +
	"for n in JOB_SVC_TOKEN LAUNCH_ONLY_TOKEN; do [ -n \"${!n}\" ] && echo \"$n=set\" || echo \"$n=MISSING\"; done\n"

func TestNoAgentScriptGetsOwningProfilesDeclaredSecretNeverLaunchResidue(t *testing.T) {
	s := scriptStore(t)
	writeFixtureFile(t, filepath.Join(s.Home(), ".env"), "LAUNCH_ONLY_TOKEN=launch-secret\n")
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("JOB_SVC_TOKEN", "")
	os.Unsetenv("JOB_SVC_TOKEN")
	routed := filepath.Join(t.TempDir(), "routed")
	writeFixtureFile(t, filepath.Join(routed, ".env"), "JOB_SVC_TOKEN=routed-secret\n")
	writeFixtureFile(t, filepath.Join(routed, "config.yaml"), "terminal:\n  env_passthrough: [JOB_SVC_TOKEN]\n")
	writeFixtureFile(t, filepath.Join(routed, "scripts", "probe.sh"), presenceProbe)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	routedStore := claimStoreAt(t, routed)
	ctx := provider.WithSecretScope(context.Background(), provider.BuildProfileSecretScope(routed), routed)
	ok, out, err := routedStore.runJobScript(ctx, "probe.sh", "", nil, nil)
	if err != nil || !ok || out != "JOB_SVC_TOKEN=set\nLAUNCH_ONLY_TOKEN=MISSING" {
		t.Fatalf("got %v %q %v", ok, out, err)
	}
}

func TestNoAgentScriptOfLaunchProfileKeepsItsOwnEnvCredential(t *testing.T) {
	s := scriptStore(t)
	writeFixtureFile(t, filepath.Join(s.Home(), ".env"), "LAUNCH_ONLY_TOKEN=launch-secret\n")
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("JOB_SVC_TOKEN", "")
	os.Unsetenv("JOB_SVC_TOKEN")
	writeScript(t, s, "probe.sh", presenceProbe)
	ok, out := runScript(t, s, "probe.sh", nil)
	if !ok || out != "JOB_SVC_TOKEN=MISSING\nLAUNCH_ONLY_TOKEN=set" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func routedScriptContext(scope map[string]string, routed string) context.Context {
	return provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), scope, routed)
}

func sortedEnviron() []string {
	env := os.Environ()
	sort.Strings(env)
	return env
}

func TestARoutedProfileScriptNeverReceivesALaunchOnlyName(t *testing.T) {
	s := scriptStore(t)
	launch := s.Home()
	writeFixtureFile(t, filepath.Join(launch, ".env"), "LAUNCH_ONLY_VALUE=launch-only\nCUSTOM_CRON_VALUE=launch\n")
	launchHelper := filepath.Join(launch, "launch-helper.sh")
	writeFixtureFile(t, launchHelper, "printf 'LAUNCH_SKIPPED_SECRET=source-value\\nLAUNCH_VAULT_ONLY=launch-vault-value\\n'\n")
	writeFixtureFile(t, filepath.Join(launch, "config.yaml"), "secrets:\n  command:\n    enabled: true\n    command: sh "+launchHelper+"\n")
	for name, value := range map[string]string{"LAUNCH_ONLY_VALUE": "launch-only", "CUSTOM_CRON_VALUE": "launch", "LAUNCH_SKIPPED_SECRET": "launch-value"} {
		t.Setenv(name, value)
	}
	for _, name := range []string{"LAUNCH_VAULT_ONLY", "ROUTED_VAULT_ONLY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	provider.ResetSecretSourceCache("")
	t.Cleanup(func() { provider.ResetSecretSourceCache("") })
	provider.ApplyExternalSecretSources(launch)
	if os.Getenv("LAUNCH_VAULT_ONLY") != "launch-vault-value" || os.Getenv("LAUNCH_SKIPPED_SECRET") != "launch-value" {
		t.Fatalf("the launch source must apply one name and lose the other to the env, got %q %q", os.Getenv("LAUNCH_VAULT_ONLY"), os.Getenv("LAUNCH_SKIPPED_SECRET"))
	}
	routed := filepath.Join(launch, "profiles", "ops")
	routedHelper := filepath.Join(routed, "routed-helper.sh")
	writeFixtureFile(t, routedHelper, "printf 'ROUTED_VAULT_ONLY=routed-vault-value\\n'\n")
	writeFixtureFile(t, filepath.Join(routed, "config.yaml"), "secrets:\n  command:\n    enabled: true\n    command: sh "+routedHelper+"\n")
	if hydrated := provider.HydrateProfileSecretSources(routed); hydrated["ROUTED_VAULT_ONLY"] != "routed-vault-value" {
		t.Fatalf("routed sources hydrated %v", hydrated)
	}
	supplied := provider.SourceSuppliedNames()
	for _, name := range []string{"LAUNCH_VAULT_ONLY", "LAUNCH_SKIPPED_SECRET", "ROUTED_VAULT_ONLY"} {
		if !slices.Contains(supplied, name) {
			t.Fatalf("%s is not recorded as source supplied in %v", name, supplied)
		}
	}
	before := sortedEnviron()
	writeFixtureFile(t, filepath.Join(routed, "scripts", "probe_launch_only.sh"), "#!/usr/bin/env bash\n"+
		"echo \"${CUSTOM_CRON_VALUE}|${ROUTED_VAULT_ONLY}|${LAUNCH_ONLY_VALUE:-<unset>}|${LAUNCH_VAULT_ONLY:-<unset>}|${LAUNCH_SKIPPED_SECRET:-<unset>}\"\n")
	ctx := routedScriptContext(map[string]string{"CUSTOM_CRON_VALUE": "routed", "ROUTED_VAULT_ONLY": "routed-vault-value"}, routed)
	ok, out, err := claimStoreAt(t, routed).runJobScript(ctx, "probe_launch_only.sh", "", nil, nil)
	if err != nil || !ok || out != "routed|routed-vault-value|<unset>|<unset>|<unset>" {
		t.Fatalf("got %v %q %v", ok, out, err)
	}
	if after := sortedEnviron(); !slices.Equal(after, before) {
		t.Fatalf("the parent environment moved\nbefore %v\nafter  %v", before, after)
	}
}

func TestARoutedProfileScriptKeepsAdministratorManagedValuesOverItsOwn(t *testing.T) {
	s := scriptStore(t)
	launch := s.Home()
	managed := filepath.Join(launch, "managed")
	writeFixtureFile(t, filepath.Join(managed, ".env"), "ORG_POLICY_FLAG=managed-value\n")
	t.Setenv(config.EnvManagedDir, managed)
	t.Setenv("ORG_POLICY_FLAG", "placeholder")
	if _, err := provider.LoadIcebathDotenv(context.Background(), provider.DotenvLoadOptions{Home: launch, SkipExternalSecrets: true}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ORG_POLICY_FLAG"); got != "managed-value" {
		t.Fatalf("the boot time managed load left %q", got)
	}
	routed := filepath.Join(launch, "profiles", "ops")
	writeFixtureFile(t, filepath.Join(routed, "scripts", "probe_policy.sh"), "#!/usr/bin/env bash\necho \"${ORG_POLICY_FLAG:-<unset>}\"\n")
	ctx := routedScriptContext(map[string]string{"ORG_POLICY_FLAG": "user-value"}, routed)
	ok, out, err := claimStoreAt(t, routed).runJobScript(ctx, "probe_policy.sh", "", nil, nil)
	if err != nil || !ok || out != "managed-value" {
		t.Fatalf("got %v %q %v", ok, out, err)
	}
	if got := os.Getenv("ORG_POLICY_FLAG"); got != "managed-value" {
		t.Fatalf("the parent value moved to %q", got)
	}
}

func TestPythonScriptsRunOnThePythonFoundOnPath(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, filepath.Join(dir, "python3"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "python3"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	s, _ := newTestStore(t)
	argv, refusal, err := s.scriptArgv("/scripts/job.py", nil)
	if err != nil || refusal != "" || !slices.Equal(argv, []string{filepath.Join(dir, "python3"), "/scripts/job.py"}) {
		t.Fatalf("argv %v refusal %q err %v", argv, refusal, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, _, err := s.scriptArgv("/scripts/job.py", nil); err == nil || !strings.Contains(err.Error(), "no Python interpreter found on PATH") {
		t.Fatalf("an empty PATH resolved: %v", err)
	}
}
