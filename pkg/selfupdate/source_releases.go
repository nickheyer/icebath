//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var (
	sourcePublicBase = "https://raw.githubusercontent.com/" + OfficialRepository + "/main"
	sourceSHARE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	runSourceGit     = sourceGitRun
)

// A pinned source build or a source branch delivery a channel resolves to
type SourceTarget struct {
	RequestedChannel string
	Channel          string
	Repository       string
	Commit           string
	Branch           string
	Version          string
	BuildID          string
	Ahead            bool
}

// Reports whether resolution moved the subscription to another channel
func (t SourceTarget) Retired() bool { return t.RequestedChannel != t.Channel }

// Human label of the target, the waiting notice when this checkout is newer than the release
func (t SourceTarget) Label() string {
	if t.Ahead {
		return t.Channel + " v" + t.Version + "; this checkout is newer and waits for the next release" +
			" (`icebath update --set-channel main` follows every commit)"
	}
	if t.Commit != "" {
		return fmt.Sprintf("%s v%s (%s)", t.Channel, t.Version, t.Commit[:12])
	}
	return t.Channel
}

// Git exit that check=True turns into a raised failure
type sourceCalledProcessError struct {
	Argv       []string
	ReturnCode int
}

func (e *sourceCalledProcessError) Error() string {
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pyListRepr(e.Argv), e.ReturnCode)
}

// GitHub repository whose releases a checkout follows, a GitHub origin's own, else the official one
func sourceRepository(ctx context.Context, gitCmd []string, cwd string) (string, error) {
	if gitCmd != nil {
		result, err := runSourceGit(ctx, gitArgs(gitCmd, "config", "--get", "remote.origin.url"), cwd, 10*time.Second)
		if err != nil {
			return "", err
		}
		if match := githubOriginRE.FindStringSubmatch(pystr.Strip(result.Stdout)); result.ReturnCode == 0 && match != nil {
			return match[1], nil
		}
	}
	return OfficialRepository, nil
}

// Resolves a subscription, pinning an unchosen default to a HEAD that already contains the release
func ResolveSourceTarget(ctx context.Context, channel string, gitCmd []string, cwd, repository string, forwardOnly bool) (SourceTarget, error) {
	target, err := resolveSourceTargetFor(ctx, channel, gitCmd, cwd, repository)
	if err != nil {
		return SourceTarget{}, err
	}
	if forwardOnly && target.Commit != "" && gitCmd != nil && cwd != "" {
		head, err := headContaining(ctx, gitCmd, cwd, target.Commit, target.Repository)
		if err != nil {
			return SourceTarget{}, err
		}
		if head != "" && head != target.Commit {
			target.Commit, target.Ahead = head, true
		}
	}
	return target, nil
}

// Runs a git probe, nil when git did not answer so the relation stays unknown
func sourceProbe(ctx context.Context, gitCmd []string, cwd string, args ...string) *sourceGitResult {
	result, err := runSourceGit(ctx, gitArgs(gitCmd, args...), cwd, 10*time.Second)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("git %s did not answer: %v", strings.Join(args, " "), err))
		return nil
	}
	return result
}

// HEAD's sha unless HEAD is proven not to contain commit, then empty
func headContaining(ctx context.Context, gitCmd []string, cwd, commit, repository string) (string, error) {
	result, err := runSourceGit(ctx, gitArgs(gitCmd, "rev-parse", "HEAD"), cwd, 10*time.Second)
	if err != nil {
		return "", err
	}
	head := pystr.Strip(result.Stdout)
	if !sourceSHARE.MatchString(head) {
		return "", nil
	}
	ancestry := sourceProbe(ctx, gitCmd, cwd, "merge-base", "--is-ancestor", commit, head)
	if ancestry != nil && ancestry.ReturnCode == 0 {
		return head, nil
	}
	if ancestry != nil && ancestry.ReturnCode == 1 {
		shallow := sourceProbe(ctx, gitCmd, cwd, "rev-parse", "--is-shallow-repository")
		if shallow != nil && pystr.Strip(shallow.Stdout) == "false" {
			return "", nil
		}
	}
	status := ""
	if payload := githubCompare(ctx, commit, head, repository); payload != nil {
		status, _ = stampGet(payload, "status").(string)
	}
	if status == "behind" || status == "diverged" {
		return "", nil
	}
	return head, nil
}

func resolveSourceTargetFor(ctx context.Context, channel string, gitCmd []string, cwd, repository string) (SourceTarget, error) {
	if _, err := ValidateChannelName(channel); err != nil {
		return SourceTarget{}, err
	}
	if repository == "" {
		var err error
		if repository, err = sourceRepository(ctx, gitCmd, cwd); err != nil {
			return SourceTarget{}, err
		}
	}
	switch channel {
	case ChannelStable:
		return resolveStable(ctx, repository, gitCmd, cwd)
	case ChannelMain:
		return SourceTarget{RequestedChannel: channel, Channel: channel, Repository: repository, Branch: "main"}, nil
	}
	return resolveNamedChannel(ctx, channel, gitCmd, cwd, repository)
}

// Named channel through its validated requested and terminal records, retirement constraints admitted before any checkout
func resolveNamedChannel(ctx context.Context, channel string, gitCmd []string, cwd, repository string) (SourceTarget, error) {
	resolved, err := resolveChannel(ctx, channel, repository)
	if err != nil {
		return SourceTarget{}, err
	}
	terminal := resolved.Terminal
	if !strings.EqualFold(terminal.Vals["repository"].(string), repository) {
		return SourceTarget{}, &ValueError{Msg: "Channel repository does not match this source installation"}
	}
	destination, err := ValidateChannelName(terminal.Vals["name"])
	if err != nil {
		return SourceTarget{}, err
	}
	if terminal.Vals["policy"] == "source-branch" {
		if resolved.Requested.Vals["state"] == "retired" {
			return SourceTarget{}, &ValueError{Msg: "Source retirement requires a published destination commit"}
		}
		delivery := terminal.Vals["delivery"].(*pyjson.Object)
		if delivery.Vals["kind"] != "source-branch" {
			return SourceTarget{}, &ValueError{Msg: "Channel has no source-branch delivery"}
		}
		return SourceTarget{RequestedChannel: channel, Channel: destination, Repository: repository,
			Branch: delivery.Vals["branch"].(string)}, nil
	}
	if resolved.Manifest == nil {
		return SourceTarget{}, &ValueError{Msg: "No build published for channel " + destination}
	}
	request := resolved.Manifest.Vals["request"].(*pyjson.Object)
	commit, _ := request.Vals["commit"].(string)
	if !sourceSHARE.MatchString(commit) {
		return SourceTarget{}, &ValueError{Msg: "Channel build has no exact source commit"}
	}
	if resolved.Requested.Vals["state"] == "retired" {
		if err := refuseRetirementDowngrade(ctx, request, terminal, gitCmd, cwd); err != nil {
			return SourceTarget{}, err
		}
	}
	return SourceTarget{RequestedChannel: channel, Channel: destination, Repository: repository, Commit: commit,
		Version: request.Vals["sourceVersion"].(string), BuildID: request.Vals["buildId"].(string)}, nil
}

// Validated channel records and manifest read from the published tree, never a GitHub listing fallback
var resolveChannel = func(ctx context.Context, name, repository string) (*ChannelResolution, error) {
	reader, err := NewChannelReader(sourcePublicBase, &repository, nil)
	if err != nil {
		return nil, err
	}
	return reader.Resolve(ctx, name)
}

var installedSourceVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Refuses a retirement build older than the running build, a newer local commit or the newer destination head
func refuseRetirementDowngrade(ctx context.Context, request, terminal *pyjson.Object, gitCmd []string, cwd string) error {
	if cwd == "" {
		return nil
	}
	sourceVersion := request.Vals["sourceVersion"].(string)
	if installed := updaterecord.BuildVersion(); installed != "" {
		installed = strings.TrimPrefix(installed, "v")
		if !installedSourceVersionRE.MatchString(installed) {
			return &ValueError{Msg: "Source retirement cannot verify the installed source version"}
		}
		if compareDottedInts(installed, sourceVersion) > 0 {
			return &ValueError{Msg: "Source retirement would downgrade a newer source version; select the destination channel explicitly"}
		}
	}
	if gitCmd == nil {
		return nil
	}
	commit := request.Vals["commit"].(string)
	result, err := runSourceGit(ctx, gitArgs(gitCmd, "rev-list", "--ancestry-path", commit+"..HEAD"), cwd, 10*time.Second)
	if err != nil {
		return err
	}
	if result.ReturnCode == 0 && strings.TrimSpace(result.Stdout) != "" {
		return &ValueError{Msg: "Source retirement would downgrade a newer source commit; select the destination channel explicitly"}
	}
	if headSequence(terminal.Vals["head"].(*pyjson.Object)) <= requestSequence(request) {
		return nil
	}
	current, err := resolveChannel(ctx, terminal.Vals["name"].(string), request.Vals["repository"].(string))
	if err != nil {
		return err
	}
	if current.Manifest == nil {
		return &ValueError{Msg: "Source retirement cannot verify the current destination build"}
	}
	argv := gitArgs(gitCmd, "rev-parse", "HEAD")
	head, err := runSourceGit(ctx, argv, cwd, 10*time.Second)
	if err != nil {
		return err
	}
	if head.ReturnCode != 0 {
		return &sourceCalledProcessError{Argv: argv, ReturnCode: head.ReturnCode}
	}
	installed := strings.TrimSpace(head.Stdout)
	if installed == current.Manifest.Vals["request"].(*pyjson.Object).Vals["commit"] && installed != commit {
		return &ValueError{Msg: "Source retirement would downgrade the newer destination build; select the destination channel explicitly"}
	}
	return nil
}

func requestSequence(request *pyjson.Object) int64 {
	n, _ := channelInteger(request.Vals["sequence"], "sequence", 1, channelMaxSequence)
	return n
}

// Names a transport failure's cause, so a rate limit never reads as a missing release
func describeReleaseFailure(failure error) string {
	var httpErr *plugins.GithubHTTPError
	var urlErr *url.Error
	var netErr net.Error
	if errors.As(failure, &httpErr) || errors.As(failure, &urlErr) || errors.As(failure, &netErr) ||
		errors.Is(failure, context.DeadlineExceeded) {
		return plugins.DescribeGithubFailure(failure, failureAuthenticated(failure), nil)
	}
	return "No published stable release could be verified: " + failure.Error()
}

// Stable is the repository's latest published GitHub release, verified against origin when git is given
func resolveStable(ctx context.Context, repository string, gitCmd []string, cwd string) (SourceTarget, error) {
	tag, commit, err := resolveRelease(ctx, gitCmd, cwd, repository)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Could not resolve the stable source release: %v", err))
		return SourceTarget{}, &ValueError{Msg: describeReleaseFailure(err)}
	}
	return SourceTarget{RequestedChannel: ChannelStable, Channel: ChannelStable, Repository: repository,
		Commit: commit, Version: strings.TrimPrefix(tag, "v")}, nil
}

// Reports whether tag is a strict stable tag
func validReleaseTag(tag any) bool {
	text, ok := tag.(string)
	return ok && updaterecord.StableTagRE.MatchString(text)
}

// Reports whether a release document is a published stable release, never a draft or a prerelease
func publishedRelease(release any) bool {
	obj, ok := release.(*pyjson.Object)
	if !ok {
		return false
	}
	draft, draftOK := stampGet(obj, "draft").(bool)
	prerelease, preOK := stampGet(obj, "prerelease").(bool)
	return draftOK && !draft && preOK && !prerelease && validReleaseTag(stampGet(obj, "tag_name"))
}

// Latest published stable release of a repository from GitHub
func publishedFallback(ctx context.Context, base string) (*pyjson.Object, error) {
	release, err := sourceJSON(ctx, base+"/releases/latest")
	if err != nil {
		return nil, err
	}
	if publishedRelease(release) {
		return release.(*pyjson.Object), nil
	}
	return nil, &ValueError{Msg: "No published stable release"}
}

// Published stable tag and commit of a repository, the failure returned instead of empty values
func resolveRelease(ctx context.Context, gitCmd []string, cwd, repository string) (string, string, error) {
	base := sourceGitHubAPI + "/repos/" + repository
	release, err := publishedFallback(ctx, base)
	if err != nil {
		return "", "", err
	}
	tag := stampGet(release, "tag_name").(string)
	commit, err := sourceJSON(ctx, base+"/commits/"+tag)
	if err != nil {
		return "", "", err
	}
	sha := ""
	if obj, ok := commit.(*pyjson.Object); ok {
		sha, _ = stampGet(obj, "sha").(string)
	}
	if !sourceSHARE.MatchString(sha) {
		return "", "", &ValueError{Msg: "No published commit for release " + tag}
	}
	if gitCmd != nil {
		ref := "refs/tags/" + tag
		argv := gitArgs(gitCmd, "ls-remote", "--tags", "origin", ref, ref+"^{}")
		result, err := runSourceGit(ctx, argv, cwd, 60*time.Second)
		if err != nil {
			return "", "", err
		}
		if result.ReturnCode != 0 {
			return "", "", &sourceCalledProcessError{Argv: argv, ReturnCode: result.ReturnCode}
		}
		refs := map[string]string{}
		for _, line := range pystr.Splitlines(result.Stdout, false) {
			if parts := strings.Fields(line); len(parts) == 2 {
				refs[parts[1]] = parts[0]
			}
		}
		origin, ok := refs[ref+"^{}"]
		if !ok {
			origin = refs[ref]
		}
		if origin != sha {
			return "", "", &ValueError{Msg: fmt.Sprintf("Origin tag %s does not match the published release commit", tag)}
		}
	}
	return tag, sha, nil
}
