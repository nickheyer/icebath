package model

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const DefaultImageTokenCost = 1500

const (
	minPlausibleImageCost = 64
	maxPlausibleImageCost = 32768
	imageCostEMA          = 0.5
)

type imageCostKey struct{}

type imageCostBinding struct {
	cost atomic.Int64
}

func WithImageCost(ctx context.Context, cost int) context.Context {
	b := &imageCostBinding{}
	b.cost.Store(int64(cost))
	return context.WithValue(ctx, imageCostKey{}, b)
}

func ImageCost(ctx context.Context) int {
	if b, ok := ctx.Value(imageCostKey{}).(*imageCostBinding); ok {
		return int(b.cost.Load())
	}
	return DefaultImageTokenCost
}

type ImageCosts struct {
	path   string
	mu     sync.Mutex
	loaded bool
	keys   []string
	table  map[string]int
}

func NewImageCosts(homeDir string) *ImageCosts {
	return &ImageCosts{path: home.ImageTokenCosts.In(homeDir)}
}

func imageCostKeyOf(model, baseURL string) string {
	return model + "@" + providers.URLHostname(baseURL)
}

func (c *ImageCosts) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.table = map[string]int{}
	var raw json.RawMessage
	if err := fileutil.ReadJSON(c.path, &raw); err != nil {
		return
	}
	doc, err := wire.ParseValue(raw)
	if err != nil || !doc.IsObject() {
		return
	}
	for _, f := range doc.Fields() {
		n, ok := integerValue(f.Value)
		if !ok || n < minPlausibleImageCost || n > maxPlausibleImageCost {
			continue
		}
		if _, seen := c.table[f.Key]; !seen {
			c.keys = append(c.keys, f.Key)
		}
		c.table[f.Key] = int(n)
	}
}

func (c *ImageCosts) Learned(model, baseURL string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if cost, ok := c.table[imageCostKeyOf(model, baseURL)]; ok {
		return cost
	}
	return DefaultImageTokenCost
}

func (c *ImageCosts) Bind(ctx context.Context, model, baseURL string) context.Context {
	return WithImageCost(ctx, c.Learned(model, baseURL))
}

type Calibration struct {
	Model        string
	BaseURL      string
	Messages     []wire.Message
	BaseCount    int
	PromptTokens int
	TextOnly     func(ctx context.Context) (int, bool)
}

func (c *ImageCosts) Calibrate(ctx context.Context, in Calibration) (int, bool) {
	if in.PromptTokens <= 0 || in.TextOnly == nil {
		return 0, false
	}
	delta := in.Messages[min(max(in.BaseCount, 0), len(in.Messages)):]
	if len(delta) > 0 && delta[0].Role == wire.RoleAssistant {
		delta = delta[1:]
	}
	images := CountImages(delta)
	if images <= 0 {
		return 0, false
	}
	textOnly, ok := in.TextOnly(WithImageCost(ctx, 0))
	if !ok {
		return 0, false
	}
	perImage := floorDiv(in.PromptTokens-textOnly, images)
	if perImage < minPlausibleImageCost || perImage > maxPlausibleImageCost {
		return 0, false
	}
	key := imageCostKeyOf(in.Model, in.BaseURL)
	c.mu.Lock()
	c.load()
	learned := perImage
	if prior, ok := c.table[key]; ok {
		learned = int(float64(prior) + imageCostEMA*float64(perImage-prior))
	} else {
		c.keys = append(c.keys, key)
	}
	c.table[key] = learned
	members := make([]wire.Member, 0, len(c.keys))
	for _, k := range c.keys {
		members = append(members, wire.Member{Key: k, Value: json.RawMessage(strconv.Itoa(c.table[k]))})
	}
	c.mu.Unlock()
	if b, ok := ctx.Value(imageCostKey{}).(*imageCostBinding); ok {
		b.cost.Store(int64(learned))
	}
	style := fileutil.JSONStyle{Lines: true, Indent: 0, Tight: true}
	if err := fileutil.WriteJSON(ctx, c.path, wire.Object(members), style); err != nil {
		slog.Debug("image token cost persist failed", "error", err)
	}
	slog.Info("image token cost calibrated from provider usage", "images", images, "per_image", perImage, "learned", learned, "key", key)
	return learned, true
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
