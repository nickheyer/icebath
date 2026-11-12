package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	sshMaxJSON             = 1024 * 1024
	sshMaxLog              = 512 * 1024
	sshOpenReparsePoint    = 0x00200000
	sshDeleteOnClose       = 0x04000000
	sshMoveReplaceExisting = 0x00000001
	sshMoveWriteThrough    = 0x00000008
	sshGenericRead         = 0x80000000
	sshGenericWrite        = 0x40000000
	sshReadControl         = 0x00020000
	sshDelete              = 0x00010000
	sshCreateNew           = 1
	sshOpenExisting        = 3
	sshAttributeNormal     = 0x00000080
	sshBackupSemantics     = 0x02000000
	sshShareRead           = 0x00000001
	sshShareWrite          = 0x00000002
	sshShareDelete         = 0x00000004
	sshInstallIDFilename   = "install_id"
)

var (
	sshHex32       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sshHex16       = regexp.MustCompile(`^[0-9a-f]{16}$`)
	sshHex64       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sshTokenNameRE = regexp.MustCompile(`^[0-9a-f]{16}\.token$`)
	sshInstallIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Open Windows file handle of the runtime
type sshHandle uintptr

// Win32 calls of the runtime, each failing off Windows with the availability error
type sshWin32 struct {
	available       func() error
	open            func(path string, access, creation, flags, share uint32) (sshHandle, error)
	read            func(h sshHandle, limit int) ([]byte, error)
	write           func(h sshHandle, data []byte) error
	flush           func(h sshHandle) error
	close           func(h sshHandle)
	createDirectory func(path string) error
	moveFileEx      func(src, dst string, flags uint32) error
	winerror        func(err error) (int, bool)
	spawnDetached   func(args, env []string, log sshHandle, logPath string) (int, error)
}

// Host facts of the runtime, swapped by tests
var (
	sshWin            = newSSHWin32()
	sshProcBirth      = proc.Birth
	sshProcCmdline    = proc.Cmdline
	sshProcExists     = proc.Exists
	sshEnviron        = os.Environ
	sshStdin          = func() io.Reader { return os.Stdin }
	errWindowsSSHOnly = errors.New("Windows SSH runtime is only available on Windows")
)

// Text of the bytes below 0x80, the rest dropped like an ASCII decode that ignores errors
func decodeASCIIIgnore(data []byte) string {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		if b < 0x80 {
			out = append(out, b)
		}
	}
	return string(out)
}

// True when an environment name is the key, compared without case on Windows where names carry none
func sshEnvNameIs(name, key string) bool {
	if dashboardGOOS == "windows" {
		return strings.EqualFold(name, key)
	}
	return name == key
}

// The value when the pattern matches it whole, the message as an error otherwise
func sshCheck(pattern *regexp.Regexp, value, message string) (string, error) {
	if !pattern.MatchString(value) {
		return "", errors.New(message)
	}
	return value, nil
}

func sshOwnership(value string) (string, error) {
	return sshCheck(sshHex32, value, "invalid ownership ID")
}

func sshNonce(value string) (string, error) { return sshCheck(sshHex16, value, "invalid spawn nonce") }

// Desktop SSH directory under the machine root, where the helper and the profiled child both look
func sshRoot() string { return filepath.Join(home.Root(), "desktop-ssh") }

func sshDirectory(ownershipID string) (string, error) {
	id, err := sshOwnership(ownershipID)
	if err != nil {
		return "", err
	}
	return filepath.Join(sshRoot(), id), nil
}

// Path of one per spawn artifact in the ownership directory, its name the nonce plus the suffix
func sshNoncePath(ownershipID, spawnNonce, suffix string) (string, error) {
	dir, err := sshDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	nonce, err := sshNonce(spawnNonce)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, nonce+suffix), nil
}

func sshTokenPath(ownershipID, spawnNonce string) (string, error) {
	return sshNoncePath(ownershipID, spawnNonce, ".token")
}

func sshLogPath(ownershipID, spawnNonce string) (string, error) {
	return sshNoncePath(ownershipID, spawnNonce, ".log")
}

func sshLockPath(ownershipID string) (string, error) {
	dir, err := sshDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tools.BackendLockName), nil
}

// Opens an existing file under the reparse guard, not found reported as a zero handle
func sshOpenExistingFile(path string, access, extraFlags, share uint32) (sshHandle, bool, error) {
	h, err := sshWin.open(path, access, sshOpenExisting, sshAttributeNormal|sshOpenReparsePoint|extraFlags, share)
	if err != nil {
		if code, ok := sshWin.winerror(err); ok && (code == 2 || code == 3) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return h, true, nil
}

// Up to limit bytes of a file opened read only with the share mode, not found reported as absent
func sshReadShared(path string, limit int, share uint32) ([]byte, bool, error) {
	h, found, err := sshOpenExistingFile(path, sshGenericRead|sshReadControl, 0, share)
	if err != nil || !found {
		return nil, false, err
	}
	defer sshWin.close(h)
	data, err := sshWin.read(h, limit)
	return data, err == nil, err
}

// Creates path new, writes the data and flushes it
func sshWriteNew(path string, data []byte, share uint32) error {
	h, err := sshWin.open(path, sshGenericWrite|sshReadControl, sshCreateNew, sshAttributeNormal|sshOpenReparsePoint, share)
	if err != nil {
		return err
	}
	defer sshWin.close(h)
	if err := sshWin.write(h, data); err != nil {
		return err
	}
	return sshWin.flush(h)
}

// Creates or replaces path with an owner and SYSTEM only protected DACL
func WritePrivateFile(path string, data []byte) error {
	if err := sshWin.available(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return sshWriteNew(path, data, 0)
}

// Creates the directory and its missing parents protected, then opens it to verify path and security
func sshEnsureDirectory(path string) error {
	if err := sshWin.available(); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	anchor := filepath.VolumeName(path) + string(filepath.Separator)
	if parent != anchor && parent != path {
		if _, err := os.Stat(parent); err != nil {
			if err := sshEnsureDirectory(parent); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(path); err != nil {
		if err := sshWin.createDirectory(path); err != nil {
			if code, ok := sshWin.winerror(err); !ok || code != 183 {
				return err
			}
		}
	}
	h, err := sshWin.open(path, sshGenericRead|sshReadControl, sshOpenExisting, sshBackupSemantics|sshOpenReparsePoint,
		sshShareRead|sshShareWrite|sshShareDelete)
	if err != nil {
		return err
	}
	sshWin.close(h)
	return nil
}

// Ensures the runtime root and the ownership directory and returns the latter
func sshEnsureScope(ownershipID string) (string, error) {
	if err := sshEnsureDirectory(sshRoot()); err != nil {
		return "", err
	}
	dir, err := sshDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	return dir, sshEnsureDirectory(dir)
}

// Writes a 64 hex session token to its private per spawn file and returns the path
func UploadToken(ownershipID, spawnNonce string, token []byte) (*pyjson.Object, error) {
	if len(token) != 64 || !sshHex64.MatchString(decodeASCIIIgnore(token)) {
		return nil, errors.New("invalid session token")
	}
	if _, err := sshEnsureScope(ownershipID); err != nil {
		return nil, err
	}
	path, err := sshTokenPath(ownershipID, spawnNonce)
	if err != nil {
		return nil, err
	}
	if err := sshWriteNew(path, token, 0); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return pyjson.ObjectOf("path", path), nil
}

// Parts of a path with separators, empty and dot segments dropped and the volume kept as the first part
func sshPathParts(p string) []string {
	volume := filepath.VolumeName(p)
	rest := p[len(volume):]
	parts := []string{}
	if volume != "" || strings.HasPrefix(filepath.ToSlash(rest), "/") {
		anchor := volume
		if strings.HasPrefix(filepath.ToSlash(rest), "/") {
			anchor += string(filepath.Separator)
		}
		parts = append(parts, anchor)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rest), "/") {
		if seg != "" && seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

// Parts of path below root under the case rule of the platform paths, not ok when path lies elsewhere
func sshRelativeParts(path, root string) ([]string, bool) {
	pathParts, rootParts := sshPathParts(path), sshPathParts(root)
	if len(pathParts) < len(rootParts) {
		return nil, false
	}
	for i, part := range rootParts {
		if pypath.Normcase(pathParts[i]) != pypath.Normcase(part) {
			return nil, false
		}
	}
	return pathParts[len(rootParts):], true
}

// Reads a Desktop SSH token through a delete on close handle of its protected runtime file
func readWindowsSSHToken(pathValue string) (string, error) {
	if err := sshWin.available(); err != nil {
		return "", err
	}
	relative, ok := sshRelativeParts(pathValue, sshRoot())
	if !ok {
		return "", sshTokenExit("--ssh-session-token-file must be under the desktop-ssh directory")
	}
	if len(relative) != 2 || !sshHex32.MatchString(relative[0]) || !sshTokenNameRE.MatchString(relative[1]) {
		return "", sshTokenExit("--ssh-session-token-file has an invalid runtime path")
	}
	h, err := sshWin.open(pathValue, sshGenericRead|sshReadControl|sshDelete, sshOpenExisting, sshAttributeNormal|sshOpenReparsePoint|sshDeleteOnClose, 0)
	if err != nil {
		return "", sshTokenExit("--ssh-session-token-file is not accessible")
	}
	data, err := sshWin.read(h, 65)
	sshWin.close(h)
	if err != nil {
		return "", err
	}
	token := decodeASCIIIgnore(data)
	if len(token) != 64 || !sshHex64.MatchString(token) {
		return "", sshTokenExit("--ssh-session-token-file contains an invalid token")
	}
	return token, nil
}

// One JSON object from standard input within the payload cap
func sshReadJSONStdin() (*pyjson.Object, error) {
	raw, err := io.ReadAll(io.LimitReader(sshStdin(), sshMaxJSON+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > sshMaxJSON {
		return nil, errors.New("runtime payload is too large")
	}
	parsed, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, errors.New("runtime payload must be an object")
	}
	return obj, nil
}

// Parsed backend lock of an ownership, nil when missing, oversized or not a JSON object
func ReadLock(ownershipID string) (*pyjson.Object, error) {
	if err := sshWin.available(); err != nil {
		return nil, err
	}
	if _, err := sshEnsureScope(ownershipID); err != nil {
		return nil, err
	}
	path, err := sshLockPath(ownershipID)
	if err != nil {
		return nil, err
	}
	data, found, err := sshReadShared(path, sshMaxJSON+1, sshShareRead)
	if err != nil {
		return nil, err
	}
	if !found || len(data) > sshMaxJSON {
		return nil, nil
	}
	text, err := pystr.DecodeUTF8(data)
	if err != nil {
		return nil, nil
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	obj, _ := parsed.(*pyjson.Object)
	return obj, nil
}

// Compact JSON of the lock payload with the log path the owner watchdog trusts
func sshLockRecord(ownershipID string, payload *pyjson.Object) ([]byte, error) {
	logPath, err := sshLogPath(ownershipID, pyrepr.Str(payload.GetOr("spawnNonce", nil)))
	if err != nil {
		return nil, err
	}
	record := payload.Copy()
	record.Set("logPath", filepath.ToSlash(logPath))
	data, err := pyjson.Dumps(record, pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	if len(data) > sshMaxJSON {
		return nil, errors.New("lock payload is too large")
	}
	return []byte(data), nil
}

// Writes the backend lock of an ownership through a private temporary file moved into place
func WriteLock(ownershipID string, payload *pyjson.Object) error {
	if err := sshWin.available(); err != nil {
		return err
	}
	dir, err := sshEnsureScope(ownershipID)
	if err != nil {
		return err
	}
	data, err := sshLockRecord(ownershipID, payload)
	if err != nil {
		return err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := filepath.Join(dir, "."+hex.EncodeToString(random)+".lock.tmp")
	if err := sshWriteNew(temporary, data, 0); err != nil {
		return err
	}
	lockPath, err := sshLockPath(ownershipID)
	if err != nil {
		return err
	}
	return sshWin.moveFileEx(temporary, lockPath, sshMoveReplaceExisting|sshMoveWriteThrough)
}

// Deletes a runtime file through a verified delete on close handle, false when it is missing
func RemoveArtifact(path string) (bool, error) {
	if err := sshWin.available(); err != nil {
		return false, err
	}
	h, found, err := sshOpenExistingFile(path, sshDelete|sshReadControl, sshDeleteOnClose, 0)
	if err != nil || !found {
		return false, err
	}
	sshWin.close(h)
	return true, nil
}

// Liveness and ownership of a backend pid judged by creation time, executable and its serve argv nonce
func ProcessState(pid int, creationTimeNs int64, hermesPath, spawnNonce string) (*pyjson.Object, error) {
	if _, err := sshNonce(spawnNonce); err != nil {
		return nil, err
	}
	denied := func(err error) bool { return errors.Is(err, fs.ErrPermission) }
	birth, err := sshProcBirth(pid)
	var argv []string
	if err == nil {
		argv, err = sshProcCmdline(pid)
	}
	if err != nil {
		if denied(err) {
			return pyjson.ObjectOf("alive", true, "owned", false, "indeterminate", true, "reason", "AccessDenied"), nil
		}
		return pyjson.ObjectOf("alive", false, "owned", false, "indeterminate", false, "reason", "NoSuchProcess"), nil
	}
	actualCreation := int64(birth * 1_000_000_000)
	if actualCreation != creationTimeNs {
		return pyjson.ObjectOf("alive", false, "owned", false, "indeterminate", false, "reason", "creation-time",
			"actualCreationTimeNs", strconv.FormatInt(actualCreation, 10), "expectedCreationTimeNs", strconv.FormatInt(creationTimeNs, 10)), nil
	}
	if len(argv) == 0 {
		return pyjson.ObjectOf("alive", true, "owned", false, "indeterminate", true, "reason", "argv-unavailable"), nil
	}
	normAbs := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		return pypath.Normcase(abs)
	}
	expected, arg0 := normAbs(hermesPath), normAbs(argv[0])
	isInterpreter := strings.HasPrefix(filepath.Base(arg0), "python")
	logicalArgv := argv
	if inline, ok := InlineBootstrapArgv(argv); ok && len(inline) > 0 {
		logicalArgv = inline
	}
	launchesModule := len(logicalArgv) >= 3 && logicalArgv[1] == "-m" && logicalArgv[2] == "icebath_cli.main"
	executableMatch := arg0 == expected || (isInterpreter && launchesModule)
	owned := false
	if serve := slices.Index(logicalArgv, "serve"); serve >= 0 {
		if rel := slices.Index(logicalArgv[serve+1:], "--ssh-owner-nonce"); rel >= 0 {
			owner := serve + 1 + rel
			owned = owner+1 < len(logicalArgv) && executableMatch && slices.Contains(logicalArgv[serve+1:], "--isolated") && logicalArgv[owner+1] == spawnNonce
		}
	}
	reason := "argv"
	if owned {
		reason = "owned"
	}
	shown := argv
	if len(shown) > 20 {
		shown = shown[:20]
	}
	shownAny := make([]any, len(shown))
	for i, a := range shown {
		shownAny[i] = a
	}
	return pyjson.ObjectOf("alive", sshProcExists(pid), "owned", owned, "indeterminate", false,
		"creationTimeNs", strconv.FormatInt(actualCreation, 10), "reason", reason, "argv", shownAny, "expectedExecutable", expected), nil
}

// Polls until pid is gone or the seconds elapse, true when it is gone
func sshWaitGone(pid int, seconds float64) bool {
	deadline := dashboardNow().Add(secondsDuration(seconds))
	for sshProcExists(pid) {
		if !dashboardNow().Before(deadline) {
			return false
		}
		dashboardSleep(50 * time.Millisecond)
	}
	return true
}

// Terminates a backend only while it is alive, owned and still the same incarnation, then force kills a survivor
func TerminateOwned(pid int, creationTimeNs int64, hermesPath, spawnNonce string) (bool, error) {
	state, err := ProcessState(pid, creationTimeNs, hermesPath, spawnNonce)
	if err != nil {
		return false, err
	}
	if state.GetOr("alive", false) != true || state.GetOr("owned", false) != true {
		return false, nil
	}
	birth, err := sshProcBirth(pid)
	if err != nil {
		return false, err
	}
	if int64(birth*1_000_000_000) != creationTimeNs {
		return false, nil
	}
	if err := dashboardKillPid(pid, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, err
	}
	if !sshWaitGone(pid, 5) {
		if err := dashboardKillPid(pid, true); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return false, err
		}
		if !sshWaitGone(pid, 5) {
			return false, fmt.Errorf("timeout after 5 seconds (pid=%d)", pid)
		}
	}
	return true, nil
}

// Direct boot selecting command the configured install reports for itself
func sshResolveDirectCommand(ctx context.Context, hermesPath string) ([]string, error) {
	out, err := dashboardRunProbe(ctx, []string{hermesPath, "--print-runtime-command"}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if out.Returncode != 0 {
		return nil, errors.New("could not resolve Icebath runtime; refresh this installation's launcher")
	}
	parsed, err := pyjson.Loads(probeText(out.Stdout))
	if err != nil {
		return nil, errors.New("Icebath launcher did not report a runtime command")
	}
	invalid := errors.New("Icebath launcher reported an invalid runtime command")
	items, ok := parsed.([]any)
	if !ok || len(items) == 0 {
		return nil, invalid
	}
	command := make([]string, len(items))
	for i, item := range items {
		part, ok := item.(string)
		if !ok || strings.Contains(part, "\x00") {
			return nil, invalid
		}
		command[i] = part
	}
	if st, err := os.Stat(command[0]); !filepath.IsAbs(command[0]) || err != nil || !st.Mode().IsRegular() {
		return nil, invalid
	}
	return command, nil
}

// Environment of the spawned serve, the Desktop dev renderer origin set only from the payload
func sshSpawnEnv(payload *pyjson.Object) ([]string, error) {
	devServer := ""
	if v := payload.GetOr("devServer", nil); pyTruthy(v) {
		devServer = pyrepr.Str(v)
	}
	if len([]rune(devServer)) > 2048 || strings.ContainsAny(devServer, "\x00\r\n") {
		return nil, errors.New("invalid devServer")
	}
	env := []string{}
	for _, entry := range sshEnviron() {
		name, _, _ := strings.Cut(entry, "=")
		if sshEnvNameIs(name, "VIRTUAL_ENV") || sshEnvNameIs(name, "PYTHONPATH") || sshEnvNameIs(name, "ICEBATH_DESKTOP_DEV_SERVER") {
			continue
		}
		env = append(env, entry)
	}
	if devServer != "" {
		env = append(env, "ICEBATH_DESKTOP_DEV_SERVER="+devServer)
	}
	return env, nil
}

// Spawns the isolated serve backend detached with its output in a private log and returns its identity
func SpawnBackend(ctx context.Context, payload *pyjson.Object) (*pyjson.Object, error) {
	field := func(key string) (string, error) {
		v, ok := payload.Get(key)
		if !ok {
			return "", fmt.Errorf("%s", pyrepr.Repr(key))
		}
		return pyrepr.Str(v), nil
	}
	rawOwnership, err := field("ownershipId")
	if err != nil {
		return nil, err
	}
	ownershipID, err := sshOwnership(rawOwnership)
	if err != nil {
		return nil, err
	}
	rawNonce, err := field("spawnNonce")
	if err != nil {
		return nil, err
	}
	spawnNonce, err := sshNonce(rawNonce)
	if err != nil {
		return nil, err
	}
	configuredPath, err := field("hermesPath")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configuredPath) {
		return nil, errors.New("Icebath path must be absolute")
	}
	hermesPath, err := filepath.Abs(configuredPath)
	if err != nil {
		return nil, err
	}
	tokenPath, err := sshTokenPath(ownershipID, spawnNonce)
	if err != nil {
		return nil, err
	}
	profile := ""
	if v := payload.GetOr("profile", nil); pyTruthy(v) {
		profile = pyrepr.Str(v)
	}
	if len([]rune(profile)) > 256 || strings.ContainsAny(profile, "\x00\r\n") {
		return nil, errors.New("invalid profile")
	}
	args, err := sshResolveDirectCommand(ctx, hermesPath)
	if err != nil {
		return nil, err
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	args = append(args, "serve", "--isolated", "--host", "127.0.0.1", "--port", "0", "--ssh-session-token-file", tokenPath, "--ssh-owner-nonce", spawnNonce)
	env, err := sshSpawnEnv(payload)
	if err != nil {
		return nil, err
	}
	if _, err := sshEnsureScope(ownershipID); err != nil {
		return nil, err
	}
	logPath, err := sshLogPath(ownershipID, spawnNonce)
	if err != nil {
		return nil, err
	}
	logHandle, err := sshWin.open(logPath, sshGenericWrite|sshReadControl, sshCreateNew, sshAttributeNormal|sshOpenReparsePoint, sshShareRead|sshShareWrite)
	if err != nil {
		return nil, err
	}
	pid, err := sshWin.spawnDetached(args, env, logHandle, logPath)
	if err != nil {
		return nil, err
	}
	birth, err := sshProcBirth(pid)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("pid", pid, "creationTimeNs", strconv.FormatInt(int64(birth*1_000_000_000), 10), "logPath", logPath, "tokenPath", tokenPath), nil
}

// Version line of an install and whether its serve command takes the Desktop SSH flags
func InspectHermes(ctx context.Context, hermesPath string) (*pyjson.Object, error) {
	path, err := filepath.Abs(hermesPath)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); !filepath.IsAbs(hermesPath) || err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("Icebath path is not an executable file")
	}
	version, err := dashboardRunProbe(ctx, []string{path, "--version"}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	help, err := dashboardRunProbe(ctx, []string{path, "serve", "--help"}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	helpText := probeText(help.Stdout) + probeText(help.Stderr)
	versionLine := ""
	if version.Returncode == 0 {
		lines := pystr.Splitlines(probeText(version.Stdout)+probeText(version.Stderr), false)
		if len(lines) == 0 {
			return nil, errors.New("list index out of range")
		}
		versionLine = lines[0]
	}
	return pyjson.ObjectOf("path", path, "version", versionLine, "runtimeBootstrapOwnership", 1,
		"supported", strings.Contains(helpText, "--ssh-session-token-file") && strings.Contains(helpText, "--ssh-owner-nonce")), nil
}

func sshProbe() (*pyjson.Object, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("os", "Windows", "arch", pyplatform.Machine(), "hermesHome", home.Root(), "executable", exe), nil
}

// Valid install id of an id file, nil when the file is missing, unreadable or malformed
func sshReadExistingInstallID(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil
	}
	existing := strings.ToLower(pystr.Strip(text))
	if !sshInstallIDRE.MatchString(existing) {
		return nil
	}
	return existing
}

// Read only roster of the install, the default profile, the live named ones and the install id
func sshInventory() (*pyjson.Object, error) {
	dirs, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	names := []any{"default"}
	for _, dir := range dirs {
		names = append(names, filepath.Base(dir))
	}
	return pyjson.ObjectOf("profiles", names, "installId", sshReadExistingInstallID(filepath.Join(home.Root(), sshInstallIDFilename))), nil
}

func sshReadLog(ownershipID, spawnNonce string) (*pyjson.Object, error) {
	if err := sshWin.available(); err != nil {
		return nil, err
	}
	path, err := sshLogPath(ownershipID, spawnNonce)
	if err != nil {
		return nil, err
	}
	data, found, err := sshReadShared(path, sshMaxLog, sshShareRead|sshShareWrite)
	if err != nil {
		return nil, err
	}
	content := ""
	if found {
		content = pystr.DecodeUTF8Replace(data)
	}
	return pyjson.ObjectOf("content", content), nil
}

// Removes one runtime artifact and reports whether it existed
func sshRemoved(path string, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	removed, err := RemoveArtifact(path)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("removed", removed), nil
}

// Writes the lock payload read from standard input as the backend lock of an ownership
func writeLockOp(ownershipID string) (any, error) {
	payload, err := sshReadJSONStdin()
	if err != nil {
		return nil, err
	}
	if err := WriteLock(ownershipID, payload); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true), nil
}

type sshOperation struct {
	argc int
	run  func(ctx context.Context, args []string) (any, error)
}

// Python int of a decimal argument
func sshIntArg(text string) (int64, error) {
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(text))
	}
	return n.Int64(), nil
}

// Operation name to its argument count, minus one for any, and handler
var sshOperations = map[string]sshOperation{
	"probe":     {-1, func(context.Context, []string) (any, error) { return sshProbe() }},
	"inventory": {-1, func(context.Context, []string) (any, error) { return sshInventory() }},
	"upload-token": {2, func(_ context.Context, a []string) (any, error) {
		token, err := io.ReadAll(io.LimitReader(sshStdin(), 65))
		if err != nil {
			return nil, err
		}
		return UploadToken(a[0], a[1], token)
	}},
	"read-lock": {1, func(_ context.Context, a []string) (any, error) {
		lock, err := ReadLock(a[0])
		if err != nil || lock == nil {
			return nil, err
		}
		return lock, nil
	}},
	"write-lock":   {1, func(_ context.Context, a []string) (any, error) { return writeLockOp(a[0]) }},
	"remove-lock":  {1, func(_ context.Context, a []string) (any, error) { return sshRemoved(sshLockPath(a[0])) }},
	"remove-token": {2, func(_ context.Context, a []string) (any, error) { return sshRemoved(sshTokenPath(a[0], a[1])) }},
	"read-log":     {2, func(_ context.Context, a []string) (any, error) { return sshReadLog(a[0], a[1]) }},
	"remove-log":   {2, func(_ context.Context, a []string) (any, error) { return sshRemoved(sshLogPath(a[0], a[1])) }},
	"spawn": {-1, func(ctx context.Context, _ []string) (any, error) {
		payload, err := sshReadJSONStdin()
		if err != nil {
			return nil, err
		}
		return SpawnBackend(ctx, payload)
	}},
	"inspect": {1, func(ctx context.Context, a []string) (any, error) { return InspectHermes(ctx, a[0]) }},
	"process-state": {4, func(_ context.Context, a []string) (any, error) {
		pid, err := sshIntArg(a[0])
		if err != nil {
			return nil, err
		}
		created, err := sshIntArg(a[1])
		if err != nil {
			return nil, err
		}
		return ProcessState(int(pid), created, a[2], a[3])
	}},
	"terminate": {4, func(_ context.Context, a []string) (any, error) {
		pid, err := sshIntArg(a[0])
		if err != nil {
			return nil, err
		}
		created, err := sshIntArg(a[1])
		if err != nil {
			return nil, err
		}
		terminated, err := TerminateOwned(int(pid), created, a[2], a[3])
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("terminated", terminated), nil
	}},
}

// Runs the operation named by the first argument with the rest as its arguments
func DispatchWindowsSSHRuntime(ctx context.Context, argv []string) (any, error) {
	if len(argv) == 0 {
		return nil, errors.New("missing operation")
	}
	entry, ok := sshOperations[argv[0]]
	if !ok || (entry.argc >= 0 && len(argv)-1 != entry.argc) {
		return nil, errors.New("invalid operation")
	}
	return entry.run(ctx, argv[1:])
}

// Argv word running the hermes_cli.windows_ssh_runtime module entry the Desktop SSH remote launches
const WindowsSSHRuntimeEntry = "__icebath_windows_ssh_runtime__"

// Runs one Windows SSH runtime operation and exits with its status when argv carries the entry word
func DispatchWindowsSSHRuntimeEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != WindowsSSHRuntimeEntry {
		return
	}
	exit(WindowsSSHRuntimeMain(ctx, os.Args[2:], os.Stdout, os.Stderr))
}

// Prints the compact JSON result of one operation, or its error on stderr with exit status one
func WindowsSSHRuntimeMain(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	result, err := DispatchWindowsSSHRuntime(ctx, argv)
	if err == nil {
		var text string
		if text, err = pyjson.Dumps(result, pyjson.Separators(",", ":")); err == nil {
			fmt.Fprintln(stdout, text)
			return 0
		}
	}
	text, _ := pyjson.Dumps(pyjson.ObjectOf("error", err.Error()), pyjson.Separators(",", ":"))
	fmt.Fprintln(stderr, text)
	return 1
}
