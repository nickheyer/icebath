package pet

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var storeLog = slog.Default().With("logger", "agent.pet.store")

// StoreError is raised on install and IO failures of the pet store
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

// InstalledPet is a pet present on disk
type InstalledPet struct {
	Slug, DisplayName, Description, Directory, Spritesheet, CreatedBy string
}

// Exists reports the spritesheet file on disk
func (p *InstalledPet) Exists() bool {
	info, err := os.Stat(p.Spritesheet)
	return err == nil && info.Mode().IsRegular()
}

// Generated reports a pet hatched locally by the generator
func (p *InstalledPet) Generated() bool { return p.CreatedBy == "generator" }

// PetsDir returns the pets directory of a home, created on demand
func PetsDir(h string) (string, error) {
	dir := filepath.Join(h, "pets")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return dir, nil
}

// Decoded pet.json of a pet directory, empty when the file is absent or unreadable
func readPetJSON(directory string) (*pyjson.Object, error) {
	path := filepath.Join(directory, "pet.json")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return &pyjson.Object{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		storeLog.Debug(fmt.Sprintf("unreadable pet.json in %s: %s", directory, err))
		return &pyjson.Object{}, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		storeLog.Debug(fmt.Sprintf("unreadable pet.json in %s: %s", directory, err))
		return &pyjson.Object{}, nil
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		storeLog.Debug(fmt.Sprintf("unreadable pet.json in %s: %s", directory, err))
		return &pyjson.Object{}, nil
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	return obj, nil
}

// Text of a pet.json key, the fallback when it is absent or falsy
func metaText(meta *pyjson.Object, key, fallback string) string {
	if v, ok := meta.Get(key); ok && pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return fallback
}

// Honors spritesheetPath, else probes the conventional names, the webp name when none is on disk
func resolveSpritesheet(directory string, meta *pyjson.Object) string {
	names := []string{}
	if declared := pystr.Strip(metaText(meta, "spritesheetPath", "")); declared != "" {
		names = append(names, declared)
	}
	for _, name := range append(names, "spritesheet.webp", "spritesheet.png", "sprite.webp", "sprite.png") {
		path := pypath.PathJoin(directory, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return filepath.Join(directory, "spritesheet.webp")
}

// One bare path segment of a slug so it can never escape the pets directory, empty when it has none
func safeSlug(slug string) string {
	segment := pypath.PathName(pystr.Strip(slug))
	if segment == "." || segment == ".." {
		return ""
	}
	return segment
}

// LoadPet returns the installed pet of a slug, nil when the pets directory holds no such pet
func LoadPet(h, slug string) (*InstalledPet, error) {
	slug = safeSlug(slug)
	if slug == "" {
		return nil, nil
	}
	root, err := PetsDir(h)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, slug)
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return nil, nil
	}
	meta, err := readPetJSON(directory)
	if err != nil {
		return nil, err
	}
	return &InstalledPet{Slug: slug, DisplayName: metaText(meta, "displayName", slug), Description: metaText(meta, "description", ""),
		Directory: directory, Spritesheet: resolveSpritesheet(directory, meta), CreatedBy: metaText(meta, "createdBy", "")}, nil
}

// The loaded pet only when its spritesheet is on disk, the given message as a store error otherwise
func usablePet(h, slug, failure string) (*InstalledPet, error) {
	p, err := LoadPet(h, slug)
	if err != nil {
		return nil, err
	}
	if p != nil && p.Exists() {
		return p, nil
	}
	if failure != "" {
		return nil, &StoreError{Msg: failure}
	}
	return nil, nil
}

// InstalledPets returns every pet directory holding a usable spritesheet, in name order
func InstalledPets(h string) ([]*InstalledPet, error) {
	root, err := PetsDir(h)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []*InstalledPet
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.IsDir() {
			continue
		}
		p, err := usablePet(h, name, "")
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// ResolveActivePet returns the configured pet when installed, else the first pet alphabetically
func ResolveActivePet(h, configuredSlug string) (*InstalledPet, error) {
	if configuredSlug != "" {
		p, err := usablePet(h, pystr.Strip(configuredSlug), "")
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	pets, err := InstalledPets(h)
	if err != nil || len(pets) == 0 {
		return nil, err
	}
	return pets[0], nil
}
