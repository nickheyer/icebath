package state

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

// Process holding a store or a sidecar, PID zero marking a scan that could not prove the store quiet
type Holder struct {
	PID    int
	Target string
}

// Executables of the product, the only programs an unreadable descriptor table is suspected for
var productExecutables = map[string]bool{"icebath": true}

// Flags that select a home or a profile on a product command line
var (
	homeFlags    = []string{"--icebath-home"}
	profileFlags = []string{"--profile", "-p"}
)

// File names of a store and its sidecars wherever they live
var storeNames = map[string]bool{"state.db": true, "state.db-wal": true, "state.db-shm": true}

// Operator form of a holder, the pid and up to eighty characters of its command line
func DescribeHolderPID(pid int) string {
	who := "command line unavailable"
	if argv := holderArgv(pid); len(argv) > 0 {
		parts := append([]string{filepath.Base(argv[0])}, argv[1:]...)
		who = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
		if r := []rune(who); len(r) > 80 {
			who = string(r[:80])
		}
	}
	return fmt.Sprintf("PID %d (%s)", pid, who)
}

// Argument vector of pid, nil when it cannot be read or is empty
func holderArgv(pid int) []string {
	argv, err := proc.Cmdline(pid)
	if err != nil || len(argv) == 0 {
		return nil
	}
	return argv
}

// Reports whether argv runs one of the product executables
func looksLikeProduct(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(filepath.ToSlash(argv[0]))), ".exe")
	return productExecutables[name]
}

// Last value of one of flags on argv, as a separate token or after an equals sign, empty when absent
func argvFlagValue(argv []string, flags []string) string {
	value := ""
	for i := 0; i < len(argv); i++ {
		if slices.Contains(flags, argv[i]) && i+1 < len(argv) {
			value = argv[i+1]
			i++
			continue
		}
		for _, f := range flags {
			if v, ok := strings.CutPrefix(argv[i], f+"="); ok {
				value = v
				break
			}
		}
	}
	return value
}

// Home an env style ICEBATH_HOME assignment on argv names, the last one winning
func argvEnvHome(argv []string) string {
	for i := len(argv) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(argv[i], home.EnvHome+"="); ok {
			return v
		}
	}
	return ""
}

// Install root and profile name of the home holding a store, both empty when the home is no install layout
func installLayout(storeHome string) (root, profile string) {
	named, err := home.NamedProfileHome(storeHome)
	if err != nil {
		return "", ""
	}
	if named != "" {
		return absPath(filepath.Dir(filepath.Dir(named))), filepath.Base(named)
	}
	if filepath.Base(storeHome) == ".icebath" {
		return absPath(storeHome), ""
	}
	return "", ""
}

// Comparable form of a path token
func normToken(p string) string { return normCase(filepath.Clean(p)) }

// Reports whether a token lies under the profiles directory of root in a profile that is not ours
func namesOtherProfile(token, root, ours string) bool {
	if root == "" {
		return false
	}
	prefix := normToken(filepath.Join(root, "profiles")) + string(filepath.Separator)
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return false
	}
	name, _, _ := strings.Cut(rest, string(filepath.Separator))
	return name != "" && name != normCase(ours)
}

// Home selection a command line makes for itself, ours, other or empty when it makes none
func argvHomeSelection(argv []string, storeHome, root, ours string) string {
	value := argvFlagValue(argv, homeFlags)
	if value == "" {
		value = argvEnvHome(argv)
	}
	if value != "" {
		if normToken(value) == normToken(storeHome) {
			return "ours"
		}
		return "other"
	}
	if profile := argvFlagValue(argv, profileFlags); profile != "" {
		if ours != "" {
			if profile == ours {
				return "ours"
			}
			return "other"
		}
		if root != "" && profile == home.DefaultProfile {
			return "ours"
		}
		return "other"
	}
	return ""
}

// One absolute path argv carries and the index of the token it came from
type pathToken struct {
	index int
	path  string
}

// Absolute paths argv carries, bare or after an option equals sign
func argvPathTokens(argv []string) []pathToken {
	var out []pathToken
	for i, tok := range argv {
		p := ""
		switch {
		case strings.HasPrefix(tok, "/"):
			p = tok
		case strings.HasPrefix(tok, "-") && strings.Contains(tok, "="):
			if _, v, _ := strings.Cut(tok, "="); strings.HasPrefix(v, "/") {
				p = v
			}
		}
		if p != "" {
			out = append(out, pathToken{index: i, path: normToken(p)})
		}
	}
	return out
}

// Reports whether argv proves its process serves a different install or profile than the store's own
func argvScopedToOtherHome(argv []string, dbPath string) bool {
	db := absPath(dbPath)
	storeHome := filepath.Dir(db)
	root, ours := installLayout(storeHome)
	family := map[string]bool{}
	for _, p := range sqliteFamily(db) {
		family[normCase(p)] = true
	}
	homeNorm := normCase(storeHome)
	rootNorm := ""
	if root != "" {
		rootNorm = normCase(root)
	}
	tokens := argvPathTokens(argv)
	for _, t := range tokens {
		if family[t.path] {
			return false
		}
	}
	selection := argvHomeSelection(argv, storeHome, root, ours)
	if selection == "ours" {
		return false
	}
	otherSeen := selection == "other"
	for _, t := range tokens {
		if namesOtherProfile(t.path, root, ours) {
			otherSeen = true
			continue
		}
		if underDir(t.path, homeNorm) {
			return false
		}
		if rootNorm != "" && underDir(t.path, rootNorm) {
			continue
		}
		if t.index == 0 && root == "" {
			continue
		}
		if strings.Contains(t.path, "/.icebath") || storeNames[filepath.Base(t.path)] {
			otherSeen = true
		}
	}
	return otherSeen
}

// Reports whether an uninspectable process should count as a possible holder of the store
func suspectHolder(pid int, dbPath string) (string, bool) {
	argv := holderArgv(pid)
	if argv == nil || !looksLikeProduct(argv) || argvScopedToOtherHome(argv, dbPath) {
		return "", false
	}
	return strings.Join(argv, " "), true
}

// First n characters of s
func headRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Processes other than this one holding the store or a sidecar, an incomplete scan reported as a PID zero holder
func ForeignHolders(path string) []Holder { return foreignHolders(path) }

// Live store generations this process itself holds on path besides exclude, the half a descriptor scan cannot see
func (e *Engine) InProcessHolders(path string, exclude *Store) []Holder {
	var out []Holder
	for _, desc := range e.otherGenerations(path, exclude) {
		out = append(out, Holder{PID: os.Getpid(), Target: desc})
	}
	return out
}

// Operator refusal for structural maintenance while another process holds the store, false when it is provably quiet
func (e *Engine) HeldStoreRefusal(ctx context.Context, path, command, forceHint string) (string, bool) {
	holders := ForeignHolders(path)
	if len(holders) == 0 {
		return "", false
	}
	byPID := map[int]map[string]bool{}
	var unknown []string
	for _, h := range holders {
		if h.PID <= 0 || strings.HasPrefix(h.Target, "uninspectable") {
			unknown = append(unknown, h.Target)
			continue
		}
		if byPID[h.PID] == nil {
			byPID[h.PID] = map[string]bool{}
		}
		byPID[h.PID][filepath.Base(strings.TrimSuffix(h.Target, deletedSuffix))] = true
	}
	lines := []string{fmt.Sprintf("Refusing `icebath sessions %s`: another process is using %s.", command, path)}
	for _, pid := range slices.Sorted(maps.Keys(byPID)) {
		lines = append(lines, fmt.Sprintf("  %s: %s", DescribeHolderPID(pid), strings.Join(slices.Sorted(maps.Keys(byPID[pid])), ", ")))
	}
	if len(unknown) > 0 {
		lines = append(lines, fmt.Sprintf("  cannot prove the database is quiet (holder scan incomplete: %s)", headRunes(unknown[0], 120)))
	}
	lines = append(lines,
		"Rewriting the database under a live writer is how every agent ends up refusing turns with the "+
			"retired state.db-wal error. Nothing is lost.",
		fmt.Sprintf("Stop them first (`icebath %sgateway stop`, quit the Desktop app, pause cron), then re-run.", e.profileArg(ctx)))
	if forceHint != "" {
		lines = append(lines, fmt.Sprintf("Override with %s if you accept the risk.", forceHint))
	}
	lines = append(lines, "Recovery guide: "+RecoveryDocsURL)
	return strings.Join(lines, "\n"), true
}

// Profile selector of the home in effect for ctx, empty for the default profile or an unresolvable home
func (e *Engine) profileArg(ctx context.Context) string {
	h, err := e.home.Home(ctx)
	if err != nil {
		return ""
	}
	return home.ProfileSelector(h)
}

// Distinct processes holding the store file itself open, this one included, false where descriptors cannot be enumerated
func CountHolders(path string) (int, bool) { return countHolders(path) }

// Processes still holding an unlinked WAL or SHM generation of the store, this one included
func DeletedSidecarHolders(path string) []Holder { return deletedSidecarHolders(path) }
