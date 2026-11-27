package auxiliary

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cast"
	"google.golang.org/protobuf/proto"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// How images reach the main model
const (
	// As they are when the model sees images, else described by a helper model
	ImagesAuto   = "auto"
	ImagesNative = "native"
	// Always described by a helper model
	ImagesText = "text"
)

// Kinds whose tool results take images, whatever their models
var toolMedia = []string{"openrouter", "vertex", "bedrock", "anthropic", "openai-codex", "openai-api", "azure-foundry"}

// Kinds that refuse images inside tool results though their models see
var toolMediaRefused = []string{"opencode-go", "xiaomi"}

// Whether the model of the route sees images, and whether that is known: a setting saying so for the main model, the capabilities its kind
// lists, what the models catalog knows of it, then the server itself for a model on this machine
func (c *Client) Sees(ctx context.Context, route provider.Route) (bool, bool) {
	if c.MainSees != nil && route.Provider == c.Main.Provider && route.Model == c.Main.Model {
		return *c.MainSees, true
	}
	if route.Model == "" {
		return false, false
	}
	spec, _ := c.Providers.Spec(route.Spec)
	for _, field := range []string{"supports_vision", "vision"} {
		if sees, ok := spec.ModelCapabilities[route.Model][field].(bool); ok {
			return sees, true
		}
	}
	catalog := c.Tasks.catalog()
	if catalog == nil {
		return false, false
	}
	if caps, ok := catalog.Capabilities(c.Config, route, true); ok && caps.Vision != nil {
		return *caps.Vision, true
	}
	if route.Local || models.Local(route.BaseURL) {
		return catalog.OllamaVision(ctx, c.Config, route)
	}
	return false, false
}

// Whether images a tool hands back go to the main model as they are, rather than being described to it by the vision task
func (c *Client) NativeImages(ctx context.Context) bool {
	return c.mode(ctx) == ImagesNative && c.toolImages(ctx, c.Main)
}

// How images reach the main model this turn: as they are when the setting says so, or when left to icebath for a model that sees and no
// vision model of its own is set; else as the vision task's descriptions
func (c *Client) mode(ctx context.Context) string {
	switch c.ImageInput {
	case ImagesNative, ImagesText:
		return c.ImageInput
	}
	if c.visionPinned() {
		return ImagesText
	}
	if sees, known := c.Sees(ctx, c.Main); known && sees {
		return ImagesNative
	}
	return ImagesText
}

// Whether the vision task names its own provider, model or endpoint, which then sees every image
func (c *Client) visionPinned() bool {
	task, ok := c.Tasks.Task("vision")
	return ok && c.settings(task).explicit()
}

// Whether the route takes images inside tool results
func (c *Client) toolImages(ctx context.Context, route provider.Route) bool {
	if route.Model == "" || slices.Contains(toolMediaRefused, route.Spec) {
		return false
	}
	if spec, ok := c.Providers.Spec(route.Spec); ok && spec.Aggregator {
		if vendor, _, routed := strings.Cut(route.Model, "/"); routed && slices.Contains(toolMediaRefused, strings.ToLower(vendor)) {
			return false
		}
	}
	if slices.Contains(toolMedia, route.Spec) {
		return true
	}
	if route.Spec == "gemini" {
		model := strings.ToLower(route.Model)
		return strings.Contains(model, "gemini-3") || strings.Contains(model, "gemini-pro-3") || strings.Contains(model, "gemini-flash-3")
	}
	if spec, ok := c.Providers.Spec(route.Spec); ok && (spec.Vision || spec.VisionToolMessages) {
		return true
	}
	sees, known := c.Sees(ctx, route)
	return known && sees
}

// Messages as the main model gets them. Images of the turn under way always go; earlier ones only for the most recent keep of them, in
// batches of keep so the prompt cache holds between turns, and none at all where the model can't take them. A message whose images are left
// out says so in its text
func (c *Client) Shown(ctx context.Context, messages []*sessionsv1.Message, keep int) []*sessionsv1.Message {
	if !slices.ContainsFunc(messages, func(message *sessionsv1.Message) bool { return len(message.GetMedia()) > 0 }) {
		return messages
	}
	sees := c.mode(ctx) == ImagesNative
	tools := sees && c.toolImages(ctx, c.Main)
	turn := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].GetRole() == sessionsv1.Role_ROLE_USER {
			turn = i
			break
		}
	}
	total := 0
	for _, message := range messages[:turn] {
		total += len(message.GetMedia())
	}
	from := 0
	if keep <= 0 {
		from = total
	} else if total > keep {
		from = (total - keep) / keep * keep
	}
	out := make([]*sessionsv1.Message, len(messages))
	slot := 0
	for i, message := range messages {
		out[i] = message
		count := len(message.GetMedia())
		if count == 0 {
			continue
		}
		first := slot
		slot += count
		takes := sees
		if message.GetRole() == sessionsv1.Role_ROLE_TOOL {
			takes = tools
		}
		if takes && (i >= turn || first >= from) {
			continue
		}
		note := "[" + render(count) + " no longer shown]"
		if !takes {
			note = "[" + render(count) + " left out: the model in use can't take images here]"
		}
		dropped := proto.CloneOf(message)
		dropped.Media = nil
		dropped.Text = strings.TrimSpace(dropped.GetText() + "\n\n" + note)
		out[i] = dropped
	}
	return out
}

// Count of images as words
func render(n int) string {
	if n == 1 {
		return "1 image"
	}
	return strconv.Itoa(n) + " images"
}

// Temperature the image task is told, its setting held between 0 and 2
func VisionTemperature(cfg *config.Config) float64 {
	return min(max(cast.ToFloat64(cfg.Get(Task{Name: auxiliaryconfig.Vision}.Key(auxiliaryconfig.Temperature))), 0), 2)
}
