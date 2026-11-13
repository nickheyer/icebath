package plugins

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	pipEntryPointGroup = "icebath_agent.plugins"
	lsRemoteTimeout    = 30 * time.Second
)

var fullGitShaRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// Waiting reason when the update's icebath floor rejects the running icebath, empty when it can be applied
func heldForNewerIcebath(result *CheckResult) string {
	if result.MinIcebath == "" || RequiresVersionError(result.MinIcebath) == "" {
		return ""
	}
	return fmt.Sprintf("needs Icebath %s; this is %s", result.MinIcebath, RunningVersion)
}

func strOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Update check of one installed plugin, its catalog pin, saved feed or git source consulted after local provenance
func checkProvenanced(ctx context.Context, prov Provenance, fetch, lsRemote func(context.Context, string) (string, error)) *CheckResult {
	row := prov.Row
	catalog, _ := row["catalog"].(map[string]any)
	catalogName := catalog["name"]
	if !pyrepr.Truthy(catalogName) {
		catalogName = row["catalog_name"]
	}
	if pyrepr.Truthy(catalogName) {
		name := pyrepr.Str(catalogName)
		result := &CheckResult{Name: prov.Name, Class: "catalog", Current: strOrEmpty(row["revision"])}
		var removed *RemovedEntry
		if allow, _ := row["allow_removed"].(bool); !allow {
			if removed = FindRemoved(ctx, name, ""); removed == nil {
				repo := catalog["repo"]
				if !pyrepr.Truthy(repo) {
					repo = row["source"]
					if repo == nil {
						repo = ""
					}
				}
				removed = FindRemoved(ctx, strings.SplitN(pyrepr.Str(repo), "#", 2)[0], "")
			}
		}
		if removed != nil {
			result.Reason = "removed from catalog: " + removed.Reason
			return result
		}
		entry := GetLiveCatalogEntry(ctx, name)
		if entry == nil {
			result.Reason = "catalog entry is unavailable; installed pin retained"
			return result
		}
		result.Latest = entry.Sha
		available := result.Current != entry.Sha || row["revision"] == nil
		result.UpdateAvailable = &available
		result.MinIcebath = entry.RequiresIcebath
		return result
	}
	result := checkLocalProvenance(prov)
	if result.Reason != "" || result.NeedsFixing != "" {
		return result
	}
	if saved := row["update_url"]; pyrepr.Truthy(saved) {
		feedText, err := fetch(ctx, pyrepr.Str(saved))
		if err != nil {
			result.Reason = "feed fetch failed: " + err.Error()
			return result
		}
		feed, err := parseFeedYml(feedText)
		if err != nil {
			result.Reason = "feed unparseable: " + err.Error()
			return result
		}
		result.Latest = strOrEmpty(feed["version"])
		result.MinIcebath = strOrEmpty(feed["min_icebath"])
		artifacts, _ := feed["artifacts"].(map[string]any)
		if feedGit, _ := artifacts["git"].(string); feedGit != "" && fullGitShaRe.MatchString(feedGit) {
			if result.Current == "" || !fullGitShaRe.MatchString(result.Current) {
				result.UpdateAvailable = nil
				result.Reason = "feed declares a git sha but the install records no full revision sha to compare"
				return result
			}
			result.Latest = strings.ToLower(feedGit)
			available := result.Latest != strings.ToLower(result.Current)
			result.UpdateAvailable = &available
			return result
		}
		installed := readManifestField(prov.Path, "version")
		if installed == "" {
			result.UpdateAvailable = nil
			result.Reason = fmt.Sprintf("feed declares version %s but the installed plugin.yaml records no version to compare", pyrepr.StrRepr(result.Latest))
			return result
		}
		result.Current = installed
		result.UpdateAvailable = versionIsNewer(result.Latest, installed)
		if result.UpdateAvailable == nil {
			result.Reason = fmt.Sprintf("cannot compare feed version %s with installed version %s", pyrepr.StrRepr(result.Latest), pyrepr.StrRepr(installed))
		}
		return result
	}
	source := row["source"]
	if !pyrepr.Truthy(source) {
		result.Reason = "no recorded source"
		return result
	}
	head, err := lsRemote(ctx, pyrepr.Str(source))
	if err != nil {
		result.Reason = "ls-remote failed: " + err.Error()
		return result
	}
	result.Latest = head
	available := head != "" && head != result.Current
	result.UpdateAvailable = &available
	return result
}

// Remote HEAD sha of a git source through the resolved git, empty for an empty repository
func defaultLsRemote(ctx context.Context, source string) (string, error) {
	gitExe := resolveGitExecutable()
	if gitExe == "" {
		gitExe = "git"
	}
	runCtx, cancel := context.WithTimeout(ctx, lsRemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, gitExe, "ls-remote", source, "HEAD")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("Command '%s' timed out after 30 seconds", pyrepr.Repr([]any{gitExe, "ls-remote", source, "HEAD"}))
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", err
	}
	if err != nil {
		msg := pystr.Strip(pystr.DecodeUTF8Replace([]byte(stderr.String())))
		if msg == "" {
			msg = "ls-remote failed"
		}
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[:200])
		}
		return "", errors.New(msg)
	}
	out := pystr.Strip(pystr.DecodeUTF8Replace([]byte(stdout.String())))
	if out == "" {
		return "", nil
	}
	head, _, _ := strings.Cut(out, "\t")
	return head, nil
}

// Options of one update check run, nil functions taking the real network and git probes
type RunChecksOptions struct {
	Fetch, LsRemote func(context.Context, string) (string, error)
}

// Every update check for one plugins dir, never mutating anything on disk
func RunChecks(ctx context.Context, pluginsDir string, opts RunChecksOptions) []*CheckResult {
	if opts.Fetch == nil {
		opts.Fetch = defaultFetch
	}
	if opts.LsRemote == nil {
		opts.LsRemote = defaultLsRemote
	}
	var results []*CheckResult
	for _, p := range PluginsProvenance(ctx, pluginsDir) {
		results = append(results, checkProvenanced(ctx, p, opts.Fetch, opts.LsRemote))
	}
	return results
}
