package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Venv is the project venv made true by a frozen uv sync of pyproject, uv.lock and the enabled extras
type Venv struct {
	BasePackage
	projectRoot string
}

func newVenv(projectRoot string) *Venv {
	p := &Venv{BasePackage: BasePackage{Name: "venv", Deps: []string{"uv"}, OffPath: true}, projectRoot: projectRoot}
	p.bind(p)
	return p
}

// State package of the project venv rooted at root, the PM installation's root when empty
func NewVenv(root string) StatePackage { return newVenv(root) }

// Project whose venv this state describes
func (v *Venv) ProjectRoot() string {
	if v.projectRoot == "" {
		return RepoRoot()
	}
	return v.projectRoot
}

// Committed environment of the project, or the original install before the first sync
func (v *Venv) VenvDir(ctx context.Context) (string, error) {
	return SelectedVenv(ctx, v.ProjectRoot())
}

func (v *Venv) ExpectedStamp(extras []string) (string, error) {
	return v.Stamp(context.Background(), extras, nil)
}

// Hash of the core lock, the extras, the pinned python identity and the plugin member inputs
func (v *Venv) Stamp(ctx context.Context, extras []string, pluginDirs []MemberDir) (string, error) {
	lock := NewLockfile(LockfilePath())
	target, err := packagesCurrentTarget()
	if err != nil {
		return "", err
	}
	digests := []any{}
	for _, artifact := range lock.Artifacts("python", target) {
		sha, ok := artifact["sha256"]
		if !ok {
			return "", keyError("sha256")
		}
		digests = append(digests, sha)
	}
	identity, err := dumpsJSON([]any{lock.versionValue("python"), target, digests}, false)
	if err != nil {
		return "", err
	}
	coreDigest, err := uvLockDigest(filepath.Join(v.ProjectRoot(), "uv.lock"))
	if err != nil {
		return "", err
	}
	sorted := append([]string{}, extras...)
	sort.Strings(sorted)
	if pluginDirs == nil {
		if pluginDirs, err = BuildMemberDirs(ctx, extras, ""); err != nil {
			return "", err
		}
	}
	members, err := MembersStamp(pluginDirs)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(coreDigest)
	h.Write([]byte(strings.Join(sorted, ",")))
	h.Write(identity)
	h.Write([]byte(members))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VenvApplyOptions shape one environment preparation, nil PluginDirs meaning the enabled members
type VenvApplyOptions struct {
	PluginDirs           []MemberDir
	Repair               bool
	Explicit             bool
	SkipInvalidSecondary bool
}

// VenvGeneration is a prepared environment and the resolved lock it was synced from
type VenvGeneration struct {
	Environment  string
	ResolvedLock string
}

func (v *Venv) Apply(extras []string, explicit bool) error {
	_, err := v.Prepare(context.Background(), extras, VenvApplyOptions{Explicit: explicit})
	return err
}

// Prepares one complete environment generation for the caller to commit, removing it on any failure
func (v *Venv) Prepare(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error) {
	project := v.ProjectRoot()
	generations := filepath.Join(InstallStateDir(ctx, project), "environments")
	generation := filepath.Join(generations, randomHex32())
	candidate := filepath.Join(generation, "venv")
	buildEnv, err := sourceBuildEnvironment(ctx, project)
	if err != nil {
		return nil, err
	}
	environment, err := ManagedEnvironment(ctx, candidate, ManagedEnvironmentOptions{
		Env: buildEnv, Explicit: opts.Explicit || opts.Repair, Output: os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	members := []MemberDir{}
	if !opts.Repair {
		if _, err := EnabledPluginsOrdered(ctx, EnabledPluginsOptions{SkipInvalidSecondary: opts.SkipInvalidSecondary}); err != nil {
			return nil, err
		}
		if opts.PluginDirs == nil {
			if members, err = BuildMemberDirs(ctx, extras, environment.Python); err != nil {
				return nil, err
			}
		} else {
			members = opts.PluginDirs
		}
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(generation)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(generation), 0o777); err != nil {
		return nil, err
	}
	if err := os.Mkdir(generation, 0o777); err != nil {
		return nil, err
	}
	if err := touchFile(filepath.Join(generation, ".lease-managed")); err != nil {
		return nil, err
	}
	if err := environment.Create(ctx); err != nil {
		return nil, err
	}
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, project), opts.Repair)
	if err != nil {
		return nil, err
	}
	prior, _ := facts.Get("venv")
	replay := ""
	_, hasEnvironment := prior["environment"]
	_, hasLock := prior["resolved_lock"]
	if opts.Repair && (hasEnvironment || hasLock) {
		recordedEnv, envOK := prior["environment"].(string)
		recordedLock, lockOK := prior["resolved_lock"].(string)
		if !envOK || !lockOK || recordedEnv == "" || recordedLock == "" {
			return nil, NewInstallError(v.Name, "recorded dependency paths are incomplete; refusing to drop plugins", "")
		}
		recorded := resolvePath(recordedLock)
		previous := filepath.Dir(resolvePath(recordedEnv))
		if filepath.Dir(previous) != resolvePath(generations) || recorded != filepath.Join(previous, "workspace", "uv.lock") ||
			!isRegularFile(recorded) {
			return nil, NewInstallError(v.Name, "recorded dependency lock is missing; refusing to drop plugins", "")
		}
		replay = filepath.Dir(recorded)
	}
	seed := filepath.Join(project, "uv.lock")
	if lockText := prior["resolved_lock"]; len(members) > 0 && truthy(lockText) {
		seed = pyStr(lockText)
	}
	workspace := filepath.Join(generation, "workspace")
	if err := LockAndSync(ctx, members, extras, LockAndSyncOptions{
		Root: workspace, Source: project, SeedLock: seed, Environment: environment,
		Frozen: opts.Repair || len(members) == 0, Replay: replay,
	}); err != nil {
		return nil, err
	}
	resolvedLock := filepath.Join(workspace, "uv.lock")
	if err := environment.Check(ctx, resolvedLock); err != nil {
		return nil, err
	}
	if opts.Repair {
		argv, err := VenvCommand(ctx, project, candidate, []string{"-I"})
		if err != nil {
			return nil, err
		}
		env := map[string]string{}
		for key, value := range environment.Env {
			env[key] = value
		}
		if err := ValidateEnvironment(ctx, "", argv, env, filepath.Dir(resolvedLock)); err != nil {
			return nil, err
		}
	}
	committed = true
	if err := touchFile(filepath.Join(generation, ".lease-managed")); err != nil {
		return nil, err
	}
	return &VenvGeneration{Environment: candidate, ResolvedLock: resolvedLock}, nil
}

// uvLockCacheEntry is one lock digest keyed on the file's mtime and size
type uvLockCacheEntry struct {
	mtime  int64
	size   int64
	digest []byte
}

var (
	uvLockDigestMu    sync.Mutex
	uvLockDigestCache = map[string]uvLockCacheEntry{}
)

// Sha256 of a uv.lock, cached on its mtime and size since startup checks read a megabyte class file
func uvLockDigest(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mtime, size := st.ModTime().UnixNano(), st.Size()
	uvLockDigestMu.Lock()
	cached, ok := uvLockDigestCache[path]
	uvLockDigestMu.Unlock()
	if ok && cached.mtime == mtime && cached.size == size {
		return cached.digest, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	uvLockDigestMu.Lock()
	uvLockDigestCache[path] = uvLockCacheEntry{mtime: mtime, size: size, digest: sum[:]}
	uvLockDigestMu.Unlock()
	return sum[:], nil
}

// Seed file copy, replaced by tests the way they patch shutil copy2
var seedCopy2 = copy2

// Payload cache file copy and directory listing the seed walks, replaced by tests
var (
	copySeedFile = seedFile
	seedReadDir  = os.ReadDir
)

// Copies one payload cache file unless an earlier seed already copied it whole, publishing only complete copies
func seedFile(src, dst string) error {
	if dstSt, err := os.Stat(dst); err == nil {
		srcSt, err := os.Stat(src)
		if err == nil && dstSt.Size() == srcSt.Size() {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging, err := os.CreateTemp(filepath.Dir(dst), ".seed-")
	if err != nil {
		return err
	}
	name := staging.Name()
	staging.Close()
	defer os.Remove(name)
	if err := seedCopy2(src, name); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// Recreates one payload cache symlink verbatim, replacing a directory an older link following seed left
func copySeedLink(src, dst string) error {
	linkTo, err := os.Readlink(src)
	if err != nil {
		return err
	}
	staging := filepath.Join(filepath.Dir(dst), ".seed-link-"+randomHex32())
	if err := os.Symlink(linkTo, staging); err != nil {
		return err
	}
	defer os.Remove(staging)
	if isDirectory(dst) && !isSymlink(dst) {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	return os.Rename(staging, dst)
}

// Merges one payload bucket into the machine cache, testing links before directories so links stay links
var seedTree func(src, dst string) error

func init() { seedTree = mergeSeedTree }

func mergeSeedTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	children, err := seedReadDir(src)
	if err != nil {
		return err
	}
	for _, child := range children {
		from, to := filepath.Join(src, child.Name()), filepath.Join(dst, child.Name())
		var err error
		switch {
		case isSymlink(from):
			err = copySeedLink(from, to)
		case isDirectory(from):
			err = seedTree(from, to)
		default:
			err = copySeedFile(from, to)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Machine scoped uv cache under the default root, never seeded or written here
func UvCachePath() string { return filepath.Join(home.Root(), "cache", "uv") }

// Machine scoped uv cache, merged once per uv.lock from the payload's shipped cache before it is used
func UvCacheDir(ctx context.Context) (string, error) {
	machine := UvCachePath()
	marker := filepath.Join(machine, ".seeded")
	seed := "1"
	if lock := filepath.Join(RepoRoot(), "uv.lock"); isRegularFile(lock) {
		digest, err := uvLockDigest(lock)
		if err != nil {
			return "", err
		}
		seed = hex.EncodeToString(digest)
	}
	storeRoot, err := StoreRoot(ctx)
	if err != nil {
		return "", err
	}
	payloadCache := LongRoot(filepath.Join(filepath.Dir(storeRoot), "uv-cache"))
	if isDirectory(payloadCache) {
		seed += "+payload2"
	}
	current := false
	if data, err := os.ReadFile(marker); err == nil {
		text := trimBOM(data)
		if err := strictUTF8(text); err != nil {
			return "", fmt.Errorf("UnicodeDecodeError: %s", err)
		}
		current = pyStrip(string(text)) == seed
	}
	if current {
		return machine, nil
	}
	if err := seedPayloadCache(payloadCache, machine); err != nil {
		slog.Warn(fmt.Sprintf("uv cache seed incomplete, retrying on the next install: %s", pyOSErrorText(err)))
		return machine, nil
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o777); err == nil {
		_ = os.WriteFile(marker, []byte(seed), 0o666)
	}
	return machine, nil
}

// Merges the payload cache into the machine cache with archive-v0 first so no wheel link points at nothing
func seedPayloadCache(payloadCache, machine string) error {
	if !isDirectory(payloadCache) {
		return nil
	}
	seeded := LongRoot(machine)
	if err := os.MkdirAll(seeded, 0o777); err != nil {
		return err
	}
	entries, err := seedReadDir(payloadCache)
	if err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Name() == "archive-v0" && entries[j].Name() != "archive-v0"
	})
	for _, entry := range entries {
		if entry.Name() == ".seeded" {
			continue
		}
		from, dest := filepath.Join(payloadCache, entry.Name()), filepath.Join(seeded, entry.Name())
		var err error
		switch {
		case isSymlink(from):
			err = copySeedLink(from, dest)
		case isDirectory(from):
			err = seedTree(from, dest)
		default:
			err = copySeedFile(from, dest)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
