package connections

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

const (
	// Lockfile schema this build writes and accepts
	LockfileSchemaVersion = 2
	// Desktop to dashboard reuse contract version, a mismatch forces a respawn
	RemoteProtocolVersion = 1
	// Remote directory of the desktop ownership records
	RemoteLockDir             = "~/.icebath/desktop-ssh"
	DefaultRemoteReadyTimeout = 120 * time.Second
	MinRemoteReadyTimeout     = 45 * time.Second
	remoteReadyPoll           = 750 * time.Millisecond
	remoteNofileSoftLimit     = 65536
	probeVerdictAttempts      = 3
	probeVerdictRetry         = 500 * time.Millisecond
)

// RemoteShell is the SSH channel a remote lifecycle runs its commands through
type RemoteShell interface {
	Exec(command string, o SSHExecOptions) (string, error)
}

// RemotePlatform is the uname facts of a remote, the Windows runtime when the remote is Windows
type RemotePlatform struct {
	OS      string
	Arch    string
	Windows *WindowsRuntime
}

func lifecycleError(kind, message string, cause error) *SSHError {
	return &SSHError{Message: message, ErrKind: kind, Cause: cause}
}

// Authenticated ownership proof of the reuse probe, ok only for this spawn and protocol with an intact runtime
func ClassifySSHReuseProof(proof any, spawnNonce string) string {
	p := jsObject(proof)
	if p != nil && p["ok"] == true && p["sshOwnerNonce"] == spawnNonce && p["protocolVersion"] == float64(RemoteProtocolVersion) && p["runtimeIntact"] != false {
		return "authenticated-ok"
	}
	return "authenticated-stale"
}

// Ready deadline from ICEBATH_DESKTOP_REMOTE_READY_TIMEOUT_MS, floored at the warm start budget
func ResolveReadyTimeout(getenv func(string) string) time.Duration {
	parsed := ecmaToNumber(getenv("ICEBATH_DESKTOP_REMOTE_READY_TIMEOUT_MS"))
	if !math.IsNaN(parsed) && !math.IsInf(parsed, 0) && parsed > 0 {
		ms := math.Max(float64(MinRemoteReadyTimeout.Milliseconds()), jsRound(parsed))
		return time.Duration(ms) * time.Millisecond
	}
	return DefaultRemoteReadyTimeout
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Fresh 32 byte spawn credential in hex
func MintToken() string { return randomHex(32) }

// Truncated SHA256 of a token, the only form a lockfile stores
func FingerprintToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:32]
}

var (
	ownershipIDRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spawnNonceRE   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	creationTimeRE = regexp.MustCompile(`^(?:linux:[0-9]+|darwin:[A-Za-z0-9 :+-]+)$`)
	remotePathBad  = regexp.MustCompile("[\x00\n\r]")
	safeRemoteHome = regexp.MustCompile(`^(/|~/)[A-Za-z0-9._/+-]+$`)
	profileHomeRE  = regexp.MustCompile(`^(.*)/profiles/[^/]+$`)
	liveVerdictRE  = regexp.MustCompile(`^LIVE:([1-9][0-9]*)$`)
	crlfSplit      = regexp.MustCompile(`\r?\n`)
	profileIDArgRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	missingProfile = regexp.MustCompile(`(?i)Error: Profile ['"]([^'"]+)['"] does not exist\.?(?: Create it with: ([^\n]+))?`)
	bindCollision  = regexp.MustCompile(`(?i)address already in use|cannot listen to port|bind.*failed`)
	readyMergedRE  = regexp.MustCompile(`ICEBATH_(?:BACKEND|DASHBOARD)_READY port=(\d+)`)
)

func validateOwnershipID(id string) (string, error) {
	if !ownershipIDRE.MatchString(id) {
		return "", errors.New("SSH ownership ID is invalid.")
	}
	return id, nil
}

func validateSpawnNonce(nonce string) (string, error) {
	if !spawnNonceRE.MatchString(nonce) {
		return "", errors.New("SSH spawn nonce is invalid.")
	}
	return nonce, nil
}

// Remote directory of one ownership id
func OwnershipDirectory(ownershipID string) (string, error) {
	id, err := validateOwnershipID(ownershipID)
	if err != nil {
		return "", err
	}
	return RemoteLockDir + "/" + id, nil
}

// Remote lockfile of one ownership id
func LockfilePath(ownershipID string) (string, error) {
	dir, err := OwnershipDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	return dir + "/backend.lock.json", nil
}

// Remote connect reservation of one ownership id
func ConnectReservationPath(ownershipID string) (string, error) {
	dir, err := OwnershipDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	return dir + "/.connect.lock", nil
}

func spawnFile(ownershipID, spawnNonce, ext string) (string, error) {
	dir, err := OwnershipDirectory(ownershipID)
	if err != nil {
		return "", err
	}
	nonce, err := validateSpawnNonce(spawnNonce)
	if err != nil {
		return "", err
	}
	return dir + "/" + nonce + ext, nil
}

// Remote log of one spawn
func SpawnLogPath(ownershipID, spawnNonce string) (string, error) {
	return spawnFile(ownershipID, spawnNonce, ".log")
}

// Remote session token file of one spawn
func SpawnTokenPath(ownershipID, spawnNonce string) (string, error) {
	return spawnFile(ownershipID, spawnNonce, ".token")
}

// Single quotes a value for a remote shell
func Shq(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// Refuses an empty, relative or newline carrying remote path
func ValidateRemotePath(p string) error {
	if p == "" {
		return errors.New("Remote path must not be empty.")
	}
	if remotePathBad.MatchString(p) {
		return errors.New("Unsafe remote path: contains NUL or newline.")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/") {
		return nil
	}
	return fmt.Errorf("Remote path must be absolute or start with ~/: \"%s\"", p)
}

// Shell word of a remote path with a leading tilde expanded through HOME
func ExpandRemotePath(p string) (string, error) {
	if err := ValidateRemotePath(p); err != nil {
		return "", err
	}
	if p == "~" {
		return `"$HOME"`, nil
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME"` + Shq(p[1:]), nil
	}
	return Shq(p), nil
}

func mustExpand(p string) string {
	out, _ := ExpandRemotePath(p)
	return out
}

// Profile id a remote spawn may pass, nil for a value that is not a slug or default
func BackendProfileArg(profile any) *string {
	s, ok := profile.(string)
	if !ok {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(s))
	if name == "" {
		return nil
	}
	if name == "default" || profileIDArgRE.MatchString(name) {
		return &name
	}
	return nil
}

// Remote icebath executable, an explicit path honored strictly, else the login shell and installer locations
func LocateHermes(ssh RemoteShell, remoteHermesPath string) (string, error) {
	isExecutable := func(candidate string) bool {
		expanded, err := ExpandRemotePath(candidate)
		if err != nil {
			return false
		}
		out, err := ssh.Exec("[ -x "+expanded+" ] && echo OK || true", SSHExecOptions{})
		return err == nil && strings.TrimSpace(out) == "OK"
	}
	if remoteHermesPath != "" {
		if isExecutable(remoteHermesPath) {
			return remoteHermesPath, nil
		}
		return "", lifecycleError("hermes-not-found", "The Icebath path you set is not an executable on the remote host: \""+remoteHermesPath+"\". "+
			"Check the path (it must be the full path to the `icebath` binary on the remote, e.g. "+
			"~/icebath-agent/.venv/bin/icebath), or clear it to auto-detect.", nil)
	}
	var candidates []string
	if found, err := ssh.Exec("bash -lc "+Shq("command -v icebath"), SSHExecOptions{}); err == nil {
		if found = strings.TrimSpace(found); found != "" {
			lines := strings.Split(found, "\n")
			candidates = append(candidates, strings.TrimSpace(lines[len(lines)-1]))
		}
	}
	candidates = append(candidates, "~/.local/bin/icebath", "/usr/local/bin/icebath", "~/.icebath/icebath-agent/venv/bin/icebath")
	for _, candidate := range candidates {
		if candidate != "" && isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", lifecycleError("hermes-not-found", "Icebath is not installed on the remote host (could not find a `icebath` executable). "+
		"Install it on the remote with:  curl -fsSL https://hermes-agent.nousresearch.com/install.sh | sh  "+
		"— or set the Icebath path explicitly in the SSH connection settings.", nil)
}

// First line of the remote icebath --version under a remote watchdog, empty on failure
func ProbeHermesVersion(ssh RemoteShell, hermesPath string) string {
	expanded, err := ExpandRemotePath(hermesPath)
	if err != nil {
		return ""
	}
	out, err := ssh.Exec(WithRemoteTimeout(expanded+" --version 2>&1", RemoteProbeTimeoutSecs), SSHExecOptions{})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
}

// Remote os and arch, refusing anything but Linux and Darwin
func ProbeRemotePlatform(ssh RemoteShell) (RemotePlatform, error) {
	out, err := ssh.Exec("uname -s; uname -m", SSHExecOptions{})
	if err != nil {
		return RemotePlatform{}, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	osName := strings.TrimSpace(lines[0])
	arch := ""
	if len(lines) > 1 {
		arch = strings.TrimSpace(lines[1])
	}
	if osName != "Linux" && osName != "Darwin" {
		shown := osName
		if shown == "" {
			shown = "unknown"
		}
		return RemotePlatform{}, lifecycleError("unsupported-platform", "Unsupported remote platform \""+shown+"\". Icebath Desktop SSH mode supports Linux, macOS, and Windows remote hosts.", nil)
	}
	return RemotePlatform{OS: osName, Arch: arch}, nil
}

// ICEBATH_HOME the remote dashboard uses
func ProbeRemoteHermesHome(ssh RemoteShell) (string, error) {
	out, err := ssh.Exec(`echo "${ICEBATH_HOME:-$HOME/.icebath}"`, SSHExecOptions{})
	if err != nil {
		return "", lifecycleError("transient-transport-error", "Could not resolve the remote Icebath home.", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; last != "" {
		return last, nil
	}
	return "~/.icebath", nil
}

// Refuses SSH reuse or spawn while the remote install is being updated, failing closed on any doubt
func AssertRemoteInstallUpdateClear(ssh RemoteShell, hermesHome, hermesPath string) error {
	root, err := RemoteInstallRoot(hermesHome)
	if err != nil {
		return err
	}
	marker, err := ExpandRemotePath(root + "/.icebath-update-in-progress")
	if err != nil {
		return err
	}
	hermes := ""
	if hermesPath != "" {
		expanded, err := ExpandRemotePath(hermesPath)
		if err != nil {
			return err
		}
		hermes = " '' " + expanded
	}
	program := RemoteMarkerGatePy
	out, err := ssh.Exec("python3 - "+marker+hermes, SSHExecOptions{StdinData: &program})
	if err != nil {
		return lifecycleError("update-in-progress", "Could not prove that the remote Icebath install is clear for SSH startup.", err)
	}
	lines := crlfSplit.Split(strings.TrimSpace(out), -1)
	observation := lines[len(lines)-1]
	if observation == "CLEAR" {
		return nil
	}
	message := "The remote Icebath update marker is unreadable, malformed, or locked; refusing SSH startup."
	if live := liveVerdictRE.FindStringSubmatch(observation); live != nil {
		message = "Remote Icebath update process " + live[1] + " is still running; SSH startup is paused."
	} else if observation == "HELD" {
		message = "A process the remote Icebath update started still holds the install; SSH startup is paused."
	}
	return lifecycleError("update-in-progress", message, nil)
}

// Profiles of the remote install, the Windows inventory for a Windows remote
func ListRemoteHermesProfiles(ssh RemoteShell, platform *RemotePlatform) ([]string, error) {
	if platform != nil && platform.OS == "Windows" {
		return ListWindowsRemoteProfiles(ssh, platform.Windows)
	}
	probed, err := ProbeRemoteHermesHome(ssh)
	if err != nil {
		return nil, err
	}
	home, err := AssertSafeRemoteHome(probed)
	if err != nil {
		return nil, err
	}
	dir, err := ExpandRemotePath(home + "/profiles")
	if err != nil {
		return nil, err
	}
	listing, err := ssh.Exec("if [ -d "+dir+" ]; then ls -1 "+dir+"; fi", SSHExecOptions{})
	if err != nil {
		return nil, lifecycleError("transient-transport-error", "Could not list remote Icebath profiles.", err)
	}
	return ParseRemoteProfileListing(listing), nil
}

// install_id of the remote install root, empty when missing or not an identity
func ReadRemoteInstallID(ssh RemoteShell, platform *RemotePlatform) (string, error) {
	if platform != nil && platform.OS == "Windows" {
		return ReadWindowsRemoteInstallID(ssh, platform.Windows)
	}
	probed, err := ProbeRemoteHermesHome(ssh)
	if err != nil {
		return "", err
	}
	home, err := AssertSafeRemoteHome(probed)
	if err != nil {
		return "", err
	}
	root, err := RemoteInstallRoot(home)
	if err != nil {
		return "", err
	}
	file, err := ExpandRemotePath(root + "/install_id")
	if err != nil {
		return "", err
	}
	out, err := ssh.Exec("if [ -f "+file+" ]; then cat "+file+"; fi", SSHExecOptions{})
	if err != nil {
		return "", lifecycleError("transient-transport-error", "Could not read the remote Icebath install id.", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	id := strings.ToLower(strings.TrimSpace(lines[len(lines)-1]))
	if ownershipIDRE.MatchString(id) {
		return id, nil
	}
	return "", nil
}

// Remote home of plain path characters without a parent segment, trailing slashes dropped
func AssertSafeRemoteHome(home string) (string, error) {
	value := strings.TrimSpace(home)
	if !safeRemoteHome.MatchString(value) || strings.Contains(value, "..") {
		return "", lifecycleError("unsafe-path", "Unsafe remote Icebath home.", nil)
	}
	return strings.TrimRight(value, "/"), nil
}

// Install root of a remote home, the parent of a profiles directory home
func RemoteInstallRoot(home string) (string, error) {
	value, err := AssertSafeRemoteHome(home)
	if err != nil {
		return "", err
	}
	if m := profileHomeRE.FindStringSubmatch(value); m != nil {
		return m[1], nil
	}
	return value, nil
}

// RemoteLock is a backend.lock.json record in key order, or the skew sentinel of a foreign or corrupt one
type RemoteLock struct {
	Object *jsjson.OrderedObject
	Skew   bool
	Reason string
}

func lockfileSkew(reason string) *RemoteLock { return &RemoteLock{Skew: true, Reason: reason} }

// Whether a lock is the skew sentinel
func IsLockfileSkew(lock *RemoteLock) bool { return lock != nil && lock.Skew }

func (l *RemoteLock) get(key string) any {
	if l == nil || l.Object == nil {
		return nil
	}
	return l.Object.Get(key)
}

func (l *RemoteLock) str(key string) string {
	s, _ := l.get(key).(string)
	return s
}

func (l *RemoteLock) num(key string) float64 {
	f, _ := l.get(key).(float64)
	return f
}

// PID of the record
func (l *RemoteLock) PID() int { return int(l.num("pid")) }

// Remote dashboard port, zero while the spawn is in progress
func (l *RemoteLock) Port() int { return int(l.num("port")) }

func jsInteger(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// The ownership record of an id, nil for none or a foreign protocol, the skew sentinel for unreadable state
func ReadLockfile(ssh RemoteShell, ownershipID string) (*RemoteLock, error) {
	lpath, err := LockfilePath(ownershipID)
	if err != nil {
		return nil, err
	}
	raw, err := ssh.Exec("if [ ! -e "+mustExpand(lpath)+" ]; then exit 0; fi; cat "+mustExpand(lpath), SSHExecOptions{})
	if err != nil {
		return nil, lifecycleError("transient-transport-error", "Could not read the SSH backend ownership record.", err)
	}
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, nil
	}
	parsed, err := jsjson.ParseOrdered(text)
	if err != nil {
		return lockfileSkew("unparseable-json"), nil
	}
	if !jsTruthy(parsed) {
		return lockfileSkew("non-object"), nil
	}
	obj, ok := parsed.(*jsjson.OrderedObject)
	if !ok {
		if _, isArray := parsed.([]any); !isArray {
			return lockfileSkew("non-object"), nil
		}
		obj = jsjson.NewOrderedObject()
	}
	lock := &RemoteLock{Object: obj}
	if obj.Get("schemaVersion") != float64(LockfileSchemaVersion) {
		return lockfileSkew("schema-version " + jsjson.Stringify(obj.Get("schemaVersion"), "", "")), nil
	}
	if pid, ok := jsInteger(obj.Get("pid")); !ok || pid <= 0 || pid > 4194304 {
		return lockfileSkew("malformed-pid"), nil
	}
	if port, ok := jsInteger(obj.Get("port")); !ok || port < 0 || port > 65535 {
		return lockfileSkew("malformed-port"), nil
	}
	if obj.Get("ownershipId") != ownershipID || !spawnNonceRE.MatchString(jsOrString(obj.Get("spawnNonce"))) {
		return lockfileSkew("ownership-mismatch"), nil
	}
	if !ownershipIDRE.MatchString(jsOrString(obj.Get("tokenFingerprint"))) {
		return lockfileSkew("malformed-token-fingerprint"), nil
	}
	if obj.Get("protocolVersion") != float64(RemoteProtocolVersion) {
		return nil, nil
	}
	if expected, err := SpawnLogPath(ownershipID, jsString(obj.Get("spawnNonce"))); err != nil || obj.Get("logPath") != expected {
		if err != nil {
			return nil, err
		}
		return lockfileSkew("log-path-mismatch"), nil
	}
	for _, field := range []string{"profile", "hermesPath", "hermesHome", "logPath", "startedAt"} {
		s, ok := obj.Get(field).(string)
		if !ok || utf16Len(s) > 1024 {
			return lockfileSkew("malformed-field " + field), nil
		}
	}
	if obj.Has("creationTime") {
		s, ok := obj.Get("creationTime").(string)
		if !ok || !creationTimeRE.MatchString(s) {
			return nil, nil
		}
	}
	return lock, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Publishes a lock atomically through a temporary file under umask 077
func WriteLockfile(ssh RemoteShell, ownershipID string, lock *jsjson.OrderedObject) error {
	directory, err := OwnershipDirectory(ownershipID)
	if err != nil {
		return err
	}
	lpath, _ := LockfilePath(ownershipID)
	temporary := directory + "/." + randomHex(8) + ".lock.tmp"
	record := lock.Clone()
	record.Set("schemaVersion", float64(LockfileSchemaVersion))
	text := jsjson.Stringify(record, "", "")
	_, err = ssh.Exec("umask 077 && mkdir -p "+mustExpand(directory)+" && "+
		"printf '%s' "+Shq(text)+" > "+mustExpand(temporary)+" && "+
		"mv -f "+mustExpand(temporary)+" "+mustExpand(lpath), SSHExecOptions{})
	return err
}

// Drops the lockfile, a failure ignored
func RemoveLockfile(ssh RemoteShell, ownershipID string) {
	lpath, err := LockfilePath(ownershipID)
	if err != nil {
		return
	}
	_, _ = ssh.Exec("rm -f "+mustExpand(lpath), SSHExecOptions{})
}

// One of the sentinels a probe prints, retried while the channel answers neither, else a transient failure
func execProbeVerdict(ssh RemoteShell, command string, sentinels []string, failureMessage string) (string, error) {
	for attempt := 0; attempt < probeVerdictAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(probeVerdictRetry)
		}
		out, err := ssh.Exec(command, SSHExecOptions{})
		if err != nil {
			return "", lifecycleError("transient-transport-error", failureMessage, err)
		}
		out = strings.TrimSpace(out)
		for _, s := range sentinels {
			if out == s {
				return out, nil
			}
		}
	}
	return "", lifecycleError("transient-transport-error", failureMessage, nil)
}

// Whether a remote pid answers kill 0
func RemotePIDAlive(ssh RemoteShell, pid int) (bool, error) {
	if pid == 0 {
		return false, nil
	}
	verdict, err := execProbeVerdict(ssh, fmt.Sprintf("kill -0 %d 2>/dev/null && echo ALIVE || echo DEAD", pid), []string{"ALIVE", "DEAD"}, "Could not verify the SSH backend process.")
	return verdict == "ALIVE", err
}

// Kernel start identity of a remote pid, linux start ticks or darwin lstart, empty when unproved
func RemoteProcessCreationTime(ssh RemoteShell, pid int) string {
	if pid == 0 {
		return ""
	}
	script := "import subprocess,sys\n" +
		"pid=" + strconv.Itoa(pid) + "\n" +
		"value=\"\"\n" +
		"if sys.platform.startswith(\"linux\"):\n" +
		" try:\n" +
		"  raw=open(f\"/proc/{pid}/stat\",\"r\",encoding=\"ascii\").read()\n" +
		"  fields=raw[raw.rfind(\")\")+2:].split()\n" +
		"  value=\"linux:\"+fields[19]\n" +
		" except (OSError,IndexError,UnicodeError):pass\n" +
		"elif sys.platform==\"darwin\":\n" +
		" try:\n" +
		"  started=subprocess.check_output([\"ps\",\"-o\",\"lstart=\",\"-p\",str(pid)],text=True).strip()\n" +
		"  if started:value=\"darwin:\"+started\n" +
		" except (OSError,subprocess.CalledProcessError):pass\n" +
		"print(value)"
	out, err := ssh.Exec("python3 -c "+Shq(script), SSHExecOptions{})
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(out)
	if creationTimeRE.MatchString(value) {
		return value
	}
	return ""
}

// Whether a remote pid is provably the isolated serve this desktop spawned, by argv nonce, token file and profile
func PIDIsOurDashboard(ssh RemoteShell, pid int, spawnNonce, hermesPath, hermesHome, ownershipID, profile string) (bool, error) {
	if pid == 0 || !spawnNonceRE.MatchString(spawnNonce) || hermesPath == "" {
		return false, nil
	}
	tokenPath := ""
	if ownershipID != "" {
		p, err := SpawnTokenPath(ownershipID, spawnNonce)
		if err != nil {
			return false, err
		}
		tokenPath = p
	}
	script := "import os,shlex,subprocess,sys\n" +
		"pid=" + strconv.Itoa(pid) + "\n" +
		"expected=os.path.expanduser(" + Shq(hermesPath) + ")\n" +
		"hermes_home=os.path.expanduser(" + Shq(hermesHome) + ") if " + Shq(hermesHome) + " else \"\"\n" +
		"expected_entries={expected}\n" +
		"if hermes_home:\n" +
		" expected_entries.add(os.path.join(hermes_home,\"icebath-agent\",\"venv\",\"bin\",\"icebath\"))\n" +
		"expected_token=os.path.expanduser(" + Shq(tokenPath) + ")\n" +
		"expected_profile=" + Shq(profile) + "\n" +
		"nonce=" + Shq(spawnNonce) + "\n" +
		"try:\n" +
		" raw=open(f\"/proc/{pid}/cmdline\",\"rb\").read()\n" +
		" args=[x.decode(\"utf-8\",\"surrogateescape\") for x in raw.split(b\"\\0\") if x]\n" +
		"except OSError:\n" +
		" try:\n" +
		"  line=subprocess.check_output([\"ps\",\"-ww\",\"-o\",\"command=\",\"-p\",str(pid)],text=True).strip()\n" +
		" except subprocess.CalledProcessError:\n" +
		"  # pid already gone — a dead process is FOREIGN, not a transport error\n" +
		"  print(\"FOREIGN\");sys.exit(0)\n" +
		" args=shlex.split(line)\n" +
		"ok=False\n" +
		"try:\n" +
		" profile_arg=args.index(\"--profile\") if expected_profile else -1\n" +
		" pval=profile_arg+1 if profile_arg>=0 else -1\n" +
		" serve_pos=[i for i,a in enumerate(args) if a==\"serve\" and i!=pval]\n" +
		" serve=serve_pos[0]\n" +
		" owner=args.index(\"--ssh-owner-nonce\",serve+1)\n" +
		" token=args.index(\"--ssh-session-token-file\",serve+1) if expected_token else -1\n" +
		" isolated=args.index(\"--isolated\",serve+1)\n" +
		" serve_count=len(serve_pos)\n" +
		" owner_count=args.count(\"--ssh-owner-nonce\")\n" +
		" token_count=args.count(\"--ssh-session-token-file\")\n" +
		" isolated_count=args.count(\"--isolated\")\n" +
		" profile_count=args.count(\"--profile\")\n" +
		" direct=args[0] in expected_entries\n" +
		" python_entry=len(args)>1 and args[1] in expected_entries and os.path.basename(args[0]).startswith(\"python\")\n" +
		" token_ok=not expected_token or args[token+1]==expected_token\n" +
		" isolated_ok=isolated_count==1 and isolated>serve\n" +
		" profile_ok=(profile_count==1 and profile_arg<serve and args[profile_arg+1]==expected_profile) if expected_profile else profile_count==0\n" +
		" spawn_proof=bool(expected_token) and owner_count==1 and token_count==1 and token_ok and profile_ok\n" +
		" ok=(direct or python_entry or spawn_proof) and serve_count==1 and isolated_ok and owner_count==1 and args[owner+1]==nonce and token_ok and profile_ok\n" +
		"except (ValueError,IndexError):pass\n" +
		"print(\"OWNED\" if ok else \"FOREIGN\")"
	verdict, err := execProbeVerdict(ssh, "python3 -c "+Shq(script), []string{"OWNED", "FOREIGN"}, "Could not verify SSH backend process ownership.")
	return verdict == "OWNED", err
}

// Kills a stale dashboard only when provably ours, escalating to SIGKILL, then drops its log and lockfile
func CleanupStale(ssh RemoteShell, ownershipID string, lock *RemoteLock, pidAlive bool) error {
	if IsLockfileSkew(lock) {
		return nil
	}
	if pidAlive && lock != nil {
		owned, err := PIDIsOurDashboard(ssh, lock.PID(), lock.str("spawnNonce"), lock.str("hermesPath"), lock.str("hermesHome"), ownershipID, lock.str("profile"))
		if err != nil {
			return err
		}
		if owned {
			pid := lock.PID()
			_, err := ssh.Exec(fmt.Sprintf("kill %d && i=0; while kill -0 %d 2>/dev/null; do i=$((i+1)); [ \"$i\" -ge 50 ] && exit 1; sleep 0.1; done", pid, pid), SSHExecOptions{})
			if err != nil {
				if _, err := ssh.Exec(fmt.Sprintf("kill -9 %d 2>/dev/null; i=0; while kill -0 %d 2>/dev/null; do i=$((i+1)); [ \"$i\" -ge 20 ] && exit 1; sleep 0.1; done", pid, pid), SSHExecOptions{}); err != nil {
					return lifecycleError("transient-transport-error", "Could not terminate the stale SSH backend.", err)
				}
			}
		}
	}
	expectedLog := ""
	if nonce := lock.str("spawnNonce"); nonce != "" {
		p, err := SpawnLogPath(ownershipID, nonce)
		if err != nil {
			return err
		}
		expectedLog = p
	}
	if lock != nil && lock.get("logPath") == expectedLog {
		_, _ = ssh.Exec("rm -f "+mustExpand(lock.str("logPath")), SSHExecOptions{})
	}
	RemoveLockfile(ssh, ownershipID)
	return nil
}

// Kills only a provably owned serve and drops its lockfile on quit or a connection switch
func RemoteDisconnect(ssh RemoteShell, ownershipID string) error {
	if ssh == nil || ownershipID == "" {
		return nil
	}
	lock, err := ReadLockfile(ssh, ownershipID)
	if err != nil {
		return err
	}
	if lock == nil || IsLockfileSkew(lock) {
		return nil
	}
	alive, err := RemotePIDAlive(ssh, lock.PID())
	if err != nil {
		return err
	}
	return CleanupStale(ssh, ownershipID, lock, alive)
}

// ManagedUpdateExpected is the ownership snapshot a managed update drain proves before signalling
type ManagedUpdateExpected struct {
	OwnershipID    string
	PID            int
	SpawnNonce     string
	Profile        string
	HermesPath     string
	HermesHome     string
	StartedAt      string
	CreationTimeNs string
	CreationTime   string
}

func lockMatchesManagedUpdateScope(lock *RemoteLock, e ManagedUpdateExpected) bool {
	if lock == nil || lock.Object == nil {
		return false
	}
	field := func(key, want string) bool {
		s, ok := lock.get(key).(string)
		return ok && s == want
	}
	pid, ok := lock.get("pid").(float64)
	return field("ownershipId", e.OwnershipID) && ok && pid == float64(e.PID) && field("spawnNonce", e.SpawnNonce) &&
		field("startedAt", e.StartedAt) && field("creationTime", e.CreationTime) && field("profile", e.Profile) &&
		field("hermesPath", e.HermesPath) && field("hermesHome", e.HermesHome)
}

func pyString(v string) string { return jsjson.Stringify(v, "", "") }

// Signal boundary program that rereads creation time and argv before a pidfd bound SIGTERM
func buildOwnedTerminationCommand(lock *RemoteLock, ownershipID string) (string, error) {
	pid := lock.PID()
	expectedToken, err := SpawnTokenPath(ownershipID, lock.str("spawnNonce"))
	if err != nil {
		return "", err
	}
	script := `
import os,select,shlex,signal,subprocess,sys,time
pid=` + strconv.Itoa(pid) + `
expected_creation=` + pyString(jsOrString(lock.get("creationTime"))) + `
expected_path=os.path.expanduser(` + pyString(jsOrString(lock.get("hermesPath"))) + `)
hermes_home=os.path.expanduser(` + pyString(jsOrString(lock.get("hermesHome"))) + `)
expected_entries={expected_path,os.path.join(hermes_home,"icebath-agent","venv","bin","icebath")}
expected_token=os.path.expanduser(` + pyString(expectedToken) + `)
expected_profile=` + pyString(jsOrString(lock.get("profile"))) + `
nonce=` + pyString(jsOrString(lock.get("spawnNonce"))) + `

def creation():
 if sys.platform.startswith("linux"):
  try:
   raw=open(f"/proc/{pid}/stat","r",encoding="ascii").read()
   return "linux:"+raw[raw.rfind(")")+2:].split()[19]
  except (OSError,IndexError,UnicodeError):return ""
 if sys.platform=="darwin":
  try:
   value=subprocess.check_output(["ps","-o","lstart=","-p",str(pid)],text=True).strip()
   return "darwin:"+value if value else ""
  except (OSError,subprocess.CalledProcessError):return ""
 return ""

def argv():
 try:
  raw=open(f"/proc/{pid}/cmdline","rb").read()
  return [part.decode("utf-8","surrogateescape") for part in raw.split(b"\0") if part]
 except OSError:
  try:return shlex.split(subprocess.check_output(["ps","-ww","-o","command=","-p",str(pid)],text=True).strip())
  except (OSError,subprocess.CalledProcessError,ValueError):return []

def identity_before_signal():
 if sys.platform=="darwin":
  try:
   line=subprocess.check_output(["ps","-ww","-p",str(pid),"-o","lstart=","-o","command="],text=True).strip()
   prefix=expected_creation.removeprefix("darwin:")
   if not prefix or not line.startswith(prefix):return "",[]
   return "darwin:"+prefix,shlex.split(line[len(prefix):].strip())
  except (OSError,subprocess.CalledProcessError,ValueError):return "",[]
 return creation(),argv()

def owned(args):
 try:
  profile_arg=args.index("--profile") if expected_profile else -1
  pval=profile_arg+1 if profile_arg>=0 else -1
  serve_pos=[i for i,a in enumerate(args) if a=="serve" and i!=pval]
  serve=serve_pos[0]
  owner=args.index("--ssh-owner-nonce",serve+1)
  token=args.index("--ssh-session-token-file",serve+1)
  isolated=args.index("--isolated",serve+1)
  direct=args[0] in expected_entries
  python_entry=len(args)>1 and args[1] in expected_entries and os.path.basename(args[0]).startswith("python")
  profile_ok=(args.count("--profile")==1 and profile_arg<serve and args[profile_arg+1]==expected_profile) if expected_profile else args.count("--profile")==0
  return ((direct or python_entry or (args[token+1]==expected_token and profile_ok)) and
          len(serve_pos)==1 and args.count("--ssh-owner-nonce")==1 and
          args.count("--ssh-session-token-file")==1 and args.count("--isolated")==1 and
          isolated>serve and args[owner+1]==nonce and args[token+1]==expected_token and profile_ok)
 except (ValueError,IndexError):return False

pidfd=None
if sys.platform.startswith("linux"):
 if not hasattr(os,"pidfd_open") or not hasattr(signal,"pidfd_send_signal"):
  print("UNAVAILABLE");sys.exit(0)
 try:pidfd=os.pidfd_open(pid,0)
 except ProcessLookupError:print("ALREADY_STOPPED");sys.exit(0)
 except (OSError,PermissionError):print("UNAVAILABLE");sys.exit(0)

try:
 live_creation,live_args=identity_before_signal()
 if live_creation!=expected_creation or not owned(live_args):
  print("REFUSED");sys.exit(0)
 if (sys.platform=="darwin"):
  print("DARWIN_UNAVAILABLE");sys.exit(0)
 try:
  if pidfd is not None:signal.pidfd_send_signal(pidfd,signal.SIGTERM)
  else:os.kill(pid,signal.SIGTERM)
 except ProcessLookupError:print("ALREADY_STOPPED");sys.exit(0)
 if pidfd is not None:
  poller=select.poll();poller.register(pidfd,select.POLLIN)
  if not poller.poll(10000):print("TIMEOUT");sys.exit(0)
 else:
  deadline=time.monotonic()+10
  while time.monotonic()<deadline:
   try:os.kill(pid,0)
   except ProcessLookupError:break
   except PermissionError:print("UNAVAILABLE");sys.exit(0)
   time.sleep(.1)
  else:print("TIMEOUT");sys.exit(0)
 print("TERMINATED")
finally:
 if pidfd is not None:os.close(pidfd)
`
	return "python3 -c " + Shq(strings.TrimSpace(script)), nil
}

// Runs command under the updaters marker lock, refusing a live, unreadable or held claim
func withRemoteUpdateMutex(command, markerPath, hermes string) string {
	return "python3 -c " + Shq(RemoteMarkerGatePy) + " " + markerPath + " " + Shq(command) + " " + hermes
}

// TerminateResult is how a managed update drain ended one owned serve
type TerminateResult struct {
	PID            int
	Terminated     bool
	AlreadyStopped bool
}

// Stops one desktop owned POSIX serve before an install update after proving its record, pid start and argv twice
func TerminateOwnedDashboardForUpdate(ssh RemoteShell, expected ManagedUpdateExpected) (TerminateResult, error) {
	ownershipID, err := validateOwnershipID(expected.OwnershipID)
	if err != nil {
		return TerminateResult{}, err
	}
	if expected.CreationTime == "" {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX serve has no process creation-time proof.", nil)
	}
	lock, err := ReadLockfile(ssh, ownershipID)
	if err != nil {
		return TerminateResult{}, err
	}
	if IsLockfileSkew(lock) || !lockMatchesManagedUpdateScope(lock, expected) {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX ownership record changed before the managed update.", nil)
	}
	alive, err := RemotePIDAlive(ssh, lock.PID())
	if err != nil {
		return TerminateResult{}, err
	}
	if !alive {
		return TerminateResult{PID: lock.PID(), AlreadyStopped: true}, nil
	}
	if RemoteProcessCreationTime(ssh, lock.PID()) != lock.str("creationTime") {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX PID creation time no longer matches its ownership record.", nil)
	}
	owned, err := PIDIsOurDashboard(ssh, lock.PID(), lock.str("spawnNonce"), lock.str("hermesPath"), lock.str("hermesHome"), ownershipID, lock.str("profile"))
	if err != nil {
		return TerminateResult{}, err
	}
	if !owned {
		return TerminateResult{}, lifecycleError("foreign-backend", "Refusing to terminate a remote process whose Desktop ownership is unproven.", nil)
	}
	lock, err = ReadLockfile(ssh, ownershipID)
	if err != nil {
		return TerminateResult{}, err
	}
	if IsLockfileSkew(lock) || !lockMatchesManagedUpdateScope(lock, expected) {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX ownership record changed during process verification.", nil)
	}
	stillOwned := RemoteProcessCreationTime(ssh, lock.PID()) == lock.str("creationTime")
	if stillOwned {
		if stillOwned, err = PIDIsOurDashboard(ssh, lock.PID(), lock.str("spawnNonce"), lock.str("hermesPath"), lock.str("hermesHome"), ownershipID, lock.str("profile")); err != nil {
			return TerminateResult{}, err
		}
	}
	if !stillOwned {
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX process identity changed during managed update drain.", nil)
	}
	command, err := buildOwnedTerminationCommand(lock, ownershipID)
	if err != nil {
		return TerminateResult{}, err
	}
	out, err := ssh.Exec(command, SSHExecOptions{})
	if err != nil {
		return TerminateResult{}, lifecycleError("transient-transport-error", "Could not terminate the Desktop-owned remote serve for update.", err)
	}
	switch strings.TrimSpace(out) {
	case "ALREADY_STOPPED":
		return TerminateResult{PID: lock.PID(), AlreadyStopped: true}, nil
	case "TERMINATED":
		return TerminateResult{PID: lock.PID(), Terminated: true}, nil
	case "REFUSED":
		return TerminateResult{}, lifecycleError("ownership-changed", "The remote POSIX process identity changed at the signal boundary.", nil)
	case "DARWIN_UNAVAILABLE":
		return TerminateResult{}, lifecycleError("ownership-changed", "Darwin cannot atomically bind a signal to the verified PID; refusing termination.", nil)
	}
	cause := lifecycleError("transient-transport-error", "The remote POSIX signal boundary was unavailable.", nil)
	return TerminateResult{}, lifecycleError("transient-transport-error", "Could not terminate the Desktop-owned remote serve for update.", cause)
}

// SpawnCommandOptions shape the detached remote serve and its lock publication
type SpawnCommandOptions struct {
	SpawnNonce       string
	TokenFilePath    string
	LogPath          string
	HermesHome       string
	DevServer        string
	OwnershipID      string
	ReservationNonce string
	LockMetadata     *jsjson.OrderedObject
}

// Detached isolated serve under the marker lock, reserving ownership and publishing the lock in the same command
func BuildSpawnCommand(hermesPath, profile string, o SpawnCommandOptions) (string, error) {
	hermes, err := ExpandRemotePath(hermesPath)
	if err != nil {
		return "", err
	}
	pinned := BackendProfileArg(profile)
	profileArgs := ""
	pinnedValue := ""
	if pinned != nil {
		pinnedValue = *pinned
		profileArgs = "--profile " + Shq(pinnedValue) + " "
	}
	if o.LockMetadata != nil && o.LockMetadata.Get("profile") != pinnedValue {
		o.LockMetadata = o.LockMetadata.Clone()
		o.LockMetadata.Set("profile", pinnedValue)
	}
	logPath, err := ExpandRemotePath(o.LogPath)
	if err != nil {
		return "", err
	}
	tokenArg := ""
	if o.TokenFilePath != "" {
		expanded, err := ExpandRemotePath(o.TokenFilePath)
		if err != nil {
			return "", err
		}
		tokenArg = " --ssh-session-token-file " + expanded
	}
	ownerArg := ""
	if o.SpawnNonce != "" {
		nonce, err := validateSpawnNonce(o.SpawnNonce)
		if err != nil {
			return "", err
		}
		ownerArg = " --ssh-owner-nonce " + nonce
	}
	subCmd := "serve --isolated --host 127.0.0.1 --port 0" + tokenArg + ownerArg
	homeArg := o.HermesHome
	if homeArg == "" {
		homeArg = "~/.icebath"
	}
	root, err := RemoteInstallRoot(homeArg)
	if err != nil {
		return "", err
	}
	marker, err := ExpandRemotePath(root + "/.icebath-update-in-progress")
	if err != nil {
		return "", err
	}
	markerAbsent := "[ ! -e " + marker + " ]"
	devServerEnv := ""
	if o.DevServer != "" {
		devServerEnv = " ICEBATH_DESKTOP_DEV_SERVER=" + Shq(o.DevServer)
	}
	dashCmd := "ulimit -n " + strconv.Itoa(remoteNofileSoftLimit) + " 2>/dev/null || true; unset ICEBATH_DESKTOP_DEV_SERVER; " +
		"exec env ICEBATH_DESKTOP=1" + devServerEnv + " " + hermes + " " + profileArgs + subCmd
	detachedShell := `eval "exec $1>&-"; ` + dashCmd + " </dev/null >> " + logPath + " 2>&1 & echo $!"
	detachedSpawn := `child=$("$(command -v setsid || echo nohup)" sh -c ` + Shq(detachedShell) + ` icebath-update-child "$1")`
	if o.OwnershipID == "" || o.LockMetadata == nil {
		return withRemoteUpdateMutex(`mkdir -p "$(dirname `+logPath+`)" && `+
			detachedSpawn+"; "+
			markerAbsent+` || { kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; exit 75; }; echo "$child"`, marker, hermes), nil
	}
	reservationPath, err := ConnectReservationPath(o.OwnershipID)
	if err != nil {
		return "", err
	}
	reservation := mustExpand(reservationPath)
	lockPathValue, _ := LockfilePath(o.OwnershipID)
	lockPath := mustExpand(lockPathValue)
	tokenPath := ""
	if o.TokenFilePath != "" {
		tokenPath = mustExpand(o.TokenFilePath)
	}
	ownerPath := reservation + "/owner"
	meta := jsjson.NewOrderedObject("schemaVersion", float64(LockfileSchemaVersion))
	for _, k := range o.LockMetadata.Keys() {
		meta.SetRaw(k, o.LockMetadata.Get(k))
	}
	meta.Set("pid", "__PID__")
	metadata := jsjson.Stringify(meta, "", "")
	nonceSource := o.ReservationNonce
	if nonceSource == "" {
		nonceSource = randomHex(8)
	}
	reservationNonce, err := validateSpawnNonce(nonceSource)
	if err != nil {
		return "", err
	}
	removeToken := ""
	if tokenPath != "" {
		removeToken = "rm -f " + tokenPath + "; "
	}
	return withRemoteUpdateMutex(`(umask 077 && mkdir -p "$(dirname `+reservation+`)"); `+
		"reservation="+reservation+"; lock="+lockPath+"; owner_file="+ownerPath+"; "+
		"reservation_nonce="+Shq(reservationNonce)+"; "+
		`i=0; while ! mkdir "$reservation" 2>/dev/null; do `+
		`owner_data=$(cat "$owner_file" 2>/dev/null || true); owner_pid=${owner_data%%:*}; `+
		`case "$owner_pid" in ''|*[!0-9]*) ;; *) kill -0 "$owner_pid" 2>/dev/null || { rm -rf "$reservation"; continue; };; esac; `+
		`i=$((i+1)); [ "$i" -ge 600 ] && exit 75; sleep 0.05; done; `+
		`printf '%s:%s' "$$" "$reservation_nonce" > "$owner_file"; `+
		`trap 'rm -rf "$reservation"' EXIT; `+
		`if [ -f "$lock" ]; then `+
		`existing_pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$lock" | head -n 1); `+
		`case "$existing_pid" in ''|*[!0-9]*) rm -f "$lock";; *) `+
		`if kill -0 "$existing_pid" 2>/dev/null; then `+removeToken+`printf EXISTING; exit 0; fi; rm -f "$lock";; esac; fi; `+
		`mkdir -p "$(dirname `+logPath+`)" && `+
		detachedSpawn+"; "+
		markerAbsent+` || { kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; exit 75; }; `+
		"lock_json=$(printf '%s' "+Shq(metadata)+` | sed "s/\"pid\":\"__PID__\"/\"pid\":${child}/"); `+
		`temporary_lock="${lock}.`+reservationNonce+`.tmp"; `+
		`printf '%s' "$lock_json" > "$temporary_lock" && mv -f "$temporary_lock" "$lock" || { kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; exit 76; }; `+
		`echo "$child"`, marker, hermes), nil
}

// Whether the remote serve accepts the token file and owner nonce flags, probed under a remote watchdog
func RemoteSupportsSSHOwnership(ssh RemoteShell, hermesPath string) (bool, error) {
	hermes, err := ExpandRemotePath(hermesPath)
	if err != nil {
		return false, err
	}
	out, err := ssh.Exec(`help="$( `+WithRemoteTimeout(hermes+" serve --help 2>&1", RemoteProbeTimeoutSecs)+` )"; `+
		`printf '%s' "$help" | grep -q ssh-session-token-file && `+
		`printf '%s' "$help" | grep -q ssh-owner-nonce && echo YES || echo NO`, SSHExecOptions{})
	if err != nil {
		return false, err
	}
	return strings.HasSuffix(strings.TrimSpace(out), "YES"), nil
}

func remoteProfileMissingError(output string) *SSHError {
	m := missingProfile.FindStringSubmatch(output)
	if m == nil {
		return nil
	}
	tail := "."
	if create := strings.TrimSpace(m[2]); create != "" {
		tail = " or create it with: " + create
	}
	err := lifecycleError("remote-profile-missing", "The remote Icebath profile '"+m[1]+"' does not exist. Select an existing remote profile"+tail, nil)
	err.Profile = m[1]
	return err
}

// First ready announcement not preceded by a word character, the lookbehind of the merged output pattern
func readyPortInMerged(text string) (int, bool) {
	for _, loc := range readyMergedRE.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 {
			c := text[loc[0]-1]
			if c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				continue
			}
		}
		port, err := strconv.Atoi(text[loc[2]:loc[3]])
		return port, err == nil
	}
	return 0, false
}

// Polls the remote spawn log for the ready port until the deadline, the cancel or a dead spawn
func ScrapeReadyPort(ssh RemoteShell, logPath string, timeout time.Duration, isAlive func() (bool, error), signal context.Context) (int, error) {
	deadline := time.Now().Add(timeout)
	remoteLog, err := ExpandRemotePath(logPath)
	if err != nil {
		return 0, err
	}
	for time.Now().Before(deadline) {
		if err := AssertBootstrapNotSuperseded(signal); err != nil {
			return 0, err
		}
		tail, err := ssh.Exec("cat "+remoteLog+" 2>/dev/null || true", SSHExecOptions{})
		if err != nil {
			tail = ""
		}
		if isAlive != nil {
			alive, err := isAlive()
			if err != nil {
				return 0, err
			}
			if !alive {
				if cause := remoteProfileMissingError(tail); cause != nil {
					return 0, cause
				}
				return 0, lifecycleError("spawn-failed", "Remote dashboard process exited before announcing its port.", nil)
			}
		}
		if port, ok := readyPortInMerged(tail); ok {
			return port, nil
		}
		time.Sleep(remoteReadyPoll)
	}
	return 0, lifecycleError("ready-timeout", fmt.Sprintf("Timed out waiting for the remote dashboard to announce its port (%dms).", timeout.Milliseconds()), nil)
}

// RemoteSpawn is a launched remote serve, or Existing when a concurrent connect already owns the record
type RemoteSpawn struct {
	Existing      bool
	PID           int
	SpawnNonce    string
	LogPath       string
	TokenFilePath string
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// RemoteSpawnOptions are the install, profile, token and ownership of one remote spawn
type RemoteSpawnOptions struct {
	HermesPath         string
	Profile            string
	Token              string
	OwnershipID        string
	HermesHome         string
	DevServer          string
	AssertInstallClear func() error
}

const tokenUploadTemplate = "import os,sys,stat\n" +
	"p=os.path.expanduser(%s)\n" +
	"d=os.path.dirname(p)\n" +
	"n=os.path.basename(p)\n" +
	"os.makedirs(d,mode=0o700,exist_ok=True)\n" +
	"df=os.O_RDONLY|getattr(os,\"O_DIRECTORY\",0)|getattr(os,\"O_NOFOLLOW\",0)\n" +
	"dd=os.open(d,df)\n" +
	"try:\n" +
	" s=os.fstat(dd)\n" +
	" if not stat.S_ISDIR(s.st_mode):raise SystemExit(\"unsafe token directory\")\n" +
	" if hasattr(os,\"getuid\") and s.st_uid!=os.getuid():raise SystemExit(\"token directory owner mismatch\")\n" +
	" if (s.st_mode&0o777)!=0o700:os.fchmod(dd,0o700)\n" +
	" fl=os.O_WRONLY|os.O_CREAT|os.O_EXCL|getattr(os,\"O_NOFOLLOW\",0)\n" +
	" now=__import__(\"time\").time()\n" +
	" for stale in os.listdir(dd):\n" +
	"  if stale.endswith(\".token\") and len(stale)==22:\n" +
	"   try:\n" +
	"    ss=os.stat(stale,dir_fd=dd,follow_symlinks=False)\n" +
	"    if stat.S_ISREG(ss.st_mode) and now-ss.st_mtime>3600:os.unlink(stale,dir_fd=dd)\n" +
	"   except OSError:pass\n" +
	" fd=os.open(n,fl,0o600,dir_fd=dd)\n" +
	" try:os.write(fd,sys.stdin.buffer.read())\n" +
	" except BaseException:\n" +
	"  try:os.unlink(n,dir_fd=dd)\n" +
	"  except OSError:pass\n" +
	"  raise\n" +
	" finally:os.close(fd)\n" +
	"finally:os.close(dd)"

// Uploads the spawn token owner only and launches the detached serve, dropping the token on every failure
func SpawnRemoteDashboard(ssh RemoteShell, o RemoteSpawnOptions) (RemoteSpawn, error) {
	if o.HermesHome == "" {
		o.HermesHome = "~/.icebath"
	}
	supported, err := RemoteSupportsSSHOwnership(ssh, o.HermesPath)
	if err != nil {
		return RemoteSpawn{}, err
	}
	if !supported {
		return RemoteSpawn{}, lifecycleError("update-required", "The remote Icebath install does not support --ssh-session-token-file and --ssh-owner-nonce. "+
			"Update Icebath on the remote host to continue using Desktop SSH mode.", nil)
	}
	spawnNonce := randomHex(8)
	tokenFilePath, err := SpawnTokenPath(o.OwnershipID, spawnNonce)
	if err != nil {
		return RemoteSpawn{}, err
	}
	logPath, _ := SpawnLogPath(o.OwnershipID, spawnNonce)
	dropToken := func() { _, _ = ssh.Exec("rm -f "+mustExpand(tokenFilePath), SSHExecOptions{}) }
	upload := fmt.Sprintf(tokenUploadTemplate, Shq(tokenFilePath))
	token := o.Token
	if _, err := ssh.Exec("python3 -c "+Shq(upload), SSHExecOptions{StdinData: &token}); err != nil {
		dropToken()
		return RemoteSpawn{}, err
	}
	if o.AssertInstallClear != nil {
		if err := o.AssertInstallClear(); err != nil {
			dropToken()
			return RemoteSpawn{}, err
		}
	}
	command, err := BuildSpawnCommand(o.HermesPath, o.Profile, SpawnCommandOptions{
		SpawnNonce: spawnNonce, TokenFilePath: tokenFilePath, LogPath: logPath, HermesHome: o.HermesHome, DevServer: o.DevServer,
		OwnershipID: o.OwnershipID, ReservationNonce: spawnNonce,
		LockMetadata: jsjson.NewOrderedObject(
			"ownershipId", o.OwnershipID, "spawnNonce", spawnNonce, "port", 0.0, "profile", o.Profile, "hermesPath", o.HermesPath,
			"hermesHome", o.HermesHome, "logPath", logPath, "tokenFingerprint", FingerprintToken(o.Token),
			"protocolVersion", float64(RemoteProtocolVersion), "devServer", o.DevServer, "startedAt", isoNow()),
	})
	if err != nil {
		dropToken()
		return RemoteSpawn{}, err
	}
	out, err := ssh.Exec(command, SSHExecOptions{})
	if err != nil {
		dropToken()
		return RemoteSpawn{}, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	last := ""
	if len(lines) > 0 {
		last = lines[len(lines)-1]
	}
	if last == "EXISTING" {
		return RemoteSpawn{Existing: true}, nil
	}
	pid, ok := jsParseInt(last)
	if !ok || pid <= 0 {
		dropToken()
		return RemoteSpawn{}, lifecycleError("spawn-failed", "Failed to launch the remote dashboard (no pid returned).", nil)
	}
	return RemoteSpawn{PID: pid, SpawnNonce: spawnNonce, LogPath: logPath, TokenFilePath: tokenFilePath}, nil
}

var leadingDecimal = regexp.MustCompile(`^[+-]?[0-9]+`)

// parseInt of base ten on a string, false for NaN
func jsParseInt(s string) (int, bool) {
	m := leadingDecimal.FindString(strings.TrimLeftFunc(s, jsSpace))
	if m == "" {
		return 0, false
	}
	n, err := strconv.Atoi(m)
	return n, err == nil
}

// ForwardDeps are the tunnel operations a remote connect opens and drops forwards through
type ForwardDeps struct {
	Forward       func(localPort, remotePort int) error
	CancelForward func(localPort, remotePort int) error
	PickLocalPort func() (int, error)
}

func cancelForwardSafe(d ForwardDeps, localPort, remotePort int) {
	if d.CancelForward == nil {
		return
	}
	_ = d.CancelForward(localPort, remotePort)
}

// Whether a forward failed because the picked local port was taken
func IsForwardBindCollision(err error) bool {
	return err != nil && bindCollision.MatchString(err.Error())
}

// Opens a forward to remotePort, retrying a fresh local port on a bind collision
func OpenForward(d ForwardDeps, remotePort int, attempts int) (int, error) {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		localPort, err := d.PickLocalPort()
		if err != nil {
			return 0, err
		}
		if err := d.Forward(localPort, remotePort); err != nil {
			lastErr = err
			if !IsForwardBindCollision(err) || attempt == attempts-1 {
				return 0, err
			}
			continue
		}
		return localPort, nil
	}
	return 0, lastErr
}

// Adopts the served token, refusing one whose spawn died while it was read
func AdoptOwnedServedToken(adopt func(baseURL, expected string, o backend.AdoptTokenOptions) (string, error), baseURL, expectedToken string, ssh RemoteShell, pid int, label string) (string, error) {
	token, err := adopt(baseURL, expectedToken, backend.AdoptTokenOptions{ChildAlive: func() bool { return true }, Label: label})
	if err != nil {
		return "", err
	}
	alive, err := RemotePIDAlive(ssh, pid)
	if err != nil {
		return "", err
	}
	if !alive {
		kind := "foreign-backend"
		if token == expectedToken {
			kind = "spawn-failed"
		}
		return "", lifecycleError(kind, label+" exited while its served token was being resolved.", nil)
	}
	return token, nil
}

func waitForRemoteSpawnCompletion(ssh RemoteShell, ownershipID string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lock, err := ReadLockfile(ssh, ownershipID)
		if err != nil {
			return false, err
		}
		if lock == nil {
			return false, nil
		}
		if lock.num("port") > 0 {
			return true, nil
		}
		time.Sleep(remoteReadyPoll)
	}
	return false, lifecycleError("spawn-failed", "Timed out waiting for the concurrent SSH connection to publish its backend.", nil)
}

// Whether a published record serves this connect as is, same profile, token, install, home and dev renderer
func LockServesConnect(lock *RemoteLock, profile, reuseToken, hermesPath, hermesHome, devServer string) bool {
	dev := ""
	if v := lock.get("devServer"); v != nil {
		dev, _ = v.(string)
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return lock.num("port") > 0 && lock.get("profile") == profile && reuseToken != "" &&
		lock.get("tokenFingerprint") == FingerprintToken(reuseToken) && lock.get("hermesPath") == hermesPath &&
		lock.get("hermesHome") == hermesHome && dev == devServer
}

// RemoteConnectDeps inject the opened SSH connection, tunnel, readiness and token adoption into a connect
type RemoteConnectDeps struct {
	ForwardDeps
	SSH              RemoteShell
	Platform         *RemotePlatform
	Profile          string
	RemoteHermesPath string
	OwnershipID      string
	ReuseToken       string
	WaitForHermes    func(baseURL, token string) error
	ProbeReuseProof  func(baseURL, token, spawnNonce string) (string, error)
	AdoptServedToken func(baseURL, expected string, o backend.AdoptTokenOptions) (string, error)
	RememberLog      func(string)
	DevServer        string
	Signal           context.Context
}

// RemoteConnectResult is the tunneled dashboard a connect established or reused
type RemoteConnectResult struct {
	BaseURL          string
	Token            string
	TokenFingerprint string
	RemotePort       int
	LocalPort        int
	PID              int
	Reused           bool
	Platform         RemotePlatform
	HermesPath       string
	HermesVersion    string
	OwnershipID      string
	SpawnNonce       string
	LogPath          string
	HermesHome       string
	StartedAt        string
	CreationTime     string
	CreationTimeNs   string
	ExecutablePath   string
}

// Establishes or reuses the remote dashboard of an ownership id and a tunnel to it
func RemoteConnect(d RemoteConnectDeps) (RemoteConnectResult, error) {
	profile := ""
	if p := BackendProfileArg(d.Profile); p != nil {
		profile = *p
	}
	log := func(msg string) {
		if d.RememberLog != nil {
			d.RememberLog("[ssh-lifecycle] " + msg)
		}
	}
	ssh := d.SSH
	readyTimeout := ResolveReadyTimeout(os.Getenv)
	if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
		return RemoteConnectResult{}, err
	}
	var platform RemotePlatform
	if d.Platform != nil {
		platform = *d.Platform
	} else {
		probed, err := ProbeRemotePlatform(ssh)
		if err != nil {
			return RemoteConnectResult{}, err
		}
		platform = probed
	}
	log("remote platform " + platform.OS + "/" + platform.Arch)
	hermesHome, err := ProbeRemoteHermesHome(ssh)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	if err := AssertRemoteInstallUpdateClear(ssh, hermesHome, ""); err != nil {
		return RemoteConnectResult{}, err
	}
	hermesPath, err := LocateHermes(ssh, d.RemoteHermesPath)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	log("located icebath at " + hermesPath)
	hermesVersion := ProbeHermesVersion(ssh, hermesPath)
	if hermesVersion != "" {
		log("remote icebath version: " + hermesVersion)
	}
	reuseToken := d.ReuseToken
	ownershipID := d.OwnershipID
	lock, err := ReadLockfile(ssh, ownershipID)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	if IsLockfileSkew(lock) {
		lpath, _ := LockfilePath(ownershipID)
		log("lockfile schema/ownership skew (" + lock.Reason + ") at " + lpath + " — failing closed: skipping reap, leaving remote state untouched")
		return RemoteConnectResult{}, lifecycleError("remote-lockfile-skew", "The remote ownership record "+lpath+" does not match this Icebath Desktop build ("+lock.Reason+"). "+
			"It was probably written by a different or modified desktop build sharing this remote, or the file is corrupt. "+
			"Refusing to reap or overwrite it — that could kill a live SSH backend owned by another build. "+
			"If nothing else uses this remote, delete that file on the remote host and reconnect.", nil)
	}
	if lock != nil {
		pidAlive, err := RemotePIDAlive(ssh, lock.PID())
		if err != nil {
			return RemoteConnectResult{}, err
		}
		owned := false
		if pidAlive {
			if owned, err = PIDIsOurDashboard(ssh, lock.PID(), lock.str("spawnNonce"), lock.str("hermesPath"), lock.str("hermesHome"), ownershipID, lock.str("profile")); err != nil {
				return RemoteConnectResult{}, err
			}
		}
		if pidAlive && owned && LockServesConnect(lock, profile, reuseToken, hermesPath, hermesHome, d.DevServer) {
			result, done, err := reuseRemoteDashboard(d, lock, platform, reuseToken, hermesPath, hermesHome, hermesVersion, log)
			if err != nil || done {
				return result, err
			}
		} else {
			if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
				return RemoteConnectResult{}, err
			}
			if err := AssertRemoteInstallUpdateClear(ssh, hermesHome, hermesPath); err != nil {
				return RemoteConnectResult{}, err
			}
			if err := CleanupStale(ssh, ownershipID, lock, pidAlive); err != nil {
				return RemoteConnectResult{}, err
			}
		}
	}
	if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
		return RemoteConnectResult{}, err
	}
	if err := AssertRemoteInstallUpdateClear(ssh, hermesHome, hermesPath); err != nil {
		return RemoteConnectResult{}, err
	}
	spawnToken := MintToken()
	spawned, err := SpawnRemoteDashboard(ssh, RemoteSpawnOptions{
		HermesPath: hermesPath, Profile: profile, Token: spawnToken, OwnershipID: ownershipID, HermesHome: hermesHome, DevServer: d.DevServer,
		AssertInstallClear: func() error { return AssertRemoteInstallUpdateClear(ssh, hermesHome, hermesPath) },
	})
	if err != nil {
		return RemoteConnectResult{}, err
	}
	if spawned.Existing {
		if reuseToken == "" {
			return RemoteConnectResult{}, lifecycleError("remote-ownership-contended", "Another SSH connection owns this remote dashboard; a session token is required to reuse it.", nil)
		}
		if _, err := waitForRemoteSpawnCompletion(ssh, ownershipID, readyTimeout); err != nil {
			return RemoteConnectResult{}, err
		}
		return RemoteConnect(d)
	}
	return completeRemoteSpawn(d, spawned, spawnToken, profile, platform, hermesPath, hermesHome, hermesVersion, readyTimeout, log)
}

// Reuses an owned published dashboard through a fresh forward, done false when the proof was stale and cleaned
func reuseRemoteDashboard(d RemoteConnectDeps, lock *RemoteLock, platform RemotePlatform, reuseToken, hermesPath, hermesHome, hermesVersion string, log func(string)) (RemoteConnectResult, bool, error) {
	ssh, ownershipID := d.SSH, d.OwnershipID
	creationTime := lock.str("creationTime")
	if creationTime == "" {
		creationTime = RemoteProcessCreationTime(ssh, lock.PID())
	}
	if creationTime != "" && lock.str("creationTime") == "" {
		updated := lock.Object.Clone()
		updated.Set("creationTime", creationTime)
		if err := WriteLockfile(ssh, ownershipID, updated); err != nil {
			return RemoteConnectResult{}, false, err
		}
		lock.Object.Set("creationTime", creationTime)
	}
	if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
		return RemoteConnectResult{}, false, err
	}
	if err := AssertRemoteInstallUpdateClear(ssh, hermesHome, hermesPath); err != nil {
		return RemoteConnectResult{}, false, err
	}
	localPort, err := OpenForward(d.ForwardDeps, lock.Port(), 3)
	if err != nil {
		return RemoteConnectResult{}, false, err
	}
	fail := func(err error) (RemoteConnectResult, bool, error) {
		cancelForwardSafe(d.ForwardDeps, localPort, lock.Port())
		return RemoteConnectResult{}, false, err
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	classification, err := d.ProbeReuseProof(baseURL, reuseToken, lock.str("spawnNonce"))
	if err != nil {
		return fail(lifecycleError("transient-transport-error", "Could not verify the existing SSH backend.", err))
	}
	switch classification {
	case "authenticated-stale":
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return fail(err)
		}
		cancelForwardSafe(d.ForwardDeps, localPort, lock.Port())
		if err := AssertRemoteInstallUpdateClear(ssh, hermesHome, hermesPath); err != nil {
			return fail(err)
		}
		if err := CleanupStale(ssh, ownershipID, lock, true); err != nil {
			return fail(err)
		}
		return RemoteConnectResult{}, false, nil
	case "authenticated-ok":
		token, err := AdoptOwnedServedToken(d.AdoptServedToken, baseURL, reuseToken, ssh, lock.PID(), "reused remote dashboard")
		if err != nil {
			return fail(err)
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return fail(err)
		}
		log(fmt.Sprintf("reusing remote dashboard pid=%d port=%d", lock.PID(), lock.Port()))
		return RemoteConnectResult{
			BaseURL: baseURL, Token: token, TokenFingerprint: FingerprintToken(token), RemotePort: lock.Port(), LocalPort: localPort,
			PID: lock.PID(), Reused: true, Platform: platform, HermesPath: hermesPath, HermesVersion: hermesVersion, OwnershipID: ownershipID,
			SpawnNonce: lock.str("spawnNonce"), LogPath: lock.str("logPath"), HermesHome: hermesHome, StartedAt: lock.str("startedAt"),
			CreationTime: lock.str("creationTime"),
		}, true, nil
	}
	return fail(lifecycleError("transient-transport-error", "SSH reuse proof returned an invalid classification.", nil))
}

// Publishes the spawn record, waits for its port, forward and readiness, reaping the spawn on any failure
func completeRemoteSpawn(d RemoteConnectDeps, spawned RemoteSpawn, spawnToken, profile string, platform RemotePlatform, hermesPath, hermesHome, hermesVersion string, readyTimeout time.Duration, log func(string)) (RemoteConnectResult, error) {
	ssh, ownershipID := d.SSH, d.OwnershipID
	pid := spawned.PID
	log(fmt.Sprintf("spawned remote dashboard pid=%d", pid))
	creationTime := RemoteProcessCreationTime(ssh, pid)
	startedAt := isoNow()
	ownedSpawn := jsjson.NewOrderedObject(
		"ownershipId", ownershipID, "spawnNonce", spawned.SpawnNonce, "pid", float64(pid), "port", 0.0, "profile", profile,
		"hermesPath", hermesPath, "hermesHome", hermesHome, "logPath", spawned.LogPath, "tokenFingerprint", FingerprintToken(spawnToken),
		"protocolVersion", float64(RemoteProtocolVersion), "devServer", d.DevServer, "startedAt", startedAt)
	if creationTime != "" {
		ownedSpawn.Set("creationTime", creationTime)
	}
	localPort, remotePort := 0, 0
	result, err := func() (RemoteConnectResult, error) {
		if err := WriteLockfile(ssh, ownershipID, ownedSpawn); err != nil {
			return RemoteConnectResult{}, err
		}
		port, err := ScrapeReadyPort(ssh, spawned.LogPath, readyTimeout, nil, d.Signal)
		if err != nil {
			return RemoteConnectResult{}, err
		}
		remotePort = port
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		log(fmt.Sprintf("remote dashboard bound port %d", remotePort))
		if localPort, err = OpenForward(d.ForwardDeps, remotePort, 3); err != nil {
			return RemoteConnectResult{}, err
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
		if err := d.WaitForHermes(baseURL, spawnToken); err != nil {
			return RemoteConnectResult{}, err
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		token, err := AdoptOwnedServedToken(d.AdoptServedToken, baseURL, spawnToken, ssh, pid, "remote dashboard")
		if err != nil {
			return RemoteConnectResult{}, err
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		fingerprint := FingerprintToken(token)
		published := ownedSpawn.Clone()
		published.Set("port", float64(remotePort))
		published.Set("tokenFingerprint", fingerprint)
		if err := WriteLockfile(ssh, ownershipID, published); err != nil {
			return RemoteConnectResult{}, err
		}
		if err := AssertBootstrapNotSuperseded(d.Signal); err != nil {
			return RemoteConnectResult{}, err
		}
		return RemoteConnectResult{
			BaseURL: baseURL, Token: token, TokenFingerprint: fingerprint, RemotePort: remotePort, LocalPort: localPort, PID: pid,
			Platform: platform, HermesPath: hermesPath, HermesVersion: hermesVersion, OwnershipID: ownershipID, SpawnNonce: spawned.SpawnNonce,
			LogPath: spawned.LogPath, HermesHome: hermesHome, StartedAt: startedAt, CreationTime: creationTime,
		}, nil
	}()
	if err == nil {
		return result, nil
	}
	if localPort != 0 && remotePort != 0 {
		cancelForwardSafe(d.ForwardDeps, localPort, remotePort)
	}
	_, _ = ssh.Exec("rm -f "+mustExpand(spawned.TokenFilePath), SSHExecOptions{})
	pidAlive, aliveErr := RemotePIDAlive(ssh, pid)
	if aliveErr != nil {
		pidAlive = true
	}
	if cleanupErr := CleanupStale(ssh, ownershipID, &RemoteLock{Object: ownedSpawn}, pidAlive); cleanupErr != nil {
		if se, ok := err.(*SSHError); ok {
			se.CleanupCause = cleanupErr
		} else {
			err = &SSHError{Message: err.Error(), ErrKind: errKind(err), Cause: err, CleanupCause: cleanupErr}
		}
	}
	return RemoteConnectResult{}, err
}
