package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

// Presentation metadata of a profile read from profile.yaml
type ProfileMeta struct {
	Description     string
	DescriptionAuto bool
	DisplayName     string
	BotTitle        string
	PreviousNames   []string
}

// Fields of profile.yaml to overwrite, nil fields keep their stored value
type MetaUpdate struct {
	Description     *string
	DescriptionAuto *bool
	DisplayName     *string
	PreviousNames   *[]string
}

// Mapping in a YAML file, nil when the file is missing, unreadable or not a mapping
func loadYAMLDict(path string) map[string]any {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	doc, err := config.FastSafeLoad(data)
	if err != nil {
		return nil
	}
	if !pyTruthy(doc) {
		return map[string]any{}
	}
	m, _ := doc.(map[string]any)
	return m
}

// Sets a key the way a Python dict assignment does, a new key lands at the end of the order
func setOrdered(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		config.AppendMappingKey(m, key)
	}
	m[key] = value
}

// Python truthiness of a YAML shaped value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case uint64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Python str of a YAML shaped value
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyReprOrdered(v)
}

// Python repr of a YAML shaped value keeping mapping insertion order
func pyReprOrdered(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := config.MappingKeys(x)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = config.PyRepr(k) + ": " + pyReprOrdered(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyReprOrdered(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return config.PyRepr(v)
}

// Profile metadata with empty defaults for a missing or corrupt file, cached by file signature
func ReadProfileMeta(profileDir string) ProfileMeta {
	path := filepath.Join(profileDir, "profile.yaml")
	meta := cachedProfileRead(path, "profile-meta", func() ProfileMeta {
		data := loadYAMLDict(path)
		if data == nil {
			data = map[string]any{}
		}
		botTitle := ""
		if ui, ok := data["ui_meta"].(map[string]any); ok {
			if bots, ok := ui["icebath-bots"].(map[string]any); ok {
				botTitle = strings.TrimSpace(strOr(bots["title"]))
			}
		}
		descAuto := false
		if v, ok := data["description_auto"]; ok {
			descAuto = pyTruthy(v)
		}
		return ProfileMeta{
			Description:     strings.TrimSpace(strOr(data["description"])),
			DescriptionAuto: descAuto,
			DisplayName:     strings.TrimSpace(strOr(data["display_name"])),
			BotTitle:        botTitle,
			PreviousNames:   cleanPreviousNames(data["previous_names"]),
		}
	}, nil)
	meta.PreviousNames = append([]string{}, meta.PreviousNames...)
	return meta
}

// Python str(value or "") of a YAML value
func strOr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

// Stripped string entries of a previous names list, deduplicated in order
func cleanPreviousNames(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	cleaned := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		name := strings.TrimSpace(strOr(item))
		if name != "" && !seen[name] {
			seen[name] = true
			cleaned = append(cleaned, name)
		}
	}
	return cleaned
}

// Updates profile.yaml in place overwriting only the given fields, the profile directory must exist
func WriteProfileMeta(profileDir string, upd MetaUpdate) error {
	if st, err := os.Stat(profileDir); err != nil || !st.IsDir() {
		return &NotFoundError{fmt.Sprintf("profile directory does not exist: %s", profileDir)}
	}
	path := filepath.Join(profileDir, "profile.yaml")
	existing := loadYAMLDict(path)
	if existing == nil {
		existing = map[string]any{}
	}
	if upd.Description != nil {
		setOrdered(existing, "description", strings.TrimSpace(*upd.Description))
	}
	if upd.DescriptionAuto != nil {
		setOrdered(existing, "description_auto", *upd.DescriptionAuto)
	}
	if upd.DisplayName != nil {
		if dn := strings.TrimSpace(*upd.DisplayName); dn != "" {
			setOrdered(existing, "display_name", dn)
		} else {
			delete(existing, "display_name")
		}
	}
	if upd.PreviousNames != nil {
		items := make([]any, len(*upd.PreviousNames))
		for i, s := range *upd.PreviousNames {
			items[i] = s
		}
		if cleaned := cleanPreviousNames(items); len(cleaned) > 0 {
			setOrdered(existing, "previous_names", cleaned)
		} else {
			delete(existing, "previous_names")
		}
	}
	return config.AtomicYAMLWrite(path, existing, "")
}

// Display name followed by the canonical id in parentheses, the bare id when they match or no name is set
func FormatProfileLabel(name, displayName string) string {
	dn := strings.TrimSpace(displayName)
	if dn != "" && dn != name {
		return fmt.Sprintf("%s (%s)", dn, name)
	}
	return name
}

// Stores or clears a presentation only display name, refusing names over 64 characters
func SetProfileDisplayName(profileName, displayName string) (string, error) {
	_, dir, err := ExistingProfileDir(profileName)
	if err != nil {
		return "", err
	}
	cleaned := strings.TrimSpace(displayName)
	if n := utf8.RuneCountInString(cleaned); n > 64 {
		return "", &ValueError{fmt.Sprintf("Display name too long (%d chars, max 64).", n)}
	}
	if err := WriteProfileMeta(dir, MetaUpdate{DisplayName: &cleaned}); err != nil {
		return "", err
	}
	return cleaned, nil
}
