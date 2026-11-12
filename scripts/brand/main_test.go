package main

import (
	"bytes"
	"encoding/json"
	"go/format"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/image/tiff"
)

const (
	stableTag = "v1.2.3"
	canaryTag = "v1.2.3+canary.20260911T010203Z"
)

func sourceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func envOf(tag, commit string) func(string) string {
	return func(k string) string {
		switch k {
		case "ICEBATH_PAYLOAD_TAG":
			return tag
		case "ICEBATH_BUILD_COMMIT":
			return commit
		}
		return ""
	}
}

func subset(rels ...string) []target {
	want := map[string]bool{}
	for _, r := range rels {
		want[r] = true
	}
	var out []target
	for _, t := range allTargets() {
		if want[t.rel] || want[filepath.Dir(t.rel)] {
			out = append(out, t)
		}
	}
	return out
}

// Writes the targets for one build identity into a fresh directory and returns it
func generate(t *testing.T, tag, commit string, targets []target) string {
	t.Helper()
	stable, desktop, err := buildArt(sourceRoot(t), envOf(tag, commit))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	var log bytes.Buffer
	if code := cmdWrite(&log, targets, stable, desktop, out); code != 0 {
		t.Fatalf("write failed:\n%s", log.String())
	}
	return out
}

// Every native frame of a PNG, ICO or ICNS output
func framesOf(t *testing.T, path string) []*image.NRGBA {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var imgs []image.Image
	switch filepath.Ext(path) {
	case ".ico":
		imgs, _, err = readICO(data)
	case ".icns":
		var reps map[string]image.Image
		reps, err = readICNS(data)
		for _, k := range icnsTypes {
			imgs = append(imgs, reps[k.kind])
		}
	default:
		var img image.Image
		img, err = png.Decode(bytes.NewReader(data))
		imgs = []image.Image{img}
	}
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := make([]*image.NRGBA, len(imgs))
	for i, img := range imgs {
		out[i] = toNRGBA(img)
	}
	return out
}

func opaqueBox(img *image.NRGBA) image.Rectangle {
	box := image.Rectangle{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.NRGBAAt(x, y).A >= 128 {
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return box
}

// Plate colour read below the mark at the bottom centre of the opaque tile
func tileColor(img *image.NRGBA) color.NRGBA {
	box := opaqueBox(img)
	return img.NRGBAAt((box.Min.X+box.Max.X)/2, box.Max.Y-1-max(1, box.Dy()/12))
}

func hsv(c color.NRGBA) (h, s, v float64) {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	hi, lo := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	v = hi
	if hi == 0 || hi == lo {
		return 0, 0, v
	}
	s = (hi - lo) / hi
	switch hi {
	case r:
		h = math.Mod((g-b)/(hi-lo), 6)
	case g:
		h = (b-r)/(hi-lo) + 2
	default:
		h = (r-g)/(hi-lo) + 4
	}
	h /= 6
	if h < 0 {
		h++
	}
	return h, s, v
}

func isDarkTile(rel string) bool {
	name := filepath.Base(rel)
	return strings.Contains(name, "dark") || strings.HasSuffix(name, "_altform-unplated.png")
}

func sameAlpha(t *testing.T, rel string, a, b *image.NRGBA) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("%s: size %v against %v", rel, a.Bounds(), b.Bounds())
	}
	for i := 3; i < len(a.Pix); i += 4 {
		if a.Pix[i] != b.Pix[i] {
			t.Fatalf("%s %v: alpha differs at %d", rel, a.Bounds().Size(), i/4)
		}
	}
	w, h := b.Bounds().Dx(), b.Bounds().Dy()
	if b.NRGBAAt(0, 0).A > 3 || b.NRGBAAt(w-1, h-1).A > 3 {
		t.Fatalf("%s %v: corners are not transparent", rel, b.Bounds().Size())
	}
}

func sameFiles(t *testing.T, a, b string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		filepath.WalkDir(filepath.Join(a, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(a, path)
			x, _ := os.ReadFile(path)
			y, _ := os.ReadFile(filepath.Join(b, rel))
			if !bytes.Equal(x, y) {
				t.Errorf("%s differs between identities", rel)
			}
			return nil
		})
	}
}

var tileRels = []string{
	"apps/desktop/assets/icon.png", "apps/desktop/assets/icon-dark.png", "apps/desktop/assets/icon.ico",
	"apps/desktop/assets/icon-dark.icns", "apps/desktop/assets/icon-mac.png", "apps/desktop/public/icebath-mark-dark.png",
	"apps/desktop/assets/appx/Square44x44Logo.targetsize-16.png", "apps/desktop/assets/appx/Square44x44Logo.targetsize-32_altform-unplated.png",
	"apps/desktop/assets/appx/Square44x44Logo.targetsize-256_altform-lightunplated.png", "apps/desktop/assets/appx/Wide310x150Logo.scale-200.png",
}

var unbrandedRels = []string{"web/public/favicon.ico", "apps/bootstrap-installer/src-tauri/icons/32x32.png", "apps/bootstrap-installer/public/icebath-mark.png"}

func TestRendererCanaryRuleIsTheCanonicalOne(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(sourceRoot(t), "pkg", "selfupdate", "channel.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("var canaryTagRE = regexp.MustCompile\\(`([^`]+)`\\)").FindSubmatch(src)
	if m == nil || string(m[1]) != canaryTagRE.String() {
		t.Fatalf("canary rule drifted from pkg/selfupdate: %q", m)
	}
	for tag, want := range map[string]bool{
		canaryTag: true, "v2026.9.15+canary.20260916T120000Z": false, stableTag: false,
		"v1.2.3-canary.20260911010203": false, "v1.2.3+canary.20260911010203": false, "": false,
	} {
		if canaryTagRE.MatchString(tag) != want {
			t.Errorf("%q: canary %v", tag, !want)
		}
	}
}

func TestCanaryChangesOnlyDesktopBackgroundPreservingArtAndNativeGeometry(t *testing.T) {
	targets := subset(append(append([]string{}, tileRels...), unbrandedRels...)...)
	stable, canary := generate(t, stableTag, "", targets), generate(t, canaryTag, "", targets)
	for _, rel := range tileRels {
		originals, yellows := framesOf(t, filepath.Join(stable, rel)), framesOf(t, filepath.Join(canary, rel))
		if len(originals) != len(yellows) {
			t.Fatalf("%s: frame count changed", rel)
		}
		for i := range originals {
			sameAlpha(t, rel, originals[i], yellows[i])
			h, s, v := hsv(tileColor(yellows[i]))
			if !(h > 0.10 && h < 0.18 && s > 0.65) || isDarkTile(rel) != (v < 0.4) || !isDarkTile(rel) && v <= 0.8 {
				t.Errorf("%s %v: tile colour %v", rel, yellows[i].Bounds().Size(), tileColor(yellows[i]))
			}
			if filepath.Ext(rel) == ".png" && originals[i].Bounds().Dx() >= 256 {
				plateA, plateB := tileColor(originals[i]), tileColor(yellows[i])
				for p := 0; p < len(originals[i].Pix); p += 4 {
					a := color.NRGBA{originals[i].Pix[p], originals[i].Pix[p+1], originals[i].Pix[p+2], originals[i].Pix[p+3]}
					b := color.NRGBA{yellows[i].Pix[p], yellows[i].Pix[p+1], yellows[i].Pix[p+2], yellows[i].Pix[p+3]}
					if a.A == 255 && b.A == 255 && (a == plateA) != (b == plateB) {
						t.Fatalf("%s: mark moved at pixel %d", rel, p/4)
					}
				}
			}
		}
	}
	sameFiles(t, stable, canary, "web", "apps/bootstrap-installer")
}

func TestCommitIconsAreRedAndPrintOnlyTheActualSevenDigitPrefix(t *testing.T) {
	targets := subset(append(append([]string{}, tileRels...), unbrandedRels...)...)
	stable := generate(t, stableTag, "", targets)
	first := generate(t, "", "0123456"+strings.Repeat("a", 33), targets)
	changed := generate(t, "", "abcdef9"+strings.Repeat("a", 33), targets)
	samePrefix := generate(t, "", "0123456"+strings.Repeat("b", 33), targets)
	sameFiles(t, first, samePrefix, "apps/desktop")
	for _, rel := range tileRels {
		plains, reds, others := framesOf(t, filepath.Join(stable, rel)), framesOf(t, filepath.Join(first, rel)), framesOf(t, filepath.Join(changed, rel))
		for i := range reds {
			sameAlpha(t, rel, plains[i], reds[i])
			h, s, v := hsv(tileColor(reds[i]))
			if !((h < 0.05 || h > 0.95) && s > 0.6) || isDarkTile(rel) != (v < 0.4) || !isDarkTile(rel) && v <= 0.8 {
				t.Errorf("%s %v: tile colour %v", rel, reds[i].Bounds().Size(), tileColor(reds[i]))
			}
			box := opaqueBox(reds[i])
			diff := image.Rectangle{}
			for y := 0; y < reds[i].Bounds().Dy(); y++ {
				for x := 0; x < reds[i].Bounds().Dx(); x++ {
					a, b := reds[i].NRGBAAt(x, y), others[i].NRGBAAt(x, y)
					if a.A >= 128 && (a.R != b.R || a.G != b.G || a.B != b.B) {
						diff = diff.Union(image.Rect(x, y, x+1, y+1))
					}
				}
			}
			if filepath.Ext(rel) == ".png" && reds[i].Bounds().Dx() < badgeMinSize {
				if !diff.Empty() {
					t.Errorf("%s: a badge free render changed with the SHA at %v", rel, diff)
				}
				continue
			}
			if diff.Empty() || diff.Min.Y < box.Min.Y || float64(diff.Max.Y) > float64(box.Min.Y)+float64(box.Dy())*0.25+3 {
				t.Errorf("%s %v: SHA change spans %v outside the top quarter of %v", rel, reds[i].Bounds().Size(), diff, box)
			}
		}
	}
	digits := [][7]uint8{
		{14, 17, 19, 21, 25, 17, 14}, {4, 12, 4, 4, 4, 4, 14}, {14, 17, 1, 2, 4, 8, 31}, {30, 1, 1, 14, 1, 1, 30},
		{2, 6, 10, 18, 31, 2, 2}, {31, 16, 16, 30, 1, 1, 30}, {14, 16, 16, 30, 17, 17, 14},
	}
	for _, name := range []string{"icon.png", "icon-dark.png"} {
		badged := framesOf(t, filepath.Join(first, "apps/desktop/assets", name))[0]
		plain := framesOf(t, filepath.Join(stable, "apps/desktop/assets", name))[0]
		plate := tileColor(plain)
		for digit, rows := range digits {
			judged := 0
			for y, row := range rows {
				for x := 0; x < 5; x++ {
					px, py := int(badgeOrigin[0])+(digit*6+x)*16+8, int(badgeOrigin[1])+y*16+8
					if plain.NRGBAAt(px, py) != plate {
						continue
					}
					judged++
					c := badged.NRGBAAt(px, py)
					if (min(c.R, c.G, c.B) > 240) != (row&(1<<(4-x)) != 0) {
						t.Errorf("%s digit %d cell %d,%d reads %v", name, digit, x, y, c)
					}
				}
			}
			if judged < 18 {
				t.Errorf("%s digit %d: only %d cells judged", name, digit, judged)
			}
		}
	}
	sameFiles(t, stable, first, "web", "apps/bootstrap-installer")
}

func manifestFills(t *testing.T, pkg string) ([3]float64, [3]float64) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pkg, "icon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc iconDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	fills := map[string][3]float64{}
	for _, f := range doc.Fills {
		prefix, channels, _ := strings.Cut(f.Value.Solid, ":")
		if prefix != "srgb" {
			t.Fatalf("fill %q", f.Value.Solid)
		}
		var rgb [3]float64
		for i, v := range strings.Split(channels, ",")[:3] {
			var err error
			if rgb[i], err = strconv.ParseFloat(v, 64); err != nil {
				t.Fatal(err)
			}
		}
		key := f.Appearance
		if key == "" {
			key = "light"
		}
		fills[key] = rgb
	}
	layers := map[string]iconLayerSpec{}
	for _, l := range doc.Groups[0].Layers {
		layers[l.Name] = l
	}
	if _, ok := layers["border"]; ok || len(layers) != 2 {
		t.Fatalf("layers %v", layers)
	}
	art, mono := layers["art"], layers["mono"]
	if art.ImageName != "" || len(art.ImageNameSpecializations) != 2 || art.ImageNameSpecializations[1].Appearance != "dark" {
		t.Fatalf("art layer %+v", art)
	}
	if !mono.Glass || mono.Hidden[0].Value != true || mono.Hidden[1] != (hiddenSpec{"tinted", false}) || art.Hidden[1] != (hiddenSpec{"tinted", true}) {
		t.Fatalf("hidden specializations art %+v mono %+v", art.Hidden, mono.Hidden)
	}
	entries, _ := os.ReadDir(filepath.Join(pkg, "Assets"))
	referenced := map[string]bool{art.ImageNameSpecializations[0].Value: true, art.ImageNameSpecializations[1].Value: true, mono.ImageName: true}
	if len(entries) != len(referenced) {
		t.Fatalf("assets %v against referenced %v", entries, referenced)
	}
	for _, e := range entries {
		if !referenced[e.Name()] {
			t.Fatalf("dangling layer image %s", e.Name())
		}
	}
	return fills["light"], fills["dark"]
}

func TestLayeredMacosIconMonoLayerAndFlavorStaysInTheFill(t *testing.T) {
	targets := subset("apps/desktop/assets/icon.icon", "apps/desktop/assets/icon.icon/Assets", "apps/desktop/packaging/dmg-volume.icns")
	stable, canary := generate(t, stableTag, "", targets), generate(t, canaryTag, "", targets)
	commit := generate(t, "", "0123456"+strings.Repeat("a", 33), targets)
	pkg := func(root string) string { return filepath.Join(root, "apps/desktop/assets/icon.icon") }
	mono := framesOf(t, filepath.Join(pkg(stable), "Assets/mono.png"))[0]
	tones := map[uint8]bool{}
	for i := 0; i < len(mono.Pix); i += 4 {
		if mono.Pix[i+3] > 127 {
			tones[mono.Pix[i]] = true
		}
	}
	ink := uint8(math.Round(monoInkTone * 255))
	if len(tones) != 2 || !tones[ink] || !tones[255] {
		t.Fatalf("mono tones %v", tones)
	}
	for _, name := range []string{"art-light.png", "art-dark.png", "mono.png"} {
		a, _ := os.ReadFile(filepath.Join(pkg(stable), "Assets", name))
		b, _ := os.ReadFile(filepath.Join(pkg(canary), "Assets", name))
		if !bytes.Equal(a, b) {
			t.Errorf("%s changed with the canary flavor", name)
		}
	}
	for _, other := range []string{canary, commit} {
		a, _ := os.ReadFile(filepath.Join(stable, "apps/desktop/packaging/dmg-volume.icns"))
		b, _ := os.ReadFile(filepath.Join(other, "apps/desktop/packaging/dmg-volume.icns"))
		if !bytes.Equal(a, b) {
			t.Error("the disk image volume icon is flavoured")
		}
	}
	for _, name := range []string{"art-light.png", "art-dark.png"} {
		plain := framesOf(t, filepath.Join(pkg(stable), "Assets", name))[0]
		badged := framesOf(t, filepath.Join(pkg(commit), "Assets", name))[0]
		diff := image.Rectangle{}
		for i := 0; i < len(plain.Pix); i += 4 {
			if !bytes.Equal(plain.Pix[i:i+4], badged.Pix[i:i+4]) {
				p := i / 4
				diff = diff.Union(image.Rect(p%canvasSide, p/canvasSide, p%canvasSide+1, p/canvasSide+1))
			}
		}
		if diff.Empty() || float64(diff.Max.Y) > canvasSide*0.25+3 {
			t.Errorf("%s: commit badge spans %v", name, diff)
		}
	}
	light, dark := manifestFills(t, pkg(stable))
	if light != [3]float64{1, 1, 1} || math.Max(dark[0], math.Max(dark[1], dark[2])) >= 0.1 {
		t.Errorf("stable fills %v %v", light, dark)
	}
	for _, c := range []struct {
		root     string
		low, top float64
	}{{canary, 0.10, 0.18}, {commit, -0.05, 0.05}} {
		light, dark := manifestFills(t, pkg(c.root))
		for i, fill := range [][3]float64{light, dark} {
			h, s, v := hsv(color.NRGBA{uint8(math.Round(fill[0] * 255)), uint8(math.Round(fill[1] * 255)), uint8(math.Round(fill[2] * 255)), 255})
			if h > 0.5 {
				h--
			}
			if !(c.low < h && h < c.top && s > 0.6) || (i == 1) != (v < 0.4) || i == 0 && v <= 0.8 {
				t.Errorf("%s fill %v", c.root, fill)
			}
		}
	}
}

func TestInvalidOrConflictingBuildIdentityCannotEmitIcons(t *testing.T) {
	for _, c := range [][2]string{
		{"", "abcdef0"}, {"", strings.Repeat("a", 41)}, {"", strings.Repeat("A", 40)},
		{"", strings.Repeat("a", 39) + "g"}, {"", strings.Repeat("a", 40) + "\n"}, {stableTag, strings.Repeat("a", 40)},
	} {
		out := filepath.Join(t.TempDir(), "out")
		var stdout, stderr bytes.Buffer
		code := run([]string{"--source", sourceRoot(t), "--out", out}, &stdout, &stderr, envOf(c[0], c[1]))
		if code == 0 {
			t.Errorf("%q: invalid build identity generated icons", c)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%q: output directory created", c)
		}
	}
}

func TestMsixLogosResolveEverySlotWindowsDraws(t *testing.T) {
	appx := filepath.Join(generate(t, stableTag, "", subset(appxDir)), appxDir)
	logos := map[string][2]int{"Square44x44Logo": {44, 44}, "Square150x150Logo": {150, 150}, "StoreLogo": {50, 50}, "Wide310x150Logo": {310, 150}}
	for name, base := range logos {
		for _, scale := range appxScales {
			q := ""
			if scale != 100 {
				q = ".scale-" + strconv.Itoa(scale)
			}
			img := framesOf(t, filepath.Join(appx, name+q+".png"))[0]
			if got, want := img.Bounds().Size(), image.Pt(appxScaled(base[0], scale), appxScaled(base[1], scale)); got != want {
				t.Errorf("%s at %d: %v, expected %v", name, scale, got, want)
			}
		}
	}
	if appxScaled(150, 125) != 188 || appxScaled(44, 400) != 176 {
		t.Error("scaled sizes do not round up like Microsoft's table")
	}
	lightPlate := framesOf(t, filepath.Join(appx, "Square44x44Logo.png"))[0].NRGBAAt(2, 22)
	for _, size := range appxTargetSizes {
		base := filepath.Join(appx, "Square44x44Logo.targetsize-"+strconv.Itoa(size))
		plain, dark, light := framesOf(t, base+".png")[0], framesOf(t, base+"_altform-unplated.png")[0], framesOf(t, base+"_altform-lightunplated.png")[0]
		for _, img := range []*image.NRGBA{plain, dark, light} {
			if img.Bounds().Size() != image.Pt(size, size) {
				t.Fatalf("targetsize-%d is %v", size, img.Bounds().Size())
			}
		}
		x, y := max(1, size/24), size/2
		white := color.NRGBA{255, 255, 255, 255}
		if light.NRGBAAt(x, y) != white || plain.NRGBAAt(x, y) != white || lightPlate != white {
			t.Errorf("targetsize-%d light plate %v %v %v", size, light.NRGBAAt(x, y), plain.NRGBAAt(x, y), lightPlate)
		}
		if d := dark.NRGBAAt(x, y); d.A != 255 || max(d.R, d.G, d.B) >= 60 {
			t.Errorf("targetsize-%d dark plate %v", size, d)
		}
	}
}

func TestWriteStatusIncludesEveryTarget(t *testing.T) {
	good, err := encodePNG(image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	if err != nil {
		t.Fatal(err)
	}
	pngShape := shape{format: "PNG", w: 2, h: 2}
	for _, failure := range []string{"", "render", "directory", "verify"} {
		out := t.TempDir()
		first := "first.png"
		if failure == "directory" {
			first = "blocked/icon.png"
			os.WriteFile(filepath.Join(out, "blocked"), []byte("not a directory"), 0o644)
		}
		targets := []target{
			{first, func(*art) ([]byte, error) {
				switch failure {
				case "render":
					return nil, io.ErrUnexpectedEOF
				case "verify":
					return []byte("not an image"), nil
				}
				return good, nil
			}, pngShape},
			{"last.png", func(*art) ([]byte, error) { return good, nil }, pngShape},
		}
		var log bytes.Buffer
		code := cmdWrite(&log, targets, nil, nil, out)
		if (code != 0) != (failure != "") {
			t.Errorf("%q: exit %d", failure, code)
		}
		if data, _ := os.ReadFile(filepath.Join(out, "last.png")); !bytes.Equal(data, good) {
			t.Errorf("%q: the target after a failure was not written", failure)
		}
		if !strings.Contains(log.String(), "last.png: PNG (2, 2)") || strings.Contains(log.String(), "FAILED") != (failure != "") {
			t.Errorf("%q: report\n%s", failure, log.String())
		}
	}
}

func TestCheckAcceptsWrittenOutputsAndReportsMissingOnes(t *testing.T) {
	targets := subset("apps/desktop/assets/icon.png", "apps/desktop/assets/icon.icns", "web/public/favicon.ico",
		"apps/desktop/packaging/icebath-dmg.tiff", "apps/desktop/src/assets/tiers", "pkg/app/banner_art.go")
	out := generate(t, stableTag, "", targets)
	stable, desktop, err := buildArt(sourceRoot(t), envOf(stableTag, ""))
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if code := cmdCheck(&log, targets, stable, desktop, out); code != 0 {
		t.Fatalf("check failed:\n%s", log.String())
	}
	os.Remove(filepath.Join(out, "web/public/favicon.ico"))
	os.WriteFile(filepath.Join(out, "pkg/app/banner_art.go"), []byte("package app\n"), 0o644)
	log.Reset()
	if code := cmdCheck(&log, targets, stable, desktop, out); code == 0 || !strings.Contains(log.String(), "favicon.ico: MISSING") || !strings.Contains(log.String(), "banner_art.go: STALE") {
		t.Fatalf("check passed a missing and a stale file:\n%s", log.String())
	}
}

func TestTiffPagesRoundTripThroughLZW(t *testing.T) {
	src, err := loadSources(sourceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for scale, dpi := range map[int]uint32{1: 72, 2: 144} {
		page := src.dmgBackground(scale)
		data := encodeTIFF([]tiffPage{{page, dpi}})
		got, err := tiff.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		back := toNRGBA(got)
		for i := 0; i < len(page.Pix); i += 4 {
			if !bytes.Equal(page.Pix[i:i+3], back.Pix[i:i+3]) {
				t.Fatalf("scale %d: pixel %d decodes to %v, wrote %v", scale, i/4, back.Pix[i:i+3], page.Pix[i:i+3])
			}
		}
	}
	sizes, err := tiffPageSizes(encodeTIFF([]tiffPage{{src.dmgBackground(1), 72}, {src.dmgBackground(2), 144}}))
	if err != nil || len(sizes) != 2 || sizes[0] != [2]int{769, 457} || sizes[1] != [2]int{1538, 914} {
		t.Fatalf("pages %v %v", sizes, err)
	}
}

func TestBannerArtKeepsTheShapeTheBannersExpect(t *testing.T) {
	src, err := loadSources(sourceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	hero, logo := plainLines(src.heroCells()), plainLines(src.logoCells())
	if len(hero) != heroRows || len(logo) != logoRows {
		t.Fatalf("hero %d rows, logo %d rows", len(hero), len(logo))
	}
	for _, line := range hero {
		if utf8.RuneCountInString(line) != heroCols {
			t.Fatalf("hero line %q is not %d cells", line, heroCols)
		}
		for _, r := range line {
			if r < 0x2800 || r > 0x28ff {
				t.Fatalf("hero line %q holds a non braille cell", line)
			}
		}
	}
	width := utf8.RuneCountInString(logo[0])
	if width >= 95 {
		t.Fatalf("logo is %d cells, the banner prints it from 95 columns", width)
	}
	for _, line := range logo {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("ragged logo line %q", line)
		}
	}
	tags := regexp.MustCompile(`\[#[0-9a-f]{6}\]|\[/\]`)
	for i, line := range src.markupLines(src.heroCells()) {
		if tags.ReplaceAllString(line, "") != hero[i] {
			t.Fatalf("hero markup line %d does not strip to its art", i)
		}
	}
	if !strings.Contains(strings.Join(src.markupLines(src.logoCells()), ""), "["+hexOf(src.red)+"]") {
		t.Fatal("the logo lost its red accent")
	}
	code := src.bannerGo()
	if formatted, err := format.Source(code); err != nil || !bytes.Equal(formatted, code) {
		t.Fatalf("banner Go source is not gofmt clean: %v", err)
	}
}
