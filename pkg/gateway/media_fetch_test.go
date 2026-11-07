package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeRemoteEnv struct {
	tools.Environment
	files   map[string][]byte
	links   map[string]string
	fetched []string
}

func (f *fakeRemoteEnv) EnvType() string            { return "ssh" }
func (f *fakeRemoteEnv) RemoteHome() (string, bool) { return "/home/agent", true }
func (f *fakeRemoteEnv) FetchRealpath(_ context.Context, p string) string {
	if target, ok := f.links[p]; ok {
		return target
	}
	return p
}
func (f *fakeRemoteEnv) FetchFile(_ context.Context, remote, dest string, maxBytes int) error {
	f.fetched = append(f.fetched, remote)
	data, ok := f.files[remote]
	if !ok {
		return &tools.FileFetchError{Msg: "missing"}
	}
	return os.WriteFile(dest, data, 0o600)
}

// Installs a fake ssh sandbox as the active remote env with the document cache as the only safe root
func withRemoteEnv(t *testing.T) (*fakeRemoteEnv, string) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "")
	env := &fakeRemoteEnv{
		files: map[string][]byte{"/home/agent/out/report.txt": []byte("hello from sandbox"), "/home/agent/.ssh/id_rsa": []byte("SECRET")},
		links: map[string]string{"/home/agent/out/innocent.txt": "/home/agent/.ssh/id_rsa"},
	}
	savedEnv, savedRoots := activeRemoteEnvSeam, mediaDeliverySafeRoots
	activeRemoteEnvSeam = func(context.Context) (tools.Environment, error) { return env, nil }
	mediaDeliverySafeRoots = func() []string { return []string{filepath.Join(dir, "cache", "documents")} }
	t.Cleanup(func() { activeRemoteEnvSeam, mediaDeliverySafeRoots = savedEnv, savedRoots })
	return env, dir
}

func TestSandboxArtifactIsFetchedButCredentialsAndSymlinksToThemAreNot(t *testing.T) {
	env, _ := withRemoteEnv(t)
	media := []MediaRef{{Path: "/home/agent/out/report.txt"}, {Path: "/home/agent/out/innocent.txt"}, {Path: "/home/agent/.ssh/id_rsa"}, {Path: "~/out/report.txt"}}
	delivered, err := FilterMediaDeliveryPaths(context.Background(), media)
	if err != nil || len(delivered) != 2 {
		t.Fatalf("%v %v", delivered, err)
	}
	for _, m := range delivered {
		data, err := os.ReadFile(m.Path)
		if err != nil || string(data) != "hello from sandbox" || !strings.HasSuffix(filepath.Base(m.Path), "_report.txt") {
			t.Fatalf("%s %q %v", m.Path, data, err)
		}
	}
	if !reflect.DeepEqual(env.fetched, []string{"/home/agent/out/report.txt", "/home/agent/out/report.txt"}) {
		t.Fatalf("fetched %v", env.fetched)
	}
}

func TestLocalBackendAndStrictModeDoNotFetch(t *testing.T) {
	env, dir := withRemoteEnv(t)
	ctx := context.Background()
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "1")
	if got, err := FilterMediaDeliveryPaths(ctx, []MediaRef{{Path: "/home/agent/out/report.txt"}}); err != nil || len(got) != 0 {
		t.Fatalf("strict %v %v", got, err)
	}
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "")
	activeRemoteEnvSeam = func(context.Context) (tools.Environment, error) { return nil, nil }
	var dropped []map[string]any
	if got, err := FilterMediaDeliveryPathsFor(ctx, []MediaRef{{Path: filepath.Join(dir, "nope.txt")}}, "", &dropped); err != nil || len(got) != 0 {
		t.Fatalf("local %v %v", got, err)
	}
	if len(env.fetched) != 0 || len(dropped) != 1 || dropped[0]["reason"] != "not found on this host" {
		t.Fatalf("%v %v", env.fetched, dropped)
	}
}

func TestFilterKeepsSafeMediaAndDropsUnsafe(t *testing.T) {
	_, dir := withRemoteEnv(t)
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "1")
	t.Setenv("ICEBATH_MEDIA_TRUST_RECENT_FILES", "0")
	root := filepath.Join(dir, "cache", "documents")
	safe, unsafe := filepath.Join(root, "speech.ogg"), filepath.Join(dir, "outside.ogg")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{safe, unsafe} {
		if err := os.WriteFile(p, []byte("OggS"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var dropped []map[string]any
	got, err := FilterMediaDeliveryPathsFor(context.Background(), []MediaRef{{Path: unsafe}, {Path: safe, IsVoice: true}}, "", &dropped)
	resolved, _ := filepath.EvalSymlinks(safe)
	if err != nil || !reflect.DeepEqual(got, []MediaRef{{Path: resolved, IsVoice: true}}) {
		t.Fatalf("%v %v", got, err)
	}
	if len(dropped) != 1 || dropped[0]["path"] != unsafe || dropped[0]["reason"] != "denied by the delivery policy" {
		t.Fatalf("dropped %v", dropped)
	}
	local, err := FilterLocalDeliveryPaths(context.Background(), []string{unsafe, safe}, "")
	if err != nil || !reflect.DeepEqual(local, []string{resolved}) {
		t.Fatalf("local %v %v", local, err)
	}
}

func TestRemotePathDenylist(t *testing.T) {
	for _, c := range []struct {
		path, home string
		want       bool
	}{
		{"relative/x", "", true}, {"/etc/passwd", "", true}, {"/root/out.txt", "/root", false}, {"/root/.ssh/id", "/root", true},
		{"/srv/a/.ssh/key", "", true}, {"/srv/a/.icebath/auth.json", "", true}, {"/srv/a/out.txt", "", false},
		{"/home/agent/.ssh/x", "/home/agent", true}, {"/srv/.ssh/x", "/home/agent", false}, {"/home/agent/out/../.aws/c", "/home/agent", true},
	} {
		if got := RemotePathIsDenied(c.path, c.home); got != c.want {
			t.Fatalf("%v: %v", c, got)
		}
	}
}

func TestVoiceRouting(t *testing.T) {
	for _, ext := range []string{".mp3", ".wav", ".flac", ".m4a", ".ogg", ".opus"} {
		if !ShouldSendMediaAsAudio("telegram", ext, true) {
			t.Fatalf("%s with is_voice must route to the voice sender", ext)
		}
	}
	if !ShouldSendMediaAsAudio("telegram", ".mp3", false) || !ShouldSendMediaAsAudio("telegram", ".M4A", false) ||
		ShouldSendMediaAsAudio("telegram", ".ogg", false) || ShouldSendMediaAsAudio("telegram", ".wav", false) {
		t.Fatal("telegram without intent")
	}
	if !ShouldSendMediaAsAudio("feishu", ".mp3", false) || ShouldSendMediaAsAudio("feishu", ".qzx7", false) || ShouldSendMediaAsAudio("telegram", ".txt", true) {
		t.Fatal("other platforms")
	}
}
