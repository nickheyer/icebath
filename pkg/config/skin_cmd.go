package config

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var skinHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}\n?$`)

// Parsed skin command line, an empty command meaning list
type SkinArgs struct {
	Command string
	Name    string
	Key     string
	Value   string
	Skin    string
}

// Name of the skin display.skin selects, default when unset or empty
func activeSkinFromConfig(ctx context.Context) (string, error) {
	cfg, err := LoadContext(ctx, home.Dir(ctx), EnvRefLookup)
	if err != nil {
		return "", err
	}
	display := map[string]any{}
	if v := cfg["display"]; PyTruthy(v) {
		m, ok := v.(map[string]any)
		if !ok {
			return "", fmt.Errorf("'%s' object has no attribute 'get'", PyTypeName(v))
		}
		display = m
	}
	if skin := display["skin"]; PyTruthy(skin) {
		return PyStr(skin), nil
	}
	return "default", nil
}

// Persists display.skin through the shared config set writer
func useSkin(ctx context.Context, name string, src ConfigCommandSources) error {
	return ConfigCommand(ctx, home.Dir(ctx), ConfigArgs{Command: "set", Key: "display.skin", Value: &name, Force: true}, src)
}

// Shallow copy of a mapping keeping its key order
func orderedCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	SetMappingKeys(out, MappingKeys(m))
	return out
}

// Sets one hex color of a user skin in place or of a forked copy of a built in, then activates the fork
func skinSet(ctx context.Context, key, value, skin string, src ConfigCommandSources, stdout, stderr io.Writer) (int, error) {
	if !skinHexRe.MatchString(value) {
		fmt.Fprintf(stderr, "✗ %s is not a #rrggbb hex color\n", pyrepr.StrRepr(value))
		return 1, nil
	}
	name := skin
	if name == "" {
		active, err := activeSkinFromConfig(ctx)
		if err != nil {
			return 0, err
		}
		name = active
	}
	path := filepath.Join(skinsDir(ctx), name+".yaml")
	var data map[string]any
	target := name
	if _, statErr := os.Stat(path); statErr == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		doc, err := FastSafeLoad(raw)
		if err != nil {
			return 0, err
		}
		if !PyTruthy(doc) {
			doc = map[string]any{}
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("'%s' object has no attribute 'get'", PyTypeName(doc))
		}
		data = m
	} else {
		resolved := LoadSkin(ctx, name)
		target = name + "-custom"
		path = filepath.Join(skinsDir(ctx), target+".yaml")
		data = map[string]any{
			"name":        target,
			"description": name + " + custom " + key,
			"colors":      orderedCopy(resolved.Colors),
			"branding":    orderedCopy(resolved.Branding),
			"tool_prefix": resolved.ToolPrefix,
		}
		SetMappingKeys(data, []string{"name", "description", "colors", "branding", "tool_prefix"})
	}
	colors, ok := data["colors"].(map[string]any)
	if !ok {
		if _, present := data["colors"]; !present {
			AppendMappingKey(data, "colors")
		}
		colors = map[string]any{}
		data["colors"] = colors
	}
	if _, present := colors[key]; !present {
		AppendMappingKey(colors, key)
	}
	colors[key] = value
	if _, present := data["name"]; !present {
		AppendMappingKey(data, "name")
		data["name"] = target
	}
	if err := AtomicYAMLWrite(path, data, ""); err != nil {
		return 0, err
	}
	if target != name {
		if err := useSkin(ctx, target, src); err != nil {
			return 0, err
		}
	}
	fmt.Fprintf(stdout, "✓ %s = %s in %s/skins/%s.yaml (live within ~1s)\n", key, value, home.Display(home.Dir(ctx)), target)
	return 0, nil
}

// Prints every skin with the active one starred, its padded name, source and description
func skinList(ctx context.Context, stdout io.Writer) (int, error) {
	active, err := activeSkinFromConfig(ctx)
	if err != nil {
		return 0, err
	}
	for _, s := range ListSkins(ctx) {
		mark := " "
		if pyEq(s.Name, active) {
			mark = "*"
		}
		name, err := pyrepr.Format(s.Name, "<16")
		if err != nil {
			return 0, err
		}
		source, _ := pyrepr.Format(s.Source, "<8")
		fmt.Fprintf(stdout, "%s %s %s %s\n", mark, name, source, pyrepr.Str(s.Description))
	}
	return 0, nil
}

// Runs icebath skin list, use or set and returns the process exit status
func SkinCommand(ctx context.Context, args SkinArgs, src ConfigCommandSources, stdout, stderr io.Writer) (int, error) {
	switch args.Command {
	case "set":
		return skinSet(ctx, args.Key, args.Value, args.Skin, src, stdout, stderr)
	case "use":
		if err := useSkin(ctx, args.Name, src); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "✓ active skin → %s (live within ~1s)\n", args.Name)
		return 0, nil
	}
	return skinList(ctx, stdout)
}
