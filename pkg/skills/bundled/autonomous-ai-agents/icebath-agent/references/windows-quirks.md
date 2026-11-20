# Windows-Specific Quirks

Icebath runs natively on Windows (PowerShell, cmd, Windows Terminal, git-bash
mintty, VS Code integrated terminal). Most of it just works, but a handful
of differences between Win32 and POSIX have bitten us — document new ones
here as you hit them so the next person (or the next session) doesn't
rediscover them from scratch.

### Input / Keybindings

**Alt+Enter doesn't insert a newline** — Windows Terminal (and mintty) grab it
for fullscreen before the Icebath terminal UI sees it. Use **Shift+Enter** or
**Ctrl+J** instead (the other keys of the `tui.newline` action, `pkg/tui/app.go`).
To inspect how your console reports a keystroke, run
`while ($true) { [Console]::ReadKey($true) }` in PowerShell (Ctrl+C ends it).

### Config / Files

**HTTP 400 "No models provided" on first run** — `config.yaml` was saved with
a UTF-8 BOM (Notepad does this). Re-save as UTF-8 without BOM;
`icebath config edit` writes correctly.

### `execute_code` / Sandbox

**WinError 10106** from the sandbox child process — it can't create an
`AF_INET` socket. Root cause is usually Icebath's env scrubber dropping
`SYSTEMROOT`/`WINDIR`/`COMSPEC` (Python's `socket` needs `SYSTEMROOT` to find
`mswsock.dll`), not a broken Winsock LSP. The `windowsEssentialEnvVars`
allowlist in `pkg/codeexec/env.go` covers it; if you still hit it,
echo `os.environ` inside an `execute_code` block to confirm `SYSTEMROOT` is set.

### Testing on Windows

Install the Go version named in `go.mod`, then fetch the modules once from
the checkout root:

```powershell
go mod download
```

Run the tests from PowerShell with the `goolm` tag and a memory limit:

```powershell
$env:GOMEMLIMIT = "6GiB"
go test -tags goolm ./pkg/foo/ -run TestBar -v
```

Every test binary gets a private user home under `%TEMP%`
(`pkg/internal/homeguard`), so tests never read or write the real
`%LOCALAPPDATA%\icebath`. Windows-only tests live in `*_windows_test.go`
files and run only on a Windows host.

### Path / Filesystem

**Line endings.** Git may warn `LF will be replaced by CRLF`. Cosmetic — the
repo's `.gitattributes` normalizes. Don't let editors auto-convert committed
POSIX-newline files to CRLF.

**Forward slashes work almost everywhere.** `C:/Users/...` is accepted by
every Icebath tool and most Windows APIs. Prefer forward slashes in code
and logs — avoids shell-escaping backslashes in bash.

