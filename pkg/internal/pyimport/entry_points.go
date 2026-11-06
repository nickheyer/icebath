package pyimport

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One entry point of an installed distribution as importlib.metadata reports it
type EntryPoint struct {
	Name, Value, Group string
	// Dist-info or egg-info directory, or the archive member prefix, that declared it
	Dist string
}

var distNameSeparators = regexp.MustCompile(`[-_.]+`)

// Distribution key importlib.metadata deduplicates on, the normalized name before the first dash of the info dir
func distKey(infoName string) string {
	low := strings.ToLower(infoName)
	stem := low[:strings.LastIndex(low, ".")]
	name, _, _ := strings.Cut(stem, "-")
	return strings.ReplaceAll(strings.ToLower(distNameSeparators.ReplaceAllString(name, "-")), "-", "_")
}

// Entry points of one group across the distributions found on path, the first distribution of a name winning
func EntryPoints(path []string, group string) []EntryPoint {
	var out []EntryPoint
	seen := map[string]bool{}
	for _, entry := range path {
		if entry == "" {
			entry = "."
		}
		for _, info := range distInfos(entry) {
			key := distKey(info.name)
			if seen[key] {
				continue
			}
			seen[key] = true
			text, ok := info.read("entry_points.txt")
			if !ok {
				continue
			}
			for _, ep := range parseEntryPoints(text) {
				if ep.Group == group {
					ep.Dist = info.dir
					out = append(out, ep)
				}
			}
		}
	}
	return out
}

// Entry points declared by an entry_points.txt text, sections as groups
func parseEntryPoints(text string) []EntryPoint {
	var out []EntryPoint
	group := ""
	for _, line := range strings.Split(pystr.TranslateNewlines(strings.TrimPrefix(text, "\xef\xbb\xbf")), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			group = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		out = append(out, EntryPoint{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value), Group: group})
	}
	return out
}

type distInfo struct {
	name, dir string
	read      func(file string) (string, bool)
}

// Info directories of one sys.path entry, a directory or a zip archive, in name order
func distInfos(entry string) []distInfo {
	isInfo := func(name string) bool {
		low := strings.ToLower(name)
		return strings.HasSuffix(low, ".dist-info") || strings.HasSuffix(low, ".egg-info")
	}
	var out []distInfo
	if st, err := os.Stat(entry); err == nil && !st.IsDir() {
		zr, err := zip.OpenReader(entry)
		if err != nil {
			return nil
		}
		defer zr.Close()
		members := map[string]string{}
		var names []string
		for _, f := range zr.File {
			top, rest, _ := strings.Cut(f.Name, "/")
			if !isInfo(top) {
				continue
			}
			if _, ok := members[top+"/"]; !ok {
				members[top+"/"] = ""
				names = append(names, top)
			}
			if rest == "" || f.FileInfo().IsDir() {
				continue
			}
			if rc, err := f.Open(); err == nil {
				raw, rerr := io.ReadAll(rc)
				rc.Close()
				if rerr == nil {
					members[f.Name] = pystr.DecodeUTF8Replace(raw)
				}
			}
		}
		sort.Strings(names)
		for _, name := range names {
			prefix := name + "/"
			out = append(out, distInfo{name: name, dir: filepath.Join(entry, name), read: func(file string) (string, bool) {
				text, ok := members[prefix+file]
				return text, ok
			}})
		}
		return out
	}
	children, err := os.ReadDir(entry)
	if err != nil {
		return nil
	}
	for _, child := range children {
		if !isInfo(child.Name()) {
			continue
		}
		dir := filepath.Join(entry, child.Name())
		out = append(out, distInfo{name: child.Name(), dir: dir, read: func(file string) (string, bool) {
			raw, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				return "", false
			}
			return pystr.DecodeUTF8Replace(raw), true
		}})
	}
	return out
}
