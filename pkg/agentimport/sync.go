package agentimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// SyncManifestName is the per profile file listing every imported source
const SyncManifestName = "import-sync.json"

// SyncManifestPath is the sync manifest of a home
func SyncManifestPath(targetRoot string) string { return filepath.Join(targetRoot, SyncManifestName) }

// Source files whose content decides the sync digest, exactly what the importer reads
func iterSyncFiles(agent, sourceRoot string) []string {
	var paths []string
	switch agent {
	case "claude-code":
		paths = []string{filepath.Join(sourceRoot, "CLAUDE.md"), filepath.Join(sourceRoot, "settings.json"),
			filepath.Join(filepath.Dir(sourceRoot), ".claude.json")}
	case "cursor":
		paths = []string{filepath.Join(sourceRoot, "AGENTS.md"), filepath.Join(sourceRoot, "mcp.json")}
		if rules := filepath.Join(sourceRoot, "rules"); isDir(rules) {
			paths = append(paths, cursorRuleFiles(rules)...)
		}
	default:
		paths = []string{filepath.Join(sourceRoot, "AGENTS.md"), filepath.Join(sourceRoot, "config.toml")}
		memories, _ := filepath.Glob(filepath.Join(globEscape(filepath.Join(sourceRoot, "memories")), "*.md"))
		sortPaths(memories)
		paths = append(paths, memories...)
	}
	paths = append(paths, treeFiles(filepath.Join(sourceRoot, "skills"))...)
	var out []string
	for _, p := range paths {
		if !credentialName[filepath.Base(p)] && isFile(p) {
			out = append(out, p)
		}
	}
	return out
}

// ComputeSourceDigest is the content digest of everything the importer would read from the source
func ComputeSourceDigest(agent, sourceRoot string) string {
	digest := sha256.New()
	for _, p := range iterSyncFiles(agent, sourceRoot) {
		rel, err := filepath.Rel(sourceRoot, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rel = "../" + filepath.Base(p)
		}
		content := "unreadable"
		if raw, err := os.ReadFile(p); err == nil {
			sum := sha256.Sum256(raw)
			content = hex.EncodeToString(sum[:])
		}
		fmt.Fprintf(digest, "%s\x00%s\x00", rel, content)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// SkillTreeDigest is the content digest of an installed skill dir, relative path and bytes of every file
func SkillTreeDigest(skillDir string) string {
	digest := sha256.New()
	for _, p := range treeFiles(skillDir) {
		raw, err := os.ReadFile(p)
		rel, rerr := filepath.Rel(skillDir, p)
		if err != nil || rerr != nil {
			digest.Write([]byte("unreadable\x00"))
			continue
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(digest, "%s\x00%s\x00", rel, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// ManagedSkills reads imported_skills as name to digest at import, a pre digest list mapping to nil
func ManagedSkills(entry any) map[string]any {
	out := map[string]any{}
	switch skills := get(entry, "imported_skills").(type) {
	case *jsonutil.Object:
		for _, name := range skills.Keys() {
			out[name] = skills.Value(name)
		}
	case []any:
		for _, name := range skills {
			out[pyStr(name)] = nil
		}
	}
	return out
}

// Fresh manifest with no sources
func emptyManifest() *jsonutil.Object {
	return jsonutil.ObjectOf("version", int64(1), "agents", jsonutil.NewObject())
}

// LoadSyncManifest reads the manifest, a missing, unreadable or malformed file giving an empty one
func LoadSyncManifest(targetRoot string) *jsonutil.Object {
	path := SyncManifestPath(targetRoot)
	if !exists(path) {
		return emptyManifest()
	}
	text, err := readText(path)
	var data any
	if err == nil {
		data, err = jsonutil.Decode(text)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Unreadable %s — starting a fresh sync manifest", path))
		return emptyManifest()
	}
	o, ok := asObject(data)
	if !ok {
		return emptyManifest()
	}
	if _, ok := asObject(o.Value("agents")); !ok {
		return emptyManifest()
	}
	return o
}

// SaveSyncManifest writes the manifest as sorted indented JSON
func SaveSyncManifest(ctx context.Context, targetRoot string, manifest *jsonutil.Object) error {
	text, err := jsonutil.Encode(manifest, jsonutil.Indent(2), jsonutil.SortKeys(), jsonutil.ASCII())
	if err != nil {
		return err
	}
	return atomicWriteText(ctx, SyncManifestPath(targetRoot), text+"\n")
}

// UpdateSyncManifest records a real import with every skill digest it wrote, refreshDigest false keeping the old source digest
func UpdateSyncManifest(ctx context.Context, agent, sourceRoot, targetRoot string, overwrite bool, report *Report, refreshDigest bool) error {
	manifest := LoadSyncManifest(targetRoot)
	agents, _ := asObject(manifest.Value("agents"))
	entry, ok := asObject(agents.Value(agent))
	if !ok {
		entry = jsonutil.NewObject()
	}
	skills := ManagedSkills(entry)
	for _, it := range report.Items {
		if it.Kind == "skill" && it.Status == "imported" && it.Destination != "" {
			skills[filepath.Base(it.Destination)] = SkillTreeDigest(it.Destination)
		}
	}
	names := make([]string, 0, len(skills))
	for n := range skills {
		names = append(names, n)
	}
	sort.Strings(names)
	imported := jsonutil.NewObject()
	for _, n := range names {
		imported.Set(n, skills[n])
	}
	entry.Set("source", sourceRoot)
	entry.Set("overwrite", overwrite)
	entry.Set("last_import", time.Now().Unix())
	entry.Set("imported_skills", imported)
	if _, has := entry.Get("digest"); refreshDigest || !has {
		entry.Set("digest", ComputeSourceDigest(agent, sourceRoot))
	}
	agents.Set(agent, entry)
	return SaveSyncManifest(ctx, targetRoot, manifest)
}

// Re-imports every registered source whose digest changed, prompt free, a dry run touching nothing
func syncImportedAgents(ctx context.Context, args Args) error {
	c := args.Console
	h, err := pathutil.Resolve(home.Dir(ctx))
	if err != nil {
		h = home.Dir(ctx)
	}
	agents, _ := asObject(LoadSyncManifest(h).Value("agents"))
	if agents == nil || agents.Len() == 0 {
		fmt.Fprintln(c.Out)
		c.PrintInfo("No import sources registered yet.")
		c.PrintInfo("Run 'icebath import-agent' first — successful imports are registered for sync automatically.")
		return nil
	}
	fmt.Fprintln(c.Out)
	c.PrintHeader("Import Sync")
	names := append([]string(nil), agents.Keys()...)
	sort.Strings(names)
	synced, unchanged, failed := 0, 0, 0
	for _, name := range names {
		entry, ok := asObject(agents.Value(name))
		if !ok || !truthy(entry.Value("source")) {
			continue
		}
		sourceDir := pyStr(entry.Value("source"))
		if !isDir(sourceDir) {
			c.PrintInfo(fmt.Sprintf("%s: source %s no longer exists — skipped", name, sourceDir))
			continue
		}
		if d, ok := entry.Value("digest").(string); ok && ComputeSourceDigest(name, sourceDir) == d {
			c.PrintInfo(name + ": unchanged since last import")
			unchanged++
			continue
		}
		suffix := " — re-importing"
		if args.DryRun {
			suffix = " (dry run)"
		}
		c.PrintInfo(fmt.Sprintf("%s: changes detected in %s%s", name, sourceDir, suffix))
		overwrite := truthy(entry.Value("overwrite"))
		report, err := func() (*Report, error) {
			imp, err := NewAgentImporter(ctx, name, sourceDir, h, !args.DryRun, overwrite, ManagedSkills(entry))
			if err != nil {
				return nil, err
			}
			return imp.Run()
		}()
		if err != nil {
			c.PrintError(fmt.Sprintf("%s: sync failed: %s", name, pyErrText(err)))
			pkgLog().Debug("import-agent sync error", "error", err)
			failed++
			continue
		}
		printImportReport(ctx, c, report, args.DryRun, false)
		hadErrors := report.Summary["error"] > 0
		if hadErrors {
			failed++
		}
		if !args.DryRun {
			if err := UpdateSyncManifest(ctx, name, sourceDir, h, overwrite, report, !hadErrors); err != nil {
				pkgLog().Warn(fmt.Sprintf("Could not update import sync manifest: %s", pyErrText(err)))
			}
		}
		synced++
	}
	fmt.Fprintln(c.Out)
	if synced == 0 && failed == 0 {
		c.PrintSuccess(fmt.Sprintf("All %d source(s) already up to date.", unchanged))
		return nil
	}
	parts := []string{fmt.Sprintf("%d synced", synced)}
	if unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", unchanged))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	c.PrintSuccess("Sync complete: " + strings.Join(parts, ", ") + ".")
	return nil
}
