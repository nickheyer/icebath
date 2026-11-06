package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
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

func gitArgs(gitCmd []string, args ...string) []string {
	return append(append([]string{}, gitCmd...), args...)
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
		slog.Debug(fmt.Sprintf("git %s did not answer: %v", strings.Join(args, " "), err))
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

// Text of a published tree object, found false for a 404 when missingOK
func sourceRead(ctx context.Context, target string, missingOK bool) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "icebath-update")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "application/json, text/html")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: pyurl.UrlopenProxy}}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if missingOK && resp.StatusCode == 404 {
			return "", false, nil
		}
		reason := http.StatusText(resp.StatusCode)
		if _, text, ok := strings.Cut(resp.Status, " "); ok && text != "" {
			reason = text
		}
		return "", false, &pm.HTTPError{Code: resp.StatusCode, Reason: reason, Header: resp.Header, URL: target}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", false, err
	}
	text, err := pystr.DecodeUTF8Sig(body)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// Content of every meta tag named icebath-build, only script and style bodies read as raw text like HTMLParser
func buildMetadataTags(page string) []any {
	var tags []any
	z := html.NewTokenizer(strings.NewReader(page))
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			return tags
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		if kind == html.StartTagToken && token.Data != "script" && token.Data != "style" {
			z.NextIsNotRawText()
		}
		if token.Data != "meta" {
			continue
		}
		fields := map[string]*string{}
		for _, attr := range token.Attr {
			value := attr.Val
			fields[attr.Key] = &value
		}
		if name := fields["name"]; name != nil && *name == "icebath-build" {
			if content := fields["content"]; content != nil {
				tags = append(tags, *content)
			} else {
				tags = append(tags, nil)
			}
		}
	}
}

// Promoted tag and pinned commit of a release channel, stable from its candidates file and every channel from its index meta
func releasePointer(ctx context.Context, channel string) (string, string, bool, error) {
	if channel == ChannelStable {
		text, found, err := sourceRead(ctx, sourcePublicBase+"/releases/stable/release-candidates.json", true)
		if err != nil {
			return "", "", false, err
		}
		if found {
			data, err := pyjson.Loads(text)
			if err != nil {
				return "", "", false, &ValueError{Msg: err.Error()}
			}
			obj, isObj := data.(*pyjson.Object)
			if !isObj || !validReleaseTag(obj.Vals["tag"], channel) {
				return "", "", false, &ValueError{Msg: "Invalid stable release pointer"}
			}
			commit, isStr := obj.Vals["commit"].(string)
			if !isStr || !sourceSHARE.MatchString(commit) {
				return "", "", false, &ValueError{Msg: "Invalid stable release pointer"}
			}
			return obj.Vals["tag"].(string), commit, true, nil
		}
	}
	text, found, err := sourceRead(ctx, sourcePublicBase+"/releases/"+channel+"/index.html", true)
	if err != nil || !found {
		return "", "", false, err
	}
	tags := buildMetadataTags(text)
	if len(tags) != 1 || !validReleaseTag(tags[0], channel) {
		return "", "", false, &ValueError{Msg: fmt.Sprintf("Invalid %s release pointer", channel)}
	}
	return tags[0].(string), "", true, nil
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
	tag, commit, err := resolveRelease(ctx, ChannelStable, gitCmd, cwd, repository, false)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not resolve the stable source release: %v", err))
		return SourceTarget{}, &ValueError{Msg: describeReleaseFailure(err)}
	}
	return SourceTarget{RequestedChannel: ChannelStable, Channel: ChannelStable, Repository: repository,
		Commit: commit, Version: strings.TrimPrefix(tag, "v")}, nil
}

// Reports whether tag is a strict stable tag, or for canary an unpadded canary tag
func validReleaseTag(tag any, channel string) bool {
	text, ok := tag.(string)
	if !ok {
		return false
	}
	if channel == ChannelStable {
		return updaterecord.StableTagRE.MatchString(text)
	}
	return text == pystr.Strip(text) && IsCanaryTag(text)
}

// Reports whether a release document is published on channel, never a draft and prerelease only for canary
func publishedRelease(release any, channel string) bool {
	obj, ok := release.(*pyjson.Object)
	if !ok {
		return false
	}
	draft, draftOK := stampGet(obj, "draft").(bool)
	prerelease, preOK := stampGet(obj, "prerelease").(bool)
	return draftOK && !draft && preOK && prerelease == (channel == ChannelCanary) &&
		validReleaseTag(stampGet(obj, "tag_name"), channel)
}

// Published release of channel from GitHub, latest for stable and a bounded newest first scan otherwise
func publishedFallback(ctx context.Context, channel, base string) (*pyjson.Object, error) {
	if channel == ChannelStable {
		release, err := sourceJSON(ctx, base+"/releases/latest")
		if err != nil {
			return nil, err
		}
		if publishedRelease(release, channel) {
			return release.(*pyjson.Object), nil
		}
	} else {
		for page := 1; page <= 10; page++ {
			entries, err := sourceJSON(ctx, fmt.Sprintf("%s/releases?per_page=100&page=%d", base, page))
			if err != nil {
				return nil, err
			}
			list, ok := entries.([]any)
			if !ok {
				break
			}
			for _, release := range list {
				if publishedRelease(release, channel) {
					return release.(*pyjson.Object), nil
				}
			}
			if len(list) < 100 {
				break
			}
		}
	}
	return nil, &ValueError{Msg: fmt.Sprintf("No published %s release", channel)}
}

// Published stable or canary tag and commit, the failure returned instead of empty values
func resolveRelease(ctx context.Context, channel string, gitCmd []string, cwd, repository string, pointer bool) (string, string, error) {
	if channel != ChannelStable && channel != ChannelCanary {
		return "", "", &ValueError{Msg: "Not a release channel: " + channel}
	}
	if repository == "" {
		var err error
		if repository, err = sourceRepository(ctx, gitCmd, cwd); err != nil {
			return "", "", err
		}
	}
	base := sourceGitHubAPI + "/repos/" + repository
	tag, pinnedSHA, pointed := "", "", false
	if pointer && strings.EqualFold(repository, OfficialRepository) {
		var err error
		if tag, pinnedSHA, pointed, err = releasePointer(ctx, channel); err != nil {
			return "", "", err
		}
	}
	var release any
	if !pointed {
		found, err := publishedFallback(ctx, channel, base)
		if err != nil {
			return "", "", err
		}
		release, tag = found, stampGet(found, "tag_name").(string)
	} else {
		var err error
		if release, err = sourceJSON(ctx, base+"/releases/tags/"+tag); err != nil {
			return "", "", err
		}
	}
	if !publishedRelease(release, channel) || release.(*pyjson.Object).Vals["tag_name"] != tag {
		return "", "", &ValueError{Msg: fmt.Sprintf("%s is not a published %s release", tag, channel)}
	}
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
	if pinnedSHA != "" && sha != pinnedSHA {
		return "", "", &ValueError{Msg: fmt.Sprintf("Release %s no longer matches its published commit", tag)}
	}
	return tag, sha, nil
}

// Published stable or canary release tag and commit, the channel pointers outranking GitHub when pointer is set
func ResolveSourceRelease(ctx context.Context, channel string, gitCmd []string, cwd, repository string, pointer bool) (string, string) {
	tag, sha, err := resolveRelease(ctx, channel, gitCmd, cwd, repository, pointer)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not resolve the %s source release: %v", channel, err))
		return "", ""
	}
	return tag, sha
}
