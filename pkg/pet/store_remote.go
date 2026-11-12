package pet

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/png"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Idle frame cell of a sheet and the width of its cached thumbnail
const (
	thumbFrameW = 192
	thumbFrameH = 208
	thumbW      = 96
)

// Cached thumbnail of a slug under pets/.thumbs, outside the pet directory
func thumbPath(h, slug string) (string, error) {
	root, err := PetsDir(h)
	if err != nil {
		return "", err
	}
	thumbs := filepath.Join(root, ".thumbs")
	if err := os.MkdirAll(thumbs, 0o777); err != nil {
		return "", err
	}
	return filepath.Join(thumbs, slug+".png"), nil
}

// Whether a URL host is petdex.dev or one of its subdomains, bounding every server side fetch
func isPetdexHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := pystr.Lower(u.Hostname())
	return host == "petdex.dev" || strings.HasSuffix(host, ".petdex.dev")
}

// Streams a URL to a file through a part file so a failed download never leaves a truncated sheet
func download(ctx context.Context, env *tools.Env, rawURL, dest string, timeout time.Duration) error {
	err := func() error {
		resp, err := httpGet(ctx, env, rawURL, timeout)
		if err != nil {
			return err
		}
		tmp := dest + ".part"
		if err := os.WriteFile(tmp, resp.Content, 0o666); err != nil {
			return err
		}
		return os.Rename(tmp, dest)
	}()
	if err != nil {
		return &StoreError{Msg: "download failed for " + rawURL + ": " + fetchErrText(err)}
	}
	return nil
}

// Downloads a manifest pet into the pets directory, idempotent unless forced
func InstallPet(ctx context.Context, env *tools.Env, h, slug string, force bool) (*InstalledPet, error) {
	slug = safeSlug(slug)
	if slug == "" {
		return nil, &StoreError{Msg: "invalid pet slug"}
	}
	if !force {
		if existing, err := usablePet(h, slug, ""); err != nil || existing != nil {
			return existing, err
		}
	}
	entry, err := FindEntry(ctx, env, slug)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &StoreError{Msg: "pet '" + slug + "' is not in the petdex manifest"}
	}
	if !isPetdexHost(entry.SpritesheetURL) {
		return nil, &StoreError{Msg: "refusing non-petdex spritesheet host for '" + slug + "'"}
	}
	root, err := PetsDir(h)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, slug)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return nil, err
	}
	ext := ".webp"
	if head, _, _ := strings.Cut(pystr.Lower(entry.SpritesheetURL), "?"); strings.HasSuffix(head, ".png") {
		ext = ".png"
	}
	spritePath := filepath.Join(directory, "spritesheet"+ext)
	if err := download(ctx, env, entry.SpritesheetURL, spritePath, downloadTimeout); err != nil {
		return nil, err
	}
	meta := pyjson.NewObject()
	if entry.PetJSONURL != "" && isPetdexHost(entry.PetJSONURL) {
		resp, err := httpGet(ctx, env, entry.PetJSONURL, downloadTimeout)
		var data any
		if err == nil {
			data, err = resp.JSON()
		}
		if err != nil {
			storeLog.Debug("pet.json fetch failed for " + slug + ": " + fetchErrText(err))
		} else if d, ok := data.(*pyjson.Object); ok {
			meta = d
		}
	}
	if meta.Len() == 0 {
		meta = pyjson.ObjectOf("id", slug, "displayName", entry.DisplayName, "description", "")
	}
	meta.Set("spritesheetPath", filepath.Base(spritePath))
	if _, ok := meta.Get("id"); !ok {
		meta.Set("id", slug)
	}
	if _, ok := meta.Get("displayName"); !ok {
		meta.Set("displayName", entry.DisplayName)
	}
	if err := writePetJSON(directory, meta); err != nil {
		return nil, err
	}
	return usablePet(h, slug, "install of '"+slug+"' did not produce a spritesheet")
}

// Zips an installed pet's folder as its file name and bytes, dotfiles skipped
func ExportPet(h, slug string) (string, []byte, error) {
	root, err := PetsDir(h)
	if err != nil {
		return "", nil, err
	}
	directory := filepath.Join(root, pystr.Strip(slug))
	resolvedParent, _ := filepath.Abs(filepath.Dir(directory))
	resolvedRoot, _ := filepath.Abs(root)
	if real, err := filepath.EvalSymlinks(filepath.Dir(directory)); err == nil {
		resolvedParent = real
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = real
	}
	if info, statErr := os.Stat(directory); resolvedParent != resolvedRoot || filepath.Base(directory) == ".." || statErr != nil || !info.IsDir() {
		return "", nil, &StoreError{Msg: "pet '" + slug + "' is not installed"}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	name := filepath.Base(directory)
	for _, file := range names {
		path := filepath.Join(directory, file)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(file, ".") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return "", nil, err
		}
		header.Name, header.Method = name+"/"+file, zip.Deflate
		w, err := archive.CreateHeader(header)
		if err != nil {
			return "", nil, err
		}
		if _, err := w.Write(data); err != nil {
			return "", nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return "", nil, err
	}
	return name + ".zip", buf.Bytes(), nil
}

// Nearest neighbour resize sampling each destination pixel centre the way Pillow NEAREST does
func resizeNearest(src *image.NRGBA, w, h int) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	xScale, yScale := float64(b.Dx())/float64(w), float64(b.Dy())/float64(h)
	for y := range h {
		sy := min(b.Dy()-1, int(math.Floor((float64(y)+0.5)*yScale)))
		for x := range w {
			sx := min(b.Dx()-1, int(math.Floor((float64(x)+0.5)*xScale)))
			out.SetNRGBA(x, y, src.NRGBAAt(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return out
}

// Small idle frame PNG of a slug cached on disk, nil on any failure, a petdex source URL only fetched when not installed
func ThumbnailPNG(ctx context.Context, env *tools.Env, h, slug, sourceURL string) []byte {
	if slug = pystr.Strip(slug); slug == "" {
		return nil
	}
	cache, err := thumbPath(h, slug)
	if err != nil {
		return nil
	}
	if info, statErr := os.Stat(cache); statErr == nil && info.Mode().IsRegular() {
		if data, readErr := os.ReadFile(cache); readErr == nil {
			return data
		}
	}
	var sheet []byte
	if p, _ := usablePet(h, slug, ""); p != nil {
		if data, readErr := os.ReadFile(p.Spritesheet); readErr == nil {
			sheet = data
		}
	}
	if sheet == nil && sourceURL != "" && isPetdexHost(sourceURL) {
		if resp, fetchErr := httpGet(ctx, env, sourceURL, 30*time.Second); fetchErr != nil {
			storeLog.Debug("thumb fetch failed for " + slug + ": " + fetchErrText(fetchErr))
		} else {
			sheet = resp.Content
		}
	}
	if len(sheet) == 0 {
		return nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(sheet))
	if err != nil {
		storeLog.Debug("thumb crop failed for " + slug + ": " + err.Error())
		return nil
	}
	rgba := pypil.ToNRGBA(decoded)
	b := rgba.Bounds()
	frame := rgba.SubImage(image.Rect(b.Min.X, b.Min.Y, b.Min.X+min(thumbFrameW, b.Dx()), b.Min.Y+min(thumbFrameH, b.Dy()))).(*image.NRGBA)
	scaled := resizeNearest(frame, thumbW, int(math.RoundToEven(float64(thumbW)*thumbFrameH/thumbFrameW)))
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		storeLog.Debug("thumb crop failed for " + slug + ": " + err.Error())
		return nil
	}
	_ = os.WriteFile(cache, buf.Bytes(), 0o666)
	return buf.Bytes()
}

// Deletes an installed pet and its cached thumbnail, true when the directory is gone
func RemovePet(h, slug string) (bool, error) {
	slug = safeSlug(slug)
	if slug == "" {
		return false, nil
	}
	if cache, err := thumbPath(h, slug); err == nil {
		_ = os.Remove(cache)
	}
	root, err := PetsDir(h)
	if err != nil {
		return false, err
	}
	directory := filepath.Join(root, slug)
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return false, nil
	}
	_ = os.RemoveAll(directory)
	_, err = os.Stat(directory)
	return err != nil, nil
}

// Renames a pet's display name and moves its directory and thumb to the slug of that name when free, empty on failure
func RenamePet(h, slug, displayName string) (string, error) {
	slug = safeSlug(slug)
	displayName = pystr.Strip(displayName)
	root, err := PetsDir(h)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, slug)
	if info, statErr := os.Stat(filepath.Join(directory, "pet.json")); slug == "" || displayName == "" || statErr != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	meta, err := readPetJSON(directory)
	if err != nil {
		return "", err
	}
	meta.Set("displayName", displayName)
	newSlug := slug
	desired := Slugify(displayName)
	if _, existsErr := os.Stat(filepath.Join(root, desired)); desired != "" && desired != slug && os.IsNotExist(existsErr) {
		if os.Rename(directory, filepath.Join(root, desired)) == nil {
			oldThumb, oldErr := thumbPath(h, slug)
			newThumb, newErr := thumbPath(h, desired)
			if oldErr == nil && newErr == nil {
				_ = os.Rename(oldThumb, newThumb)
			}
			directory = filepath.Join(root, desired)
			newSlug = desired
			meta.Set("id", desired)
		}
	}
	if err := writePetJSON(directory, meta); err != nil {
		return "", nil
	}
	return newSlug, nil
}
