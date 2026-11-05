package pm

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FeaturesFilename is the enabled features file beside a payload or the runtime store
const FeaturesFilename = "enabled-features.json"

// FeatureProbeError is a failed inventory, never read as an empty feature list
type FeatureProbeError struct{ Message string }

func (e *FeatureProbeError) Error() string { return e.Message }

// RuntimeError class callers catch it as
func (e *FeatureProbeError) Unwrap() error {
	return &PyError{Class: "RuntimeError", Message: e.Message}
}

// Enabled features file in baseDir, or beside the byte store of this process when baseDir is empty
func FeaturesPath(ctx context.Context, baseDir string) (string, error) {
	if baseDir != "" {
		return filepath.Join(baseDir, FeaturesFilename), nil
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(store), FeaturesFilename), nil
}

// Records the exact extras an install carries, sorted and deduplicated
func WriteFeatures(ctx context.Context, extras []string, baseDir string) (string, error) {
	path, err := FeaturesPath(ctx, baseDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	doc := NewOrderedObject()
	doc.Set("schema", 1)
	doc.Set("extras", sortedSet(extras))
	data, err := dumpsJSON(doc, true)
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o666)
}

// Frozen or baseline feature list, nil when no readable file exists
func readFeatures(ctx context.Context) ([]string, error) {
	path, err := FeaturesPath(ctx, "")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return nil, nil
	}
	doc, err := loadJSON(data)
	if err != nil {
		return nil, nil
	}
	record, ok := doc.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(doc))}
	}
	list, ok := record["extras"].([]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, len(list))
	for i, item := range list {
		names[i] = pyStr(item)
	}
	return sortedSet(names), nil
}

// Parsed pyproject of a source tree the way tomllib load reads it
func loadPyproject(repoDir string) (*OrderedObject, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "pyproject.toml"))
	if err != nil {
		return nil, err
	}
	if err := utf8Strict(data); err != nil {
		return nil, err
	}
	return tomlLoads(string(data))
}

// Keys of the optional dependencies table of a parsed pyproject
func optionalDependencyNames(data *OrderedObject) ([]string, error) {
	project, err := tomlGet(data, "project", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	table, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	items, err := tomlIter(table)
	if err != nil {
		return nil, err
	}
	return tomlStrings(items)
}

// Strings of decoded TOML items, a TypeError for a value Python could not sort with them
func tomlStrings(items []any) ([]string, error) {
	out := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'<' not supported between instances of '%s' and 'str'", tomlTypeName(item))}
		}
		out[i] = s
	}
	return out, nil
}

// Every extra the source tree's pyproject declares, sorted
func declaredExtras(repoDir string) ([]string, error) {
	data, err := loadPyproject(repoDir)
	if err != nil {
		return nil, err
	}
	names, err := optionalDependencyNames(data)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// Extras only an explicit selection installs, a ValueError when the opt in list names undeclared extras
func optInExtras(repoDir string) ([]string, error) {
	data, err := loadPyproject(repoDir)
	if err != nil {
		return nil, err
	}
	tool, err := tomlGet(data, "tool", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	own, err := tomlGet(tool, "icebath", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	raw, err := tomlGet(own, "opt-in-extras", []any{})
	if err != nil {
		return nil, err
	}
	items, err := tomlIter(raw)
	if err != nil {
		return nil, err
	}
	names, err := tomlStrings(items)
	if err != nil {
		return nil, err
	}
	declared, err := optionalDependencyNames(data)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range declared {
		known[name] = true
	}
	var unknown []string
	for _, name := range sortedSet(names) {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, &PyError{Class: "ValueError", Message: "[tool.icebath] opt-in-extras names undeclared extras: " + pyListRepr(unknown)}
	}
	return sortedSet(names), nil
}

//go:embed feature_probe.py
var featureProbeSource string

// Runs a feature inventory child, replaced by tests the way they patch subprocess run
var runFeatureProbe = func(ctx context.Context, argv []string, cwd string) (uvResult, error) {
	return runCaptured(ctx, argv, cwd, pyEnviron(), 60*time.Second)
}

// Extras whose every anchor the target tree resolves, probed in one isolated interpreter process
func InstalledExtras(ctx context.Context, repoDir, venvDir, python string) ([]string, error) {
	declared, err := declaredExtras(repoDir)
	if err != nil {
		return nil, err
	}
	required := map[string][]string{}
	var anchorList []string
	for _, extra := range declared {
		required[extra] = anchors(extra)
		anchorList = append(anchorList, required[extra]...)
	}
	anchorList = sortedSet(anchorList)
	encoded, err := json.Marshal(anchorList)
	if err != nil {
		return nil, err
	}
	argv := []string{python, "-B", "-I", "-S", "-c", featureProbeSource, resolvePath(venvDir), string(asciiEscape(pySeparators(encoded)))}
	result, err := runFeatureProbe(ctx, argv, repoDir)
	if err == nil && result.Code != 0 {
		err = fmt.Errorf("Command %s returned non-zero exit status %d.", pyListRepr(argv), result.Code)
	}
	if err != nil {
		detail := pyStrip(result.Stderr)
		if detail == "" {
			detail = pyStrip(pyExceptionText(err))
		}
		return nil, &FeatureProbeError{Message: fmt.Sprintf("feature inventory failed on %s: %s", python, detail)}
	}
	doc, err := loadJSON([]byte(result.Stdout))
	if err != nil {
		return nil, &FeatureProbeError{Message: "feature inventory returned invalid JSON"}
	}
	resolved, ok := doc.(map[string]any)
	incomplete := &FeatureProbeError{Message: "feature inventory returned incomplete anchor results"}
	if !ok || len(resolved) != len(anchorList) {
		return nil, incomplete
	}
	for _, anchor := range anchorList {
		if _, isBool := resolved[anchor].(bool); !isBool {
			return nil, incomplete
		}
	}
	var out []string
	for _, extra := range declared {
		all := true
		for _, anchor := range required[extra] {
			all = all && resolved[anchor].(bool)
		}
		if all {
			out = append(out, extra)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
