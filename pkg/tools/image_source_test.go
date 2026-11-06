package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Decodes a base64 fixture or fails the test binary at init
func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// Minimal valid 1x1 grey PNG
var isrcPNG = mustB64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

// PNG shaped bytes whose image data chunk fails its checksum
var isrcCorruptPNG = mustB64("iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAIAAAACUFjqAAAAFElEQVR4nGP8z8Dwn4EIwESJ5gAAVQ4CH1evYJQAAAAASUVORK5CYII=")

// 640x480 HEIC still from the gen2brain heic test corpus
var isrcHEIC = mustB64("AAAAHGZ0eXBoZWljAAAAAG1pZjFoZWljbWlhZgAAAY5tZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAAA5waXRtAAAAAAABAAAANGlsb2MAAAAAREAAAgABAAAAAAGyAAEAAAAAAAAB1AACAAAAAAOGAAEAAAAAAAAA5AAAADhpaW5mAAAAAAACAAAAFWluZmUCAAAAAAEAAGh2YzEAAAAAFWluZmUCAAABAAIAAEV4aWYAAAAAzWlwcnAAAACuaXBjbwAAAHlodmNDAQNwAAAAAAAAAAAAWvAA/P34+AAADwNgAAEAGEABDAH//wNwAAADAJAAAAMAAAMAWroCQGEAAQAsQgEBA3AAAAMAkAAAAwAAAwBaoAUCAeFlupJKa5uAhoMCAAADADIAAAMAAhBiAAEAB0QBwXKwYkAAAAAUaXNwZQAAAAAAAAKAAAAB4AAAABBwaXhpAAAAAAMICAgAAAAJaXJvdAMAAAAXaXBtYQAAAAAAAAABAAEEgQIDhAAAABppcmVmAAAAAAAAAA5jZHNjAAIAAQABAAACwG1kYXQAAAHQKAGvExB7DZ2ufw1k5TKPE//K+YnsdKChs9tP9NRKDeHdR62JE7rBlMqcWFV8SfFPnuCGhviCUvtAd8ZvhLPJ9bLynm9+beFUnrRf354QX0OrRr8ArMJ1fxeAAbLu0VkoI8vAPtKjDIpjCsIC6aulFw6/BFJLAp+J+oueLdF5W7Ld14VzcYejal/pF6oLQxQBEwPoAAADAAADADXgqLFxxDLFToGyXIAlkg25bSijcf+rh9IO04Te79jOlkkRCxTJ5ODQ3mziXw15CIvPfQYKbUfEHoqhWcHAmtFuGLCkZNwotC2m+xbwOIdycWk4afqgRg2X8rzLx6AAAAMAAAMAAAMAAAMAHNAb4h3ClqEPq2GMdJEDvzFT/ciuf3hCSR8fwvx5pOlx1JF0FI2Fv9IaSBrxKLMIqIAC6oAe7AQFAS0AmeBG0CPpJN/5+QRrrheOQ68HxtVVq9L1de4kDTrJykpwnBrZ5TNfjIMXFZEEYfSPRbeKib5Mmm7Os8ytfd4tQeua+s9Zb2m+aoBUTl9FZcZM5fimyYD1BCsl15Ib5DnaexxRaySFnbcy2AAB3AAAAwAAAwAABmyF4lqV8pR+Cks2lPbJ1RXKiAAAAwAAGVAAAAAGRXhpZgAATU0AKgAAAAgACAEPAAIAAAAIAAAAbgEQAAIAAAAJAAAAdgESAAMAAAABAAYAAAEaAAUAAAABAAAAgAEbAAUAAAABAAAAiAEoAAMAAAABAAEAAAITAAMAAAABAAEAAIdpAAQAAAABAAAAkAAAAABUZXN0Q2FtAE1vZGVsMTIzAAAAAAABAAAAAQAAAAEAAAABAAWCnQAFAAAAAQAAANKIJwADAAAAAQMgAACQAAAHAAAABDAyMzKRAQAHAAAABAECAwCgAQADAAAAAf//AAAAAAAAAAAAHAAAAAU=")

// ISO BMFF headers exercising the ftyp brand scan
var (
	heicHeader          = append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"), make([]byte, 32)...)
	avifHeader          = append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1"), make([]byte, 32)...)
	mif1Header          = append([]byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1heic"), make([]byte, 32)...)
	mif1MajorAvifCompat = append([]byte("\x00\x00\x00\x1cftypmif1\x00\x00\x00\x00mif1avif"), make([]byte, 32)...)
	mif1MajorAv01Compat = append([]byte("\x00\x00\x00 ftypmif1\x00\x00\x00\x00mif1av01avif"), make([]byte, 32)...)
)

// Sandbox double answering exec reads with a scripted function
type visionFakeSandbox struct {
	Environment
	exec    func(cmd string) (*ExecResult, error)
	hostCwd string
	mount   string
}

func (f *visionFakeSandbox) Execute(ctx context.Context, cmd string, opts ExecOptions) (*ExecResult, error) {
	return f.exec(cmd)
}
func (f *visionFakeSandbox) HostCwd() string      { return f.hostCwd }
func (f *visionFakeSandbox) HostCwdMount() string { return f.mount }

// Session env rooted at a fresh home under the given terminal backend
func isrcEnv(t *testing.T, backend string) *Env {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_ENV", backend)
	ResetTerminalScopeCache()
	return &Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}, Cwd: root}
}

// Replaces the sandbox seams for one test
func stubSandbox(t *testing.T, ensure func(taskID string), active func(taskID string) Environment) {
	t.Helper()
	prevEnsure, prevActive, prevDelay := imageSourceEnsureEnv, imageSourceActiveEnv, imageSourceRetryDelay
	imageSourceEnsureEnv = func(ctx context.Context, env *Env, taskID string) {
		if ensure != nil {
			ensure(taskID)
		}
	}
	imageSourceActiveEnv = func(env *Env, taskID string) Environment {
		if active == nil {
			return nil
		}
		return active(taskID)
	}
	imageSourceRetryDelay = 0
	t.Cleanup(func() {
		imageSourceEnsureEnv, imageSourceActiveEnv, imageSourceRetryDelay = prevEnsure, prevActive, prevDelay
	})
}

// Failure class of a resolver error, empty when it is not one
func resolutionKind(err error) string {
	var re *ImageResolutionError
	if errors.As(err, &re) {
		return re.Kind
	}
	return ""
}

func TestImageSourceDataURL(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, nil)
	res, err := ResolveImageSource(context.Background(), env, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(isrcPNG), "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Data, isrcPNG) || res.Mime != "image/png" || res.Origin != "data" {
		t.Fatalf("got mime %q origin %q", res.Mime, res.Origin)
	}
	_, err = ResolveImageSource(context.Background(), env, "data:text/plain;base64,"+base64.StdEncoding.EncodeToString([]byte("not an image")), "")
	if resolutionKind(err) != ImageNotAnImage {
		t.Fatalf("non image data URL: %v", err)
	}
	_, err = ResolveImageSource(context.Background(), env, "data:image/png,rawtext", "")
	if resolutionKind(err) != ImageNotAnImage || !strings.Contains(err.Error(), "base64-encoded") {
		t.Fatalf("non base64 data URL: %v", err)
	}
}

func TestImageSourceCorruptPNGRejectedAtResolverBoundary(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, nil)
	img := filepath.Join(t.TempDir(), "corrupt.png")
	os.WriteFile(img, isrcCorruptPNG, 0o644)
	if _, err := ResolveImageSource(context.Background(), env, img, ""); resolutionKind(err) != ImageNotAnImage {
		t.Fatalf("corrupt PNG: %v", err)
	}
}

func TestImageSourceLocalBackend(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, func(string) Environment { t.Fatal("local backend touched the sandbox"); return nil })
	img := filepath.Join(t.TempDir(), "outside", "pic.png")
	os.MkdirAll(filepath.Dir(img), 0o755)
	os.WriteFile(img, isrcPNG, 0o644)
	res, err := ResolveImageSource(context.Background(), env, img, "")
	if err != nil || !bytes.Equal(res.Data, isrcPNG) || res.Origin != "file" {
		t.Fatalf("any host path: %v %+v", err, res)
	}
	os.WriteFile(filepath.Join(env.Cwd, "pic.png"), isrcPNG, 0o644)
	res, err = ResolveImageSource(context.Background(), env, "pic.png", "")
	if err != nil || res.Origin != "file" {
		t.Fatalf("bare relative path: %v", err)
	}
	_, err = ResolveImageSource(context.Background(), env, filepath.Join(env.Cwd, "missing.png"), "")
	if resolutionKind(err) != ImageSourceNotFound || !strings.Contains(err.Error(), "media file not found") {
		t.Fatalf("missing file: %v", err)
	}
	_, err = ResolveImageSource(context.Background(), env, "ftp://example.com/x.png", "")
	if resolutionKind(err) != ImageUnsupportedScheme {
		t.Fatalf("ftp scheme: %v", err)
	}
	res, err = ResolveImageSource(context.Background(), env, "file://"+img, "")
	if err != nil || res.Origin != "file" {
		t.Fatalf("file URI: %v", err)
	}
}

func TestImageSourceNonLocalConfinement(t *testing.T) {
	t.Run("media cache path host read", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		cached := filepath.Join(env.Home, "cache", "images", "inbound.png")
		os.MkdirAll(filepath.Dir(cached), 0o755)
		os.WriteFile(cached, isrcPNG, 0o644)
		res, err := ResolveImageSource(context.Background(), env, cached, "")
		if err != nil || res.Origin != "file" || !bytes.Equal(res.Data, isrcPNG) {
			t.Fatalf("cache read: %v", err)
		}
	})
	t.Run("desktop upload images dir host read", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		upload := filepath.Join(env.Home, "images", "upload_20260722_181019_1.png")
		os.MkdirAll(filepath.Dir(upload), 0o755)
		os.WriteFile(upload, isrcPNG, 0o644)
		res, err := ResolveImageSource(context.Background(), env, upload, "")
		if err != nil || res.Origin != "file" {
			t.Fatalf("upload read: %v", err)
		}
	})
	t.Run("host secret outside cache routes to sandbox", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		secret := filepath.Join(t.TempDir(), "id_rsa")
		os.WriteFile(secret, []byte("HOST-PRIVATE-KEY-DO-NOT-LEAK"), 0o600)
		var seen string
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				seen = cmd
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
			}}
		})
		res, err := ResolveImageSource(context.Background(), env, secret, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if res.Origin != "container" || !bytes.Equal(res.Data, isrcPNG) || bytes.Contains(res.Data, []byte("HOST-PRIVATE-KEY")) {
			t.Fatalf("expected container bytes, origin %q", res.Origin)
		}
		if !strings.Contains(seen, "head -c") || !strings.Contains(seen, "< ") {
			t.Fatalf("exec read not bounded and redirected: %q", seen)
		}
	})
	t.Run("non cache path fails closed without sandbox", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		secret := filepath.Join(t.TempDir(), "id_rsa")
		os.WriteFile(secret, []byte("HOST-PRIVATE-KEY"), 0o600)
		if _, err := ResolveImageSource(context.Background(), env, secret, "t1"); resolutionKind(err) != ImageSourceNotFound {
			t.Fatalf("expected fail closed: %v", err)
		}
	})
	t.Run("symlink in cache pointing outside is not host read", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		secret := filepath.Join(t.TempDir(), "outside", "id_rsa")
		os.MkdirAll(filepath.Dir(secret), 0o755)
		os.WriteFile(secret, []byte("HOST-PRIVATE-KEY"), 0o600)
		dir := filepath.Join(env.Home, "cache", "images")
		os.MkdirAll(dir, 0o755)
		link := filepath.Join(dir, "sneaky.png")
		if err := os.Symlink(secret, link); err != nil {
			t.Skip("symlinks unsupported")
		}
		if _, err := ResolveImageSource(context.Background(), env, link, "t1"); resolutionKind(err) != ImageSourceNotFound {
			t.Fatalf("symlink escape must fail closed: %v", err)
		}
	})
}

func TestImageSourceExecReadSafety(t *testing.T) {
	t.Run("bounded and redirect safe", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		var seen string
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				seen = cmd
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
			}}
		})
		if _, err := ResolveImageSource(context.Background(), env, "/workspace/-i-etc-shadow.png", "t1"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(seen, fmt.Sprintf("head -c %d < ", maxImageIngestBytes+1)) || !strings.Contains(seen, "< /workspace/-i-etc-shadow.png ") {
			t.Fatalf("command %q", seen)
		}
		if !strings.HasSuffix(seen, `| base64 | tr -d '\n'`) {
			t.Fatalf("command tail %q", seen)
		}
	})
	t.Run("retries cold start then succeeds", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		calls := 0
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				calls++
				if calls == 1 {
					return &ExecResult{ReturnCode: 1}, nil
				}
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
			}}
		})
		res, err := ResolveImageSource(context.Background(), env, "/workspace/cold.png", "t1")
		if err != nil || res.Origin != "container" || calls != 2 {
			t.Fatalf("err %v calls %d", err, calls)
		}
	})
	t.Run("retries exhausted include diagnostic", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				return &ExecResult{ReturnCode: 1, Output: "head: can't open '/x': No such file or directory"}, nil
			}}
		})
		_, err := ResolveImageSource(context.Background(), env, "/workspace/missing.png", "t1")
		if resolutionKind(err) != ImageSourceNotFound || !strings.Contains(err.Error(), "No such file or directory") {
			t.Fatalf("diagnostic missing: %v", err)
		}
	})
	t.Run("mounted host path translated to the container mount", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		var seen string
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{hostCwd: "/home/me/proj", mount: "/workspace", exec: func(cmd string) (*ExecResult, error) {
				seen = cmd
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
			}}
		})
		if _, err := ResolveImageSource(context.Background(), env, "/home/me/proj/shots/a.png", "t1"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(seen, "< /workspace/shots/a.png ") {
			t.Fatalf("not translated: %q", seen)
		}
	})
}

func TestImageSourceSVG(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, nil)
	svg := filepath.Join(t.TempDir(), "art.svg")
	os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"/>`), 0o644)
	prev := rasterizeSVGToPNG
	t.Cleanup(func() { rasterizeSVGToPNG = prev })
	rasterizeSVGToPNG = func(svgPath, outPath string) bool { return os.WriteFile(outPath, isrcPNG, 0o644) == nil }
	res, err := ResolveImageSource(context.Background(), env, svg, "")
	if err != nil || res.Mime != "image/svg+xml" {
		t.Fatalf("svg resolve: %v %+v", err, res)
	}
	path, mime, msg := normalizeToSupportedImage(env, svg, "image/svg+xml")
	if msg != "" || mime != "image/png" {
		t.Fatalf("svg normalize: %q %q", mime, msg)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, isrcPNG) {
		t.Fatal("rasterized bytes not used")
	}
	os.Remove(path)
	rasterizeSVGToPNG = func(svgPath, outPath string) bool { return false }
	path, _, msg = normalizeToSupportedImage(env, svg, "image/svg+xml")
	if path != "" || !strings.Contains(msg, "rasterizer") {
		t.Fatalf("no converter: %q %q", path, msg)
	}
}

func TestImageSourceLazySandboxBringUp(t *testing.T) {
	t.Run("first read brings up the sandbox then reads", func(t *testing.T) {
		env := isrcEnv(t, "ssh")
		var broughtUp []string
		sandbox := &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
			return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
		}}
		stubSandbox(t, func(taskID string) { broughtUp = append(broughtUp, taskID) }, func(string) Environment {
			if len(broughtUp) > 0 {
				return sandbox
			}
			return nil
		})
		res, err := ResolveImageSource(context.Background(), env, "/tmp/test.png", "t1")
		if err != nil || res.Origin != "container" || len(broughtUp) != 1 || broughtUp[0] != "t1" {
			t.Fatalf("err %v brought %v", err, broughtUp)
		}
	})
	t.Run("bring up without an env still fails closed", func(t *testing.T) {
		env := isrcEnv(t, "ssh")
		stubSandbox(t, func(string) {}, nil)
		secret := filepath.Join(t.TempDir(), "id_rsa")
		os.WriteFile(secret, []byte("HOST-PRIVATE-KEY"), 0o600)
		if _, err := ResolveImageSource(context.Background(), env, secret, "t1"); resolutionKind(err) != ImageSourceNotFound {
			t.Fatalf("expected fail closed: %v", err)
		}
	})
}

func TestHeicDetection(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"heic brand", heicHeader, "image/heic"},
		{"generic mif1 brand", mif1Header, "image/heic"},
		{"avif brand", avifHeader, "image/avif"},
		{"mif1 major avif compatible", mif1MajorAvifCompat, "image/avif"},
		{"mif1 major av01 compatible", mif1MajorAv01Compat, "image/avif"},
		{"scan stays inside the ftyp box", append([]byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00heic\x00\x00\x00\x10metaavif"), make([]byte, 32)...), "image/heic"},
		{"honest oversized box scans what arrived", append(binary.BigEndian.AppendUint32(nil, 9999), []byte("ftypmif1\x00\x00\x00\x00mif1avif")...), "image/avif"},
		{"truncated ftyp header", []byte("\x00\x00\x00\x18ftyp"), ""},
		{"bogus huge box size", []byte("\xff\xff\xff\xffftypmif1\x00\x00\x00\x00mif1avif"), "image/avif"},
		{"mp4 brand not an image", append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), make([]byte, 32)...), ""},
	}
	for _, c := range cases {
		if got := detectImageMimeTypeFromBytes(c.data); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	for _, size := range []uint32{0, 1, 4, 8, 12, 15, 17, 18, 19, 21, 22, 23} {
		hdr := binary.BigEndian.AppendUint32(nil, size)
		hdr = append(hdr, []byte("ftypmif1\x00\x00\x00\x00heic\x00\x00\x00\x10metaavif")...)
		hdr = append(hdr, make([]byte, 24)...)
		if got := detectImageMimeTypeFromBytes(hdr); got != "image/heic" {
			t.Errorf("declared size %d widened the scan: %q", size, got)
		}
	}
}

func TestHeicResolvesAndNormalizesToPNG(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, nil)
	path := filepath.Join(t.TempDir(), "photo.heic")
	os.WriteFile(path, isrcHEIC, 0o644)
	res, err := ResolveImageSource(context.Background(), env, path, "")
	if err != nil || res.Mime != "image/heic" {
		t.Fatalf("resolve heic: %v %+v", err, res)
	}
	out, mime, msg := normalizeToSupportedImage(env, path, "image/heic")
	if msg != "" || mime != "image/png" {
		t.Fatalf("normalize heic: %q", msg)
	}
	b, _ := os.ReadFile(out)
	if !bytes.HasPrefix(b, pngSignature) {
		t.Fatal("not a PNG")
	}
	if w, h, _, err := rasterConfig(b); err != nil || w != 480 || h != 640 {
		t.Fatalf("converted size %dx%d %v", w, h, err)
	}
	os.Remove(out)
}

func TestHeicAndAvifActionableDecodeErrors(t *testing.T) {
	env := isrcEnv(t, "local")
	dir := t.TempDir()
	heicPath := filepath.Join(dir, "photo.heic")
	os.WriteFile(heicPath, heicHeader, 0o644)
	path, _, msg := normalizeToSupportedImage(env, heicPath, "image/heic")
	if path != "" || !strings.Contains(msg, "HEIC/HEIF") {
		t.Fatalf("heic error %q", msg)
	}
	avifPath := filepath.Join(dir, "broken.avif")
	os.WriteFile(avifPath, avifHeader, 0o644)
	path, _, msg = normalizeToSupportedImage(env, avifPath, "image/avif")
	if path != "" || !strings.Contains(msg, "AV1") {
		t.Fatalf("avif error %q", msg)
	}
}

func TestResolveLocalSourceToDataURL(t *testing.T) {
	t.Run("urls pass through under a sandbox", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		for _, u := range []string{"https://x/y.png", "data:image/png;base64,AAAA", ""} {
			got, err := ResolveLocalSourceToDataURL(context.Background(), env, u, "t1")
			if err != nil || got != u {
				t.Fatalf("%q -> %q %v", u, got, err)
			}
		}
	})
	t.Run("path resolves to a data URL of the container bytes", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, func(string) Environment {
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(isrcPNG)}, nil
			}}
		})
		got, err := ResolveLocalSourceToDataURL(context.Background(), env, "/workspace/pic.png", "t1")
		if err != nil || !strings.HasPrefix(got, "data:image/png;base64,") {
			t.Fatalf("%q %v", got, err)
		}
		if b, _ := base64.StdEncoding.DecodeString(strings.SplitN(got, ",", 2)[1]); !bytes.Equal(b, isrcPNG) {
			t.Fatal("payload mismatch")
		}
	})
	t.Run("unreadable path returns an error", func(t *testing.T) {
		env := isrcEnv(t, "docker")
		stubSandbox(t, nil, nil)
		if _, err := ResolveLocalSourceToDataURL(context.Background(), env, "/workspace/pic.png", "t1"); resolutionKind(err) != ImageSourceNotFound {
			t.Fatalf("expected not found: %v", err)
		}
	})
}

func TestImageSourceVideoPermitted(t *testing.T) {
	env := isrcEnv(t, "local")
	stubSandbox(t, nil, nil)
	dir := t.TempDir()
	mp4 := append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), make([]byte, 64)...)
	clip := filepath.Join(dir, "clip.bin")
	os.WriteFile(clip, mp4, 0o644)
	res, err := ResolveImageSource(context.Background(), env, clip, "", "video")
	if err != nil || res.Mime != "video/mp4" {
		t.Fatalf("ftyp sniff: %v %+v", err, res)
	}
	webm := filepath.Join(dir, "clip.webm")
	os.WriteFile(webm, []byte("\x1aE\xdf\xa3 webm payload"), 0o644)
	if res, err := ResolveImageSource(context.Background(), env, webm, "", "video"); err != nil || res.Mime != "video/webm" {
		t.Fatalf("extension table: %v %+v", err, res)
	}
	img := filepath.Join(dir, "pic.png")
	os.WriteFile(img, isrcPNG, 0o644)
	if _, err := ResolveImageSource(context.Background(), env, img, "", "video"); err == nil || !strings.Contains(err.Error(), "takes a video") {
		t.Fatalf("image for a video argument: %v", err)
	}
	junk := filepath.Join(dir, "junk.bin")
	os.WriteFile(junk, []byte("plain text"), 0o644)
	if _, err := ResolveImageSource(context.Background(), env, junk, "", "video"); err == nil || !strings.Contains(err.Error(), "mp4 expected") {
		t.Fatalf("junk video: %v", err)
	}
}

func TestReadBlockErrorExported(t *testing.T) {
	h := t.TempDir()
	env := &Env{Home: h}
	if got := ReadBlockError(env, filepath.Join(h, "auth.json")); !strings.Contains(got, "is an icebath credential store") {
		t.Fatal(got)
	}
	if got := ReadBlockError(env, filepath.Join(h, "skills", ".hub", "lock.json")); !strings.Contains(got, "internal icebath cache file") {
		t.Fatal(got)
	}
	if got := ReadBlockError(env, filepath.Join(h, "notes.txt")); got != "" {
		t.Fatal(got)
	}
}
