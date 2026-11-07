package backup

import (
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Profile id grammar with Python match semantics, a trailing newline before the end accepted
var profileIDRe = func() *regexp2.Regexp {
	re, err := pyre.Compile(home.ProfileIDRe.String())
	if err != nil {
		panic(err)
	}
	return re
}()

// Reports whether a path is a directory after following symlinks, false on any stat error
func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Name and home of every other profile of this install, default for the root home, never failing
func SiblingProfileHomes(invokingHome string) [][2]string {
	homes := [][2]string{}
	fail := func(err error) [][2]string {
		slog.Debug(fmt.Sprintf("Sibling profile enumeration failed: %s", pypath.OSErrorText(err)))
		return homes
	}
	invoking, err := pypath.Resolve(invokingHome)
	if err != nil {
		return fail(err)
	}
	if def := profiles.DefaultHome(); isDir(def) {
		resolved, err := pypath.Resolve(def)
		if err != nil {
			return fail(err)
		}
		if resolved != invoking {
			homes = append(homes, [2]string{"default", pypath.PathStr(def)})
		}
	}
	root := profiles.ProfilesRoot()
	if !isDir(root) {
		return homes
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fail(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		p := pypath.PathJoin(root, e.Name())
		if !isDir(p) || e.Name() == "default" {
			continue
		}
		if ok, err := profileIDRe.MatchString(e.Name()); err != nil || !ok {
			if err != nil {
				return fail(err)
			}
			continue
		}
		resolved, err := pypath.Resolve(p)
		if err != nil {
			return fail(err)
		}
		if resolved != invoking {
			homes = append(homes, [2]string{e.Name(), p})
		}
	}
	return homes
}
