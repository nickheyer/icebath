package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Largest file the probe reads, anything bigger counts as unreadable
const installMaxFileBytes = 1 << 20

// Why the machine counts as used, for logs and tests and never shown to users
type InstallEvidence string

// Each reason a machine counts as used, in the order a root is checked after the marker
const (
	EvidenceMachineMarker  InstallEvidence = "machine-marker"
	EvidenceOnboardingDone InstallEvidence = "onboarding-done"
	EvidenceNamedProfile   InstallEvidence = "named-profile"
	EvidenceProviderConfig InstallEvidence = "provider-config"
	EvidenceProviderKey    InstallEvidence = "provider-key"
	EvidenceProviderLogin  InstallEvidence = "provider-login"
	EvidenceMessages       InstallEvidence = "messages"
	EvidenceUnreadable     InstallEvidence = "unreadable"
)

// One piece of evidence and the root or marker it was found at
type InstallFinding struct {
	Evidence InstallEvidence
	Where    string
}

// Probe answer, existing when it holds any finding
type InstallState struct {
	Findings []InstallFinding
}

// Reports that this OS user already used icebath or hermes on this computer
func (s InstallState) Existing() bool { return len(s.Findings) > 0 }

// Reports that no install was found for this OS user
func (s InstallState) Fresh() bool { return len(s.Findings) == 0 }

// Lazily yields candidate roots, stopping as soon as the probe has its answer
type InstallSource func(yield func(string) bool)

// Where to look, the first marker being the one this process writes
type InstallPlaces struct {
	Markers   []string
	Sources   []InstallSource
	Protected []string
}

// Places of the real machine, markers and protected folders expanded now and sources run when read
func installPlacesOfThisMachine() InstallPlaces {
	return InstallPlaces{
		Markers:   slices.Collect(iter.Seq[string](installExpandAll(machineMarkerFiles()))),
		Sources:   []InstallSource{installActiveRoot, installHomeGlobs, installSharedAuthDir, installRegistryHome, installServiceHomes, installLauncherRoot},
		Protected: installProtectedDirs(),
	}
}

// Seam tests replace, production always looks at the real machine
var machinePlaces = installPlacesOfThisMachine

var (
	installProbeMu     sync.Mutex
	installProbeCached *InstallState
)

// Answers once per process whether this OS user used icebath here, the first finding winning
func InstallStateProbe(ctx context.Context) InstallState {
	installProbeMu.Lock()
	if cached := installProbeCached; cached != nil {
		installProbeMu.Unlock()
		return *cached
	}
	installProbeMu.Unlock()
	state, settled := installProbeCompute(ctx)
	if settled {
		installProbeMu.Lock()
		installProbeCached = &state
		installProbeMu.Unlock()
	}
	return state
}

// Drops the cached probe answer so the next probe looks again
func InstallStateProbeClear() {
	installProbeMu.Lock()
	installProbeCached = nil
	installProbeMu.Unlock()
}

// Looks at every marker then every candidate root, marking the machine on proven use, unsettled when marking failed
func installProbeCompute(ctx context.Context) (InstallState, bool) {
	places := machinePlaces()
	for _, marker := range places.Markers {
		if installIsFile(marker) {
			return InstallState{Findings: []InstallFinding{{Evidence: EvidenceMachineMarker, Where: marker}}}, true
		}
	}
	for root := range installCandidateRoots(places) {
		if finding, ok := installRootEvidence(root, places); ok {
			state := InstallState{Findings: []InstallFinding{finding}}
			if finding.Evidence != EvidenceUnreadable {
				if err := installWriteMarker(ctx, nil, false); err != nil {
					toolsLogger(nil).Warn("install probe could not write the machine marker, the next probe retries", "error", err)
					return state, false
				}
			}
			return state, true
		}
	}
	return InstallState{}, true
}

// Reports whether the probe found any use of icebath for this OS user
func ExistingInstall(ctx context.Context) bool { return InstallStateProbe(ctx).Existing() }

// Records a used install or a claimed trial, replacing any reservation
func MarkMachineUsed(ctx context.Context) error { return installWriteMarker(ctx, nil, true) }

// Covers a claim's checks, its mint under its own auth lock and HTTP timeouts, and the marker write
const ClaimLockTimeout = 30 * time.Second

// Reentrancy state of the claim lock, owned by one goroutine at a time
var installClaimHolder fileLockHolder

// Welcome trial claim lock another holder kept past the timeout
type ClaimLockTimeoutError struct{ Message string }

func (e *ClaimLockTimeoutError) Error() string { return e.Message }

// Takes the OS user's welcome trial claim lock beside the marker it guards and returns its release
func ClaimLock() (func(), error) {
	places := machinePlaces()
	if len(places.Markers) == 0 {
		return func() {}, nil
	}
	lockPath := places.Markers[0] + ".lock"
	return acquireFileLock(lockPath, &installClaimHolder, ClaimLockTimeout,
		"Timed out waiting for the welcome-trial claim lock ("+lockPath+")")
}

// Keeps the trial for a fresh install found on its first desktop boot
func ReserveInstallTrial(ctx context.Context) error {
	return installWriteMarker(ctx, map[string]string{"trial": "reserved"}, true)
}

// Reports whether any marker holds a trial reservation
func TrialReserved() bool {
	for _, marker := range machinePlaces().Markers {
		if obj, ok := installReadMarker(marker).(*pyjson.Object); ok && len(obj.Keys) == 1 {
			if v, _ := obj.Get("trial"); v == "reserved" {
				return true
			}
		}
	}
	return false
}

// Writes the marker atomically under a unique temp name, then drops the cached probe and the onboarding answer
func installWriteMarker(ctx context.Context, content map[string]string, answerMoved bool) error {
	places := machinePlaces()
	if len(places.Markers) > 0 {
		marker := places.Markers[0]
		if err := os.MkdirAll(filepath.Dir(marker), 0o777); err != nil {
			return err
		}
		if content == nil {
			content = map[string]string{}
		}
		if err := writeApprovalJSONAtomic(ctx, marker, content); err != nil {
			return err
		}
	}
	InstallStateProbeClear()
	if answerMoved {
		OnboardingDueInvalidate()
	}
	return nil
}

// Parsed marker content, nil when it is absent, unreadable or not JSON
func installReadMarker(marker string) any {
	if !installIsFile(marker) {
		return nil
	}
	text, err := installReadText(marker)
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	return v
}

// Reentrant holder of a cross process file lock, keyed by the goroutine that took it
type fileLockHolder struct {
	mu    sync.Mutex
	owner uint64
	depth int
}

// Reentrant cross process file lock holder
type FileLockHolder = fileLockHolder

// Takes an advisory lock file within the timeout, reentrant for the holding goroutine, stamping the holder pid
func acquireFileLock(lockPath string, holder *fileLockHolder, timeout time.Duration, timeoutMessage string) (func(), error) {
	gid := installGoroutineID()
	holder.mu.Lock()
	if holder.depth > 0 && holder.owner == gid {
		holder.depth++
		holder.mu.Unlock()
		return func() {
			holder.mu.Lock()
			holder.depth--
			holder.mu.Unlock()
		}, nil
	}
	holder.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return nil, err
	}
	flags := os.O_RDWR | os.O_CREATE | os.O_APPEND
	if isWindowsHost {
		if st, err := os.Stat(lockPath); err != nil || st.Size() == 0 {
			_ = os.WriteFile(lockPath, []byte(" "), 0o666)
		}
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(lockPath, flags, 0o666)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(max(time.Second, timeout))
	for {
		locked, err := checkpointTryLock(f)
		if err != nil && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EDEADLK) {
			f.Close()
			return nil, err
		}
		if locked {
			break
		}
		if !time.Now().Before(deadline) {
			f.Close()
			if hint := lockHolderHintText(lockPath); hint != "" {
				return nil, &ClaimLockTimeoutError{Message: timeoutMessage + "; " + hint}
			}
			return nil, &ClaimLockTimeoutError{Message: timeoutMessage}
		}
		time.Sleep(50 * time.Millisecond)
	}
	holder.mu.Lock()
	holder.owner, holder.depth = gid, 1
	holder.mu.Unlock()
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Sync()
	}
	return func() {
		holder.mu.Lock()
		holder.depth = 0
		holder.mu.Unlock()
		if err := f.Truncate(0); err == nil && isWindowsHost {
			_, _ = f.Write([]byte(" "))
		}
		_ = f.Sync()
		f.Close()
	}, nil
}

// Takes the advisory lock file within the timeout and returns its release
func AcquireFileLock(lockPath string, holder *FileLockHolder, timeout time.Duration, msg string) (func(), error) {
	return acquireFileLock(lockPath, holder, timeout, msg)
}

// Live holder hint from the pid a holder stamped into the lock file, empty when there is none
func lockHolderHintText(lockPath string) string {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	raw = []byte(strings.TrimPrefix(decodeUTF8Replace(raw), "\ufeff"))
	fields := strings.FieldsFunc(string(raw), pyIsSpace)
	if len(fields) == 0 {
		return ""
	}
	pid, ok := pyIntParse(fields[0])
	if !ok || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	if !isWindowsHost && !syncPidExists(pid) {
		return ""
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}

// Numeric id of the calling goroutine read from its stack header
func installGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

// Each named root once, existing directories only, a root inside a protected folder yielded unresolved
func installCandidateRoots(places InstallPlaces) iter.Seq[string] {
	return func(yield func(string) bool) {
		var seen []string
		for _, source := range places.Sources {
			stop := false
			source(func(raw string) bool {
				p, err := pyAbspath(osPathExpandUser(raw))
				if err != nil {
					return true
				}
				if !installInside(p, places.Protected) {
					p = installResolve(p)
					if st, err := os.Stat(p); err != nil || !st.IsDir() {
						return true
					}
				}
				for _, s := range seen {
					if pathlibEqual(s, p) {
						return true
					}
				}
				seen = append(seen, p)
				if !yield(p) {
					stop = true
					return false
				}
				return true
			})
			if stop {
				return
			}
		}
	}
}

// Reports whether a path is one of the folders or lies beneath one, compared the way pathlib does
func installInside(p string, dirs []string) bool {
	for _, d := range dirs {
		if pathlibEqual(p, d) || pathlibIsAncestor(d, p) {
			return true
		}
	}
	return false
}

// Folders whose contents the probe must never open
func installProtectedDirs() []string {
	return slices.Collect(iter.Seq[string](installExpandAll(macPrivacyProtectedDirs())))
}

// Patterns with environment references then a tilde expanded and globbed, dropping any naming an unset variable
func installExpandAll(patterns []string) InstallSource {
	return func(yield func(string) bool) {
		for _, pattern := range patterns {
			expanded := osPathExpandUser(osPathExpandVars(pattern))
			if strings.ContainsAny(expanded, "$%") {
				continue
			}
			if strings.ContainsAny(expanded, "*?[") {
				hits := pyGlob(expanded)
				sort.Strings(hits)
				for _, hit := range hits {
					if !yield(pathlibStr(hit)) {
						return
					}
				}
				continue
			}
			if !yield(pathlibStr(expanded)) {
				return
			}
		}
	}
}

// Root of the active profile tree and the platform default root
func installActiveRoot(yield func(string) bool) {
	if yield(home.Root()) {
		yield(home.PlatformDefault())
	}
}

// Every default place a data root can sit
func installHomeGlobs(yield func(string) bool) {
	installExpandAll(icebathHomeGlobs())(yield)
}

// The shared auth directory override, icebath first and the older hermes variable after
func installSharedAuthDir(yield func(string) bool) {
	for _, name := range []string{"ICEBATH_SHARED_AUTH_DIR", "HERMES_SHARED_AUTH_DIR"} {
		if raw := strings.TrimFunc(os.Getenv(name), pyIsSpace); raw != "" {
			if !yield(raw) {
				return
			}
		}
	}
}

// Home of every gateway service this OS user runs, its own definitions, machine wide units and Scheduled Tasks
func installServiceHomes(yield func(string) bool) {
	tables := []struct {
		machineWide bool
		globs       []string
	}{{false, gatewayServiceDefinitionGlobs()}, {true, systemGatewayServiceDefinitionGlobs()}}
	for _, table := range tables {
		stop := false
		installExpandAll(table.globs)(func(definition string) bool {
			if !installDefinitionHomes(definition, table.machineWide, true, yield) {
				stop = true
				return false
			}
			return true
		})
		if stop {
			return
		}
	}
	for _, launcher := range installTaskLaunchers() {
		if !installDefinitionHomes(launcher, false, false, yield) {
			return
		}
	}
}

// Seam tests replace to name the Scheduled Task launchers
var installTaskLaunchers = registeredTaskLaunchers

// Homes one definition names and those of the launchers it starts, false when the consumer stopped
func installDefinitionHomes(definition string, machineWide, follow bool, yield func(string) bool) bool {
	raw, err := installReadBytes(definition)
	if err != nil {
		return true
	}
	text, err := launcherText(raw)
	if err != nil {
		return true
	}
	account, named := installUnitAccount(text)
	if !named && machineWide {
		account, named = "root", true
	}
	if named && !installIsThisAccount(account) {
		return true
	}
	for _, h := range serviceDefinitionHomes(text) {
		if !yield(strings.Trim(strings.TrimFunc(h, pyIsSpace), `"`)) {
			return false
		}
	}
	if follow {
		for _, launcher := range serviceDefinitionLaunchers(text) {
			if !installDefinitionHomes(launcher, false, false, yield) {
				return false
			}
		}
	}
	return true
}

var unitUserRe = regexp.MustCompile(`(?m)^` + pyWSClass + `*User=` + pyWSClass + `*([^` + pyWSClass[1:len(pyWSClass)-1] + `]+)` + pyWSClass + `*$`)

// Account a systemd unit runs as from its last User line, false when it names none
func installUnitAccount(unit string) (string, bool) {
	m := unitUserRe.FindAllStringSubmatch(unit, -1)
	if len(m) == 0 {
		return "", false
	}
	return m[len(m)-1][1], true
}

// Reports whether a unit account is this process's user, an unknown account running as no one here
func installIsThisAccount(account string) bool {
	digits := account != ""
	for _, r := range account {
		if !unicode.IsDigit(r) {
			digits = false
			break
		}
	}
	if digits {
		uid, ok := pyDecimalValue(account)
		return ok && uid == os.Getuid()
	}
	u, err := user.Lookup(account)
	if err != nil {
		return false
	}
	uid, err := strconv.Atoi(u.Uid)
	return err == nil && uid == os.Getuid()
}

// Integer value of a string of Unicode decimal digits the way int() reads it, false past any uid
func pyDecimalValue(s string) (int, bool) {
	n := 0
	for _, r := range s {
		start := r
		for unicode.IsDigit(start - 1) {
			start--
		}
		n = n*10 + int(r-start)%10
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

// Seams tests replace to find the launcher on PATH and read its interpreter
var (
	installLocateCommand  = locateCommand
	installLauncherPython = launcherPython
)

// Roots owning the icebath and hermes launchers on PATH, each launcher starting its root's tools interpreter
func installLauncherRoot(yield func(string) bool) {
	for _, launcherName := range [][2]string{{"icebath", ".icebath"}, {"hermes", ".hermes"}} {
		if root, ok := installLauncherOwner(launcherName[0], launcherName[1]); ok {
			if !yield(root) {
				return
			}
		}
	}
}

// Root owning one launcher on PATH, its folder when it sits in a protected place, false when unknown
func installLauncherOwner(command, dotDir string) (string, bool) {
	found := installLocateCommand(command, nil, nil).Command()
	if len(found) == 0 {
		return "", false
	}
	protected := installProtectedDirs()
	launcher, err := pyAbspath(found[0])
	if err != nil {
		return "", false
	}
	if !installInside(launcher, protected) {
		launcher = installResolve(launcher)
	}
	if installInside(launcher, protected) {
		return pathlibParent(launcher), true
	}
	if python, ok := installLauncherPython(launcher); ok {
		for _, p := range pathlibParents(python) {
			if pathlibName(p) == "tools" {
				return pathlibParent(p), true
			}
		}
	}
	bin := pathlibParent(launcher)
	if pathlibName(bin) == "bin" && pathlibName(pathlibParent(bin)) == dotDir {
		return pathlibParent(pathlibParent(pathlibParent(bin))), true
	}
	return "", false
}

// One root's files, each read at most once, never through the config loader that writes into the root
type installRoot struct {
	path       string
	protected  []string
	config     map[string]any
	configErr  error
	configRead bool
	auth       []any
	authErr    error
	authRead   bool
}

// A file of the root resolved before anything opens it, an error when it leads into a protected folder
func (r *installRoot) file(name string) (string, error) {
	return installUnprotected(filepath.Join(r.path, name), r.protected)
}

// The root's config.yaml mapping, empty when absent or not a mapping
func (r *installRoot) configMap() (map[string]any, error) {
	if !r.configRead {
		r.configRead = true
		p, err := r.file("config.yaml")
		if err == nil {
			var doc any
			doc, err = installReadMapping(p, hermesYAMLSafeLoad)
			r.config, _ = doc.(map[string]any)
		}
		r.configErr = err
	}
	return r.config, r.configErr
}

// The parsed auth stores of the root, the shared auth override naming the directory itself
func (r *installRoot) authStores() ([]any, error) {
	if !r.authRead {
		r.authRead = true
		for _, name := range []string{"auth.json", "shared/nous_auth.json", "nous_auth.json"} {
			p, err := r.file(name)
			if err != nil {
				r.authErr = err
				break
			}
			store, err := installReadMapping(p, pyjson.Loads)
			if err != nil {
				r.authErr = err
				break
			}
			r.auth = append(r.auth, store)
		}
	}
	return r.auth, r.authErr
}

// A path resolved, refused when it lands inside a protected folder because opening it raises a privacy prompt
func installUnprotected(p string, protected []string) (string, error) {
	resolved := installResolve(p)
	if installInside(resolved, protected) {
		return "", &fs.PathError{Op: "open", Path: p, Err: errors.New("leads into a protected folder")}
	}
	return resolved, nil
}

// One check of a root, true when the root shows use and an error when it cannot be read
type installCheck struct {
	evidence InstallEvidence
	check    func(*installRoot) (bool, error)
}

// Root checks in order, messages last as the only one that opens a database
var installChecks = []installCheck{
	{EvidenceOnboardingDone, installOnboardingDone},
	{EvidenceNamedProfile, installNamedProfile},
	{EvidenceProviderConfig, installProviderConfig},
	{EvidenceProviderKey, installProviderKey},
	{EvidenceProviderLogin, installProviderLogin},
	{EvidenceMessages, installMessages},
}

// First evidence of use in a root, unreadable when protected or when a check cannot read it
func installRootEvidence(root string, places InstallPlaces) (InstallFinding, bool) {
	if installInside(root, places.Protected) {
		return InstallFinding{Evidence: EvidenceUnreadable, Where: root}, true
	}
	r := &installRoot{path: root, protected: places.Protected}
	for _, c := range installChecks {
		used, err := c.check(r)
		if err != nil {
			return InstallFinding{Evidence: EvidenceUnreadable, Where: root}, true
		}
		if used {
			return InstallFinding{Evidence: c.evidence, Where: root}, true
		}
	}
	return InstallFinding{}, false
}

// Mapping value under a string key of a loaded YAML or JSON mapping, false when absent or not a mapping
func installMapGet(m any, key string) (any, bool) {
	switch t := m.(type) {
	case map[string]any:
		v, ok := t[key]
		return v, ok
	case *pyjson.Object:
		return t.Get(key)
	}
	return nil, false
}

// Reports whether a loaded value is a mapping the way isinstance(value, dict) does
func installIsMapping(v any) bool {
	switch v.(type) {
	case map[string]any, *pyjson.Object:
		return true
	}
	return false
}

// A root's config sets onboarding.run to false
func installOnboardingDone(r *installRoot) (bool, error) {
	cfg, err := r.configMap()
	if err != nil {
		return false, err
	}
	section, _ := installMapGet(cfg, "onboarding")
	if !installIsMapping(section) {
		return false, nil
	}
	run, _ := installMapGet(section, "run")
	b, ok := run.(bool)
	return ok && !b, nil
}

// A live named profile someone created under the root
func installNamedProfile(r *installRoot) (bool, error) {
	profiles, err := r.file(home.ProfilesDir)
	if err != nil {
		return false, err
	}
	if st, err := os.Stat(profiles); err != nil || !st.IsDir() {
		return false, nil
	}
	dir, err := os.Open(profiles)
	if err != nil {
		return false, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "default" || !home.ProfileIDRe.MatchString(strings.TrimSuffix(name, "\n")) {
			continue
		}
		entry, err := installUnprotected(filepath.Join(profiles, name), r.protected)
		if err != nil {
			return false, err
		}
		if st, err := os.Stat(entry); err == nil && st.IsDir() && home.ProfileHasIdentity(entry) && !home.ProfileDeleted(entry) {
			return true, nil
		}
	}
	return false, nil
}

// The root's config picks a provider other than auto
func installProviderConfig(r *installRoot) (bool, error) {
	cfg, err := r.configMap()
	if err != nil {
		return false, err
	}
	model, _ := installMapGet(cfg, "model")
	var provider any
	if installIsMapping(model) {
		provider, _ = installMapGet(model, "provider")
	}
	s, ok := provider.(string)
	if !ok {
		return false, nil
	}
	s = strings.TrimFunc(s, pyIsSpace)
	return s != "" && s != "auto", nil
}

// Deployment secrets that are not something a user entered
var installIgnoredEnvKeys = map[string]bool{"ICEBATH_ANON_API_SECRET": true, "HERMES_ANON_API_SECRET": true}

// Registry categories of keys a user enters
var installUserEnvCategories = map[string]bool{"provider": true, "tool": true, "messaging": true, "skill": true}

// The root's .env holds a provider, tool, messaging or skill key with a value
func installProviderKey(r *installRoot) (bool, error) {
	p, err := r.file(".env")
	if err != nil {
		return false, err
	}
	if !installIsFile(p) {
		return false, nil
	}
	if _, err := installReadText(p); err != nil {
		return false, err
	}
	for key, value := range config.LoadEnvFile(p) {
		if value == "" || installIgnoredEnvKeys[key] {
			continue
		}
		spec, ok := config.OptionalEnvVars.Get(key)
		if !ok && strings.HasPrefix(key, "HERMES_") {
			spec, ok = config.OptionalEnvVars.Get("ICEBATH_" + strings.TrimPrefix(key, "HERMES_"))
		}
		if ok && installUserEnvCategories[spec.Category] {
			return true, nil
		}
	}
	return false, nil
}

// A login, an active provider, a guest, or a credential the user added by hand in any auth store
func installProviderLogin(r *installRoot) (bool, error) {
	stores, err := r.authStores()
	if err != nil {
		return false, err
	}
	for _, store := range stores {
		active, _ := installMapGet(store, "active_provider")
		if pyTruthy(active) {
			return true, nil
		}
		if providers, ok := store.(*pyjson.Object); ok {
			if inner, _ := providers.Get("providers"); inner != nil {
				if obj, ok := inner.(*pyjson.Object); ok {
					for _, k := range obj.Keys {
						if pyTruthy(obj.Vals[k]) {
							return true, nil
						}
					}
				}
			}
		}
		for _, key := range []string{"auth_method", "anon_token", "refresh_token"} {
			if v, _ := installMapGet(store, key); pyTruthy(v) {
				return true, nil
			}
		}
		pool, _ := installMapGet(store, "credential_pool")
		obj, ok := pool.(*pyjson.Object)
		if !ok {
			continue
		}
		for _, k := range obj.Keys {
			rows, ok := obj.Vals[k].([]any)
			if !ok {
				continue
			}
			for _, row := range rows {
				if !installIsMapping(row) {
					continue
				}
				source, present := installMapGet(row, "source")
				if !present {
					source = ""
				}
				if s, ok := source.(string); ok && strings.SplitN(s, ":", 2)[0] == "manual" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// How long the read only connection waits on another writer's lock before giving up
var installDBBusy = time.Second

// The root's state.db holds a message, a lock another process still holds counting as none
func installMessages(r *installRoot) (bool, error) {
	p, err := r.file("state.db")
	if err != nil {
		return false, err
	}
	if !installIsFile(p) {
		return false, nil
	}
	db, err := sql.Open("sqlite", installSQLiteURI(p)+"?mode=ro&_pragma=busy_timeout("+strconv.FormatInt(installDBBusy.Milliseconds(), 10)+")")
	if err != nil {
		return installBusyIsNoMessage(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var one int
	err = db.QueryRow("SELECT 1 FROM sqlite_master WHERE type='table' AND name='messages'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return installBusyIsNoMessage(err)
	}
	err = db.QueryRow("SELECT 1 FROM messages LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return installBusyIsNoMessage(err)
	}
	return true, nil
}

// A database still busy or locked after the wait is a first boot creating it, never a message
func installBusyIsNoMessage(err error) (bool, error) {
	var se *sqlite.Error
	if errors.As(err, &se) && (se.Code() == sqlite3.SQLITE_BUSY || se.Code() == sqlite3.SQLITE_LOCKED) {
		return false, nil
	}
	return false, err
}

// File URI of an absolute path the way pathlib as_uri spells it
func installSQLiteURI(p string) string {
	if !isWindowsHost {
		return "file://" + fileURIPath(p)
	}
	drive, _, _ := ntSplitRoot(p)
	slashed := strings.ReplaceAll(p, `\`, "/")
	if len(drive) == 2 && drive[1] == ':' {
		return "file:///" + drive + fileURIPath(slashed[2:])
	}
	if drive != "" {
		return "file:" + fileURIPath(slashed)
	}
	return "file://" + fileURIPath(slashed)
}

// Reports a regular file, following links, false on any stat failure
func installIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Bytes of a small file, an error when it is larger than the probe reads
func installReadBytes(p string) ([]byte, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > installMaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, installMaxFileBytes)
	}
	return os.ReadFile(p)
}

// Text of a small file as UTF-8 with an optional byte order mark, an error when not UTF-8
func installReadText(p string) (string, error) {
	raw, err := installReadBytes(p)
	if err != nil {
		return "", err
	}
	text, ok := decodeUTF8Sig(raw)
	if !ok {
		return "", fmt.Errorf("%s is not valid UTF-8", p)
	}
	return text, nil
}

// A parsed mapping file, empty when absent or not a mapping
func installReadMapping(p string, parse func(string) (any, error)) (any, error) {
	if !installIsFile(p) {
		return map[string]any{}, nil
	}
	text, err := installReadText(p)
	if err != nil {
		return nil, err
	}
	data, err := parse(text)
	if err != nil {
		return nil, err
	}
	if installIsMapping(data) {
		return data, nil
	}
	return map[string]any{}, nil
}

// Matches of a shell pattern the way glob.glob finds them, hidden names only for patterns that start with a dot
func pyGlob(pattern string) []string {
	var out []string
	pyIGlob(pattern, false, func(p string) { out = append(out, p) })
	return out
}

var (
	globMagicRe     = regexp.MustCompile(`[*?[]`)
	fnmatchSetOpsRe = regexp.MustCompile(`([&~|])`)
)

// One level of the glob walk, yielding matches of the pattern and only directories when asked
func pyIGlob(pattern string, dirOnly bool, yield func(string)) {
	dirname, basename := osPathSplit(pattern)
	if !globMagicRe.MatchString(pattern) {
		if basename != "" {
			if _, err := os.Lstat(pattern); err == nil {
				yield(pattern)
			}
		} else if st, err := os.Stat(dirname); err == nil && st.IsDir() {
			yield(pattern)
		}
		return
	}
	if dirname == "" {
		for _, name := range pyGlob1("", basename, dirOnly) {
			yield(name)
		}
		return
	}
	var dirs []string
	if dirname != pattern && globMagicRe.MatchString(dirname) {
		pyIGlob(dirname, true, func(d string) { dirs = append(dirs, d) })
	} else {
		dirs = []string{dirname}
	}
	for _, d := range dirs {
		var names []string
		if globMagicRe.MatchString(basename) {
			names = pyGlob1(d, basename, dirOnly)
		} else if basename != "" {
			if _, err := os.Lstat(osPathJoin(d, basename)); err == nil {
				names = []string{basename}
			}
		} else if st, err := os.Stat(d); err == nil && st.IsDir() {
			names = []string{basename}
		}
		for _, name := range names {
			yield(osPathJoin(d, name))
		}
	}
}

// Names in a directory matching one pattern component, hidden names skipped unless the pattern starts with a dot
func pyGlob1(dirname, pattern string, dirOnly bool) []string {
	open := dirname
	if open == "" {
		open = "."
	}
	dir, err := os.Open(open)
	if err != nil {
		return nil
	}
	entries, _ := dir.ReadDir(-1)
	dir.Close()
	re, err := regexp.Compile(pyFnmatchRegex(osPathNormcase(pattern)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if dirOnly {
			st, err := os.Stat(filepath.Join(open, name))
			if err != nil || !st.IsDir() {
				continue
			}
		}
		if !strings.HasPrefix(pattern, ".") && strings.HasPrefix(name, ".") {
			continue
		}
		if re.MatchString(osPathNormcase(name)) {
			out = append(out, name)
		}
	}
	return out
}

// Anchored regex of a shell pattern built the way fnmatch.translate does for CPython 3.14
func pyFnmatchRegex(pat string) string {
	runes := []rune(pat)
	var b strings.Builder
	b.WriteString(`\A(?s:`)
	i, n := 0, len(runes)
	for i < n {
		c := runes[i]
		i++
		switch c {
		case '*':
			b.WriteString(".*")
			for i < n && runes[i] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < n && runes[j] == '!' {
				j++
			}
			if j < n && runes[j] == ']' {
				j++
			}
			for j < n && runes[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
				continue
			}
			var stuff string
			escape := func(s string) string {
				s = strings.ReplaceAll(s, `\`, `\\`)
				return strings.ReplaceAll(s, "[", `\[`)
			}
			if !slices.Contains(runes[i:j], '-') {
				stuff = escape(string(runes[i:j]))
			} else {
				var chunks []string
				k := i + 1
				if runes[i] == '!' {
					k = i + 2
				}
				for {
					at := -1
					for x := k; x < j; x++ {
						if runes[x] == '-' {
							at = x
							break
						}
					}
					if at < 0 {
						break
					}
					chunks = append(chunks, string(runes[i:at]))
					i = at + 1
					k = at + 3
				}
				if chunk := string(runes[i:j]); chunk != "" {
					chunks = append(chunks, chunk)
				} else {
					chunks[len(chunks)-1] += "-"
				}
				for k := len(chunks) - 1; k > 0; k-- {
					prev, cur := []rune(chunks[k-1]), []rune(chunks[k])
					if len(prev) > 0 && len(cur) > 0 && prev[len(prev)-1] > cur[0] {
						chunks[k-1] = string(prev[:len(prev)-1]) + string(cur[1:])
						chunks = append(chunks[:k], chunks[k+1:]...)
					}
				}
				parts := make([]string, len(chunks))
				for x, s := range chunks {
					parts[x] = strings.ReplaceAll(escape(s), "-", `\-`)
				}
				stuff = strings.Join(parts, "-")
			}
			i = j + 1
			switch {
			case stuff == "":
				b.WriteString(`[^\x00-\x{10FFFF}]`)
			case stuff == "!":
				b.WriteString(".")
			default:
				stuff = fnmatchSetOpsRe.ReplaceAllString(stuff, `\$1`)
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' {
					stuff = `\` + stuff
				}
				b.WriteString("[" + stuff + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)\z`)
	return b.String()
}

// Absolute normalized path the way os.path.abspath builds it, two leading slashes kept on POSIX
func pyAbspath(p string) (string, error) {
	if isWindowsHost {
		return filepath.Abs(p)
	}
	if !strings.HasPrefix(p, "/") {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = cwd + "/" + p
	}
	return posixNormpath(p), nil
}

// Canonical path with links resolved as far as they exist, the way Path.resolve does without strict
func installResolve(p string) string {
	if isWindowsHost {
		return resolveNonStrict(p)
	}
	return posixRealpath(p)
}

// CPython 3.14 posixpath.realpath without strict, every link followed and errors leaving the rest unresolved
func posixRealpath(filename string) string {
	rest := strings.Split(filename, "/")
	slices.Reverse(rest)
	type item struct {
		name   string
		marker bool
	}
	stack := make([]item, 0, len(rest))
	for _, r := range rest {
		stack = append(stack, item{name: r})
	}
	partCount := len(rest)
	path := "/"
	if !strings.HasPrefix(filename, "/") {
		cwd, err := os.Getwd()
		if err != nil {
			return filename
		}
		path = cwd
	}
	seen := map[string]*string{}
	for partCount > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.marker {
			link := stack[len(stack)-1].name
			stack = stack[:len(stack)-1]
			resolved := path
			seen[link] = &resolved
			continue
		}
		name := top.name
		partCount--
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			path = path[:strings.LastIndex(path, "/")]
			if path == "" {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + name
		if path == "/" {
			newpath = "/" + name
		}
		st, err := os.Lstat(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if st.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
				continue
			}
			path = newpath
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		stack = append(stack, item{name: newpath}, item{marker: true})
		parts := strings.Split(target, "/")
		slices.Reverse(parts)
		for _, t := range parts {
			stack = append(stack, item{name: t})
		}
		partCount += len(parts)
	}
	return path
}

// Anchor and parts of a path the way pathlib splits it, dot parts and repeated separators dropped
func pathlibParts(p string) (string, []string) {
	var anchor, rest string
	if isWindowsHost {
		p = strings.ReplaceAll(p, "/", `\`)
		drive, root, tail := ntSplitRoot(p)
		anchor, rest = drive+root, tail
	} else {
		switch {
		case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
			anchor = "//"
		case strings.HasPrefix(p, "/"):
			anchor = "/"
		}
		rest = strings.TrimLeft(p, "/")
	}
	sep := "/"
	if isWindowsHost {
		sep = `\`
	}
	var parts []string
	for _, part := range strings.Split(rest, sep) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return anchor, parts
}

// A path in the normalized spelling pathlib gives it
func pathlibStr(p string) string {
	return pathlibJoin(pathlibParts(p))
}

// String form of an anchor and parts the way pathlib prints a path
func pathlibJoin(anchor string, parts []string) string {
	sep := "/"
	if isWindowsHost {
		sep = `\`
	}
	s := anchor + strings.Join(parts, sep)
	if s == "" {
		return "."
	}
	return s
}

// Final component of a path the way pathlib names it, empty for an anchor or the current directory
func pathlibName(p string) string {
	_, parts := pathlibParts(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// Logical parent of a path the way pathlib takes it, the path itself for an anchor or the current directory
func pathlibParent(p string) string {
	anchor, parts := pathlibParts(p)
	if len(parts) == 0 {
		return pathlibJoin(anchor, nil)
	}
	return pathlibJoin(anchor, parts[:len(parts)-1])
}

// Every logical ancestor of a path nearest first the way pathlib lists parents
func pathlibParents(p string) []string {
	anchor, parts := pathlibParts(p)
	var out []string
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, pathlibJoin(anchor, parts[:i]))
	}
	return out
}

// Folded parts of a path for comparisons, case folded on Windows as pathlib compares there
func pathlibKey(p string) (string, []string) {
	anchor, parts := pathlibParts(p)
	if isWindowsHost {
		anchor = strings.ToLower(anchor)
		for i := range parts {
			parts[i] = strings.ToLower(parts[i])
		}
	}
	return anchor, parts
}

// Reports whether two paths are equal the way pathlib compares them
func pathlibEqual(a, b string) bool {
	aa, ap := pathlibKey(a)
	ba, bp := pathlibKey(b)
	return aa == ba && slices.Equal(ap, bp)
}

// Reports whether the first path is a proper logical ancestor of the second
func pathlibIsAncestor(ancestor, p string) bool {
	aa, ap := pathlibKey(ancestor)
	pa, pp := pathlibKey(p)
	return aa == pa && len(ap) < len(pp) && slices.Equal(ap, pp[:len(ap)])
}
