package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

var skinHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Store) SetSkinColor(ctx context.Context, key, value, skin string) ([]Line, error) {
	if !skinHex.MatchString(value) {
		return nil, &CommandError{Text: fmt.Sprintf("✗ %s is not a #rrggbb hex color", quoted(value)), Err: ErrWriteRefused}
	}
	name := skin
	if name == "" {
		cfg, err := s.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		name = storedSkin(cfg)
	}
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := s.skinsDir(ctx)
	if err != nil {
		return nil, err
	}
	target := name
	path := filepath.Join(dir, name+".yaml")
	var data *yaml.Node
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if data, err = readSkinForEdit(path); err != nil {
			return nil, err
		}
	case errors.Is(statErr, fs.ErrPermission):
		return nil, statErr
	default:
		resolved, err := s.LoadSkin(ctx, name)
		if err != nil {
			return nil, err
		}
		target = name + "-custom"
		path = filepath.Join(dir, target+".yaml")
		data = emptyMapping()
		putKey(data, "name", stringNode(target))
		putKey(data, "description", stringNode(fmt.Sprintf("%s + custom %s", name, key)))
		putKey(data, "colors", textMapNode(resolved.Colors))
		putKey(data, "branding", textMapNode(resolved.Branding))
		putKey(data, "tool_prefix", stringNode(resolved.ToolPrefix))
	}
	colors := childOf(data, "colors")
	if !isMapping(colors) {
		colors = emptyMapping()
		putKey(data, "colors", colors)
	}
	putKey(colors, key, stringNode(value))
	if childOf(data, "name") == nil {
		putKey(data, "name", stringNode(target))
	}
	out, err := DumpYAML(data)
	if err != nil {
		return nil, err
	}
	if err := s.homes.Mkdir(ctx, dir); err != nil {
		return nil, err
	}
	if err := fileutil.WriteFile(ctx, path, out, fileutil.KeepOwner(),
		fileutil.TempPrefix("."+target+"_"), fileutil.TempSuffix(".tmp")); err != nil {
		return nil, err
	}
	var lines []Line
	if target != name {
		res, err := s.SetValue(ctx, "display.skin", target, true)
		if res != nil {
			lines = append(lines, res.Lines...)
		}
		if err != nil {
			return lines, err
		}
	}
	shown, err := home.Display(h)
	if err != nil {
		return lines, err
	}
	return append(lines, plainLine(fmt.Sprintf("✓ %s = %s in %s/skins/%s.yaml (live within ~1s)", key, value, shown, target))), nil
}

func readSkinForEdit(path string) (*yaml.Node, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := LoadYAML(raw, &root); err != nil {
		return nil, err
	}
	switch {
	case root.Kind == 0 || isNull(&root):
		return emptyMapping(), nil
	case root.Kind == yaml.MappingNode:
		return &root, nil
	case root.Kind == yaml.SequenceNode && len(root.Content) == 0:
		return emptyMapping(), nil
	case root.Kind == yaml.ScalarNode && falsyScalar(&root):
		return emptyMapping(), nil
	}
	return nil, fmt.Errorf("%w: skin file %s holds a %s, not a mapping", ErrShape, path, typeName(&root))
}

func falsyScalar(n *yaml.Node) bool {
	tag, text := scalarKind(n)
	switch tag {
	case tagStr:
		return text == ""
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		return v != "true"
	case tagInt:
		v, ok := constructInt(text)
		return ok && v.Sign() == 0
	case tagFloat:
		f, ok := constructFloat(text)
		return ok && f == 0
	}
	return false
}

func textMapNode(m Map[string]) *yaml.Node {
	n := emptyMapping()
	for _, k := range m.Keys() {
		v, _ := m.Get(k)
		putKey(n, k, stringNode(v))
	}
	return n
}

func (s *Store) UseSkin(ctx context.Context, name string) ([]Line, error) {
	res, err := s.SetValue(ctx, "display.skin", name, true)
	var lines []Line
	if res != nil {
		lines = append(lines, res.Lines...)
	}
	if err != nil {
		return lines, err
	}
	return append(lines, plainLine(fmt.Sprintf("✓ active skin → %s (live within ~1s)", name))), nil
}

func (s *Store) SkinListLines(ctx context.Context) ([]Line, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	active := storedSkin(cfg)
	skins, err := s.ListSkins(ctx)
	if err != nil {
		return nil, err
	}
	lines := make([]Line, 0, len(skins))
	for _, k := range skins {
		mark := " "
		if k.Name == active {
			mark = "*"
		}
		lines = append(lines, plainLine(fmt.Sprintf("%s %-16s %-8s %s", mark, k.Name, string(k.Source), k.Description)))
	}
	return lines, nil
}
