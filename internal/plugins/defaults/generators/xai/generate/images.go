package generate

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/models"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/config"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	defaultImageModel = "grok-imagine-image"
	// Model edits take when the one chosen reads no pictures
	editModel = "grok-imagine-image-quality"
	// Most pictures one edit takes
	maxSources = 3
	liveTTL    = 5 * time.Minute
	liveLimit  = 10 * time.Second
	imageLimit = 120 * time.Second
	noKey      = "No xAI credentials found. Connect xAI or xAI Grok OAuth with /providers, or set XAI_API_KEY."
)

// One picture model of xAI and what it reads
type picture struct {
	id, title, speed, strengths string
	inputs                      []string
}

// Picture models of xAI that are known, in the order the picker lists them
var pictures = []picture{
	{id: "grok-imagine-image", title: "Grok Imagine Image", speed: "5 to 10 seconds", strengths: "Fast and high quality"},
	{id: "grok-imagine-image-2.0", title: "Grok Imagine Image 2.0", speed: "10 to 20 seconds", strengths: "Minds lettering and layout, readable small text, the best quality"},
	{id: "grok-imagine-image-quality", title: "Grok Imagine Image Quality", speed: "10 to 20 seconds", strengths: "More faithful and detailed, slower than the standard model"},
}

var aspectRatios = map[string]string{"landscape": "16:9", "square": "1:1", "portrait": "9:16"}

// Picture models as the live list gives them with the known ones' text, the known ones it leaves out after them; the known ones alone
// when the list can't be read
type catalog struct {
	order  []string
	models map[string]picture
	at     time.Time
}

// Picture models of xAI for the profile's bearer, read again after five minutes
func (s *Services) catalog(ctx context.Context, cfg *config.Config) catalog {
	bearer, _ := s.bearer(cfg)
	key := bearer.BaseURL + "#" + models.Identity(bearer.Key)
	s.mu.Lock()
	held, ok := s.live[key]
	s.mu.Unlock()
	if ok && time.Since(held.at) < liveTTL {
		return held
	}
	fresh := catalog{models: map[string]picture{}, at: time.Now()}
	if bearer.Key != "" {
		reply, err := generation.Get(ctx, bearer.BaseURL+"/image-generation-models", map[string]string{"Authorization": "Bearer " + bearer.Key}, liveLimit)
		if err != nil {
			slog.Debug("xAI live image model catalog unavailable", "error", err)
		}
		entries := generation.List(reply, "models")
		if len(entries) == 0 {
			entries = generation.List(reply, "data")
		}
		for _, entry := range entries {
			id := generation.Text(entry, "id")
			if id == "" {
				id = generation.Text(entry, "name")
			}
			if id == "" {
				continue
			}
			p := picture{id: id, title: id, strengths: "A new xAI Imagine model, from the current xAI list"}
			if i := slices.IndexFunc(pictures, func(known picture) bool { return known.id == id }); i >= 0 {
				p = pictures[i]
			}
			p.inputs = nil
			for _, input := range generation.List(entry, "input_modalities") {
				if text, ok := input.(string); ok {
					p.inputs = append(p.inputs, text)
				}
			}
			if _, seen := fresh.models[id]; !seen {
				fresh.order = append(fresh.order, id)
			}
			fresh.models[id] = p
		}
	}
	for _, known := range pictures {
		if _, ok := fresh.models[known.id]; !ok {
			fresh.order = append(fresh.order, known.id)
			fresh.models[known.id] = known
		}
	}
	s.mu.Lock()
	s.live[key] = fresh
	s.mu.Unlock()
	return fresh
}

func (s *Services) imageModels(ctx context.Context, cfg *config.Config) []generation.Model {
	c := s.catalog(ctx, cfg)
	out := make([]generation.Model, len(c.order))
	for i, id := range c.order {
		p := c.models[id]
		out[i] = generation.Model{ID: id, Title: p.title, Speed: p.speed, Strengths: p.strengths}
	}
	return out
}

// Model the call draws with: the one image_gen.xai.model names when the catalog has it, else the default
func (s *Services) model(c catalog, name string) string {
	if _, ok := c.models[strings.TrimSpace(name)]; ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return defaultImageModel
}

// Model an edit takes: the one image_gen.xai.model names when it reads pictures, else the quality model
func (s *Services) editing(c catalog, name string) string {
	chosen := strings.TrimSpace(name)
	if p, ok := c.models[chosen]; ok && slices.Contains(p.inputs, "image") {
		return chosen
	}
	return editModel
}

// Posts to xAI, renewing a Grok sign-in xAI refused and posting once more with it
func (s *Services) post(ctx context.Context, cfg *config.Config, bearer web.XAI, endpoint string, payload map[string]any, limit time.Duration) (any, *generation.Failure) {
	reply, failure := generation.Post(ctx, endpoint, headers(bearer.Key), payload, limit, limit, "xAI", nil)
	if failure == nil || failure.Status != 401 || bearer.Source != web.XAIOAuth {
		return reply, failure
	}
	if web.RenewXAI(ctx, s.providers, s.creds, cfg) != nil {
		return reply, failure
	}
	renewed, ok := s.bearer(cfg)
	if !ok {
		return reply, failure
	}
	return generation.Post(ctx, strings.Replace(endpoint, bearer.BaseURL, renewed.BaseURL, 1), headers(renewed.Key), payload, limit, limit, "xAI", nil)
}

// Whether the path names a regular file of this machine, ~ expanded
func fileExists(path string) bool {
	return fsx.IsFile(fsx.ExpandHome(strings.TrimSpace(path)))
}

// Image field of an edit: a URL or data URL as it is, a file of this machine inlined as a data URL
func field(job *generation.Job, source string) (map[string]any, error) {
	if generation.Remote(source) {
		return map[string]any{"url": source, "type": "image_url"}, nil
	}
	data, err := job.Local(source)
	if err != nil {
		return nil, err
	}
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(source)), ".")
	switch extension {
	case "":
		extension = "png"
	case "jpg":
		extension = "jpeg"
	}
	return map[string]any{"url": generation.DataURL("image/"+extension, data), "type": "image_url"}, nil
}

// Draws through images/generations or edits through images/edits, the picture kept in xAI storage with a public link when it is on, else
// in the cache
func (s *Services) draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	bearer, ok := s.bearer(cfg)
	source := strings.TrimSpace(bearer.Source)
	if source == "" {
		source = xaiconfig.Name
	}
	aspect := generation.Aspect(request.Aspect)
	if !ok {
		return generation.Call{Provider: source, Aspect: strings.TrimSpace(request.Aspect)}.Fail(noKey, "missing_api_key")
	}
	c := s.catalog(ctx, cfg)
	model := s.model(c, request.Model)
	sources := request.Sources(-1)
	refused := generation.Call{Provider: source, Model: editModel, Prompt: request.Prompt, Aspect: aspect}
	if len(sources) > maxSources {
		return refused.Fail(fmt.Sprintf("xAI image editing supports at most %d source images", maxSources), "too_many_references")
	}
	for i, ref := range sources {
		if generation.Remote(ref) || fileExists(ref) {
			continue
		}
		field := "reference_image_urls"
		if i == 0 && strings.TrimSpace(request.Image) == ref {
			field = "image_url"
		}
		return refused.Fail(field+" must be a public HTTPS URL or data URI (e.g. the `image`/`public_url` from a prior Imagine result)", "invalid_image_url")
	}
	edit := len(sources) > 0
	st := storageOf(cfg, xaiconfig.ImageKey)
	resolution := cfg.GetString(xaiconfig.Resolution)
	var payload map[string]any
	endpoint := bearer.BaseURL + "/images/generations"
	if edit {
		model = s.editing(c, request.Model)
		var fields []any
		for _, ref := range sources {
			f, err := field(job, ref)
			if err != nil {
				return generation.Call{Provider: source, Model: model, Prompt: request.Prompt, Aspect: aspect}.Fail("Could not load source image for editing: "+err.Error(), "io_error")
			}
			fields = append(fields, f)
		}
		payload = map[string]any{"model": model, "prompt": request.Prompt}
		if len(fields) == 1 {
			payload["image"] = fields[0]
		} else {
			payload["images"] = fields
		}
		endpoint = bearer.BaseURL + "/images/edits"
	} else {
		ratio, ok := aspectRatios[aspect]
		if !ok {
			ratio = "1:1"
		}
		payload = map[string]any{"model": model, "prompt": request.Prompt, "aspect_ratio": ratio, "resolution": resolution}
	}
	if options := st.options("icebath-xai-image", "png"); options != nil {
		payload["storage_options"] = options
	}
	notice := st.once(cfg, "image_gen")
	call := generation.Call{Provider: source, Model: model, Prompt: request.Prompt, Aspect: aspect}
	reply, failure := s.post(ctx, cfg, bearer, endpoint, payload, imageLimit)
	if failure != nil {
		if failure.Kind == "http" {
			slog.Error(fmt.Sprintf("xAI image gen failed (%d): %s", failure.Status, failure.Message))
		}
		return call.Fail(failure.Error, failure.Type)
	}
	data := generation.List(reply, "data")
	if len(data) == 0 {
		return call.Fail("xAI returned no image data", "empty_response")
	}
	first := generation.Object(data[0])
	output := generation.Object(first["file_output"])
	public := generation.Text(output, "public_url")
	image := public
	if public == "" {
		path, failed := job.Picture(ctx, generation.Call{Provider: xaiconfig.Name, Model: model, Prompt: request.Prompt, Aspect: aspect},
			generation.Text(first, "b64_json"), generation.Text(first, "url"), "xai_"+model, "xAI", bearer.BaseURL)
		if failed != nil {
			return *failed
		}
		image = path
	}
	extra := map[string]any{"storage_enabled": st.enabled}
	if !edit {
		extra["resolution"] = resolution
	}
	if notice != "" {
		extra["storage_notice"] = notice
	}
	if public != "" {
		extra["public_url"] = public
	}
	for _, key := range fileOutput {
		if value, ok := output[key]; ok {
			extra[key] = value
		}
	}
	if usage := generation.Object(reply)["usage"]; usage != nil {
		extra["usage"] = usage
	}
	modality := "text"
	if edit {
		modality = "image"
	}
	call.Provider = xaiconfig.Name
	made := call.Made(image, modality, extra)
	if public != "" {
		if copied, err := job.Download(ctx, generation.Image, public, generation.Fetch{Prefix: "xai_" + model}); err == nil {
			made.Copy = copied
		} else {
			slog.Debug("xAI stored image could not be kept in the cache", "url", public, "error", err)
		}
	}
	return made
}
