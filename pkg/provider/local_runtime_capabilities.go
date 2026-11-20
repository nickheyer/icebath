package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Image formats the managed server's stb_image decoder handles, anything else is transcoded first
var AcceptedImageMIMEs = map[string]bool{"image/png": true, "image/jpeg": true}

// Seams the capability answers read through
var (
	capStagedModelIDs  = func() []string { return stagedModelIDs() }
	capPropsModalities = propsModalities
	capEntryForModel   = EntryForModel
	capAssetsDir       = func() string { return localRuntimeAssetsDir() }
	capIsManagedURL    = IsManagedEndpoint
	capManagedRoot     = managedRoot
)

// Whether provider and baseURL point at the managed server, custom only when baseURL is that endpoint
func IsManagedProvider(provider, baseURL string) (managed bool) {
	p := strings.ToLower(strings.TrimFunc(provider, pyIsSpace))
	if LlamacppAliases[p] {
		return true
	}
	if p != "custom" || baseURL == "" {
		return false
	}
	defer func() {
		if recover() != nil {
			managed = false
		}
	}()
	return capIsManagedURL(baseURL)
}

// Whether the running server reports vision for this loaded child, nil when down, unloaded or unreported
func propsModalities(ctx context.Context, modelID string) (vision *bool) {
	defer func() {
		if recover() != nil {
			vision = nil
		}
	}()
	base, apiKey, ok := capManagedRoot(ctx)
	if !ok {
		return nil
	}
	body, err := managedGetJSON(ctx, base, apiKey, "/props?"+childQuery(modelID), 3*time.Second)
	if err != nil {
		return nil
	}
	props, isDict := body.(map[string]any)
	if !isDict {
		return nil
	}
	modalities, isDict := props["modalities"].(map[string]any)
	if !isDict {
		return nil
	}
	raw, has := modalities["vision"]
	if !has {
		return nil
	}
	answer := pyTruthy(raw)
	return &answer
}

// Ground truth vision capability of a model staged with the managed runtime, nil when it is not ours
func ManagedModelSupportsVision(ctx context.Context, modelID string) (vision *bool) {
	if modelID == "" {
		return nil
	}
	defer func() {
		if recover() != nil {
			vision = nil
		}
	}()
	if !slices.Contains(capStagedModelIDs(), modelID) {
		return nil
	}
	if live := capPropsModalities(ctx, modelID); live != nil {
		return live
	}
	entry := capEntryForModel(modelID)
	if entry == nil {
		return nil
	}
	answer := false
	if entry.MMProj != nil {
		_, err := os.Stat(filepath.Join(capAssetsDir(), entry.MMProj.LocalName()))
		answer = err == nil
	}
	return &answer
}
