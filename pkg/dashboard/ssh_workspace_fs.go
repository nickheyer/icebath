package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Filesystem error the SSH execution target reported, its code mapped to an HTTP answer
type SshWorkspaceFsError struct {
	Code  string
	Msg   string
	Cause error
}

func (e *SshWorkspaceFsError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Code
}

func (e *SshWorkspaceFsError) Unwrap() error { return e.Cause }

// Error of a code, its text the message or the code when the message is empty
func newSshWorkspaceFsError(code, message string) *SshWorkspaceFsError {
	return &SshWorkspaceFsError{Code: code, Msg: message}
}

// Connection settings an SSH workspace adapter is built from
type sshFsConfig struct {
	host, user string
	port       int
	keyPath    string
	cwd        string
	timeout    int
}

// SSH environment calls the workspace adapter makes
type sshFsEnv interface {
	Execute(ctx context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error)
	Cwd() string
	Timeout() int
	RemoteHome() (string, bool)
	ExecPrefix(user string, interactive bool) ([]string, error)
	Cleanup(forceRemove bool) error
}

// Lock table keyed by profile or endpoint, one lock per key for the life of the process
type sshFsLocks interface {
	lockFor(key any) sync.Locker
}

type sshFsLockMap struct{ m map[any]*sync.Mutex }

func (l *sshFsLockMap) lockFor(key any) sync.Locker {
	sshFsCacheMu.Lock()
	defer sshFsCacheMu.Unlock()
	if lock, ok := l.m[key]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	l.m[key] = lock
	return lock
}

type sshFsCached struct {
	config  sshFsConfig
	backend *SshWorkspaceFs
}

// Running remote streaming process, its stdout and its lifecycle
type sshFsProcess interface {
	Stdout() io.Reader
	Running() bool
	Terminate()
	Wait() error
}

type sshFsExecProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	done   chan struct{}
	err    error
}

func (p *sshFsExecProcess) Stdout() io.Reader { return p.stdout }

func (p *sshFsExecProcess) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *sshFsExecProcess) Terminate() { _ = proc.Terminate(p.cmd.Process.Pid) }

func (p *sshFsExecProcess) Wait() error {
	<-p.done
	return p.err
}

var (
	sshFsCacheMu       sync.Mutex
	sshFsBackends                 = map[string]sshFsCached{}
	sshFsDialLocks     sshFsLocks = &sshFsLockMap{m: map[any]*sync.Mutex{}}
	sshFsEndpointLocks sshFsLocks = &sshFsLockMap{m: map[any]*sync.Mutex{}}
	newSSHWorkspaceEnv            = dialSSHWorkspaceEnv
	sshFsPopen                    = startSSHFsProcess
)

// Starts argv with stdin and stderr discarded and stdout piped, hidden on Windows
func startSSHFsProcess(argv []string) (sshFsProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = tools.WindowsHideAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &sshFsExecProcess{cmd: cmd, stdout: stdout, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Connects the SSH environment of a workspace adapter under the context profile
func dialSSHWorkspaceEnv(ctx context.Context, opts tools.SSHOptions) (sshFsEnv, error) {
	env, err := tools.NewSSHEnvironment(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, opts)
	if err != nil {
		return nil, err
	}
	return env, nil
}

// User, host and port the SSH control socket is derived from
func sshFsEndpoint(cfg sshFsConfig) [3]any { return [3]any{cfg.user, cfg.host, cfg.port} }

// Binary safe workspace operations over the configured SSH transport
type SshWorkspaceFs struct {
	env sshFsEnv
	mu  sync.Mutex
}

// Adapter over an SSH environment
func newSshWorkspaceFs(env sshFsEnv) *SshWorkspaceFs { return &SshWorkspaceFs{env: env} }

// Normalized working directory of the remote workspace
func (f *SshWorkspaceFs) Cwd() (string, error) { return f.normalize(f.env.Cwd()) }

// Absolute normalized remote path, home and cwd relative forms expanded, EINVAL for an empty or invalid one
func (f *SshWorkspaceFs) normalize(path string) (string, error) {
	raw := pystr.Strip(path)
	if raw == "" {
		return "", newSshWorkspaceFsError("EINVAL", "Path is required")
	}
	if strings.Contains(raw, "\x00") || strings.HasPrefix(strings.ToLower(raw), "file:") {
		return "", newSshWorkspaceFsError("EINVAL", "Invalid path")
	}
	remoteHome, _ := f.env.RemoteHome()
	switch {
	case raw == "~":
		raw = remoteHome
	case strings.HasPrefix(raw, "~/"):
		raw = pypath.PosixJoin(remoteHome, raw[2:])
	case !strings.HasPrefix(raw, "/"):
		raw = pypath.PosixJoin(f.env.Cwd(), raw)
	}
	return pypath.PosixNormpath(raw), nil
}

// Runs a remote command in the workspace cwd under the adapter lock
func (f *SshWorkspaceFs) execute(command string, stdinData *string, timeout int) (*tools.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if timeout == 0 {
		timeout = f.env.Timeout()
	}
	return f.env.Execute(context.Background(), command, tools.ExecOptions{Cwd: f.env.Cwd(), Stdin: stdinData, Timeout: timeout})
}

// Error code of the first marker line in remote output, fallback when none
func sshFsErrorCode(output, fallback string) string {
	const marker = "__ICEBATH_FS_ERROR__:"
	for _, line := range pystr.Splitlines(output, false) {
		if strings.HasPrefix(line, marker) {
			if code := pystr.Strip(line[len(marker):]); code != "" {
				return code
			}
			return fallback
		}
	}
	return fallback
}

// Directory listing of a remote path, hidden names dropped, directories first
func (f *SshWorkspaceFs) ListDir(path string, hiddenNames map[string]bool) (map[string]any, error) {
	target, err := f.normalize(path)
	if err != nil {
		return nil, err
	}
	command := "\np=" + pyshlex.Quote(target) + `
if [ ! -e "$p" ] && [ ! -L "$p" ]; then printf '__ICEBATH_FS_ERROR__:ENOENT\n'; exit 44; fi
if [ ! -d "$p" ]; then printf '__ICEBATH_FS_ERROR__:ENOTDIR\n'; exit 45; fi
if [ ! -r "$p" ]; then printf '__ICEBATH_FS_ERROR__:EACCES\n'; exit 46; fi
for child in "$p"/* "$p"/.[!.]* "$p"/..?*; do
  [ -e "$child" ] || [ -L "$child" ] || continue
  name=${child##*/}
  name64=$(printf '%s' "$name" | base64 | tr -d '\r\n')
  path64=$(printf '%s' "$child" | base64 | tr -d '\r\n')
  if [ -d "$child" ]; then kind=d; else kind=f; fi
  printf '%s\t%s\t%s\n' "$name64" "$path64" "$kind"
done
`
	result, err := f.execute(command, nil, 0)
	if err != nil {
		return nil, err
	}
	if result.ReturnCode != 0 {
		return map[string]any{"entries": []*pyjson.Object{}, "error": sshFsErrorCode(result.Output, "read-error")}, nil
	}
	invalid := func(cause error) error {
		return &SshWorkspaceFsError{Code: "EIO", Msg: "SSH filesystem returned an invalid directory listing", Cause: cause}
	}
	entries := []*pyjson.Object{}
	for _, line := range pystr.Splitlines(result.Output, false) {
		if pystr.Strip(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil, invalid(fmt.Errorf("not enough values to unpack (expected 3, got %d)", len(fields)))
		}
		nameBytes, err := pybase64.B64DecodeValidate(fields[0])
		if err != nil {
			return nil, invalid(err)
		}
		pathBytes, err := pybase64.B64DecodeValidate(fields[1])
		if err != nil {
			return nil, invalid(err)
		}
		name, _ := pycodecs.DecodeReplace(nameBytes, "utf-8")
		childPath, _ := pycodecs.DecodeReplace(pathBytes, "utf-8")
		if hiddenNames[name] {
			continue
		}
		entries = append(entries, pyjson.ObjectOf("name", name, "path", childPath, "isDirectory", fields[2] == "d"))
	}
	sortFsEntries(entries)
	return map[string]any{"entries": entries}, nil
}

// Sorts listing entries directories first, then by lowercased name, then by name
func sortFsEntries(entries []*pyjson.Object) {
	sort.SliceStable(entries, func(i, j int) bool {
		di, _ := entries[i].GetOr("isDirectory", false).(bool)
		dj, _ := entries[j].GetOr("isDirectory", false).(bool)
		if di != dj {
			return di
		}
		ni, _ := entries[i].GetOr("name", "").(string)
		nj, _ := entries[j].GetOr("name", "").(string)
		if li, lj := pystr.Lower(ni), pystr.Lower(nj); li != lj {
			return li < lj
		}
		return ni < nj
	})
}

// Bytes of a remote regular file up to readLimit, its full size and its normalized path
func (f *SshWorkspaceFs) ReadBytes(path string, maxBytes int64, readLimit *int64) ([]byte, int64, string, error) {
	target, err := f.normalize(path)
	if err != nil {
		return nil, 0, "", err
	}
	readCommand := `base64 < "$p"`
	if readLimit != nil {
		readCommand = fmt.Sprintf(`set -o pipefail; head -c %d < "$p" | base64`, *readLimit)
	}
	command := "\np=" + pyshlex.Quote(target) + `
if [ ! -e "$p" ] && [ ! -L "$p" ]; then printf '__ICEBATH_FS_ERROR__:ENOENT\n'; exit 44; fi
if [ ! -f "$p" ]; then printf '__ICEBATH_FS_ERROR__:ENOTREG\n'; exit 45; fi
size=$(stat -c %s "$p" 2>/dev/null || stat -f %z "$p" 2>/dev/null) || { printf '__ICEBATH_FS_ERROR__:EACCES\n'; exit 46; }
if [ "$size" -gt ` + strconv.FormatInt(maxBytes, 10) + ` ]; then printf '__ICEBATH_FS_ERROR__:EFBIG\n'; exit 47; fi
printf '__ICEBATH_FS_SIZE__:%s\n' "$size"
{ ` + readCommand + `; } || { printf '__ICEBATH_FS_ERROR__:EACCES\n'; exit 46; }
`
	result, err := f.execute(command, nil, 0)
	if err != nil {
		return nil, 0, "", err
	}
	if result.ReturnCode != 0 {
		return nil, 0, "", newSshWorkspaceFsError(sshFsErrorCode(result.Output, "EIO"), "")
	}
	lines := pystr.Splitlines(result.Output, false)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "__ICEBATH_FS_SIZE__:") {
		return nil, 0, "", newSshWorkspaceFsError("EIO", "SSH filesystem did not return a file size")
	}
	size, err := sshFsInt(strings.SplitN(lines[0], ":", 2)[1])
	if err != nil {
		return nil, 0, "", &SshWorkspaceFsError{Code: "EIO", Msg: "SSH filesystem returned an invalid file size", Cause: err}
	}
	data, err := pybase64.B64DecodeValidate(strings.Join(lines[1:], ""))
	if err != nil {
		return nil, 0, "", &SshWorkspaceFsError{Code: "EIO", Msg: "SSH filesystem returned invalid file data", Cause: err}
	}
	return data, size, target, nil
}

// Integer the way Python int parses a decimal string, surrounding whitespace allowed
func sshFsInt(text string) (int64, error) {
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))
	}
	return n.Int64(), nil
}

// Resolved remote path and size of a regular file without reading it
func (f *SshWorkspaceFs) InspectFile(path string) (string, int64, error) {
	target, err := f.normalize(path)
	if err != nil {
		return "", 0, err
	}
	command := "\np=" + pyshlex.Quote(target) + `
target=$(realpath "$p" 2>/dev/null || readlink -f "$p" 2>/dev/null) || { printf '__ICEBATH_FS_ERROR__:ENOENT\n'; exit 44; }
if [ ! -f "$target" ]; then printf '__ICEBATH_FS_ERROR__:ENOTREG\n'; exit 45; fi
size=$(stat -c %s "$target" 2>/dev/null || stat -f %z "$target" 2>/dev/null) || { printf '__ICEBATH_FS_ERROR__:EACCES\n'; exit 46; }
printf '__ICEBATH_FS_TARGET__:%s\n' "$(printf '%s' "$target" | base64 | tr -d '\r\n')"
printf '__ICEBATH_FS_SIZE__:%s\n' "$size"
`
	result, err := f.execute(command, nil, 0)
	if err != nil {
		return "", 0, err
	}
	if result.ReturnCode != 0 {
		return "", 0, newSshWorkspaceFsError(sshFsErrorCode(result.Output, "EIO"), "")
	}
	targetLine, sizeLine := "", ""
	for _, line := range pystr.Splitlines(result.Output, false) {
		if targetLine == "" && strings.HasPrefix(line, "__ICEBATH_FS_TARGET__:") {
			targetLine = line
		}
		if sizeLine == "" && strings.HasPrefix(line, "__ICEBATH_FS_SIZE__:") {
			sizeLine = line
		}
	}
	if targetLine == "" || sizeLine == "" {
		return "", 0, newSshWorkspaceFsError("EIO", "SSH filesystem did not return file metadata")
	}
	invalid := func(cause error) error {
		return &SshWorkspaceFsError{Code: "EIO", Msg: "SSH filesystem returned invalid file metadata", Cause: cause}
	}
	raw, err := pybase64.B64DecodeValidate(strings.SplitN(targetLine, ":", 2)[1])
	if err != nil {
		return "", 0, invalid(err)
	}
	resolved, err := pycodecs.Decode(raw, "utf-8")
	if err != nil {
		return "", 0, invalid(err)
	}
	size, err := sshFsInt(strings.SplitN(sizeLine, ":", 2)[1])
	if err != nil {
		return "", 0, invalid(err)
	}
	return resolved, size, nil
}

// Streams an authorized remote file, or one byte range of it, without buffering it
func (f *SshWorkspaceFs) StreamFile(path string, chunkSize int, offset int64, length *int64) func(ctx context.Context, yield func([]byte) error) error {
	return func(ctx context.Context, yield func([]byte) error) error {
		target, err := f.normalize(path)
		if err != nil {
			return err
		}
		source := "< " + pyshlex.Quote(target)
		script := "exec cat " + source
		if offset != 0 || length != nil {
			script = fmt.Sprintf("tail -c +%d %s", offset+1, source)
			if length != nil {
				script += fmt.Sprintf(" | head -c %d", *length)
			}
		}
		command, err := f.env.ExecPrefix("", false)
		if err != nil {
			return err
		}
		command = append(append([]string{}, command...), "bash", "-c", pyshlex.Quote(script))
		process, err := sshFsPopen(command)
		if err != nil {
			return err
		}
		defer func() {
			if process.Running() {
				process.Terminate()
			}
			_ = process.Wait()
		}()
		buf := make([]byte, chunkSize)
		for {
			n, rerr := io.ReadFull(process.Stdout(), buf)
			if n > 0 {
				if err := yield(append([]byte(nil), buf[:n]...)); err != nil {
					return err
				}
			}
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	}
}

// Replaces a remote file with content through a sibling temp file, EFBIG past maxBytes
func (f *SshWorkspaceFs) WriteText(path, content string, maxBytes int64) (string, int64, error) {
	target, err := f.normalize(path)
	if err != nil {
		return "", 0, err
	}
	encodedSize := int64(len(content))
	if encodedSize > maxBytes {
		return "", 0, newSshWorkspaceFsError("EFBIG", "")
	}
	parent := pypath.PosixDirname(target)
	if parent == "" {
		parent = "/"
	}
	suffix := make([]byte, 16)
	_, _ = rand.Read(suffix)
	tempPath := pypath.PosixJoin(parent, "."+pypath.PosixBasename(target)+".hermes-tmp-"+hex.EncodeToString(suffix)[:12])
	command := "\ntarget=" + pyshlex.Quote(target) + "\nparent=" + pyshlex.Quote(parent) + "\ntmp=" + pyshlex.Quote(tempPath) + `
if [ ! -d "$parent" ]; then printf '__ICEBATH_FS_ERROR__:ENOENT\n'; exit 44; fi
if [ -e "$target" ] && [ ! -f "$target" ]; then printf '__ICEBATH_FS_ERROR__:ENOTREG\n'; exit 45; fi
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat > "$tmp" && mv -f "$tmp" "$target"
`
	result, err := f.execute(command, &content, 0)
	if err != nil {
		return "", 0, err
	}
	if result.ReturnCode != 0 || result.StdinError != "" {
		return "", 0, newSshWorkspaceFsError(sshFsErrorCode(result.Output, "EIO"), "")
	}
	return target, encodedSize, nil
}

// Nearest remote ancestor holding a git entry, false when none
func (f *SshWorkspaceFs) GitRoot(path string) (string, bool, error) {
	target, err := f.normalize(path)
	if err != nil {
		return "", false, err
	}
	command := "\np=" + pyshlex.Quote(target) + `
[ -d "$p" ] || p=${p%/*}
while [ -n "$p" ]; do
  if [ -e "$p/.git" ]; then printf '%s\n' "$p"; exit 0; fi
  [ "$p" = / ] && break
  p=${p%/*}; [ -n "$p" ] || p=/
done
exit 1
`
	result, err := f.execute(command, nil, 0)
	if err != nil {
		return "", false, err
	}
	if result.ReturnCode != 0 {
		return "", false, nil
	}
	root := pystr.Strip(result.Output)
	return root, root != "", nil
}

// Current branch of the remote checkout at path, empty when git fails
func (f *SshWorkspaceFs) GitBranch(path string) (string, error) {
	target, err := f.normalize(path)
	if err != nil {
		return "", err
	}
	result, err := f.execute("git -C "+pyshlex.Quote(target)+" branch --show-current 2>/dev/null", nil, 0)
	if err != nil {
		return "", err
	}
	if result.ReturnCode != 0 {
		return "", nil
	}
	return pystr.Strip(result.Output), nil
}

// Text of a terminal config value the way Python str of value or default reads it
func sshFsConfigText(terminal map[string]any, key, def string) string {
	v := terminal[key]
	if !pyrepr.Truthy(v) {
		return def
	}
	return pyrepr.Str(v)
}

// Integer of a terminal config value the way Python int of value or default reads it
func sshFsConfigInt(terminal map[string]any, key string, def int) (int, error) {
	v := terminal[key]
	if !pyrepr.Truthy(v) {
		return def, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("Python int too large to convert to C int")
	}
	return int(n.Int64()), nil
}

// Cached workspace adapter of a profile that selects the SSH backend, nil for any other backend
func getSshWorkspaceFs(ctx context.Context, profileKey string, terminal map[string]any) (*SshWorkspaceFs, error) {
	if pystr.Lower(pystr.Strip(sshFsConfigText(terminal, "backend", "local"))) != "ssh" {
		return nil, nil
	}
	port, err := sshFsConfigInt(terminal, "ssh_port", 22)
	if err != nil {
		return nil, err
	}
	timeout, err := sshFsConfigInt(terminal, "timeout", 180)
	if err != nil {
		return nil, err
	}
	cfg := sshFsConfig{
		host:    pystr.Strip(sshFsConfigText(terminal, "ssh_host", "")),
		user:    pystr.Strip(sshFsConfigText(terminal, "ssh_user", "")),
		port:    port,
		keyPath: pystr.Strip(sshFsConfigText(terminal, "ssh_key", "")),
		cwd:     pystr.Strip(sshFsConfigText(terminal, "cwd", "~")),
		timeout: timeout,
	}
	if cfg.cwd == "" {
		cfg.cwd = "~"
	}
	if cfg.host == "" || cfg.user == "" {
		return nil, newSshWorkspaceFsError("ECONN", "SSH host and user are required")
	}
	dial := sshFsDialLocks.lockFor(profileKey)
	dial.Lock()
	defer dial.Unlock()
	sshFsCacheMu.Lock()
	cached, ok := sshFsBackends[profileKey]
	if ok && cached.config == cfg {
		sshFsCacheMu.Unlock()
		return cached.backend, nil
	}
	delete(sshFsBackends, profileKey)
	sshFsCacheMu.Unlock()
	if ok {
		if err := func() error {
			endpoint := sshFsEndpointLocks.lockFor(sshFsEndpoint(cached.config))
			endpoint.Lock()
			defer endpoint.Unlock()
			return cached.backend.env.Cleanup(false)
		}(); err != nil {
			return nil, err
		}
	}
	env, err := func() (sshFsEnv, error) {
		endpoint := sshFsEndpointLocks.lockFor(sshFsEndpoint(cfg))
		endpoint.Lock()
		defer endpoint.Unlock()
		return newSSHWorkspaceEnv(ctx, tools.SSHOptions{
			Host: cfg.host, User: cfg.user, Cwd: cfg.cwd, Timeout: cfg.timeout, Port: cfg.port, KeyPath: cfg.keyPath,
			SyncFiles: false, Persistent: true,
		})
	}()
	if err != nil {
		return nil, &SshWorkspaceFsError{Code: "ECONN", Msg: "Could not connect to the SSH workspace", Cause: err}
	}
	backend := newSshWorkspaceFs(env)
	sshFsCacheMu.Lock()
	sshFsBackends[profileKey] = sshFsCached{config: cfg, backend: backend}
	sshFsCacheMu.Unlock()
	return backend, nil
}
