package pet

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/HugoSmits86/nativewebp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var slugifyRe = regexp.MustCompile(`[^a-z0-9]+`)

// Lowercased hyphenated filesystem slug of a display name, pet when nothing remains
func Slugify(name string) string {
	slug := strings.Trim(slugifyRe.ReplaceAllString(pystr.Lower(pystr.Strip(name)), "-"), "-")
	if slug == "" {
		return "pet"
	}
	return slug
}

// Slug of the name that does not collide with an existing pet directory
func UniqueSlug(h, name string) (string, error) {
	dir, err := PetsDir(h)
	if err != nil {
		return "", err
	}
	base := Slugify(name)
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		if _, err := os.Stat(filepath.Join(dir, slug)); os.IsNotExist(err) {
			return slug, nil
		}
	}
}

// Writes the pet.json of a pet directory indented two spaces with ASCII escapes
func writePetJSON(directory string, meta *pyjson.Object) error {
	text, err := pyjson.Dumps(meta, pyjson.Indent(2))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "pet.json"), []byte(text), 0o666)
}

// Writes a locally generated pet into the store as a lossless WebP spritesheet marked as generator made
func RegisterLocalPet(h string, spritesheet image.Image, encoded []byte, slug, displayName, description string) (*InstalledPet, error) {
	slug = Slugify(slug)
	dir, err := PetsDir(h)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(dir, slug)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return nil, err
	}
	spritePath := filepath.Join(directory, "spritesheet.webp")
	data := encoded
	if data == nil {
		var buf bytes.Buffer
		if err := nativewebp.Encode(&buf, spritesheet, &nativewebp.Options{CompressionLevel: nativewebp.BestCompression}); err != nil {
			return nil, &StoreError{Msg: "could not write spritesheet for '" + slug + "': " + err.Error()}
		}
		data = buf.Bytes()
	}
	if err := os.WriteFile(spritePath, data, 0o666); err != nil {
		return nil, &StoreError{Msg: "could not write spritesheet for '" + slug + "': " + err.Error()}
	}
	if displayName == "" {
		displayName = slug
	}
	meta := pyjson.NewObject()
	meta.Set("id", slug)
	meta.Set("displayName", displayName)
	meta.Set("description", description)
	meta.Set("spritesheetPath", "spritesheet.webp")
	meta.Set("createdBy", "generator")
	if err := writePetJSON(directory, meta); err != nil {
		return nil, err
	}
	return usablePet(h, slug, "register of generated pet '"+slug+"' did not produce a spritesheet")
}
