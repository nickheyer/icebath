package verify

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const ManifestVersion = 1

var manifestRelpath = filepath.Join(".icebath", "environment.json")

// Verify manifest path of the project at root
func ManifestPath(root string) string { return filepath.Join(root, manifestRelpath) }

// Saved recipe in the wrapped or bare shape, nil on any read, decode or shape problem
func LoadManifest(root string) (*Recipe, error) {
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		return nil, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, nil
	}
	manifest, err := pyjson.Loads(pystr.TranslateNewlines(text))
	if err != nil {
		return nil, nil
	}
	obj, ok := manifest.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	if recipe, ok := obj.Get("recipe"); ok {
		return RecipeFromDict(recipe)
	}
	return RecipeFromDict(obj)
}

// Writes the recipe in the versioned wrapper shape and returns the manifest path
func SaveManifest(root string, recipe *Recipe) (string, error) {
	path := ManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", &OSError{Err: err}
	}
	payload := pyjson.ObjectOf("version", int64(ManifestVersion), "recipe", recipe.ToDict(),
		"updatedAt", pydatetime.ISOFormatUTC(time.Now()))
	text, err := pyjson.Dumps(payload, pyjson.Indent(2))
	if err != nil {
		return "", err
	}
	text += "\n"
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return "", &OSError{Err: err}
	}
	return path, nil
}

// Saved manifest recipe with source manifest, else the detected recipe with source detected
func LoadOrDetect(root string) (*Recipe, string, error) {
	saved, err := LoadManifest(root)
	if err != nil {
		return nil, "", err
	}
	if saved != nil {
		return saved, "manifest", nil
	}
	detected, err := DetectRecipe(root)
	return detected, "detected", err
}
