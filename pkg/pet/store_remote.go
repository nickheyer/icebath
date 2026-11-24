package pet

import (
	"archive/zip"
	"compress/flate"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	ziputil "github.com/nickheyer/icebath/pkg/utils/zip"
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
	host := strings.ToLower(u.Hostname())
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
	if head, _, _ := strings.Cut(strings.ToLower(entry.SpritesheetURL), "?"); strings.HasSuffix(head, ".png") {
		ext = ".png"
	}
	spritePath := filepath.Join(directory, "spritesheet"+ext)
	if err := download(ctx, env, entry.SpritesheetURL, spritePath, downloadTimeout); err != nil {
		return nil, err
	}
	meta := jsonutil.NewObject()
	if entry.PetJSONURL != "" && isPetdexHost(entry.PetJSONURL) {
		resp, err := httpGet(ctx, env, entry.PetJSONURL, downloadTimeout)
		var data any
		if err == nil {
			data, err = resp.JSON()
		}
		if err != nil {
			pkgLog().Debug("pet.json fetch failed for " + slug + ": " + fetchErrText(err))
		} else if d, ok := data.(*jsonutil.Object); ok {
			meta = d
		}
	}
	if meta.Len() == 0 {
		meta = jsonutil.ObjectOf("id", slug, "displayName", entry.DisplayName, "description", "")
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
	directory := filepath.Join(root, strings.TrimSpace(slug))
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
	scratch, err := os.CreateTemp("", "pet-export-*.zip")
	if err != nil {
		return "", nil, err
	}
	zipPath := scratch.Name()
	scratch.Close()
	defer os.Remove(zipPath)
	archive, err := ziputil.Create(zipPath, zip.Deflate, flate.DefaultCompression)
	if err != nil {
		return "", nil, err
	}
	name := filepath.Base(directory)
	for _, file := range names {
		path := filepath.Join(directory, file)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(file, ".") {
			continue
		}
		if err := archive.AddFile(path, name+"/"+file); err != nil {
			archive.Close()
			return "", nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return "", nil, err
	}
	return name + ".zip", data, nil
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
	displayName = strings.TrimSpace(displayName)
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
