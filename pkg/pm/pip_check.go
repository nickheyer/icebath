//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var (
	pipReportLine      = regexp.MustCompile("^The package `[^`]+` requires ")
	pipVersionMismatch = regexp.MustCompile("^The package `[^`]+` requires `([A-Za-z0-9][A-Za-z0-9._-]*)[^`]*`, but `([^`]+)` is installed$")
	pipFound           = regexp.MustCompile(`(?m)^Found (\p{Nd}+) incompatibilit(?:y|ies)$`)
)

// PEP 503 spelling of a name, runs of separators collapsed and lowercased like Python lower
func pep503Name(name string) string {
	return strings.ToLower(pep503Runs.ReplaceAllString(name, "-"))
}

// Integer a run of Unicode decimal digits spells, as Python int reads it
func unicodeDecimal(digits string) int {
	n := 0
	for _, r := range digits {
		run := 0
		for unicode.Is(unicode.Nd, r-rune(run)-1) {
			run++
		}
		n = n*10 + run%10
		if n > 1<<40 {
			return -1
		}
	}
	return n
}

// Manifest overrides and the name and version of every locked package of a uv.lock text
func parseUVLock(text string) ([]any, [][2]string, error) {
	data, err := tomlLoads(text)
	if err != nil {
		return nil, nil, err
	}
	manifest, err := tomlGet(data, "manifest", NewOrderedObject())
	if err != nil {
		return nil, nil, err
	}
	overrides, err := tomlGet(manifest, "overrides", []any{})
	if err != nil {
		return nil, nil, err
	}
	entries, err := tomlIter(overrides)
	if err != nil {
		return nil, nil, err
	}
	listed, err := tomlGet(data, "package", []any{})
	if err != nil {
		return nil, nil, err
	}
	rows, err := tomlIter(listed)
	if err != nil {
		return nil, nil, err
	}
	var packages [][2]string
	for _, row := range rows {
		obj, ok := row.(*OrderedObject)
		if !ok {
			return nil, nil, &ClassError{Class: "TypeError", Message: fmt.Sprintf("a uv.lock package entry must be a table, got %s", tomlKind(row))}
		}
		version, has := obj.Values["version"]
		if !has {
			continue
		}
		name, err := tomlIndex(obj, "name")
		if err != nil {
			return nil, nil, err
		}
		packages = append(packages, [2]string{tomlText(name), tomlText(version)})
	}
	return entries, packages, nil
}

// Text of a decoded value the way str formats it
func tomlText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return tomlRepr(v)
}

// Reads a lock file as strict UTF-8
func readUTF8(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := utf8Strict(data); err != nil {
		return "", err
	}
	return string(data), nil
}

// Locked versions of every dependency the lock's manifest overrides, keyed by normalized name
func OverriddenVersions(lock string) (map[string]map[string]bool, error) {
	text, err := readUTF8(lock)
	if err != nil {
		return nil, err
	}
	entries, packages, err := parseUVLock(text)
	if err != nil {
		return nil, err
	}
	overridden := map[string]bool{}
	for _, entry := range entries {
		name, err := tomlIndex(entry, "name")
		if err != nil {
			return nil, err
		}
		s, ok := name.(string)
		if !ok {
			return nil, &ClassError{Class: "TypeError", Message: fmt.Sprintf("a uv.lock manifest override name must be text, got %s", tomlKind(name))}
		}
		overridden[pep503Name(s)] = true
	}
	versions := map[string]map[string]bool{}
	for _, pkg := range packages {
		key := pep503Name(pkg[0])
		if overridden[key] {
			if versions[key] == nil {
				versions[key] = map[string]bool{}
			}
			versions[key][pkg[1]] = true
		}
	}
	return versions, nil
}

// Report lines a lock does not account for, nil when the report cannot be read line by line
func UnexpectedIncompatibilities(report, lock string) ([]string, error) {
	var lines []string
	for _, line := range textutil.Lines(report) {
		if stripped := strings.TrimSpace(line); pipReportLine.MatchString(stripped) {
			lines = append(lines, stripped)
		}
	}
	found := pipFound.FindStringSubmatch(report)
	if len(lines) == 0 || found == nil || unicodeDecimal(found[1]) != len(lines) {
		return nil, nil
	}
	if lock == "" || !isRegularFile(lock) {
		return lines, nil
	}
	expected, err := OverriddenVersions(lock)
	if err != nil {
		return nil, err
	}
	unexpected := []string{}
	for _, line := range lines {
		m := pipVersionMismatch.FindStringSubmatch(line)
		if !(m != nil && expected[pep503Name(m[1])][m[2]]) {
			unexpected = append(unexpected, line)
		}
	}
	return unexpected, nil
}

// Reports whether any of the needles occurs in text
func containsAny(text string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}
