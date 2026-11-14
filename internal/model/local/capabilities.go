package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const propsTimeout = 3 * time.Second

var acceptedImageMIMEs = map[string]bool{"image/png": true, "image/jpeg": true}

func AcceptedImageMIME(mime string) bool { return acceptedImageMIMEs[mime] }

func (r *Runtime) IsManagedProvider(provider, baseURL string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if llamaCppAliases[p] {
		return true
	}
	if p != "custom" || baseURL == "" {
		return false
	}
	return r.IsManagedEndpoint(baseURL)
}

func (r *Runtime) propsModalities(ctx context.Context, modelID string) (bool, bool) {
	base, key, ok := r.managedRoot()
	if !ok {
		return false, false
	}
	props, err := r.managedGetJSON(ctx, base, key, "/props?"+childQuery(modelID), propsTimeout)
	if err != nil {
		return false, false
	}
	fields, _ := props.(map[string]any)
	modalities, ok := fields["modalities"].(map[string]any)
	if !ok {
		return false, false
	}
	vision, ok := modalities["vision"]
	if !ok {
		return false, false
	}
	return jsonTruthy(vision), true
}

func (r *Runtime) ManagedModelSupportsVision(ctx context.Context, modelID string) (bool, bool) {
	if modelID == "" {
		return false, false
	}
	staged := false
	for _, id := range r.StagedModelIDs() {
		if id == modelID {
			staged = true
			break
		}
	}
	if !staged {
		return false, false
	}
	if vision, ok := r.propsModalities(ctx, modelID); ok {
		return vision, true
	}
	entry := r.EntryForModel(modelID)
	if entry == nil {
		return false, false
	}
	if entry.MMProj == nil {
		return false, true
	}
	_, err := os.Stat(filepath.Join(r.AssetsDir(), entry.MMProj.LocalName()))
	return err == nil, true
}
