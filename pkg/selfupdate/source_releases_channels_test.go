package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Origin with installed, published and unpublished commits and a clone detached at the installed one
type chSource struct {
	root, origin string
	commits      []string
}

func newChSource(t *testing.T) *chSource {
	tmp := t.TempDir()
	s := &chSource{origin: filepath.Join(tmp, "origin"), root: filepath.Join(tmp, "checkout")}
	os.MkdirAll(s.origin, 0o755)
	srGit(t, s.origin, "init", "-b", "main")
	srGit(t, s.origin, "config", "user.name", "Channel Fixture")
	srGit(t, s.origin, "config", "user.email", "fixture@example.invalid")
	srGit(t, s.origin, "config", "commit.gpgsign", "false")
	for _, label := range []string{"installed", "published", "unpublished"} {
		os.WriteFile(filepath.Join(s.origin, "content.txt"), []byte(label), 0o644)
		srGit(t, s.origin, "add", "content.txt")
		srGit(t, s.origin, "commit", "-m", label)
		s.commits = append(s.commits, srGit(t, s.origin, "rev-parse", "HEAD"))
	}
	srGit(t, tmp, "clone", s.origin, s.root)
	srGit(t, s.root, "checkout", "--detach", s.commits[0])
	return s
}

// Archive serving the published tree for the duration of the test, the running build version unset
func chPublishedTree(t *testing.T) *chArchive {
	a := newChArchive(t)
	saved := sourcePublicBase
	sourcePublicBase = a.url
	version := updaterecord.BuildVersion()
	updaterecord.SetBuildVersion("")
	t.Cleanup(func() { sourcePublicBase = saved; updaterecord.SetBuildVersion(version) })
	return a
}

// One build published as its channel head, preview or stable-release policy
func (a *chArchive) publishChannelBuild(t *testing.T, name, buildID, commit string, sequence int, stable bool) map[string]any {
	prefix := "releases/channel-builds/" + buildID + "/"
	identity := map[string]any{"token": strings.Repeat("a", 16), "displayName": "Preview fixture", "appNamePascal": "Fixture",
		"artifactNamePascal": "Fixture", "appId": "ai.fixture.preview", "msixAppIdWithOrg": "Fixture.Preview",
		"cliName": "fixture-preview", "windowsExecutableName": "Fixture"}
	version := fmt.Sprintf("0.0.%d", sequence)
	if stable {
		version = fmt.Sprintf("1.2.%d", sequence)
	}
	request := map[string]any{"schema": 1, "channel": name, "buildId": buildID, "sequence": sequence,
		"repository": OfficialRepository, "commit": commit, "sourceVersion": fmt.Sprintf("1.2.%d", sequence),
		"version": version, "windowsVersion": version + ".0", "identity": identity, "bundleEnv": map[string]any{}, "publicBase": a.url}
	policy := "preview"
	if stable {
		request["releaseTag"], policy = "v"+version, "stable-release"
	}
	manifest := map[string]any{"schema": 1, "receiverProtocol": 1, "request": request, "packages": []any{map[string]any{
		"platform": "darwin", "arch": "arm64", "variant": "bundled",
		"artifact": map[string]any{"key": prefix + "fixture.zip", "size": 1, "sha256": strings.Repeat("d", 64)},
		"identity": identity["appId"], "version": version, "teamId": "ABCDEFGHIJ",
		"feed": map[string]any{"key": prefix + "darwin/stable.yml", "channel": "stable"}}}}
	body := a.put(t, prefix+"build.json", manifest)
	sum := sha256.Sum256(body)
	channel := map[string]any{"schema": 1, "name": name, "repository": OfficialRepository, "policy": policy, "state": "active",
		"identity": identity, "revision": sequence, "nextSequence": sequence + 1, "head": map[string]any{"buildId": buildID,
			"sequence": sequence, "manifestKey": prefix + "build.json", "sha256": hex.EncodeToString(sum[:])}}
	a.put(t, "releases/channels/"+name+".json", channel)
	return channel
}

type chRetired struct {
	name               string
	retired, qualified map[string]any
}

// Preview retired onto the qualified stable build while stable has since advanced to the unpublished commit
func (a *chArchive) retiredChannel(t *testing.T, s *chSource) *chRetired {
	name := "offline-preview"
	preview := a.publishChannelBuild(t, name, strings.Repeat("a", 32), s.commits[0], 1, false)
	qualified := a.publishChannelBuild(t, "stable", strings.Repeat("b", 32), s.commits[1], 1, true)
	retired := chCopy(t, preview)
	retired["state"], retired["destination"], retired["minimumVersion"] = "retired", "stable", "1.0.0"
	retired["destinationHead"], retired["receiverProtocol"] = chCopy(t, qualified)["head"], 1
	retired["receiver"], retired["lastHead"] = map[string]any{"kind": "discontinued"}, preview["head"]
	a.put(t, "releases/channels/"+name+".json", retired)
	a.publishChannelBuild(t, "stable", strings.Repeat("c", 32), s.commits[2], 2, true)
	return &chRetired{name: name, retired: retired, qualified: qualified}
}

func TestSourceNamedChannelResolvesItsPublishedRecord(t *testing.T) {
	s := newChSource(t)
	a := chPublishedTree(t)
	ctx := context.Background()
	a.publishChannelBuild(t, "canary", strings.Repeat("b", 32), s.commits[1], 1, false)
	target, err := ResolveSourceTarget(ctx, "canary", []string{"git"}, s.root, "", false)
	if err != nil || target.Commit != s.commits[1] || target.Version != "1.2.1" || target.BuildID != strings.Repeat("b", 32) ||
		target.Branch != "" || target.Retired() {
		t.Fatalf("canary %+v %v", target, err)
	}
	main, err := ResolveSourceTarget(ctx, "main", []string{"git"}, s.root, "", false)
	if err != nil || main.Branch != "main" || main.Commit != "" || main.Label() != "main" {
		t.Fatalf("main %+v %v", main, err)
	}
	var notFound *ChannelNotFound
	if _, err := ResolveSourceTarget(ctx, "pm-preview", nil, "", "", false); !errors.As(err, &notFound) ||
		err.Error() != "Channel object not found: releases/channels/pm-preview.json" {
		t.Fatalf("unpublished channel %v", err)
	}
	empty := a.get(t, "releases/channels/canary.json")
	empty["head"] = nil
	a.put(t, "releases/channels/canary.json", empty)
	if _, err := ResolveSourceTarget(ctx, "canary", nil, "", "", false); err == nil || err.Error() != "No build published for channel canary" {
		t.Fatalf("headless channel %v", err)
	}
}

func TestSourceRealHTTPReaderRefusesAMutatedManifest(t *testing.T) {
	s := newChSource(t)
	a := chPublishedTree(t)
	name := "http-preview-351"
	channel := a.publishChannelBuild(t, name, strings.Repeat("b", 32), s.commits[1], 1, false)
	target, err := ResolveSourceTarget(context.Background(), name, []string{"git"}, s.root, "", false)
	if err != nil || target.Commit != s.commits[1] {
		t.Fatalf("target %+v %v", target, err)
	}
	key := channel["head"].(map[string]any)["manifestKey"].(string)
	a.mu.Lock()
	a.objects[key] = append(a.objects[key], ' ')
	a.mu.Unlock()
	if _, err := ResolveSourceTarget(context.Background(), name, []string{"git"}, s.root, "", false); err == nil ||
		!strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("mutated manifest %v", err)
	}
}

func TestSourceMainFollowsTheGitBranchWithoutReadingAChannelRecord(t *testing.T) {
	s := newChSource(t)
	for _, failure := range []string{"not-found", "forbidden", "timeout"} {
		var reads []string
		saved := resolveChannel
		resolveChannel = func(_ context.Context, name, _ string) (*ChannelResolution, error) {
			reads = append(reads, name)
			switch failure {
			case "forbidden":
				return nil, &pm.HTTPError{Code: 403, Reason: "Forbidden", URL: "https://x/releases/channels/" + name + ".json"}
			case "timeout":
				return nil, os.ErrDeadlineExceeded
			}
			return nil, &ChannelNotFound{ChannelError{Msg: "Channel object not found: releases/channels/" + name + ".json"}}
		}
		target, err := ResolveSourceTarget(context.Background(), "main", []string{"git"}, s.root, "", false)
		if err != nil || target.Branch != "main" || target.Commit != "" || len(reads) != 0 {
			t.Fatalf("%s main %+v %v %v", failure, target, err, reads)
		}
		if _, err := ResolveSourceTarget(context.Background(), "canary", []string{"git"}, s.root, "", false); err == nil ||
			len(reads) != 1 || reads[0] != "canary" {
			t.Fatalf("%s canary %v %v", failure, err, reads)
		}
		resolveChannel = saved
	}
}

// Validated shape the reader returns, injected through the resolveChannel seam
func chReaderResult(t *testing.T, s *chSource, name, destination, repository string) *ChannelResolution {
	record := func(n, state string) map[string]any {
		r := map[string]any{"schema": 1, "name": n, "repository": repository, "policy": "preview", "state": state,
			"identity": map[string]any{}, "nextSequence": 2, "head": nil}
		if state == "retired" {
			r["destination"] = destination
		}
		return r
	}
	state, terminalName := "active", name
	if destination != "" {
		state, terminalName = "retired", destination
	}
	terminal := record(terminalName, "active")
	terminal["head"] = map[string]any{"buildId": "build-fixture", "sequence": 1}
	manifest := map[string]any{"schema": 1, "request": map[string]any{"buildId": "build-fixture", "channel": terminalName,
		"sequence": 1, "repository": repository, "commit": s.commits[1], "sourceVersion": "1.2.3", "version": "0.0.1",
		"identity": map[string]any{}, "bundleEnv": map[string]any{}, "publicBase": sourcePublicBase}, "packages": []any{}}
	return &ChannelResolution{Requested: jv(t, record(name, state)).(*pyjson.Object), Terminal: jv(t, terminal).(*pyjson.Object),
		Manifest: jv(t, manifest).(*pyjson.Object)}
}

func chInstallReader(t *testing.T, result *ChannelResolution) {
	saved := resolveChannel
	resolveChannel = func(context.Context, string, string) (*ChannelResolution, error) {
		return &ChannelResolution{Requested: pyjson.Clone(result.Requested).(*pyjson.Object),
			Terminal: pyjson.Clone(result.Terminal).(*pyjson.Object), Manifest: pyjson.Clone(result.Manifest).(*pyjson.Object)}, nil
	}
	t.Cleanup(func() { resolveChannel = saved })
}

func TestSourceForkChannelCannotSelectOfficialSource(t *testing.T) {
	s := newChSource(t)
	url := "https://github.com/Fixture/fork.git"
	srGit(t, s.root, "config", "remote.origin.url", url)
	srGit(t, s.root, "config", "url."+s.origin+".insteadOf", url)
	chInstallReader(t, chReaderResult(t, s, "preview", "", OfficialRepository))
	if _, err := ResolveSourceTarget(context.Background(), "preview", []string{"git"}, s.root, "", false); err == nil ||
		!strings.Contains(err.Error(), "repository") {
		t.Fatalf("official record selected for a fork %v", err)
	}
	chInstallReader(t, chReaderResult(t, s, "preview", "", "Fixture/fork"))
	target, err := ResolveSourceTarget(context.Background(), "preview", []string{"git"}, s.root, "", false)
	if err != nil || target.Repository != "Fixture/fork" || target.Commit != s.commits[1] {
		t.Fatalf("fork target %+v %v", target, err)
	}
}

func TestSourceRetiredChannelResolvesTheQualifiedBuildBeforeCurrentStable(t *testing.T) {
	s := newChSource(t)
	a := chPublishedTree(t)
	fixture := a.retiredChannel(t, s)
	target, err := ResolveSourceTarget(context.Background(), fixture.name, []string{"git"}, s.root, "", false)
	if err != nil || !target.Retired() || target.Channel != "stable" || target.Commit != s.commits[1] ||
		target.BuildID != fixture.qualified["head"].(map[string]any)["buildId"] {
		t.Fatalf("retired target %+v %v", target, err)
	}
	for _, fault := range []string{"binding", "manifest"} {
		b := chPublishedTree(t)
		f := b.retiredChannel(t, s)
		if fault == "binding" {
			head := f.retired["destinationHead"].(map[string]any)
			seq, _ := head["sequence"].(interface{ Int64() (int64, error) }).Int64()
			head["sequence"] = seq + 1
			b.put(t, "releases/channels/"+f.name+".json", f.retired)
		} else {
			key := f.qualified["head"].(map[string]any)["manifestKey"].(string)
			b.mu.Lock()
			b.objects[key] = append(b.objects[key], ' ')
			b.mu.Unlock()
		}
		if _, err := ResolveSourceTarget(context.Background(), f.name, []string{"git"}, s.root, "", false); err == nil {
			t.Fatalf("%s fault resolved", fault)
		}
	}
}

func TestSourceRetirementRefusesDowngradingNewerSource(t *testing.T) {
	for _, transport := range []string{"git", "shallow", "zip"} {
		s := newChSource(t)
		a := chPublishedTree(t)
		fixture := a.retiredChannel(t, s)
		srGit(t, s.root, "checkout", "--detach", s.commits[2])
		gitCmd := []string{"git"}
		switch transport {
		case "shallow":
			os.RemoveAll(s.root)
			srGit(t, filepath.Dir(s.root), "clone", "--depth=1", "file://"+filepath.ToSlash(s.origin), s.root)
		case "zip":
			updaterecord.SetBuildVersion("1.2.2")
			gitCmd = nil
		}
		_, err := ResolveSourceTarget(context.Background(), fixture.name, gitCmd, s.root, "", false)
		if err == nil || !(strings.Contains(err.Error(), "newer") || strings.Contains(err.Error(), "downgrade")) {
			t.Fatalf("%s retirement %v", transport, err)
		}
		if head := srGit(t, s.root, "rev-parse", "HEAD"); head != s.commits[2] {
			t.Fatalf("%s moved HEAD to %s", transport, head)
		}
	}
}

func TestSourceRetirementCannotVerifyAnUnparsableBuildVersion(t *testing.T) {
	s := newChSource(t)
	a := chPublishedTree(t)
	fixture := a.retiredChannel(t, s)
	updaterecord.SetBuildVersion("dev")
	if _, err := ResolveSourceTarget(context.Background(), fixture.name, nil, s.root, "", false); err == nil ||
		err.Error() != "Source retirement cannot verify the installed source version" {
		t.Fatalf("unparsable version %v", err)
	}
	updaterecord.SetBuildVersion("v1.2.0")
	if target, err := ResolveSourceTarget(context.Background(), fixture.name, nil, s.root, "", false); err != nil || target.Commit != s.commits[1] {
		t.Fatalf("older build %+v %v", target, err)
	}
}

func TestSourceRetirementRejectsInvalidArchiveConstraints(t *testing.T) {
	s := newChSource(t)
	for _, fault := range []string{"floor", "protocol", "build", "chain", "missing"} {
		a := chPublishedTree(t)
		fixture := a.retiredChannel(t, s)
		switch fault {
		case "floor":
			fixture.retired["minimumVersion"] = "9.0.0"
		case "protocol":
			fixture.retired["receiverProtocol"] = 2
		case "build":
			fixture.retired["destinationHead"].(map[string]any)["buildId"] = strings.Repeat("f", 32)
		case "chain":
			fixture.retired["destination"] = fixture.name
		default:
			delete(fixture.retired, "destinationHead")
		}
		a.put(t, "releases/channels/"+fixture.name+".json", fixture.retired)
		if _, err := ResolveSourceTarget(context.Background(), fixture.name, []string{"git"}, s.root, "", false); err == nil {
			t.Fatalf("%s constraint resolved", fault)
		}
	}
}

func TestSourceBuildMetadataReadsOnlyMarkupOutsideScriptAndStyle(t *testing.T) {
	page := `<title>x <META NAME="icebath-build" content="v1.0.1"></title><noscript><meta name="icebath-build" content="v1.0.2"/></noscript>` +
		`<script><meta name="icebath-build" content="v1.0.3"></script><style><meta name="icebath-build"></style><meta name="icebath-build">`
	tags := buildMetadataTags(page)
	if len(tags) != 2 || tags[0] != "v1.0.2" || tags[1] != nil {
		t.Fatalf("tags %v", tags)
	}
	meta := func(content string) string { return `<meta name="icebath-build" content="` + content + `">` }
	for _, tc := range []struct {
		name, page string
		want       []any
	}{
		{"selfclose_script", `<script/>` + meta("a"), []any{"a"}},
		{"selfclose_title", `<title/>` + meta("a"), []any{"a"}},
		{"noscript", `<noscript>` + meta("a") + `</noscript>`, []any{"a"}},
		{"plaintext", `<plaintext>` + meta("a"), nil},
		{"textarea", `<textarea>` + meta("a") + `</textarea>` + meta("b"), []any{"b"}},
		{"title_upper_close", `<title>x</TITLE>` + meta("a"), []any{"a"}},
		{"xmp", `<xmp>` + meta("a") + `</xmp>` + meta("b"), []any{"b"}},
		{"script_lt", `<script>if (a</script) {}</script>` + meta("a"), []any{"a"}},
		{"iframe", `<iframe>` + meta("a") + `</iframe>` + meta("b"), []any{"b"}},
		{"noembed_noframes", `<noembed>` + meta("a") + `</noembed><noframes>` + meta("b") + `</noframes>` + meta("c"), []any{"c"}},
		{"comment", `<!-- ` + meta("a") + ` -->` + meta("b"), []any{"b"}},
		{"unterminated_title", `<title>x ` + meta("q"), nil},
		{"escaped_content", `<META Name="icebath-build" CONTENT="x&amp;y"/>`, []any{"x&y"}},
	} {
		if got := buildMetadataTags(tc.page); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s tags %#v, want %#v", tc.name, got, tc.want)
		}
	}
}
