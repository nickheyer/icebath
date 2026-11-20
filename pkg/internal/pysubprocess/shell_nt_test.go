package pysubprocess

import (
	"errors"
	"testing"
)

func TestNtShellBuildsTheCPythonCommandLine(t *testing.T) {
	env := map[string]string{"ComSpec": `C:\Windows\system32\cmd.exe`, "SystemRoot": `D:\Win`}
	getenv := func(k string) string { return env[k] }
	exe, line, err := NtShell(`echo "hi"`, getenv)
	if err != nil || exe != `C:\Windows\system32\cmd.exe` || line != `C:\Windows\system32\cmd.exe /c "echo "hi""` {
		t.Fatalf("%q %q %v", exe, line, err)
	}
	delete(env, "ComSpec")
	exe, line, err = NtShell("dir", getenv)
	if err != nil || exe != `D:\Win\System32\cmd.exe` || line != `D:\Win\System32\cmd.exe /c "dir"` {
		t.Fatalf("%q %q %v", exe, line, err)
	}
	env["SystemRoot"] = "C:"
	if _, _, err = NtShell("dir", getenv); !errors.Is(err, ErrNtShellNotFound) || err.Error() != "shell not found: neither %ComSpec% nor %SystemRoot% is set" {
		t.Fatalf("drive relative SystemRoot %v", err)
	}
	delete(env, "SystemRoot")
	if _, _, err = NtShell("dir", getenv); !errors.Is(err, ErrNtShellNotFound) {
		t.Fatalf("no SystemRoot %v", err)
	}
	env["ComSpec"] = "cmd.exe"
	exe, line, err = NtShell("ver", getenv)
	if err != nil || exe != "" || line != `cmd.exe /c "ver"` {
		t.Fatalf("relative ComSpec %q %q %v", exe, line, err)
	}
}

func TestNtCommandLineModuleSearchesLikeCreateProcess(t *testing.T) {
	s := NtSearch{AppDir: `C:\app`, Cwd: `C:\work`, SystemDir: `C:\Windows\System32`, WindowsDir: `C:\Windows`, Path: `C:\tools;;C:\bin`}
	files := map[string]bool{}
	isFile := func(p string) bool { return files[p] }
	order := []string{`C:\app\cmd.exe`, `C:\work\cmd.exe`, `C:\Windows\System32\cmd.exe`, `C:\Windows\System\cmd.exe`, `C:\Windows\cmd.exe`, `C:\tools\cmd.exe`, `C:\bin\cmd.exe`}
	for i := len(order) - 1; i >= 0; i-- {
		files[order[i]] = true
		if got, ok := NtCommandLineModule(`cmd /c "ver"`, s, isFile); !ok || got != order[i] {
			t.Fatalf("step %d found %q %v", i, got, ok)
		}
	}
	s.SkipCwd = true
	delete(files, `C:\app\cmd.exe`)
	if got, _ := NtCommandLineModule(`cmd /c "ver"`, s, isFile); got != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("NoDefaultCurrentDirectoryInExePath kept the cwd %q", got)
	}
	files = map[string]bool{`C:\work\my shell.exe`: true}
	if got, ok := NtCommandLineModule(`my shell.exe /c "ver"`, NtSearch{Cwd: `C:\work`}, isFile); !ok || got != `C:\work\my shell.exe` {
		t.Fatalf("word by word %q %v", got, ok)
	}
	files = map[string]bool{`C:\work\sub\sh.com`: true, `C:\q d\x.exe`: true}
	if got, ok := NtCommandLineModule(`sub\sh.com /c "ver"`, NtSearch{Cwd: `C:\work`}, isFile); !ok || got != `C:\work\sub\sh.com` {
		t.Fatalf("relative path %q %v", got, ok)
	}
	if got, ok := NtCommandLineModule(`"C:\q d\x" /c "ver"`, NtSearch{}, isFile); !ok || got != `C:\q d\x.exe` {
		t.Fatalf("quoted %q %v", got, ok)
	}
	if got, ok := NtCommandLineModule(`nope /c "ver"`, s, isFile); ok {
		t.Fatalf("missing module found %q", got)
	}
}
