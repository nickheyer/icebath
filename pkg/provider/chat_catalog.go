package provider

import (
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
)

// Structured catalog types that mean the row is not a chat completion
var generationTypes = map[string]bool{
	"image": true, "image_generation": true, "image-generation": true, "image-gen": true,
	"video": true, "video_generation": true, "video-generation": true, "video-gen": true,
}

var generationOutputs = map[string]bool{"image": true, "video": true}

var chatOutputs = map[string]bool{"text": true, "chat": true}

// Separator qualified generation tokens, vision and vl stay chat
var generationIDRe = regexp2.MustCompile(
	`(?i)(?:^|[/_.-])(?:image(?:[/_.-]gen(?:eration)?)?|video(?:[/_.-]gen(?:eration)?)?|text[/_.-]to[/_.-](?:image|video)|stable-diffusion|sdxl|flux)(?=$|[/_.-])`,
	regexp2.None)

var (
	seenGenerationMu  sync.Mutex
	seenGenerationIDs = map[string]bool{}
)

// NoteGenerationModel remembers an id a live catalog row proved to be generation only
func NoteGenerationModel(modelID any) {
	key := strings.ToLower(strings.TrimSpace(stringOf(modelID)))
	if key == "" {
		return
	}
	seenGenerationMu.Lock()
	seenGenerationIDs[key] = true
	seenGenerationMu.Unlock()
}

// ModelIDIsGeneration reports whether the id names an image or video generation surface
func ModelIDIsGeneration(modelID any) bool {
	m, _ := generationIDRe.MatchString(strings.TrimSpace(stringOf(modelID)))
	return m
}

func tagListIsGeneration(tags any) bool {
	list, ok := tags.([]any)
	if !ok {
		return false
	}
	for _, tag := range list {
		if generationTypes[strings.ToLower(strings.TrimSpace(stringOf(tag)))] {
			return true
		}
	}
	return false
}

func outputModalitiesAreGeneration(outputs any) bool {
	list, ok := outputs.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	kinds := map[string]bool{}
	for _, item := range list {
		if s := strings.ToLower(strings.TrimSpace(stringOf(item))); s != "" {
			kinds[s] = true
		}
	}
	hasGen, hasChat := false, false
	for k := range kinds {
		hasGen = hasGen || generationOutputs[k]
		hasChat = hasChat || chatOutputs[k]
	}
	return hasGen && !hasChat
}

// CatalogItemIsGeneration fails open unless the row's own type or name proves it is generation only
func CatalogItemIsGeneration(item any) bool {
	row, ok := item.(map[string]any)
	if !ok {
		return false
	}
	if ModelIDIsGeneration(row["id"]) {
		return true
	}
	if caps, ok := row["capabilities"].(map[string]any); ok {
		if generationTypes[strings.ToLower(strings.TrimSpace(stringOf(caps["type"])))] {
			return true
		}
		if tagListIsGeneration(caps["tags"]) {
			return true
		}
	}
	if topType, ok := row["type"].(string); ok && generationTypes[strings.ToLower(strings.TrimSpace(topType))] {
		return true
	}
	if tagListIsGeneration(row["tags"]) {
		return true
	}
	if arch, ok := row["architecture"].(map[string]any); ok && outputModalitiesAreGeneration(arch["output_modalities"]) {
		return true
	}
	if meta, ok := row["metadata"].(map[string]any); ok && tagListIsGeneration(meta["tags"]) {
		return true
	}
	return false
}

// NoteCatalogItem remembers a generation row and reports whether it must leave the chat catalog
func NoteCatalogItem(item any) bool {
	if !CatalogItemIsGeneration(item) {
		return false
	}
	if row, ok := item.(map[string]any); ok {
		NoteGenerationModel(row["id"])
	}
	return true
}

// IsKnownNonChatModel is true when the name shape or an already parsed catalog item says not chat
func IsKnownNonChatModel(model string) bool {
	mid := strings.TrimSpace(model)
	if ModelIDIsGeneration(mid) {
		return true
	}
	seenGenerationMu.Lock()
	defer seenGenerationMu.Unlock()
	return seenGenerationIDs[strings.ToLower(mid)]
}

// WithoutGenerationModels drops generation ids and returns the same slice when none were dropped
func WithoutGenerationModels(models []string) []string {
	if len(models) == 0 {
		return models
	}
	kept := make([]string, 0, len(models))
	for _, m := range models {
		if !IsKnownNonChatModel(m) {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(models) {
		return models
	}
	return kept
}

// ChatCatalogIDs lists the model ids of a models payload minus generation rows
func ChatCatalogIDs(items any) []string {
	list, ok := items.([]any)
	if !ok {
		return []string{}
	}
	ids := []string{}
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, has := row["id"]
		if !has {
			continue
		}
		if NoteCatalogItem(item) {
			continue
		}
		ids = append(ids, stringOf(id))
	}
	return ids
}
