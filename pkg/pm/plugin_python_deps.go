package pm

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	requirementNameEnd = regexp.MustCompile(`[<>=!~\[;\s]`)
	distNameRuns       = regexp.MustCompile(`[-_.]+`)
)

// Distribution name a requirement string starts with
func RequirementDistName(req string) string {
	return pystr.Strip(requirementNameEnd.Split(req, 2)[0])
}

// Version of the distribution of a name inside a site directory, found false when none is installed there
func siteDistributionVersion(site, dist string) (version string, found bool, err error) {
	wanted := strings.ReplaceAll(strings.ToLower(distNameRuns.ReplaceAllString(dist, "-")), "-", "_")
	entries, err := os.ReadDir(site)
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		low := strings.ToLower(entry.Name())
		if !strings.HasSuffix(low, ".dist-info") && !strings.HasSuffix(low, ".egg-info") {
			continue
		}
		stem := low[:strings.LastIndex(low, ".")]
		name, _, _ := strings.Cut(stem, "-")
		if strings.ReplaceAll(name, "-", "_") != wanted {
			continue
		}
		metadata := "METADATA"
		if strings.HasSuffix(low, ".egg-info") {
			metadata = "PKG-INFO"
		}
		version, err := metadataVersion(filepath.Join(site, entry.Name(), metadata))
		return version, true, err
	}
	return "", false, nil
}

// Version header of a distribution metadata file, empty when it carries none
func metadataVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, "version") {
			return strings.TrimSpace(value), nil
		}
	}
	return "", scanner.Err()
}

// Whether a distribution the requirement accepts is installed in a site directory, known false when it cannot be probed
func DistInstalled(req, site string) (installed, known bool) {
	dist := RequirementDistName(req)
	if dist == "" {
		return false, false
	}
	version, found, err := siteDistributionVersion(site, dist)
	if err != nil {
		return false, false
	}
	if !found {
		return false, true
	}
	parsed, err := ParseRequirement(req)
	if err != nil {
		return true, true
	}
	accepted, err := specifierSetContains(parsed.Specifier, version)
	if err != nil {
		return true, true
	}
	return accepted, true
}

// Whether the environment marker of a requirement admits this machine, true for one that cannot be read
func RequirementAppliesHere(ctx context.Context, req string) bool {
	if !strings.Contains(req, ";") {
		return true
	}
	parsed, err := ParseRequirement(req)
	if err != nil || len(parsed.Marker) == 0 {
		return true
	}
	environment := markerEnvironment()
	var defaults map[string]string
	if markerNeedsDefaults(parsed.Marker, environment) {
		if defaults, err = loaderMarkerEnvironment(ctx); err != nil {
			return true
		}
	}
	holds, err := evaluateMarker(parsed.Marker, defaults, environment)
	return err != nil || holds
}

// Site packages directory of the dependency environment the install currently selects
func SelectedSitePackages(ctx context.Context) (string, error) {
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return "", err
	}
	return SitePackages(venv)
}

// One entry point an installed distribution declares, with the distribution it came from
type EntryPoint struct {
	Name        string
	Value       string
	Group       string
	DistName    string
	DistVersion string
	DistSummary string
}

// Value of a header of a distribution metadata file, empty when absent
func distMetadataHeader(path, header string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, header) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Entry points every distribution of the selected dependency environment declares, in directory order
func SelectedEntryPoints(ctx context.Context) ([]EntryPoint, error) {
	site, err := SelectedSitePackages(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(site)
	if err != nil {
		return nil, err
	}
	var out []EntryPoint
	for _, entry := range entries {
		low := strings.ToLower(entry.Name())
		metadata := ""
		switch {
		case strings.HasSuffix(low, ".dist-info"):
			metadata = "METADATA"
		case strings.HasSuffix(low, ".egg-info"):
			metadata = "PKG-INFO"
		default:
			continue
		}
		dir := filepath.Join(site, entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "entry_points.txt"))
		if err != nil {
			continue
		}
		metaPath := filepath.Join(dir, metadata)
		dist := EntryPoint{DistName: distMetadataHeader(metaPath, "Name"), DistVersion: distMetadataHeader(metaPath, "Version"),
			DistSummary: distMetadataHeader(metaPath, "Summary")}
		group := ""
		for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			line = pystr.Strip(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				group = line[1 : len(line)-1]
				continue
			}
			name, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			ep := dist
			ep.Name, ep.Value, ep.Group = pystr.Strip(name), pystr.Strip(value), group
			out = append(out, ep)
		}
	}
	return out, nil
}

// Import path of the selected dependency environment, its site directory then the directories its pth files add
func selectedImportPath(ctx context.Context) ([]string, error) {
	site, err := SelectedSitePackages(ctx)
	if err != nil {
		return nil, err
	}
	paths := []string{site}
	entries, err := os.ReadDir(site)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".pth") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(site, entry.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			line = strings.TrimRight(line, " \t")
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import\t") {
				continue
			}
			dir := line
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(site, dir)
			}
			if isDirectory(dir) {
				paths = append(paths, dir)
			}
		}
	}
	return paths, nil
}

// Source file of a module of the selected environment found without importing it, false when it has none
func ResolveModuleOrigin(ctx context.Context, moduleName string) (string, bool) {
	var parts []string
	for _, part := range strings.Split(moduleName, ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	search, err := selectedImportPath(ctx)
	if err != nil {
		return "", false
	}
	for i, part := range parts {
		origin := ""
		var next []string
		for _, base := range search {
			if init := filepath.Join(base, part, "__init__.py"); isRegularFile(init) {
				origin, next = init, []string{filepath.Join(base, part)}
				break
			}
			if file := filepath.Join(base, part+".py"); isRegularFile(file) {
				origin = file
				break
			}
		}
		if origin == "" {
			return "", false
		}
		if i == len(parts)-1 {
			return origin, true
		}
		if next == nil {
			return "", false
		}
		search = next
	}
	return "", false
}
