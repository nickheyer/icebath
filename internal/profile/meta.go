package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// File in a profile's folder that tells about the profile: what it is for, the name it shows, the names it had
const metaFile = "profile.yaml"

// What profile.yaml tells about a profile
type Meta struct {
	Description string
	// Description written by the helper model rather than a person
	DescriptionAuto bool
	DisplayName     string
	// Title its bot shows, kept by the bots of icebath
	BotTitle string
	// Names it had before it was renamed, oldest first
	PreviousNames []string
}

// Fields of profile.yaml to change, nil keeping what it holds
type MetaUpdate struct {
	Description     *string
	DescriptionAuto *bool
	DisplayName     *string
	PreviousNames   *[]string
}

var bom = []byte("\xef\xbb\xbf")

// Mapping a YAML file holds, nil when it is not there, does not read or holds no mapping
func readYAML(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	var doc any
	if yaml.Unmarshal(bytes.TrimPrefix(data, bom), &doc) != nil {
		return nil
	}
	if doc == nil {
		return map[string]any{}
	}
	m, _ := doc.(map[string]any)
	return m
}

// Whether a YAML value is set: not null, false, a zero number, or an empty text, list or mapping
func truthy(v any) bool {
	if v == nil {
		return false
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Bool:
		return value.Bool()
	case reflect.String, reflect.Slice, reflect.Map:
		return value.Len() > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return value.Float() != 0
	}
	return true
}

// Text of a YAML value that is set, empty for one that is not
func text(v any) string {
	if !truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Names of a list of previous names, trimmed and each once
func cleanNames(raw any) []string {
	items, _ := raw.([]any)
	var out []string
	for _, item := range items {
		if name := strings.TrimSpace(text(item)); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// What the profile folder's profile.yaml tells, empty for one missing or broken
func ReadMeta(dir string) Meta {
	data := readYAML(filepath.Join(dir, metaFile))
	meta := Meta{Description: strings.TrimSpace(text(data["description"])), DescriptionAuto: truthy(data["description_auto"]),
		DisplayName: strings.TrimSpace(text(data["display_name"])), PreviousNames: cleanNames(data["previous_names"])}
	if ui, ok := data["ui_meta"].(map[string]any); ok {
		if bots, ok := ui["icebath-bots"].(map[string]any); ok {
			meta.BotTitle = strings.TrimSpace(text(bots["title"]))
		}
	}
	return meta
}

// Top mapping of a YAML file as a node, so a change keeps the order and comments of the rest; an empty one for a file missing, broken or
// holding no mapping
func mappingNode(path string) *yaml.Node {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	if err == nil && utf8.Valid(data) && yaml.Unmarshal(bytes.TrimPrefix(data, bom), &doc) == nil && len(doc.Content) == 1 &&
		doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

// Sets the key of a mapping node to the value, in its place when it is there and last when not; nil removes it
func put(mapping *yaml.Node, key string, value any) error {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if value == nil {
			mapping.Content = slices.Delete(mapping.Content, i, i+2)
			return nil
		}
		return mapping.Content[i+1].Encode(value)
	}
	if value == nil {
		return nil
	}
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return err
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
	return nil
}

// Changes the fields given of the profile folder's profile.yaml, keeping everything else it holds in its order
func WriteMeta(dir string, update MetaUpdate) error {
	if !fsx.IsDir(dir) {
		return &NotFoundError{fmt.Sprintf("The folder of the profile, %s, does not exist.", dir)}
	}
	path := filepath.Join(dir, metaFile)
	mapping := mappingNode(path)
	var errs []error
	if update.Description != nil {
		errs = append(errs, put(mapping, "description", strings.TrimSpace(*update.Description)))
	}
	if update.DescriptionAuto != nil {
		errs = append(errs, put(mapping, "description_auto", *update.DescriptionAuto))
	}
	if update.DisplayName != nil {
		var value any
		if name := strings.TrimSpace(*update.DisplayName); name != "" {
			value = name
		}
		errs = append(errs, put(mapping, "display_name", value))
	}
	if update.PreviousNames != nil {
		items := make([]any, len(*update.PreviousNames))
		for i, name := range *update.PreviousNames {
			items[i] = name
		}
		var value any
		if names := cleanNames(items); len(names) > 0 {
			value = names
		}
		errs = append(errs, put(mapping, "previous_names", value))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	out, err := yaml.Marshal(mapping)
	if err != nil {
		return err
	}
	return fsx.Replace(path, out, 0o644)
}
