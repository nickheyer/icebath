package vault

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// Time a password manager command may run
const managerTimeout = 30 * time.Second

// Terminal escapes a password manager prints, removed before its error text is shown
var (
	// CSI and OSC sequences, an OSC cut short by a killed CLI removed too
	shortANSI = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?)`)
	// Every ECMA-48 sequence and eight bit control
	fullANSI = regexp.MustCompile(`(?s)\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\].*?(?:\x07|\x1b\\)|[PX^_].*?\x1b\\|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])|\x{9b}[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x{9d}.*?(?:\x07|\x{9c})|[\x{80}-\x{9f}]`)
)

// One password manager command with its whole environment and the secret it reads on stdin
type managerCall struct {
	path           string
	args           []string
	env            map[string]string
	input          string
	hasInput       bool
	label          string
	timeoutMessage string
}

// Outcome of a password manager command with its output read as text
type managerResult struct {
	ok     bool
	stdout string
	stderr string
}

// Runs a manager command under its deadline, never through a shell, the secret on stdin and never in argv
func runManager(ctx context.Context, c managerCall) (managerResult, error) {
	run, cancel := context.WithTimeout(ctx, managerTimeout)
	defer cancel()
	cmd := exec.Command(c.path, c.args...)
	cmd.Env = make([]string, 0, len(c.env))
	for _, name := range slices.Sorted(maps.Keys(c.env)) {
		cmd.Env = append(cmd.Env, name+"="+c.env[name])
	}
	if c.hasInput {
		cmd.Stdin = strings.NewReader(strings.ReplaceAll(c.input, "\n", stdinNewline))
	}
	res, err := proc.Run(run, cmd)
	if err != nil {
		if ctx.Err() == nil && errors.Is(run.Err(), context.DeadlineExceeded) {
			return managerResult{}, errors.New(c.timeoutMessage)
		}
		if ctx.Err() != nil {
			return managerResult{}, ctx.Err()
		}
		return managerResult{}, fmt.Errorf("failed to invoke %s: %w", c.label, err)
	}
	return managerResult{ok: res.Exit.Success(), stdout: outputText(res.Stdout), stderr: outputText(res.Stderr)}, nil
}

// Launch values of the named variables present, keyed as the list spells them
func environment(names ...string) map[string]string {
	env := map[string]string{}
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	return env
}

// Error text of the 1Password CLI with every terminal sequence and stray escape removed, trimmed
func scrubOp(text string) string {
	return trimSpace(strings.ReplaceAll(fullANSI.ReplaceAllString(text, ""), "\x1b", ""))
}

// Error text of the Bitwarden CLI with CSI and OSC sequences removed
func scrubBw(text string) string { return shortANSI.ReplaceAllString(text, "") }

// Reports whether a pinned binary path is a regular file
func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Path of the 1Password CLI, a pinned path used verbatim only when executable
func findOp(binaryPath string) string {
	if binaryPath != "" {
		if !fileutil.Accessible(binaryPath, fileutil.AccessExec) {
			return ""
		}
		return binaryPath
	}
	found, err := exec.LookPath("op")
	if err != nil {
		return ""
	}
	return found
}

// Path of the Bitwarden CLI, a pinned path used verbatim
func findBw(binaryPath string) string {
	if binaryPath != "" {
		return binaryPath
	}
	found, err := exec.LookPath("bw")
	if err != nil {
		return ""
	}
	return found
}

// Reports whether a manager CLI is reachable, a pinned path winning over PATH
func managerInstalled(name, binaryPath string) bool {
	if binaryPath != "" {
		return isRegularFile(binaryPath)
	}
	if name == onePasswordName {
		return findOp("") != ""
	}
	return findBw("") != ""
}

// Handle without the prefix a backend owns, empty when shorter than the prefix
func handleID(handle, prefix string) string {
	if len(handle) < len(prefix) {
		return ""
	}
	return handle[len(prefix):]
}

// Every normalized origin among urls, deduplicated with order kept, unparseable ones skipped
func allOrigins(urls []string) []string {
	var out []string
	for _, u := range urls {
		origin, err := NormalizeOrigin(u)
		if err != nil || slices.Contains(out, origin) {
			continue
		}
		out = append(out, origin)
	}
	return out
}

// Web page origins among origins, or the first origin alone when none is a web origin
func webOrigins(origins []string) []string {
	var web []string
	for _, o := range origins {
		if strings.HasPrefix(o, "http://") || strings.HasPrefix(o, "https://") {
			web = append(web, o)
		}
	}
	if len(web) == 0 {
		return []string{origins[0]}
	}
	return web
}

// Entries of each listed manager item, none for empty output or a document that is not a list
func listedItems(text, tool string) ([][]member, error) {
	if text == "" {
		return nil, nil
	}
	raw, err := parseValue([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("%s returned output that is not JSON", tool)
	}
	items, ok := arrayItems(raw)
	if !ok {
		return nil, nil
	}
	out := make([][]member, 0, len(items))
	for _, item := range items {
		ms, ok := objectMembers(item)
		if !ok {
			return nil, fmt.Errorf("%s returned an item that is not an object", tool)
		}
		out = append(out, ms)
	}
	return out, nil
}
