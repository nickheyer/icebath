package manage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	"math/big"
)

// Paths a distribution owns when the manifest names none, config.yaml preserved on update by default
var DefaultDistOwned = []string{"SOUL.md", "config.yaml", "mcp.json", "skills", "cron", profiles.ManifestFilename}

// Top level names a distribution never writes, the export exclusions plus per user data dirs
var UserOwnedExclude = func() map[string]bool {
	out := map[string]bool{}
	for k := range profiles.DefaultExportExcludeRoot {
		out[k] = true
	}
	for _, k := range []string{"memories", "sessions", "plans", "workspace", "home", "backups", "cache", "local"} {
		out[k] = true
	}
	return out
}()

var cronStoreRel = []string{"cron", "jobs.json"}

// Install preview with the resolved manifest, where it was staged from and where it lands
type InstallPlan struct {
	Manifest        *profiles.DistributionManifest
	StagedDir       string
	Provenance      string
	TargetDir       string
	Existing        bool
	PreservesConfig bool
	HasCron         bool
}

func distErr(format string, args ...any) error {
	return &profiles.DistributionError{Msg: fmt.Sprintf(format, args...)}
}

// Credential stores at any depth, runtime cron siblings and skills root dotfiles, none of which a distribution writes
func isInstallerOwnedPath(parts []string) bool {
	if profiles.ProfilePathIsPrivate(parts) {
		return true
	}
	if len(parts) < 2 {
		return false
	}
	if parts[0] == "cron" {
		return !equalParts(parts[:2], cronStoreRel)
	}
	return parts[0] == "skills" && len(parts) == 2 && strings.HasPrefix(parts[1], ".")
}

var versionOpRe = regexp.MustCompile(`^\s*(>=|<=|==|!=|>|<)\s*(.+?)\s*\n?$`)
var semverSplitRe = regexp.MustCompile(`[-+]`)

// Major, minor and patch of a version with any pre-release or build suffix dropped
func parseSemver(v string) ([3]*big.Int, error) {
	head := strings.TrimLeft(pystr.Strip(v), "v")
	if loc := semverSplitRe.FindStringIndex(head); loc != nil {
		head = head[:loc[0]]
	}
	parts := strings.Split(head, ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	var out [3]*big.Int
	for i := range out {
		n, ok := pyint.Parse(parts[i])
		if !ok {
			return out, distErr("Unparseable version: %s", pyrepr.Repr(v))
		}
		out[i] = n
	}
	return out, nil
}

// Errors when current does not satisfy spec, a bare version meaning at least that version
func CheckHermesRequires(spec, current string) error {
	if pystr.Strip(spec) == "" {
		return nil
	}
	op, target := ">=", pystr.Strip(spec)
	if m := versionOpRe.FindStringSubmatch(spec); m != nil {
		op, target = m[1], m[2]
	}
	cur, err := parseSemver(current)
	if err != nil {
		return err
	}
	want, err := parseSemver(target)
	if err != nil {
		return err
	}
	cmp := 0
	for i := 0; i < 3 && cmp == 0; i++ {
		cmp = cur[i].Cmp(want[i])
	}
	ok := map[string]bool{">=": cmp >= 0, "<=": cmp <= 0, "==": cmp == 0, "!=": cmp != 0, ">": cmp > 0, "<": cmp < 0}[op]
	if !ok {
		return distErr("This distribution requires Icebath %s%s, but you have %s.", op, target, current)
	}
	return nil
}

// Body of a .env.template listing env_requires, optional keys commented out
func envTemplateFromManifest(m *profiles.DistributionManifest) string {
	lines := []string{
		"# Environment variables required by this Icebath distribution.",
		"# Copy to `.env` and fill in your own values before running.", "",
	}
	for _, req := range m.EnvRequires {
		if req.Description != "" {
			lines = append(lines, "# "+req.Description)
		}
		def := ""
		if req.Default != nil {
			def = pyrepr.Str(req.Default)
		}
		if req.Required {
			lines = append(lines, "# (required)", req.Name+"="+def, "")
		} else {
			lines = append(lines, "# (optional)", "# "+req.Name+"="+def, "")
		}
	}
	return pystr.Rstrip(strings.Join(lines, "\n")) + "\n"
}

var githubShorthandRe = regexp.MustCompile(`^github\.com/[\p{L}\p{N}_.-]+/[\p{L}\p{N}_.-]+/?\n?$`)

// Whether a source is a git remote, every http URL included since git is the only remote transport
func looksLikeGitURL(s string) bool {
	s = pystr.Strip(s)
	for _, p := range []string{"git@", "ssh://", "git://", "http://", "https://"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return strings.HasSuffix(s, ".git") || githubShorthandRe.MatchString(s)
}

// Shallow clone of url into dest through the credential fallback, github shorthand made https
func gitClone(ctx context.Context, url, dest string) error {
	if githubShorthandRe.MatchString(url) {
		url = "https://" + strings.TrimRight(url, "/")
	}
	res, err := provider.RunGitWithCredentialFallback(ctx, []string{"git", "clone", "--depth", "1", url, dest}, url,
		tools.NoninteractiveGitEnv(nil), nil)
	code, stderr := res.Code, res.Stderr
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return distErr("git is required for git-URL installs")
		}
		return err
	}
	if code != 0 {
		return distErr("git clone failed: %s", pystr.Strip(stderr))
	}
	return nil
}

var distGitClone = gitClone

// Staged dir and provenance of a source, git URLs cloned into workdir without .git, local dirs used in place
func stageSource(ctx context.Context, source, workdir string) (string, string, error) {
	src := pystr.Strip(source)
	var staged, provenance, missing string
	if looksLikeGitURL(src) {
		staged, provenance = filepath.Join(workdir, "clone"), src
		if err := distGitClone(ctx, src, staged); err != nil {
			return "", "", err
		}
		if err := tools.RmtreeReadonly(filepath.Join(staged, ".git"), false); err != nil {
			return "", "", err
		}
		missing = fmt.Sprintf("No %s at the root of %s. This repository is not an Icebath profile distribution.",
			profiles.ManifestFilename, pyrepr.Repr(src))
	} else if guess := pypath.Expanduser(src); isDir(guess) {
		staged = resolvePath(guess)
		provenance = staged
		missing = fmt.Sprintf("No %s in %s. A local-directory source must contain a distribution.yaml at its root.",
			profiles.ManifestFilename, pypath.PathStr(guess))
	} else {
		return "", "", distErr("Cannot resolve distribution source: %s. Expected a git URL (e.g. github.com/user/repo) or a local directory.",
			pyrepr.Repr(source))
	}
	if !isRegularFile(filepath.Join(staged, profiles.ManifestFilename)) {
		return "", "", &profiles.DistributionError{Msg: missing}
	}
	return staged, provenance, nil
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Refuses a staged tree holding any symlink before a file is read or copied
func rejectDistributionSymlinks(staged string) error {
	return filepath.WalkDir(staged, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == staged {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(staged, p)
			if relErr != nil {
				rel = p
			}
			return distErr("Profile distributions cannot contain symlinks: %s", rel)
		}
		return nil
	})
}

func hasCronJobs(staged string) bool {
	return isRegularFile(filepath.Join(append([]string{staged}, cronStoreRel...)...))
}

// Base version of the running build, unknown when the build carries none
var distBaseVersion = func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "unknown"
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

var distNow = time.Now

// Stages source and resolves the plan an install would follow, name and source stamped into the manifest
func PlanInstall(ctx context.Context, source, workdir, overrideName string) (*InstallPlan, error) {
	staged, provenance, err := stageSource(ctx, source, workdir)
	if err != nil {
		return nil, err
	}
	if err := rejectDistributionSymlinks(staged); err != nil {
		return nil, err
	}
	manifest, err := profiles.ReadManifest(staged)
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		return nil, distErr("No %s found at the distribution root — this source is not an Icebath distribution.", profiles.ManifestFilename)
	}
	if err := CheckHermesRequires(manifest.HermesRequires, distBaseVersion()); err != nil {
		return nil, err
	}
	name := overrideName
	if name == "" {
		name = manifest.Name
	}
	canon, err := profiles.CanonValid(name)
	if err != nil {
		return nil, err
	}
	if canon == "default" {
		return nil, distErr("Cannot install a distribution as 'default' — that is the built-in root profile (~/.icebath).  Pass --name <name> to install under a new profile.")
	}
	manifest.Name, manifest.Source = canon, provenance
	manifest.InstalledAt = distNow().UTC().Format("2006-01-02T15:04:05") + "+00:00"
	target, err := profiles.GetProfileDir(canon)
	if err != nil {
		return nil, err
	}
	existing := isDir(target)
	return &InstallPlan{Manifest: manifest, StagedDir: staged, Provenance: provenance, TargetDir: target, Existing: existing,
		PreservesConfig: existing, HasCron: hasCronJobs(staged)}, nil
}

// Staged paths the distribution owns with their relative parts, the whole payload when no allowlist is declared
func ownedEntries(staged string, m *profiles.DistributionManifest) ([]ownedEntry, error) {
	var explicit []string
	for _, p := range m.DistributionOwned {
		if p = strings.Trim(pystr.Strip(p), "/"); p != "" {
			explicit = append(explicit, p)
		}
	}
	var out []ownedEntry
	if len(explicit) == 0 {
		entries, err := os.ReadDir(staged)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !UserOwnedExclude[e.Name()] && !isInstallerOwnedPath([]string{e.Name()}) {
				out = append(out, ownedEntry{filepath.Join(staged, e.Name()), []string{e.Name()}})
			}
		}
		return out, nil
	}
	for _, rel := range explicit {
		parts, err := pyfs.NormalizeArchiveParts(rel)
		if err != nil || len(parts) == 0 {
			continue
		}
		if UserOwnedExclude[parts[0]] || isInstallerOwnedPath(parts) {
			continue
		}
		src := filepath.Join(append([]string{staged}, parts...)...)
		if _, err := os.Stat(src); err == nil {
			out = append(out, ownedEntry{src, parts})
		}
	}
	return out, nil
}

type ownedEntry struct {
	src   string
	parts []string
}

// Removes one destination entry without following a destination symlink
func removeExisting(p string) error {
	if isDir(p) && !isSymlink(p) {
		return pyfs.RmTree(p, nil)
	}
	if lexists(p) {
		return os.Remove(p)
	}
	return nil
}

var (
	distRemoveExisting = removeExisting
	distCopyTree       = func(src, dst string) error { return pyfs.CopyTree(src, dst, false, false, nil) }
	distRename         = os.Rename
)

func uuidHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Replaces dest with src wholesale, a directory copied beside dest and swapped in so a failure keeps the old tree
func replaceEntry(src, dest string) (err error) {
	if !isDir(src) {
		if err := distRemoveExisting(dest); err != nil {
			return err
		}
		return pyfs.Copy2(src, dest)
	}
	holder := filepath.Join(filepath.Dir(dest), skills.DistSwapDir)
	token := uuidHex()
	staging := filepath.Join(holder, filepath.Base(dest)+".staging-"+token)
	backup := filepath.Join(holder, filepath.Base(dest)+".old-"+token)
	if err := os.Mkdir(holder, 0o777); err != nil && !(errors.Is(err, os.ErrExist) && isDir(holder)) {
		return err
	}
	defer func() {
		_ = pyfs.RmTree(staging, func(pyfs.RmTreeOp, string, error) error { return nil })
		_ = os.Remove(holder)
	}()
	if err := distCopyTree(src, staging); err != nil {
		return err
	}
	if lexists(dest) {
		if err := distRename(dest, backup); err != nil {
			return err
		}
	}
	if err := distRename(staging, dest); err != nil {
		if lexists(backup) {
			if rerr := distRename(backup, dest); rerr != nil {
				return rerr
			}
		}
		return err
	}
	_ = distRemoveExisting(backup)
	return nil
}

// Staged cron/jobs.json when the distribution owns it through cron or by its exact path
func shippedCronStore(entries []ownedEntry) string {
	for _, e := range entries {
		if equalParts(e.parts, cronStoreRel) {
			return e.src
		}
		if equalParts(e.parts, cronStoreRel[:1]) && isRegularFile(filepath.Join(e.src, cronStoreRel[1])) {
			return filepath.Join(e.src, cronStoreRel[1])
		}
	}
	return ""
}

// Whether an error is a Python OSError, which the cron merge lets propagate unchanged
func isOSError(err error) bool {
	var pe *os.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &errno)
}

// Merges a shipped cron store into home by job id, new jobs paused and a bad job refusing the merge
func mergeCronStore(ctx context.Context, src, home string) error {
	dest := filepath.Join(append([]string{home}, cronStoreRel...)...)
	h := profiles.CronDistributionHooks
	if h.LoadJobs == nil || h.ImportJobDefinitions == nil {
		panic("manage: pkg/cron must install profiles.CronDistributionHooks (cron/jobs.py load_jobs, cron/job_definition.py import_job_definitions)")
	}
	wrap := func(err error) error {
		if isOSError(err) {
			return err
		}
		return distErr("Could not merge cron jobs into %s: %v", dest, err)
	}
	shipped, err := loadShippedCronJobs(ctx, src, h.LoadJobs, wrap)
	if err != nil {
		return err
	}
	if err := h.ImportJobDefinitions(ctx, home, shipped, "Installed from a profile distribution; review it, then resume."); err != nil {
		return wrap(err)
	}
	return nil
}

// Jobs of a shipped cron store keyed by id, loaded from a scratch store removed before returning
func loadShippedCronJobs(ctx context.Context, src string, load func(ctx context.Context, home string) ([]any, error), wrap func(error) error) (_ *pyjson.Object, err error) {
	tmp, err := os.MkdirTemp("", "icebath_dist_cron_")
	if err != nil {
		return nil, err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	if err := os.Mkdir(filepath.Join(tmp, "cron"), 0o777); err != nil {
		return nil, err
	}
	if err := pyfs.Copy2(src, filepath.Join(tmp, "cron", "jobs.json")); err != nil {
		return nil, err
	}
	jobs, err := load(ctx, tmp)
	if err != nil {
		return nil, wrap(err)
	}
	shipped := pyjson.NewObject()
	for _, j := range jobs {
		if job, ok := j.(map[string]any); ok && pyTruthy(job["id"]) {
			shipped.Set(profilesPyStr(job["id"]), job)
		}
	}
	return shipped, nil
}

// Python str of a job id, the text itself for a string
func profilesPyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Python truthiness of a decoded JSON or YAML value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Path of base joined with parts as real directories, a file replaced and a symlinked ancestor refused
func realDir(base string, parts []string) (string, error) {
	p := base
	for _, part := range parts {
		p = filepath.Join(p, part)
		if err := refuseSymlink(p); err != nil {
			return "", err
		}
		if _, err := os.Stat(p); err == nil && !isDir(p) {
			if err := distRemoveExisting(p); err != nil {
				return "", err
			}
		}
		if err := os.Mkdir(p, 0o777); err != nil && !(errors.Is(err, os.ErrExist) && isDir(p)) {
			return "", err
		}
	}
	return p, nil
}

func refuseSymlink(p string) error {
	if isSymlink(p) {
		return distErr("%s is a symlink; refusing to replace it — remove the link (or replace it with a real directory) and re-run", p)
	}
	return nil
}

// Whether a directory only groups roots, a skills category without SKILL.md at or above it or a dir of metadata
func isContainer(p string, rel []string) (bool, error) {
	if !isDir(p) {
		return false, nil
	}
	if rel[0] == "skills" {
		cur := p
		for i := 0; i < len(rel); i++ {
			if isRegularFile(filepath.Join(cur, "SKILL.md")) {
				return false, nil
			}
			cur = filepath.Dir(cur)
		}
		return true, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() != "DESCRIPTION.md" && !strings.HasPrefix(e.Name(), ".") && isRegularFile(filepath.Join(p, e.Name())) {
			return false, nil
		}
	}
	return true, nil
}

// Whether an owned dir merges per authored root rather than being replaced whole
func mergesPerRoot(src string, rel []string) (bool, error) {
	if !isDir(src) {
		return false, nil
	}
	if len(rel) == 1 || profiles.ProfilePathContainsPrivateStore(rel) {
		return true, nil
	}
	return isContainer(src, rel)
}

func childParts(rel []string, name string) []string {
	return append(append([]string(nil), rel...), name)
}

// Merges authored roots of src into dest leaving installer owned nested state untouched
func mergeDir(src, dest string, rel []string) error {
	children, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, c := range children {
		parts := childParts(rel, c.Name())
		if isInstallerOwnedPath(parts) || equalParts(parts, cronStoreRel) {
			continue
		}
		child := filepath.Join(src, c.Name())
		merge, err := mergesPerRoot(child, parts)
		if err != nil {
			return err
		}
		if merge {
			sub, err := realDir(dest, []string{c.Name()})
			if err != nil {
				return err
			}
			if err := mergeDir(child, sub, parts); err != nil {
				return err
			}
			continue
		}
		if err := replaceEntry(child, filepath.Join(dest, c.Name())); err != nil {
			return err
		}
	}
	return nil
}

func refuseSymlinkedContainers(src, dest string, rel []string) error {
	children, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, c := range children {
		parts := childParts(rel, c.Name())
		if isInstallerOwnedPath(parts) {
			continue
		}
		child, target := filepath.Join(src, c.Name()), filepath.Join(dest, c.Name())
		merge, err := mergesPerRoot(child, parts)
		if err != nil {
			return err
		}
		if merge {
			if err := refuseSymlink(target); err != nil {
				return err
			}
			if err := refuseSymlinkedContainers(child, target, parts); err != nil {
				return err
			}
		} else if err := refuseStoreAncestorReplacement(target, parts); err != nil {
			return err
		}
	}
	return nil
}

// Refuses a shipped file that would replace a profile directory holding credential stores
func refuseStoreAncestorReplacement(dest string, rel []string) error {
	if profiles.ProfilePathContainsPrivateStore(rel) && isDir(dest) && !isSymlink(dest) {
		return distErr("%s is a directory that holds credential stores, and the distribution ships a file named %s; refusing to replace it",
			dest, strings.Join(rel, "/"))
	}
	return nil
}

// Refuses every symlinked target before the first write so a refused update leaves the profile whole
func refuseSymlinkedTargets(target string, entries []ownedEntry) error {
	for _, e := range entries {
		depth := len(e.parts) - 1
		if isDir(e.src) {
			depth = len(e.parts)
		}
		p := target
		for _, part := range e.parts[:depth] {
			p = filepath.Join(p, part)
			if err := refuseSymlink(p); err != nil {
				return err
			}
		}
		merge, err := mergesPerRoot(e.src, e.parts)
		if err != nil {
			return err
		}
		if merge {
			err = refuseSymlinkedContainers(e.src, p, e.parts)
		} else {
			err = refuseStoreAncestorReplacement(filepath.Join(p, e.parts[len(e.parts)-1]), e.parts)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Copies the owned payload into target, cron merged first, config kept when preserved, manifest rewritten last
func copyDistPayload(ctx context.Context, staged, target string, m *profiles.DistributionManifest, preserveConfig bool) error {
	if err := config.RequireFileTooling(ctx, "Profile distribution install"); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o777); err != nil {
		return err
	}
	entries, err := ownedEntries(staged, m)
	if err != nil {
		return err
	}
	if err := refuseSymlinkedTargets(target, entries); err != nil {
		return err
	}
	if store := shippedCronStore(entries); store != "" {
		if _, err := realDir(target, cronStoreRel[:1]); err != nil {
			return err
		}
		if err := mergeCronStore(ctx, store, target); err != nil {
			return err
		}
	}
	for _, e := range entries {
		if equalParts(e.parts, cronStoreRel) {
			continue
		}
		if len(e.parts) == 1 {
			name := e.parts[0]
			if name == profiles.EnvTemplateFilename {
				if err := replaceEntry(e.src, filepath.Join(target, profiles.EnvExampleFilename)); err != nil {
					return err
				}
				continue
			}
			if _, err := os.Stat(filepath.Join(target, "config.yaml")); name == "config.yaml" && preserveConfig && err == nil {
				continue
			}
		}
		merge, err := mergesPerRoot(e.src, e.parts)
		if err != nil {
			return err
		}
		if merge {
			dest, err := realDir(target, e.parts)
			if err != nil {
				return err
			}
			if err := mergeDir(e.src, dest, e.parts); err != nil {
				return err
			}
			continue
		}
		parent, err := realDir(target, e.parts[:len(e.parts)-1])
		if err != nil {
			return err
		}
		if err := replaceEntry(e.src, filepath.Join(parent, e.parts[len(e.parts)-1])); err != nil {
			return err
		}
	}
	example := filepath.Join(target, profiles.EnvExampleFilename)
	if _, err := os.Stat(example); len(m.EnvRequires) > 0 && err != nil {
		if err := os.WriteFile(example, []byte(textForWrite(envTemplateFromManifest(m))), 0o666); err != nil {
			return err
		}
	}
	_, err = profiles.WriteManifest(ctx, target, m)
	return err
}

func bootstrapUserDirs(target string) error {
	for _, d := range profiles.ProfileDirs {
		if err := os.MkdirAll(filepath.Join(target, d), 0o777); err != nil {
			return err
		}
	}
	return nil
}

// Installs a distribution from source into a new profile, or over an existing one with force, returning its plan
func InstallDistribution(ctx context.Context, source, name string, force, createAlias bool) (_ *InstallPlan, err error) {
	tmp, err := os.MkdirTemp("", "icebath_dist_install_")
	if err != nil {
		return nil, err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	plan, err := PlanInstall(ctx, source, tmp, name)
	if err != nil {
		return nil, err
	}
	if plan.Existing && !force {
		return nil, distErr("Profile '%s' already exists at %s. Use `icebath profile update` to upgrade in place, or pass --force to overwrite.",
			plan.Manifest.Name, plan.TargetDir)
	}
	if err := bootstrapUserDirs(plan.TargetDir); err != nil {
		return nil, err
	}
	if err := copyDistPayload(ctx, plan.StagedDir, plan.TargetDir, plan.Manifest, false); err != nil {
		return nil, err
	}
	if createAlias {
		collision, err := profiles.CheckAliasCollision(plan.Manifest.Name)
		if err != nil {
			return nil, err
		}
		if collision == "" {
			if _, err := profiles.CreateWrapperScript(plan.Manifest.Name, ""); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// Canonical name and dir of an existing profile, a missing one reported as a DistributionError
func existingDistProfile(name string) (string, string, error) {
	canon, dir, err := profiles.ExistingProfileDir(name)
	var nf *profiles.NotFoundError
	if errors.As(err, &nf) {
		return "", "", distErr("%s", nf.Msg)
	}
	return canon, dir, err
}

// Re-pulls an installed distribution from its recorded source, user data kept and config.yaml kept unless forced
func UpdateDistribution(ctx context.Context, profileName string, forceConfig bool) (_ *InstallPlan, err error) {
	canon, target, err := existingDistProfile(profileName)
	if err != nil {
		return nil, err
	}
	existing, err := profiles.ReadManifest(target)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, distErr("Profile '%s' is not a distribution (no %s). Only profiles installed via `icebath profile install` can be updated.",
			canon, profiles.ManifestFilename)
	}
	if existing.Source == "" {
		return nil, distErr("Profile '%s' has no recorded source.  Re-install with `icebath profile install <source> --name {canon} --force`.", canon)
	}
	tmp, err := os.MkdirTemp("", "icebath_dist_update_")
	if err != nil {
		return nil, err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	plan, err := PlanInstall(ctx, existing.Source, tmp, canon)
	if err != nil {
		return nil, err
	}
	plan.PreservesConfig = !forceConfig
	if err := copyDistPayload(ctx, plan.StagedDir, plan.TargetDir, plan.Manifest, plan.PreservesConfig); err != nil {
		return nil, err
	}
	return plan, nil
}

// Manifest mapping of a profile, empty when the profile is not a distribution
func DescribeDistribution(profileName string) (map[string]any, error) {
	_, dir, err := existingDistProfile(profileName)
	if err != nil {
		return nil, err
	}
	m, err := profiles.ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m.ToDict(), nil
}
