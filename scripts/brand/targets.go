package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
)

// One generated file, how it renders from the art and the shape it must have
type target struct {
	rel    string
	render func(a *art) ([]byte, error)
	want   shape
}

const (
	appxDir      = "apps/desktop/assets/appx"
	desktopAsset = "apps/desktop/assets"
	wailsBuild   = "apps/bootstrap-installer/build"
)

var (
	appxScales      = []int{100, 125, 150, 200, 400}
	appxTargetSizes = []int{16, 20, 24, 30, 32, 36, 40, 48, 60, 64, 72, 80, 96, 256}
	appxAltforms    = []struct {
		suffix string
		dark   bool
	}{{"", false}, {"_altform-unplated", true}, {"_altform-lightunplated", false}}
	desktopICO = []int{16, 24, 32, 48, 64, 128, 256}
	wailsICO   = []int{256, 128, 64, 48, 32, 16}
	faviconICO = []int{16, 32, 48}
	tiers      = []string{"automation", "connect", "memory", "sandbox"}
)

// Microsoft's scaled asset tables round up, 150 at 125 percent is 188
func appxScaled(base, scale int) int {
	return int(math.Ceil(float64(base*scale)/100 - 1e-9))
}

func pngOf(img image.Image, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return encodePNG(img)
}

// Square tile master on the full bleed squircle, badge free below the badge size
func squareTile(size int, dark bool) func(a *art) ([]byte, error) {
	return func(a *art) ([]byte, error) {
		return pngOf(a.tile(size, squirclePlate, dark, size >= badgeMinSize), nil)
	}
}

// ICO of the squircle rendered at its largest frame, planes as the original writer set them
func icoTile(sizes []int, dark bool, planes uint16) func(a *art) ([]byte, error) {
	return func(a *art) ([]byte, error) {
		return encodeICO(a.tile(slices.Max(sizes), squirclePlate, dark, true), sizes, planes)
	}
}

// ICNS of the macOS grid tile in the chunk layout of the original
func icnsTile(dark bool, layout icnsLayout) func(a *art) ([]byte, error) {
	return func(a *art) ([]byte, error) {
		return encodeICNS(a.tile(canvasSide, macPlate, dark, true), layout)
	}
}

// Wide tile canvas with the squircle tile centred on it
func wideTile(w, h, tile int) func(a *art) ([]byte, error) {
	return func(a *art) ([]byte, error) {
		canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
		t := a.tile(tile, squirclePlate, false, true)
		x, y := (w-tile)/2, (h-tile)/2
		for row := 0; row < tile; row++ {
			copy(canvas.Pix[(y+row)*canvas.Stride+x*4:], t.Pix[row*t.Stride:row*t.Stride+tile*4])
		}
		return encodePNG(canvas)
	}
}

// Icon Composer colour literal for an opaque sRGB colour
func iconColor(c color.NRGBA) string {
	return fmt.Sprintf("srgb:%.5f,%.5f,%.5f,1.00000", float64(c.R)/255, float64(c.G)/255, float64(c.B)/255)
}

// Icon Composer manifest pieces in the key order actool and the originals use
type (
	solidFill struct {
		Solid string `json:"solid"`
	}
	fillSpec struct {
		Appearance string    `json:"appearance,omitempty"`
		Value      solidFill `json:"value"`
	}
	nameSpec struct {
		Appearance string `json:"appearance,omitempty"`
		Value      string `json:"value"`
	}
	hiddenSpec struct {
		Appearance string `json:"appearance,omitempty"`
		Value      bool   `json:"value"`
	}
	iconLayerSpec struct {
		Name                     string       `json:"name"`
		ImageNameSpecializations []nameSpec   `json:"image-name-specializations,omitempty"`
		ImageName                string       `json:"image-name,omitempty"`
		Glass                    bool         `json:"glass"`
		Hidden                   []hiddenSpec `json:"hidden-specializations"`
	}
	iconGroup struct {
		Lighting     string `json:"lighting"`
		Specular     bool   `json:"specular"`
		Translucency struct {
			Enabled bool `json:"enabled"`
			Value   int  `json:"value"`
		} `json:"translucency"`
		Shadow struct {
			Kind    string `json:"kind"`
			Opacity int    `json:"opacity"`
		} `json:"shadow"`
		BlurMaterial *string         `json:"blur-material"`
		Layers       []iconLayerSpec `json:"layers"`
	}
	iconDoc struct {
		Fills     []fillSpec  `json:"fill-specializations"`
		Groups    []iconGroup `json:"groups"`
		Platforms struct {
			Squares string `json:"squares"`
		} `json:"supported-platforms"`
	}
)

// icon.json of the layered macOS icon, flavoured fill, art layer per appearance and the tinted mono layer
func iconManifest(a *art) ([]byte, error) {
	group := iconGroup{Lighting: "individual"}
	group.Shadow.Kind = "none"
	group.Layers = []iconLayerSpec{
		{
			Name:                     "art",
			ImageNameSpecializations: []nameSpec{{Value: "art-light.png"}, {Appearance: "dark", Value: "art-dark.png"}},
			Hidden:                   []hiddenSpec{{Value: false}, {Appearance: "tinted", Value: true}},
		},
		{Name: "mono", ImageName: "mono.png", Glass: true, Hidden: []hiddenSpec{{Value: true}, {Appearance: "tinted", Value: false}}},
	}
	doc := iconDoc{
		Fills:  []fillSpec{{Value: solidFill{iconColor(a.light)}}, {Appearance: "dark", Value: solidFill{iconColor(a.dark)}}},
		Groups: []iconGroup{group},
	}
	doc.Platforms.Squares = "shared"
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Icon Composer package targets in the package dir, manifest, light and dark art and the mono material
func iconPackage(dir string) []target {
	return []target{
		{dir + "/icon.json", iconManifest, shape{format: "JSON"}},
		{dir + "/Assets/art-light.png", func(a *art) ([]byte, error) { return encodePNG(a.iconLayer(false)) }, shape{format: "PNG", w: canvasSide, h: canvasSide}},
		{dir + "/Assets/art-dark.png", func(a *art) ([]byte, error) { return encodePNG(a.iconLayer(true)) }, shape{format: "PNG", w: canvasSide, h: canvasSide}},
		{dir + "/Assets/mono.png", func(a *art) ([]byte, error) { return encodePNG(a.iconMono()) }, shape{format: "PNG", w: canvasSide, h: canvasSide}},
	}
}

func squareShape(size int, corners bool) shape {
	return shape{format: "PNG", w: size, h: size, corners: corners}
}

func appxTargets() []target {
	var out []target
	logos := []struct {
		name string
		w, h int
	}{{"Square44x44Logo", 44, 44}, {"Square150x150Logo", 150, 150}, {"StoreLogo", 50, 50}, {"Wide310x150Logo", 310, 150}}
	for _, logo := range logos {
		for _, scale := range appxScales {
			qualifier := ""
			if scale != 100 {
				qualifier = fmt.Sprintf(".scale-%d", scale)
			}
			rel := fmt.Sprintf("%s/%s%s.png", appxDir, logo.name, qualifier)
			w, h := appxScaled(logo.w, scale), appxScaled(logo.h, scale)
			if logo.w != logo.h {
				out = append(out, target{rel, wideTile(w, h, appxScaled(100, scale)), shape{format: "PNG", w: w, h: h}})
				continue
			}
			out = append(out, target{rel, squareTile(w, false), squareShape(w, false)})
		}
	}
	for _, size := range appxTargetSizes {
		for _, alt := range appxAltforms {
			rel := fmt.Sprintf("%s/Square44x44Logo.targetsize-%d%s.png", appxDir, size, alt.suffix)
			out = append(out, target{rel, squareTile(size, alt.dark), squareShape(size, false)})
		}
	}
	return out
}

// Every generated output relative to the checkout root, in write order
func allTargets() []target {
	var out []target
	add := func(rel string, render func(a *art) ([]byte, error), want shape) {
		out = append(out, target{rel, render, want})
	}
	add(desktopAsset+"/icon.png", squareTile(1024, false), squareShape(1024, true))
	add(desktopAsset+"/icon.ico", icoTile(desktopICO, false, 0), shape{format: "ICO", frames: desktopICO})
	add(desktopAsset+"/icon.icns", icnsTile(false, pillowIcns), shape{format: "ICNS", w: 1024, h: 1024, icns: pillowIcns})
	add(desktopAsset+"/icon-dark.png", squareTile(1024, true), squareShape(1024, true))
	add(desktopAsset+"/icon-dark.ico", icoTile(desktopICO, true, 0), shape{format: "ICO", frames: desktopICO})
	add(desktopAsset+"/icon-dark.icns", icnsTile(true, pillowIcns), shape{format: "ICNS", w: 1024, h: 1024, icns: pillowIcns})
	add(desktopAsset+"/icon-mac.png", func(a *art) ([]byte, error) { return encodePNG(a.tile(1024, macPlate, false, true)) }, squareShape(1024, true))
	add("apps/desktop/packaging/dmg-volume.icns", func(a *art) ([]byte, error) { return encodeICNS(a.src.volumeIcon(1024), pillowIcns) }, shape{format: "ICNS", w: 1024, h: 1024, icns: pillowIcns})
	add("apps/desktop/packaging/icebath-dmg.tiff", func(a *art) ([]byte, error) {
		return encodeTIFF([]tiffPage{{a.src.dmgBackground(769, 457, 1), 72}, {a.src.dmgBackground(1538, 914, 2), 144}}), nil
	}, shape{format: "TIFF", w: 769, h: 457, frames: []int{769, 1538}})
	out = append(out, iconPackage(desktopAsset+"/icon.icon")...)
	out = append(out, appxTargets()...)
	add("apps/desktop/public/apple-touch-icon.png", squareTile(1024, false), squareShape(1024, true))
	add("apps/desktop/public/icebath-mark.png", squareTile(256, false), squareShape(256, true))
	add("apps/desktop/public/icebath-mark-dark.png", squareTile(256, true), squareShape(256, true))
	add("apps/desktop/public/icebath.png", func(a *art) ([]byte, error) { return encodePNG(a.src.logo) }, shape{format: "PNG", w: 1254, h: 1254})
	add("apps/desktop/public/icebath-sprite.png", func(a *art) ([]byte, error) { return encodePNG(a.src.mascotSheet(1536, 1024)) }, shape{format: "PNG", w: 1536, h: 1024})
	for i := 0; i < mascotFrames; i++ {
		add(fmt.Sprintf("apps/desktop/public/icebath-frames/icebath-frame-%d.png", i), func(a *art) ([]byte, error) {
			return encodePNG(a.src.mascotFrame(560, 500, i))
		}, shape{format: "PNG", w: 560, h: 500})
	}
	add("apps/desktop/public/ds-assets/filler-bg0.jpg", func(a *art) ([]byte, error) {
		return encodeJPEG(cover(a.src.tub, a.src.tub.Bounds(), 1444, 1444, a.src.tubDark), 99)
	}, shape{format: "JPEG", w: 1444, h: 1444})
	for _, name := range tiers {
		add(fmt.Sprintf("apps/desktop/src/assets/tiers/feature-%s.webp", name), func(a *art) ([]byte, error) {
			return encodeWebP(a.src.tier(name, 128))
		}, shape{format: "WEBP", w: 128, h: 128})
	}
	add("apps/desktop/src/assets/catalog-art.svg", func(a *art) ([]byte, error) {
		return []byte(a.src.catalogSVG(a.src.letter, color.NRGBA{0xF2, 0xF2, 0xF2, 255})), nil
	}, shape{format: "SVG"})
	add("apps/desktop/src/assets/catalog-art-retro.svg", func(a *art) ([]byte, error) {
		return []byte(a.src.catalogSVG(mustHex("#1c2733"), color.NRGBA{0xF2, 0xF2, 0xF2, 255})), nil
	}, shape{format: "SVG"})
	add(wailsBuild+"/appicon.png", squareTile(256, false), squareShape(256, true))
	add(wailsBuild+"/linux/appimage/icebath-setup.png", squareTile(256, false), squareShape(256, true))
	add(wailsBuild+"/windows/icon.ico", icoTile(wailsICO, false, 1), shape{format: "ICO", frames: wailsICO})
	add(wailsBuild+"/darwin/icons.icns", icnsTile(false, wailsIcns), shape{format: "ICNS", w: 256, h: 256, icns: wailsIcns})
	add(wailsBuild+"/darwin/dmg-file-icon.png", func(a *art) ([]byte, error) { return encodePNG(a.src.volumeIcon(1254)) }, squareShape(1254, true))
	add(wailsBuild+"/darwin/dmg-file-icon.icns", func(a *art) ([]byte, error) {
		return encodeICNS(a.src.volumeIcon(1024), iconutilIcns)
	}, shape{format: "ICNS", w: 1024, h: 1024, icns: iconutilIcns})
	add(wailsBuild+"/darwin/dmg-background.png", func(a *art) ([]byte, error) {
		return encodePNG(a.src.dmgBackground(540, 380, 0))
	}, shape{format: "PNG", w: 540, h: 380})
	out = append(out, iconPackage(wailsBuild+"/appicon.icon")...)
	add("apps/bootstrap-installer/public/icebath-mark.png", squareTile(256, false), squareShape(256, true))
	add("web/public/favicon.ico", icoTile(faviconICO, false, 0), shape{format: "ICO", frames: faviconICO})
	add("pkg/app/banner_art.go", func(a *art) ([]byte, error) { return a.src.bannerGo(), nil }, shape{format: "TEXT"})
	add("ui-tui/src/bannerArt.ts", func(a *art) ([]byte, error) { return a.src.bannerTS(), nil }, shape{format: "TEXT"})
	return out
}
