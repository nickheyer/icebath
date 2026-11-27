package cua

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// A systemd user unit or XDG autostart entry that runs cua-driver
type unit struct {
	kind, unit, exec string
	// It runs `cua-driver serve`, on the socket named or the default one
	serve  bool
	socket string
}

func userHome() string {
	if h := os.Getenv("HOME"); h != "" && !windows {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// Every systemd user unit and XDG autostart entry under the config folder whose command runs cua-driver
func units(dir string) []unit {
	home := userHome()
	base := cmp.Or(dir, os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	var out []unit
	for _, src := range []struct{ kind, dir, suffix, key string }{
		{"systemd user unit", filepath.Join(base, "systemd", "user"), ".service", "ExecStart"},
		{"XDG autostart entry", filepath.Join(base, "autostart"), ".desktop", "Exec"},
	} {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			if st, err := os.Stat(filepath.Join(src.dir, e.Name())); strings.HasSuffix(e.Name(), src.suffix) && err == nil && st.Mode().IsRegular() {
				names = append(names, e.Name())
			}
		}
		slices.Sort(names)
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(src.dir, name))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(strings.TrimPrefix(textOf(raw), "\ufeff"), "\n") {
				rest, ok := strings.CutPrefix(strings.TrimSpace(line), src.key+"=")
				if !ok {
					continue
				}
				tokens := strings.FieldsFunc(rest, unicode.IsSpace)
				if len(tokens) == 0 || !strings.Contains(tokens[0], "cua-driver") {
					continue
				}
				u := unit{kind: src.kind, unit: name, exec: strings.TrimLeft(tokens[0], "-+:!")}
				for i, t := range tokens {
					if t == "--socket" && i+1 < len(tokens) && u.socket == "" {
						u.socket = fsx.ExpandHome(strings.ReplaceAll(tokens[i+1], "%h", home))
					}
					u.serve = u.serve || i > 0 && t == "serve"
				}
				out = append(out, u)
			}
		}
	}
	return out
}

// Units whose cua-driver command points into a versioned release folder an upgrade pruned
func staleUnits(dir string) []unit {
	var out []unit
	for _, u := range units(dir) {
		resolved := fsx.ExpandHome(strings.ReplaceAll(u.exec, "%h", userHome()))
		if _, err := os.Stat(resolved); strings.Contains(resolved, "/packages/releases/") && err != nil {
			out = append(out, u)
		}
	}
	return out
}
