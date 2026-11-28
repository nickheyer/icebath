package cards

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Kind of path a field holds, which tab completes
type PathKind int

const (
	// No path: tab goes on to the next field
	PathNone PathKind = iota
	// A file, offered among the files and folders of the folder typed so far
	PathFile
	// A folder, offered among the folders alone
	PathFolder
	// A program: a bare name offered among the programs on PATH, anything else as a file
	PathProgram
)

// Completions of the path a word starts, as tab offers them: the entries of its folder whose names start with what follows its last slash,
// hidden ones only once a dot is typed, folders ending in a slash, at most limit of them. A word starting ~/ is in the home folder, ~ alone
// is that folder, and any other that is not absolute is in base. A bare word for a program is offered the programs on PATH it starts
func PathOffers(word, base string, kind PathKind, limit int) []Offer {
	if kind == PathProgram && word != "" && !strings.ContainsAny(word, `/\~`) {
		return programs(word, limit)
	}
	// The home folder typed as ~ alone is completed to the folder it stands for
	if word == "~" {
		return []Offer{{Text: "~/", Display: "~/"}}
	}
	target := word
	if rest, home := strings.CutPrefix(word, "~/"); home {
		if directory, err := os.UserHomeDir(); err == nil {
			target = directory + string(filepath.Separator) + rest
		}
	} else if !filepath.IsAbs(word) {
		target = base + string(filepath.Separator) + word
	}
	directory, prefix := filepath.Split(target)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var offers []Offer
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if entry.IsDir() {
			name += "/"
		} else if kind == PathFolder {
			continue
		}
		if offers = append(offers, Offer{Text: word[:len(word)-len(prefix)] + name, Display: name}); len(offers) == limit {
			break
		}
	}
	return offers
}

// Programs on PATH whose names start with the word, each once, by name
func programs(word string, limit int) []Offer {
	var offers []Offer
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasPrefix(name, word) || slices.ContainsFunc(offers, func(o Offer) bool { return o.Text == name }) {
				continue
			}
			if info, err := entry.Info(); err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				continue
			}
			if offers = append(offers, Offer{Text: name, Display: name}); len(offers) == limit {
				return offers
			}
		}
	}
	return offers
}
