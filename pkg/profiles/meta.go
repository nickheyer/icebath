package profiles

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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

// Mapping reader the profile file readers call, the module attribute tests count calls through
var loadYAMLDict = readYAMLDictFile

// Mapping in a YAML file, nil when the file is missing, unreadable or not a mapping
func readYAMLDictFile(path string) map[string]any {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	doc, err := config.FastSafeLoad(data)
	if err != nil {
		return nil
	}
	if !truthy(doc) {
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

// Whether a YAML value is set: false for null, false, zero numbers, empty text, lists and mappings
func truthy(v any) bool { return anyutil.IsTrue(v) }

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
			descAuto = truthy(v)
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

// Text of a YAML value that is set, as YAML writes a scalar; empty for null, false, zero and empty values
func strOr(v any) string {
	if !truthy(v) {
		return ""
	}
	return config.ShowValue(v)
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
func WriteProfileMeta(ctx context.Context, profileDir string, upd MetaUpdate) error {
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
	return config.AtomicYAMLWrite(ctx, path, existing, "")
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
func SetProfileDisplayName(ctx context.Context, profileName, displayName string) (string, error) {
	_, dir, err := ExistingProfileDir(profileName)
	if err != nil {
		return "", err
	}
	cleaned := strings.TrimSpace(displayName)
	if n := utf8.RuneCountInString(cleaned); n > 64 {
		return "", &ValueError{fmt.Sprintf("Display name too long (%d chars, max 64).", n)}
	}
	if err := WriteProfileMeta(ctx, dir, MetaUpdate{DisplayName: &cleaned}); err != nil {
		return "", err
	}
	return cleaned, nil
}
