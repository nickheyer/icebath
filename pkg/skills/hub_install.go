package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Resolved lock install path strictly under the skills dir, refusing redirects and the root itself
func resolveLockInstallPath(s *Scope, installPath, skillName string) (string, error) {
	normalized, err := normalizeLockInstallPath(installPath, skillName)
	if err != nil {
		return "", err
	}
	target := s.SkillsDir()
	skillsRoot := resolvePath(target)
	for _, part := range strings.Split(normalized, "/") {
		target = filepath.Join(target, part)
		if isPathRedirect(target) {
			return "", fmt.Errorf("Unsafe install path: %s", installPath)
		}
	}
	resolved := resolvePath(target)
	if resolved == skillsRoot || !isRelativeTo(resolved, skillsRoot) {
		return "", fmt.Errorf("Unsafe install path: %s", installPath)
	}
	return resolved, nil
}

// Writes a bundle into its quarantine directory after validating every member path
func (h *Hub) QuarantineBundle(b *SkillBundle) (string, error) {
	if err := h.EnsureHubDirs(); err != nil {
		return "", err
	}
	skillName, err := validateHubSkillName(b.Name)
	if err != nil {
		return "", err
	}
	rels := make([]string, len(b.Files))
	for i, f := range b.Files {
		if rels[i], err = validateBundleRelPath(f.Path); err != nil {
			return "", err
		}
	}
	dest := filepath.Join(h.QuarantineDir(), skillName)
	if _, err := os.Stat(dest); err == nil {
		if err := pyfs.RmTree(dest, nil); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return "", err
	}
	if err := os.Mkdir(dest, 0o777); err != nil {
		return "", err
	}
	for i, f := range b.Files {
		fileDest := filepath.Join(append([]string{dest}, strings.Split(rels[i], "/")...)...)
		if err := os.MkdirAll(filepath.Dir(fileDest), 0o777); err != nil {
			return "", err
		}
		if !f.Binary {
			if err := pystr.StrictUTF8EncodeError(string(f.Data)); err != nil {
				return "", err
			}
		}
		if err := os.WriteFile(fileDest, f.Data, 0o666); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// Names of visible child dirs holding an active SKILL.md anywhere below, vendored and support paths pruned
func categorySkillDirs(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		entry := filepath.Join(directory, e.Name())
		if strings.HasPrefix(e.Name(), ".") || !isDir(entry) {
			continue
		}
		mds, err := pypath.Glob(entry, "**/SKILL.md")
		if err != nil {
			return nil, err
		}
		for _, md := range mds {
			rel, _ := filepath.Rel(directory, md)
			if !isExcludedSkillPath(rel, directory) {
				out = append(out, e.Name())
				break
			}
		}
	}
	return out, nil
}

// Refuses an install that would nest in a skill, replace a stray file or wipe a category of other skills
func (h *Hub) checkInstallTarget(installDir string) error {
	skillsRoot := resolvePath(h.S.SkillsDir())
	for ancestor := filepath.Dir(installDir); ancestor != skillsRoot && isRelativeTo(ancestor, skillsRoot); ancestor = filepath.Dir(ancestor) {
		if st, err := os.Stat(filepath.Join(ancestor, "SKILL.md")); err == nil && st.Mode().IsRegular() {
			return fmt.Errorf("Refusing to install into '%s': it is an existing skill directory, not a category. Choose a different category.", filepath.Base(ancestor))
		}
	}
	st, err := os.Stat(installDir)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		return fmt.Errorf("Refusing to install: '%s' already exists and is not a directory. Remove it or choose a different skill name.", filepath.Base(installDir))
	}
	if _, err := os.Stat(filepath.Join(installDir, "SKILL.md")); err == nil {
		return nil
	}
	dirs, err := categorySkillDirs(installDir)
	if err != nil {
		return err
	}
	if len(dirs) > 0 {
		sorted := slices.Clone(dirs)
		slices.Sort(sorted)
		return fmt.Errorf("Refusing to overwrite category directory '%s' which contains %d skill(s): %s. Use a different --name or install into a subcategory.",
			installDir, len(dirs), strings.Join(sorted, ", "))
	}
	return nil
}

var scanProvenanceOrder = []string{"bundle_hash", "scanner_version", "source", "source_url", "verdict", "trust_level",
	"findings", "rules", "scanned_at", "summary", "fresh"}

// Scan attestation as the ordered mapping skills_guard builds, findings as dataclass field mappings
func scanProvenanceObject(m map[string]any) *pyjson.Object {
	o := pyjson.NewObject()
	keys := slices.Clone(scanProvenanceOrder)
	var extra []string
	for k := range m {
		if !slices.Contains(scanProvenanceOrder, k) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range append(keys, extra...) {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case []Finding:
			list := make([]any, len(t))
			for i, f := range t {
				fo := pyjson.NewObject()
				for _, kv := range [][2]any{{"pattern_id", f.PatternID}, {"severity", f.Severity}, {"category", f.Category},
					{"file", f.File}, {"line", int64(f.Line)}, {"match", f.Match}, {"description", f.Description}} {
					fo.Set(kv[0].(string), kv[1])
				}
				list[i] = fo
			}
			v = list
		case []string:
			list := make([]any, len(t))
			for i, s := range t {
				list[i] = s
			}
			v = list
		}
		o.Set(k, v)
	}
	return o
}

// Moves a scanned skill from quarantine into the skills dir and records its provenance
func (h *Hub) InstallFromQuarantine(ctx context.Context, quarantinePath, skillName, category string, b *SkillBundle,
	scan ScanResult, scanProvenance *pyjson.Object) (string, error) {
	safeName, err := validateHubSkillName(skillName)
	if err != nil {
		return "", err
	}
	safeCategory := ""
	if category != "" {
		if safeCategory, err = validateInstallParentPath(category); err != nil {
			return "", err
		}
	}
	quarantineResolved := resolvePath(quarantinePath)
	if !isRelativeTo(quarantineResolved, resolvePath(h.QuarantineDir())) {
		return "", fmt.Errorf("Unsafe quarantine path: %s", quarantinePath)
	}
	installRel := safeName
	if safeCategory != "" {
		installRel = safeCategory + "/" + safeName
	}
	installDir, err := resolveLockInstallPath(h.S, installRel, safeName)
	if err != nil {
		return "", err
	}
	if err := h.checkInstallTarget(installDir); err != nil {
		return "", err
	}
	if _, err := os.Stat(installDir); err == nil {
		if err := pyfs.RmTree(installDir, nil); err != nil {
			return "", err
		}
	}
	var skillSize int64
	if st, err := os.Stat(filepath.Join(quarantinePath, "SKILL.md")); err == nil {
		skillSize = st.Size()
	}
	if skillSize > 100_000 {
		h.S.logger().Warn(fmt.Sprintf("Skill '%s' has a large SKILL.md (%s chars). Large skills consume significant context when loaded. Consider asking the author to split it into smaller files.",
			safeName, groupThousands(skillSize)))
	}
	entries, err := pypath.Glob(quarantinePath, "**/*")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if isPathRedirect(entry) {
			rel, rerr := filepath.Rel(quarantineResolved, entry)
			if rerr != nil || !isRelativeTo(entry, quarantineResolved) {
				rel = entry
			}
			return "", fmt.Errorf("Installed skill contains symlinks, which is not allowed: %s", rel)
		}
	}
	if err := os.MkdirAll(filepath.Dir(installDir), 0o777); err != nil {
		return "", err
	}
	if _, err := pyfs.Move(quarantinePath, installDir); err != nil {
		return "", err
	}
	installedHash := ContentHash(installDir)
	rel, _ := filepath.Rel(resolvePath(h.S.SkillsDir()), resolvePath(installDir))
	files := make([]string, len(b.Files))
	for i, f := range b.Files {
		files[i] = f.Path
	}
	if scanProvenance == nil || scanProvenance.Len() == 0 {
		if scan.ScanProvenance != nil {
			scanProvenance = scanProvenanceObject(scan.ScanProvenance)
		}
	}
	if err := h.LockFile("").RecordInstall(safeName, b.Source, b.Identifier, b.TrustLevel, scan.Verdict, installedHash,
		filepath.ToSlash(rel), files, b.Metadata, scanProvenance); err != nil {
		return "", err
	}
	if err := h.AppendAuditLog("INSTALL", safeName, b.Source, b.TrustLevel, scan.Verdict, installedHash); err != nil {
		return "", err
	}
	h.S.RecordInstalled(ctx, safeName)
	return installDir, nil
}

// Thousands separated integer like the comma format spec
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Removes a hub installed skill inside the skills dir, refusing builtins and unsafe lock paths
func (h *Hub) UninstallSkill(skillName string) (bool, string, error) {
	lock := h.LockFile("")
	raw, err := lock.GetInstalled(skillName)
	if err != nil {
		return false, "", err
	}
	if !pyrepr.Truthy(raw) {
		return false, fmt.Sprintf("'%s' is not a hub-installed skill (may be a builtin)", skillName), nil
	}
	entry, ok := raw.(*pyjson.Object)
	if !ok {
		return false, "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
	}
	ip, ok := entry.Get("install_path")
	if !ok {
		ip = ""
	}
	ipStr, isStr := ip.(string)
	if !isStr {
		return false, fmt.Sprintf("Refusing to uninstall '%s': Unsafe install path: expected a string", skillName), nil
	}
	installPath, err := resolveLockInstallPath(h.S, ipStr, skillName)
	if err != nil {
		return false, fmt.Sprintf("Refusing to uninstall '%s': %s", skillName, err), nil
	}
	if _, err := os.Stat(installPath); err == nil {
		if err := pyfs.RmTree(installPath, nil); err != nil {
			return false, "", err
		}
	}
	if err := lock.RecordUninstall(skillName); err != nil {
		return false, "", err
	}
	source, ok1 := entry.Get("source")
	trust, ok2 := entry.Get("trust_level")
	if !ok1 || !ok2 {
		return false, "", errors.New("KeyError: lock entry lacks source or trust_level")
	}
	if err := h.AppendAuditLog("UNINSTALL", skillName, pyrepr.Str(source), pyrepr.Str(trust), "n/a", "user_request"); err != nil {
		return false, "", err
	}
	observability.RecordSkillRemoved(skillName)
	return true, fmt.Sprintf("Uninstalled '%s' from %s", skillName, ipStr), nil
}

// Deterministic hash of an in memory bundle, symmetric with the on disk content hash
func bundleContentHash(b *SkillBundle) (string, error) {
	normalized := map[string][]byte{}
	for _, f := range b.Files {
		if !f.Binary {
			if err := pystr.StrictUTF8EncodeError(string(f.Data)); err != nil {
				return "", err
			}
		}
		normalized[strings.ReplaceAll(f.Path, "\\", "/")] = f.Data
	}
	keys := slices.Sorted(maps.Keys(normalized))
	sum := sha256.New()
	for _, k := range keys {
		sum.Write([]byte(k))
		sum.Write([]byte{0})
		sum.Write(normalized[k])
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))[:16], nil
}

var sourceIDAliases = map[string]string{"skills.sh": "skills-sh"}

func sourceMatches(src SkillSource, sourceName string) bool {
	want := sourceName
	if a, ok := sourceIDAliases[sourceName]; ok {
		want = a
	}
	return src.SourceID() == want
}

// Upstream revision of an identifier, empty when the probe fails so the row takes the full fetch path
func (h *Hub) currentRevisionOrEmpty(ctx context.Context, src SkillSource, identifier string) string {
	rev, err := src.CurrentRevision(ctx, identifier)
	if err != nil {
		h.S.logger().Debug(fmt.Sprintf("current_revision probe failed for %s", identifier), "error", err)
		return ""
	}
	return rev
}

// Update check row of one installed hub skill, hashes and bundle set only for the statuses that carry them
type SkillUpdateCheck struct {
	Name, Identifier, Source, Status string
	CurrentHash, LatestHash          string
	HasHashes                        bool
	Bundle                           *SkillBundle
}

func lockString(entry *pyjson.Object, key, fallback string) string {
	v, ok := entry.Get(key)
	if !ok {
		return fallback
	}
	if s, isStr := v.(string); isStr {
		return s
	}
	return pyrepr.Str(v)
}

// Checks installed hub skills against the adapters of their recorded source only
func (h *Hub) CheckForSkillUpdates(ctx context.Context, name string, lock *HubLockFile, sources []SkillSource, auth *GitHubAuth) ([]SkillUpdateCheck, error) {
	if lock == nil {
		lock = h.LockFile("")
	}
	installed, err := lock.ListInstalled()
	if err != nil {
		return nil, err
	}
	if name != "" {
		kept := installed[:0:0]
		for _, e := range installed {
			if v, _ := e.Get("name"); v == name {
				kept = append(kept, e)
			}
		}
		installed = kept
	}
	if sources == nil {
		var rerr error
		func() {
			defer hubRecover(&rerr)
			sources = h.createSourceRouter(auth)
		}()
		if rerr != nil {
			return nil, rerr
		}
	}
	var results []SkillUpdateCheck
	for _, entry := range installed {
		row := SkillUpdateCheck{Name: lockString(entry, "name", ""), Identifier: lockString(entry, "identifier", ""),
			Source: lockString(entry, "source", "")}
		ip, _ := entry.Get("install_path")
		_, ipIsStr := ip.(string)
		installDir, err := resolveLockInstallPath(h.S, lockString(entry, "install_path", ""), lockString(entry, "name", "skill"))
		if err != nil || (ip != nil && !ipIsStr) {
			row.Status = "invalid_install"
			results = append(results, row)
			continue
		}
		if !isDir(installDir) {
			row.Status = "orphaned"
			results = append(results, row)
			continue
		}
		var matching []SkillSource
		for _, src := range sources {
			if sourceMatches(src, row.Source) {
				matching = append(matching, src)
			}
		}
		recorded := ""
		if md, ok := entry.Get("metadata"); ok && pyrepr.Truthy(md) {
			if mo, isObj := md.(*pyjson.Object); isObj {
				if rv, ok := mo.Get("source_revision"); ok {
					recorded = pyrepr.Str(rv)
				}
			}
		}
		if recorded != "" && slices.ContainsFunc(matching, func(src SkillSource) bool {
			return h.currentRevisionOrEmpty(ctx, src, row.Identifier) == recorded
		}) {
			row.Status, row.HasHashes = "up_to_date", true
			row.CurrentHash = lockString(entry, "content_hash", "")
			row.LatestHash = row.CurrentHash
			results = append(results, row)
			continue
		}
		var bundle *SkillBundle
		for _, src := range matching {
			b, ferr := src.Fetch(ctx, row.Identifier)
			if ferr != nil {
				b = nil
			}
			if b != nil {
				bundle = b
				break
			}
		}
		if bundle == nil {
			row.Status = "unavailable"
			results = append(results, row)
			continue
		}
		latest, err := bundleContentHash(bundle)
		if err != nil {
			return nil, err
		}
		row.CurrentHash, row.LatestHash, row.HasHashes, row.Bundle = lockString(entry, "content_hash", ""), latest, true, bundle
		row.Status = "update_available"
		if row.CurrentHash == latest {
			row.Status = "up_to_date"
		}
		results = append(results, row)
	}
	return results, nil
}
