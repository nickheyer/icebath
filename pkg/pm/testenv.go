package pm

import (
	"context"
	"path/filepath"
	"strings"
)

// Side environment name of a checkout's test suite
const testenvName = "test-environment"

// Extras the developer test environment selects by default, the app's normal features
var defaultTestExtras = []string{"all"}

// Dependency groups the test environment carries beside the locked project
var testenvGroups = []string{"dev", "test"}

// Directory of a checkout's test environment beside its install state
func TestenvRoot(ctx context.Context, projectRoot string) string {
	return filepath.Join(InstallStateDir(ctx, projectRoot), testenvName)
}

// Builds the test environment unless its locked inputs are unchanged and returns its interpreter, nil extras meaning the defaults
func EnsureTestenv(ctx context.Context, projectRoot string, extras []string) (string, error) {
	if extras == nil {
		extras = defaultTestExtras
	}
	return EnsureProjectEnvironment(ctx, EnsureProjectEnvironmentOptions{Name: testenvName, Project: projectRoot,
		Extras: sortedSet(extras), Groups: testenvGroups, Root: TestenvRoot(ctx, projectRoot), Explicit: true})
}

// Selected test interpreter read without acquiring tools or writing state, empty when none is selected
func TestenvPython(ctx context.Context, projectRoot string) (string, error) {
	python, ok, err := environmentPython(ctx, testenvName, TestenvRoot(ctx, projectRoot))
	if err != nil || !ok {
		return "", err
	}
	return python, nil
}

// Extras an override value names split on commas and whitespace, nil selecting the defaults
func ParseTestExtras(value string) []string {
	fields := strings.FieldsFunc(strings.ReplaceAll(value, ",", " "), pyIsSpace)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
