package plugin

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Bytes an extracted bundle may hold at most
const bundleLimit = 256 << 20

// Manifest of an MCP bundle, the server it starts and the settings it needs
type bundleManifest struct {
	Name   string `json:"name"`
	Server struct {
		Type       string `json:"type"`
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcp_config"`
	} `json:"server"`
	UserConfig map[string]Option `json:"user_config"`
}

// Extracts a .mcpb or .dxt bundle into the cache and reads the server it declares
func readBundle(path, cache string) ([]*Server, error) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dir := filepath.Join(cache, name)
	if err := extractZip(path, dir); err != nil {
		return nil, fmt.Errorf("bundle %s: %w", path, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("bundle %s: manifest.json: %w", path, err)
	}
	var m bundleManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("bundle %s: manifest.json: %w", path, err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("bundle %s: manifest names no server", path)
	}
	cfg := m.Server.MCPConfig
	vars := map[string]string{"__dirname": dir}
	s := &Server{Name: m.Name, Transport: TransportStdio, Command: substituteVars(cfg.Command, vars), Dir: dir, UserConfig: m.UserConfig}
	for _, a := range cfg.Args {
		s.Args = append(s.Args, substituteVars(a, vars))
	}
	if len(cfg.Env) > 0 {
		s.Env = map[string]string{}
		for k, v := range cfg.Env {
			s.Env[k] = substituteVars(v, vars)
		}
	}
	if s.Command == "" {
		switch m.Server.Type {
		case "node":
			s.Command, s.Args = "node", []string{filepath.Join(dir, m.Server.EntryPoint)}
		case "python":
			s.Command, s.Args = "python3", []string{filepath.Join(dir, m.Server.EntryPoint)}
		case "binary":
			s.Command = filepath.Join(dir, m.Server.EntryPoint)
		default:
			return nil, fmt.Errorf("bundle %s: server type %q names no command", path, m.Server.Type)
		}
	}
	return []*Server{s}, nil
}

// Extracts a zip into the directory, refusing entries that escape it, skipping when the directory already holds it
func extractZip(path, dir string) error {
	marker := filepath.Join(dir, ".extracted")
	if st, err := os.Stat(path); err == nil {
		if data, err := os.ReadFile(marker); err == nil && string(data) == st.ModTime().UTC().String() {
			return nil
		}
	}
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var total int64
	for _, f := range r.File {
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(filepath.Separator)) && target != filepath.Clean(dir) {
			return fmt.Errorf("entry %q escapes the directory", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractOne(f, target, &total); err != nil {
			return err
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(st.ModTime().UTC().String()), 0o644)
}

func extractOne(f *zip.File, target string, total *int64) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(rc, bundleLimit-*total+1))
	*total += n
	if *total > bundleLimit {
		return errors.New("bundle is larger than 256 MiB")
	}
	return err
}
