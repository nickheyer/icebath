package plugin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Output style, instructions that stand in for the conduct sections of the system prompt
type Style struct {
	// Full name, plugin:name for a plugin style
	Name        string
	Short       string
	Description string
	// Keeps the built in coding instructions beside the style
	KeepCoding bool
	// Applies on its own while the plugin is enabled
	Force  bool
	Body   string
	File   string
	Plugin *Plugin
	Origin string
}

// Parses an output style file, the file name standing in for a missing name
func ParseStyle(text, path string) *Style {
	f := ParseFront(text)
	s := &Style{File: path, Body: strings.TrimSpace(f.Body)}
	s.Short = strings.TrimSpace(f.String("name"))
	if s.Short == "" {
		s.Short = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	s.Description = strings.TrimSpace(f.String("description"))
	s.KeepCoding = f.Bool("keep-coding-instructions", false)
	s.Force = f.Bool("force-for-plugin", false)
	return s
}

// Reads every style file under a directory, or one file
func readStyles(path string) ([]*Style, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	if st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	} else {
		files = []string{path}
	}
	sort.Strings(files)
	var out []*Style
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		out = append(out, ParseStyle(string(data), file))
	}
	return out, nil
}
