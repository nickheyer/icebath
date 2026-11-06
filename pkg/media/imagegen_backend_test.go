package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shell gate that holds the first marked upload until the second postprocess finishes or one second passes
const fakeSSHGate = `gate() {
  if [ -e "$CTL/first_pending" ]; then
    rm -f "$CTL/first_pending"; touch "$CTL/first_started"
    for _ in 1 2 3 4 5 6 7 8 9 10; do [ -e "$CTL/second_done" ] && break; sleep 0.1; done
  fi
}
`

// Remote host double whose ssh and scp run on this machine with the remote home in a temp dir
type fakeSSHHost struct {
	remote, ctl, log string
}

func newFakeSSHHost(t *testing.T) *fakeSSHHost {
	t.Helper()
	h := &fakeSSHHost{remote: t.TempDir(), ctl: t.TempDir()}
	h.log = filepath.Join(h.ctl, "calls.log")
	bin := t.TempDir()
	head := "#!/bin/bash\nCTL='" + h.ctl + "'\nREMOTE='" + h.remote + "'\n" + fakeSSHGate
	ssh := head + `echo "ssh $*" >> "$CTL/calls.log"
op=
while [ $# -gt 0 ]; do
  case "$1" in
    -O) op=1; shift 2;;
    -o|-p|-i|-S|-F|-l|-E|-J) shift 2;;
    -*) shift;;
    *) shift; break;;
  esac
done
[ -n "$op" ] && exit 0
cd "$REMOTE" || exit 1
export HOME="$REMOTE"
[ $# -eq 0 ] && exec /bin/bash
case "$*" in "tar xf -"*) gate;; esac
exec /bin/bash -c "$*"
`
	scp := head + `echo "scp $*" >> "$CTL/calls.log"
args=()
while [ $# -gt 0 ]; do case "$1" in -o|-P|-i) shift 2;; -*) shift;; *) args+=("$1"); shift;; esac; done
gate
dst="${args[1]#*:}"
mkdir -p "$(dirname "$dst")" && cp "${args[0]}" "$dst"
`
	for name, body := range map[string]string{"ssh": ssh, "scp": scp} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TERMINAL_ENV", "ssh")
	t.Setenv("TERMINAL_SSH_HOST", "fakehost")
	t.Setenv("TERMINAL_SSH_USER", "tester")
	return h
}

// Live ssh terminal environment of the task, torn down with the test
func (h *fakeSSHHost) start(t *testing.T, env *tools.Env, taskID string) tools.Environment {
	t.Helper()
	t.Cleanup(func() { tools.CleanupVM(env, taskID, true) })
	e, err := tools.EnsureTaskEnv(context.Background(), env, taskID)
	if err != nil || e == nil || e.EnvType() != "ssh" {
		t.Fatalf("ssh environment %v %v", e, err)
	}
	if tools.EnvironmentSyncManager(e) == nil {
		t.Fatal("ssh environment without a file sync manager")
	}
	return e
}

func (h *fakeSSHHost) uploads(t *testing.T) int {
	raw, _ := os.ReadFile(h.log)
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "scp ") || strings.Contains(line, "tar xf -") {
			n++
		}
	}
	return n
}

func writeCacheImage(t *testing.T, env *tools.Env, name, body string) string {
	t.Helper()
	dir := filepath.Join(env.Home, "cache", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertRemoteFile(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("remote %s holds %q %v", path, got, err)
	}
}

func TestPostprocessAddsAgentVisibleImageForActiveSSHEnv(t *testing.T) {
	host := newFakeSSHHost(t)
	env := imageEnv(t, config.Tree{})
	host.start(t, env, "task-1")
	image := writeCacheImage(t, env, "xai_grok-imagine-image_test.jpg", "jpg")
	raw, err := postprocessImageGenerateResult(env, `{"success": true, "image": "`+image+`"}`, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, raw)
	remote := host.remote + "/.icebath/cache/images/xai_grok-imagine-image_test.jpg"
	if out["image"] != image || out["host_image"] != image || out["agent_visible_image"] != remote {
		t.Fatalf("%v", out)
	}
	assertRemoteFile(t, remote, "jpg")
}

func TestConcurrentImageResultsPreserveSharedRemoteSyncState(t *testing.T) {
	host := newFakeSSHHost(t)
	env := imageEnv(t, config.Tree{})
	e := host.start(t, env, "shared-task")
	first := writeCacheImage(t, env, "first.png", "first")
	if err := os.WriteFile(filepath.Join(host.ctl, "first_pending"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() {
		_, errs[0] = postprocessImageGenerateResult(env, `{"success": true, "image": "`+first+`"}`, "shared-task")
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(host.ctl, "first_started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first upload never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := writeCacheImage(t, env, "second.png", "second")
	wg.Go(func() {
		defer os.WriteFile(filepath.Join(host.ctl, "second_done"), nil, 0o644)
		_, errs[1] = postprocessImageGenerateResult(env, `{"success": true, "image": "`+second+`"}`, "shared-task")
	})
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("%v", errs)
	}
	assertRemoteFile(t, host.remote+"/.icebath/cache/images/first.png", "first")
	assertRemoteFile(t, host.remote+"/.icebath/cache/images/second.png", "second")
	before := host.uploads(t)
	tools.EnvironmentSyncManager(e).Sync(true)
	if after := host.uploads(t); after != before {
		t.Fatalf("synced state lost a file, a later sync uploaded again (%d -> %d)", before, after)
	}
}

func TestHandleImageGeneratePostprocessesPluginResult(t *testing.T) {
	host := newFakeSSHHost(t)
	env := imageEnv(t, config.Tree{})
	host.start(t, env, "plugin-task")
	image := writeCacheImage(t, env, "plugin.png", "png")
	var seen []string
	prevEnv, prevDispatch := activeTerminalEnvHook, dispatchToPluginProviderHook
	activeTerminalEnvHook = func(env *tools.Env, taskID string) tools.Environment {
		seen = append(seen, taskID)
		return activeTerminalEnv(env, taskID)
	}
	dispatchToPluginProviderHook = func(context.Context, *tools.Env, string, string, imageSources, map[string]any) (string, bool) {
		return `{"success": true, "image": "` + image + `"}`, true
	}
	t.Cleanup(func() { activeTerminalEnvHook, dispatchToPluginProviderHook = prevEnv, prevDispatch })
	out := decodeResult(t, handleImageGenerate(context.Background(), &tools.Call{Env: env, TaskID: "plugin-task",
		Args: map[string]any{"prompt": "draw", "aspect_ratio": "square"}}))
	if len(seen) != 1 || seen[0] != "plugin-task" || out["agent_visible_image"] != host.remote+"/.icebath/cache/images/plugin.png" {
		t.Fatalf("seen %v out %v", seen, out)
	}
}

var onePixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// Ssh backend that cannot start because no host is configured
func unreachableSandbox(t *testing.T) *tools.Env {
	t.Setenv("TERMINAL_ENV", "ssh")
	t.Setenv("TERMINAL_SSH_HOST", "")
	t.Setenv("TERMINAL_SSH_USER", "")
	return imageEnv(t, config.Tree{})
}

func TestConfineSourceImagesLocalBackendIsPassthrough(t *testing.T) {
	t.Setenv("TERMINAL_ENV", "local")
	url, refs, msg, err := confineSourceImages(context.Background(), imageEnv(t, config.Tree{}), "/some/host/pic.png", []any{"/other/ref.png"}, "t1")
	if err != nil || msg != "" || url != "/some/host/pic.png" || !jsonEqual(refs, []any{"/other/ref.png"}) {
		t.Fatalf("%v %v %q %v", url, refs, msg, err)
	}
}

func TestConfineSourceImagesURLsPassThroughUnderSandbox(t *testing.T) {
	env := unreachableSandbox(t)
	url, refs, msg, err := confineSourceImages(context.Background(), env, "https://x/y.png", []any{"data:image/png;base64,AAAA"}, "t1")
	if err != nil || msg != "" || url != "https://x/y.png" || !jsonEqual(refs, []any{"data:image/png;base64,AAAA"}) {
		t.Fatalf("%v %v %q %v", url, refs, msg, err)
	}
}

func TestConfineSourceImagesPathResolvesThroughSSHSandbox(t *testing.T) {
	host := newFakeSSHHost(t)
	env := imageEnv(t, config.Tree{})
	host.start(t, env, "t1")
	if err := os.WriteFile(filepath.Join(host.remote, "pic.png"), onePixelPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	url, refs, msg, err := confineSourceImages(context.Background(), env, host.remote+"/pic.png", nil, "t1")
	if err != nil || msg != "" || refs != nil {
		t.Fatalf("%v %q %v", refs, msg, err)
	}
	s, _ := url.(string)
	if !strings.HasPrefix(s, "data:image/png;base64,") {
		t.Fatalf("url %.60v", url)
	}
	if got, _ := base64.StdEncoding.DecodeString(strings.SplitN(s, ",", 2)[1]); !bytes.Equal(got, onePixelPNG) {
		t.Fatal("data url does not carry the sandbox bytes")
	}
	if raw, _ := os.ReadFile(host.log); !strings.Contains(string(raw), "pic.png") {
		t.Fatal("source was not read through the ssh sandbox")
	}
}

func TestConfineSourceImagesUnreadablePathReturnsErrorPayload(t *testing.T) {
	env := unreachableSandbox(t)
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("HOST-PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, msg, err := confineSourceImages(context.Background(), env, secret, nil, "t1")
	if err != nil || msg == "" {
		t.Fatalf("%q %v", msg, err)
	}
	out := decodeResult(t, msg)
	if out["success"] != false || !strings.Contains(out["error"].(string), "Could not read source image") || strings.Contains(msg, "HOST-PRIVATE-KEY") {
		t.Fatalf("%s", msg)
	}
}

func TestImageHandlerRejectsBeforeProviderDispatch(t *testing.T) {
	env := unreachableSandbox(t)
	dispatched := 0
	prev := dispatchToPluginProviderHook
	dispatchToPluginProviderHook = func(context.Context, *tools.Env, string, string, imageSources, map[string]any) (string, bool) {
		dispatched++
		return "", false
	}
	t.Cleanup(func() { dispatchToPluginProviderHook = prev })
	out := decodeResult(t, handleImageGenerate(context.Background(), &tools.Call{Env: env, TaskID: "t1",
		Args: map[string]any{"prompt": "edit it", "image_url": filepath.Join(t.TempDir(), "nope.png")}}))
	if out["success"] != false || dispatched != 0 {
		t.Fatalf("%v dispatched %d", out, dispatched)
	}
}

func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func decodeDataURLImage(t *testing.T, u string) image.Image {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(strings.SplitN(u, ",", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestShrinkDataURLKeepsAlphaAsPNGAndReencodesOpaqueAsJPEG(t *testing.T) {
	env := imageEnv(t, config.Tree{})
	alpha := solidRGBA(400, 300, color.NRGBA{10, 200, 30, 128})
	out := shrinkDataURL(env, dataURL("image/png", pngBytes(t, alpha)), 1<<30)
	if !strings.HasPrefix(out, "data:image/png;base64,") || decodeDataURLImage(t, out).Bounds() != image.Rect(0, 0, 400, 300) {
		t.Fatalf("alpha source %.40s", out)
	}
	opaque := solidRGBA(400, 300, color.NRGBA{10, 200, 30, 255})
	out = shrinkDataURL(env, dataURL("image/png", pngBytes(t, opaque)), 1<<30)
	if !strings.HasPrefix(out, "data:image/jpeg;base64,") || decodeDataURLImage(t, out).Bounds() != image.Rect(0, 0, 400, 300) {
		t.Fatalf("opaque source %.40s", out)
	}
}

func TestShrinkDataURLAppliesExifOrientation(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			c := color.NRGBA{0, 0, 255, 255}
			if x < 20 {
				c = color.NRGBA{255, 0, 0, 255}
			}
			src.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	tiff := []byte{'I', 'I', 0x2a, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	app1 := append([]byte{0xff, 0xe1, 0, byte(2 + 6 + len(tiff))}, append([]byte("Exif\x00\x00"), tiff...)...)
	raw := append(append([]byte{0xff, 0xd8}, app1...), buf.Bytes()[2:]...)
	out := decodeDataURLImage(t, shrinkDataURL(imageEnv(t, config.Tree{}), dataURL("image/jpeg", raw), 1<<30))
	if out.Bounds() != image.Rect(0, 0, 20, 40) {
		t.Fatalf("bounds %v", out.Bounds())
	}
	top, bottom := color.NRGBAModel.Convert(out.At(10, 5)).(color.NRGBA), color.NRGBAModel.Convert(out.At(10, 35)).(color.NRGBA)
	if top.R < 200 || top.B > 60 || bottom.B < 200 || bottom.R > 60 {
		t.Fatalf("orientation 6 not transposed, top %v bottom %v", top, bottom)
	}
}

func TestShrinkDataURLStopsAtTheFloor(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 512))
	r := rand.New(rand.NewPCG(3, 4))
	for i := range img.Pix {
		img.Pix[i] = byte(r.UintN(256))
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	out := decodeDataURLImage(t, shrinkDataURL(imageEnv(t, config.Tree{}), dataURL("image/png", pngBytes(t, img)), 100))
	if out.Bounds() != image.Rect(0, 0, 243, 122) {
		t.Fatalf("bounds %v", out.Bounds())
	}
}

func TestShrinkDataURLPassesThroughWhatPillowCannotOpen(t *testing.T) {
	env := imageEnv(t, config.Tree{})
	for _, u := range []string{"data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>")), "data:image/png;base64,@@@"} {
		if got := shrinkDataURL(env, u, 10); got != u {
			t.Fatalf("%q -> %.40q", u, got)
		}
	}
}
