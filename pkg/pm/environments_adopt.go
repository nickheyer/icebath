package pm

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Generation of this install, selected or superseded, whose site packages this process resolves imports from
func RunningEnvironment(ctx context.Context, projectRoot string) (string, error) {
	entries := map[string]bool{}
	for _, entry := range importPathEntries() {
		if entry != "" {
			entries[resolvePath(entry)] = true
		}
	}
	selected, err := SelectedVenv(ctx, projectRoot)
	if err != nil {
		return "", nil
	}
	candidates := append([]string{selected}, globUnder(filepath.Join(InstallStateDir(ctx, projectRoot), "environments"), filepath.Join("*", "venv"))...)
	for _, environment := range candidates {
		site, err := SitePackages(environment)
		if err != nil {
			return "", err
		}
		if entries[resolvePath(site)] {
			return environment, nil
		}
	}
	return "", nil
}

// Files live children launched from a generation run, counted per path until each child releases them
var generationHolds struct {
	sync.Mutex
	files map[string]int
}

// Records the files a child launched from a generation runs, held until the returned release
func HoldGenerationFiles(files []string) func() {
	generationHolds.Lock()
	if generationHolds.files == nil {
		generationHolds.files = map[string]int{}
	}
	for _, file := range files {
		generationHolds.files[file]++
	}
	generationHolds.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			generationHolds.Lock()
			defer generationHolds.Unlock()
			for _, file := range files {
				if generationHolds.files[file]--; generationHolds.files[file] <= 0 {
					delete(generationHolds.files, file)
				}
			}
		})
	}
}

// installedDistribution is one dist info entry, its version and the files its record lists resolved
type installedDistribution struct {
	version *string
	files   []string
}

// Header value of an RFC 822 metadata text, matched case insensitively, nil when absent
func metadataHeader(text, name string) *string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found || line[0] == ' ' || line[0] == '\t' || !strings.EqualFold(key, name) {
			continue
		}
		value = strings.TrimLeft(value, " \t")
		for _, more := range lines[i+1:] {
			if more == "" || (more[0] != ' ' && more[0] != '\t') {
				break
			}
			value += "\n" + more
		}
		return &value
	}
	return nil
}

// Readable text of a file the way importlib metadata reads it, empty when absent
func distributionText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

var distributionSeparators = regexp.MustCompile(`[-_.]+`)

// Distributions installed in a generation's site packages keyed by normalized name, as importlib metadata finds them
func generationDistributions(environment string) (map[string]installedDistribution, error) {
	site, err := SitePackages(environment)
	if err != nil {
		return nil, err
	}
	out := map[string]installedDistribution{}
	entries, err := os.ReadDir(site)
	if err != nil {
		return out, nil
	}
	for _, entry := range entries {
		low := strings.ToLower(entry.Name())
		if !strings.HasSuffix(low, ".dist-info") && !strings.HasSuffix(low, ".egg-info") {
			continue
		}
		path := filepath.Join(site, entry.Name())
		text := distributionText(filepath.Join(path, "METADATA"))
		if text == "" {
			text = distributionText(filepath.Join(path, "PKG-INFO"))
		}
		if text == "" {
			text = distributionText(path)
		}
		name := metadataHeader(text, "Name")
		if name == nil {
			return nil, &PyError{Class: "TypeError", Message: "expected string or bytes-like object, got 'NoneType'"}
		}
		dist := installedDistribution{version: metadataHeader(text, "Version")}
		if record := distributionText(filepath.Join(path, "RECORD")); record != "" {
			reader := csv.NewReader(strings.NewReader(record))
			reader.FieldsPerRecord = -1
			reader.LazyQuotes = true
			rows, _ := reader.ReadAll()
			for _, row := range rows {
				if len(row) > 0 {
					dist.files = append(dist.files, resolvePath(filepath.Join(site, filepath.FromSlash(row[0]))))
				}
			}
		} else if installed := distributionText(filepath.Join(path, "installed-files.txt")); installed != "" {
			for _, line := range pySplitLines(installed) {
				dist.files = append(dist.files, resolvePath(filepath.Join(path, filepath.FromSlash(line))))
			}
		} else if sources := distributionText(filepath.Join(path, "SOURCES.txt")); sources != "" {
			for _, line := range pySplitLines(sources) {
				dist.files = append(dist.files, resolvePath(filepath.Join(site, filepath.FromSlash(line))))
			}
		}
		out[pyLower(distributionSeparators.ReplaceAllString(*name, "-"))] = dist
	}
	return out, nil
}

// Held files of live children from the previous generation whose distribution changed or vanished in the selected one
func loadedFromChanged(previous, selected string) ([]string, error) {
	updated, err := generationDistributions(selected)
	if err != nil {
		return nil, err
	}
	prior, err := generationDistributions(previous)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for name, dist := range prior {
		next, ok := updated[name]
		same := ok && (next.version == nil) == (dist.version == nil) && (next.version == nil || *next.version == *dist.version)
		if !same {
			for _, file := range dist.files {
				changed[file] = true
			}
		}
	}
	generationHolds.Lock()
	defer generationHolds.Unlock()
	var loaded []string
	for _, file := range sortedKeys(generationHolds.files) {
		if (isRelativeTo(file, previous) || isRelativeTo(file, resolvePath(previous))) && changed[resolvePath(file)] {
			loaded = append(loaded, file)
		}
	}
	return loaded, nil
}

// Path list with every entry naming old replaced by new
func replacePathEntry(value, old, updated string) string {
	parts := strings.Split(value, string(os.PathListSeparator))
	for i, part := range parts {
		if part != "" && resolvePath(part) == resolvePath(old) {
			parts[i] = updated
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// Moves this process from previous onto selected, false when only a restart can load the selection
func Adopt(ctx context.Context, previous, selected, running string) (bool, error) {
	if resolvePath(running) != resolvePath(previous) {
		return false, nil
	}
	if resolvePath(selected) == resolvePath(previous) {
		return true, nil
	}
	loaded, err := loadedFromChanged(previous, selected)
	if err != nil || len(loaded) > 0 {
		return false, err
	}
	if _, err := LeaseGeneration(selected); err != nil {
		return false, err
	}
	old, err := SitePackages(previous)
	if err != nil {
		return false, err
	}
	updated, err := SitePackages(selected)
	if err != nil {
		return false, err
	}
	importPath.Lock()
	for i, entry := range importPath.entries {
		if entry != "" && resolvePath(entry) == resolvePath(old) {
			importPath.entries[i] = updated
		}
	}
	importPath.Unlock()
	if value := os.Getenv("PYTHONPATH"); value != "" {
		os.Setenv("PYTHONPATH", replacePathEntry(value, old, updated))
	}
	windows := runtime.GOOS == "windows"
	os.Setenv("PATH", replacePathEntry(os.Getenv("PATH"), VenvBinDir(previous, windows), VenvBinDir(selected, windows)))
	return true, nil
}

// Running and selected generations of the install, ok false when this process runs from none of them
func runningAndSelected(ctx context.Context, projectRoot string) (string, string, bool, error) {
	running, err := RunningEnvironment(ctx, projectRoot)
	if err != nil || running == "" {
		return "", "", false, err
	}
	selected, err := SelectedVenv(ctx, projectRoot)
	if err != nil {
		return "", "", false, nil
	}
	return running, selected, true, nil
}

// Why this process must restart to load the selected generation, empty when it runs it or runs from none
func RestartNeeded(ctx context.Context, projectRoot string) (string, error) {
	running, selected, ok, err := runningAndSelected(ctx, projectRoot)
	if err != nil || !ok || resolvePath(running) == resolvePath(selected) {
		return "", err
	}
	return fmt.Sprintf("dependency generation %s was published after this process loaded %s",
		filepath.Base(filepath.Dir(selected)), filepath.Base(filepath.Dir(running))), nil
}

var restartPending struct {
	sync.Mutex
	reason string
}

// Records that an install this process ran can only load in a new process
func NoteRestartPending(reason string) {
	restartPending.Lock()
	restartPending.reason = reason
	restartPending.Unlock()
}

// Reason a restart is pending, empty when none was noted
func RestartPending() string {
	restartPending.Lock()
	defer restartPending.Unlock()
	return restartPending.reason
}

// Moves this process onto the selected generation, true when it runs it, adopted it or runs from none
func AdoptSelected(ctx context.Context, projectRoot string) (bool, error) {
	running, selected, ok, err := runningAndSelected(ctx, projectRoot)
	if err != nil || !ok {
		return err == nil, err
	}
	return Adopt(ctx, running, selected, running)
}
