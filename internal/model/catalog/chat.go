package catalog

import (
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var generationTypes = []string{
	"image", "image_generation", "image-generation", "image-gen",
	"video", "video_generation", "video-generation", "video-gen",
}

var generationOutputs = []string{"image", "video"}

var chatOutputs = []string{"text", "chat"}

var generationID = regexp.MustCompile(`(?i)(?:^|[/_.-])(?:image(?:[/_.-]gen(?:eration)?)?|video(?:[/_.-]gen(?:eration)?)?|text[/_.-]to[/_.-](?:image|video)|stable-diffusion|sdxl|flux)(?:$|[/_.-])`)

func ModelIDIsGeneration(modelID string) bool {
	return generationID.MatchString(strings.TrimSpace(modelID))
}

func tagListIsGeneration(tags wire.Value) bool {
	if !tags.IsArray() {
		return false
	}
	return slices.ContainsFunc(tags.Elems(), func(tag wire.Value) bool {
		return tag.Truthy() && slices.Contains(generationTypes, strings.ToLower(strings.TrimSpace(tag.Display())))
	})
}

func outputModalitiesAreGeneration(outputs wire.Value) bool {
	if !outputs.IsArray() || len(outputs.Elems()) == 0 {
		return false
	}
	generation, chat := false, false
	for _, item := range outputs.Elems() {
		if !item.Truthy() {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(item.Display()))
		generation = generation || slices.Contains(generationOutputs, kind)
		chat = chat || slices.Contains(chatOutputs, kind)
	}
	return generation && !chat
}

func itemID(item wire.Value) string {
	id := item.Get("id")
	if !id.Truthy() {
		return ""
	}
	return id.Display()
}

func CatalogItemIsGeneration(item wire.Value) bool {
	if !item.IsObject() {
		return false
	}
	if ModelIDIsGeneration(itemID(item)) {
		return true
	}
	if caps := item.Get("capabilities"); caps.IsObject() {
		kind := ""
		if declared := caps.Get("type"); declared.Truthy() {
			kind = strings.ToLower(strings.TrimSpace(declared.Display()))
		}
		if slices.Contains(generationTypes, kind) || tagListIsGeneration(caps.Get("tags")) {
			return true
		}
	}
	if top, ok := item.Get("type").Str(); ok && slices.Contains(generationTypes, strings.ToLower(strings.TrimSpace(top))) {
		return true
	}
	if tagListIsGeneration(item.Get("tags")) {
		return true
	}
	if arch := item.Get("architecture"); arch.IsObject() && outputModalitiesAreGeneration(arch.Get("output_modalities")) {
		return true
	}
	if metadata := item.Get("metadata"); metadata.IsObject() && tagListIsGeneration(metadata.Get("tags")) {
		return true
	}
	return false
}

func (c *Catalog) NoteGenerationModel(modelID string) {
	key := strings.ToLower(strings.TrimSpace(modelID))
	if key == "" {
		return
	}
	c.mu.Lock()
	c.generation[key] = true
	c.mu.Unlock()
}

func (c *Catalog) NoteCatalogItem(item wire.Value) bool {
	if !CatalogItemIsGeneration(item) {
		return false
	}
	c.NoteGenerationModel(itemID(item))
	return true
}

func (c *Catalog) IsKnownNonChatModel(modelID string) bool {
	id := strings.TrimSpace(modelID)
	if ModelIDIsGeneration(id) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation[strings.ToLower(id)]
}

func (c *Catalog) WithoutGenerationModels(models []string) []string {
	if len(models) == 0 {
		return models
	}
	kept := make([]string, 0, len(models))
	for _, model := range models {
		if !c.IsKnownNonChatModel(model) {
			kept = append(kept, model)
		}
	}
	if len(kept) == len(models) {
		return models
	}
	return kept
}

func (c *Catalog) ChatCatalogIDs(items []wire.Value) []string {
	ids := []string{}
	for _, item := range items {
		if !item.IsObject() || !item.Get("id").Exists() {
			continue
		}
		if c.NoteCatalogItem(item) {
			continue
		}
		ids = append(ids, item.Get("id").Display())
	}
	return ids
}
