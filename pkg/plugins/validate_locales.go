package plugins

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var localeKeyExports = map[string]string{"tui": "_keys.tui.json", "desktop": "_keys.desktop.json"}

// English key set of a surface, nil when the bundled reference is unavailable
func ReferenceKeys(surface string) map[string]bool {
	keys, err := referenceKeys(surface)
	if err != nil {
		logDebug(fmt.Sprintf("i18n reference keys for %s unavailable: %v", surface, err))
		return nil
	}
	return keys
}

func referenceKeys(surface string) (map[string]bool, error) {
	locales := i18n.LocalesDir()
	if surface == i18n.CoreSurface {
		raw, err := fs.ReadFile(locales.FS, "en.yaml")
		if err != nil {
			return nil, err
		}
		flat, err := i18n.ParseLocaleYAML(filepath.Join(locales.Label, "en.yaml"), raw)
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for k := range flat {
			out[k] = true
		}
		return out, nil
	}
	export, known := localeKeyExports[surface]
	if !known {
		return nil, fmt.Errorf("KeyError: %s", pyrepr.Repr(surface))
	}
	info, err := fs.Stat(locales.FS, export)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := fs.ReadFile(locales.FS, export)
	if err != nil {
		return nil, err
	}
	if err := strictUTF8(raw); err != nil {
		return nil, err
	}
	data, err := pyjson.LoadsBytes(stripBOM(raw))
	if err != nil {
		return nil, err
	}
	keys := data
	if obj, ok := data.(*pyjson.Object); ok {
		keys, _ = obj.Get("keys")
	}
	list, ok := keys.([]any)
	if !ok {
		return nil, nil
	}
	out := map[string]bool{}
	for _, k := range list {
		out[pyrepr.Str(k)] = true
	}
	return out, nil
}

// Locale pack admission checks, nothing when provides_locales is absent
func checkLanguagePacks(report *ValidationReport, manifest map[string]any, pluginDir string) {
	raw, present := manifest["provides_locales"]
	key := filepath.Base(pluginDir)
	if config.PyTruthy(manifest["name"]) {
		key = config.PyStr(manifest["name"])
	}
	declared, _ := parseProvidesLocales(raw, key)
	if !present || raw == nil {
		return
	}
	if len(declared) == 0 {
		report.add("locales", false, "provides_locales declares no valid language id (expected e.g. pl, pt-br)")
		return
	}
	found := i18n.ScanLocaleDir(filepath.Join(pluginDir, "locales"))
	byLang := map[string][]i18n.LocaleFile{}
	for _, f := range found {
		byLang[f.Lang] = append(byLang[f.Lang], f)
	}
	for _, lang := range declared {
		files := byLang[lang]
		core := false
		for _, f := range files {
			if f.Surface == i18n.CoreSurface {
				core = true
			}
		}
		if !core {
			report.add("locale "+lang, false, fmt.Sprintf("provides_locales declares %s but locales/%s.yaml is missing", pyrepr.Repr(lang), lang))
			continue
		}
		for _, f := range files {
			checkLocaleFile(report, lang, f.Surface, f.Path)
		}
	}
	declaredSet := toSet(declared)
	undeclared := []string{}
	for lang := range byLang {
		if !declaredSet[lang] {
			undeclared = append(undeclared, lang)
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		report.warn(fmt.Sprintf("locales/ ships %s not listed in provides_locales "+
			"(loaded anyway; declare them so users can see the pack's languages)", strings.Join(undeclared, ", ")))
	}
}

// Parses one locale YAML document as hermes_yaml safe_load reads a utf-8-sig file
func loadLocaleDocument(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := strictUTF8(raw); err != nil {
		return nil, err
	}
	return config.FastSafeLoad(raw)
}

func checkLocaleFile(report *ValidationReport, lang, surface, path string) {
	label := "locale " + lang
	if surface != i18n.CoreSurface {
		label = fmt.Sprintf("locale %s.%s", lang, surface)
	}
	name := filepath.Base(path)
	document, err := loadLocaleDocument(path)
	if err != nil {
		report.add(label, false, fmt.Sprintf("%s failed to parse: %v", name, err))
		return
	}
	if document == nil {
		document = map[string]any{}
	}
	mapping, ok := document.(map[string]any)
	if !ok {
		report.add(label, false, fmt.Sprintf("%s must be a mapping, got %s", name, pyrepr.TypeName(document)))
		return
	}
	if bad := i18n.NonTextLeaves(mapping, ""); len(bad) > 0 {
		sorted := append([]string{}, bad...)
		sort.Strings(sorted)
		report.add(label, false, fmt.Sprintf("%s has non-text value(s) at: %s", name, strings.Join(sorted, ", ")))
		return
	}
	flat := i18n.Flatten(mapping, "", nil)
	reference := ReferenceKeys(surface)
	if reference == nil {
		report.add(label, true, fmt.Sprintf("%s: %d key(s); no %s key reference available, key check skipped", name, len(flat), surface))
		return
	}
	unknown := []string{}
	for k := range flat {
		if !reference[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		report.warn(fmt.Sprintf("%s: %d key(s) not in the English %s catalog (ignored at runtime): ", name, len(unknown), surface) +
			strings.Join(unknown, ", "))
	}
	report.add(label, true, fmt.Sprintf("%s: %d key(s), %d match the English %s catalog", name, len(flat), len(flat)-len(unknown), surface))
}
