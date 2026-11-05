package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Observed per image prices outside this band are text estimate noise, and the learned price moves halfway
const (
	imageCostMinPlausible = 64
	imageCostMaxPlausible = 32_768
	imageCostEMAAlpha     = 0.5
)

type imageCostCell struct {
	mu   sync.Mutex
	cost *int
}

type imageCostCtxKey struct{}

// Launch profile table plus one table per routed home, each loaded lazily from its own cache file
var imageCostTables struct {
	mu     sync.Mutex
	launch *pyjson.Object
	byHome map[string]*pyjson.Object
}

func imageCostCachePath(ctx context.Context) string {
	return metadataCacheFile(ctx, "image_token_costs.json")
}

func imageCostTableKey(model, baseURL string) string {
	return model + "@" + baseURLHostname(baseURL)
}

// Plausible integer prices of the cache file in file order, empty when missing or invalid
func readImageCostCache(ctx context.Context) *pyjson.Object {
	out := pyjson.NewObject()
	raw, err := os.ReadFile(imageCostCachePath(ctx))
	if err != nil {
		return out
	}
	v, err := pyjson.Loads(string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	if err != nil {
		return out
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return out
	}
	for _, k := range obj.Keys {
		if n, ok := obj.Vals[k].(int64); ok && n >= imageCostMinPlausible && n <= imageCostMaxPlausible {
			out.Set(k, n)
		}
	}
	return out
}

// Learned table of the active profile, the caller holds the tables lock
func imageCostTable(ctx context.Context) *pyjson.Object {
	if home.Override(ctx) == "" {
		if imageCostTables.launch == nil {
			imageCostTables.launch = readImageCostCache(ctx)
		}
		return imageCostTables.launch
	}
	key := home.Key(metadataHome(ctx))
	if imageCostTables.byHome == nil {
		imageCostTables.byHome = map[string]*pyjson.Object{}
	}
	table, ok := imageCostTables.byHome[key]
	if !ok {
		table = readImageCostCache(ctx)
		imageCostTables.byHome[key] = table
	}
	return table
}

// LearnedImageTokenCost is the learned per image price for model at the base URL host, else the flat default
func LearnedImageTokenCost(ctx context.Context, model, baseURL string) int {
	imageCostTables.mu.Lock()
	defer imageCostTables.mu.Unlock()
	if v, ok := imageCostTable(ctx).Get(imageCostTableKey(model, baseURL)); ok {
		return int(v.(int64))
	}
	return DefaultImageTokenCost
}

// CurrentImageTokenCost is the per image price bound to the running turn, else the flat default
func CurrentImageTokenCost(ctx context.Context) int {
	if cell, ok := ctx.Value(imageCostCtxKey{}).(*imageCostCell); ok {
		cell.mu.Lock()
		defer cell.mu.Unlock()
		if cell.cost != nil {
			return *cell.cost
		}
	}
	return DefaultImageTokenCost
}

// ImageCostContext scopes a per image price over the returned context, nil restoring the flat default
func ImageCostContext(ctx context.Context, cost *int) context.Context {
	cell := &imageCostCell{}
	if cost != nil {
		c := *cost
		cell.cost = &c
	}
	return context.WithValue(ctx, imageCostCtxKey{}, cell)
}

// Sets the turn price in place when the context already carries one, else binds a new scope
func setImageCost(ctx context.Context, cost int) context.Context {
	if cell, ok := ctx.Value(imageCostCtxKey{}).(*imageCostCell); ok {
		cell.mu.Lock()
		cell.cost = &cost
		cell.mu.Unlock()
		return ctx
	}
	return ImageCostContext(ctx, &cost)
}

// BindImageTokenCost binds the learned per image price of model and base URL for the rest of the turn
func BindImageTokenCost(ctx context.Context, model, baseURL string) context.Context {
	return setImageCost(ctx, LearnedImageTokenCost(ctx, model, baseURL))
}

// CountImages is the number of image parts across the messages
func CountImages(messages []*state.Message) int {
	n := 0
	for _, m := range messages {
		if m != nil {
			_, images := messageShadow(m)
			n += images
		}
	}
	return n
}

// CalibrateImageTokenCost learns the per image price from the residual of a response over the still matching previous anchor
func CalibrateImageTokenCost(ctx context.Context, model, baseURL string, anchor *UsageAnchor, messages []*state.Message, promptTokens any) (context.Context, int, bool) {
	real, ok := anchorInt(promptTokens)
	if !ok || real <= 0 || anchor == nil {
		return ctx, 0, false
	}
	start := anchor.BaseCount
	if start < 0 {
		start = max(len(messages)+start, 0)
	}
	var delta []*state.Message
	if start < len(messages) {
		delta = messages[start:]
	}
	if len(delta) > 0 && delta[0] != nil && delta[0].Role == "assistant" {
		delta = delta[1:]
	}
	nImages := CountImages(delta)
	if nImages <= 0 {
		return ctx, 0, false
	}
	zero := 0
	textOnly, ok := AnchoredContextTokens(ImageCostContext(ctx, &zero), messages, anchor, true)
	if !ok {
		return ctx, 0, false
	}
	perImage := int(pyFloorDiv(int64(real-textOnly), int64(nImages)))
	if perImage < imageCostMinPlausible || perImage > imageCostMaxPlausible {
		return ctx, 0, false
	}
	key := imageCostTableKey(model, baseURL)
	imageCostTables.mu.Lock()
	table := imageCostTable(ctx)
	learned := perImage
	if prior, ok := table.Get(key); ok {
		p := prior.(int64)
		learned = int(float64(p) + imageCostEMAAlpha*float64(int64(perImage)-p))
	}
	table.Set(key, int64(learned))
	snapshot := pyjson.NewObject()
	for _, k := range table.Keys {
		snapshot.Set(k, table.Vals[k])
	}
	imageCostTables.mu.Unlock()
	ctx = setImageCost(ctx, learned)
	if err := config.AtomicJSONWriteWith(imageCostCachePath(ctx), snapshot, pyjson.Indent(0), pyjson.Separators(",", ":")); err != nil {
		slog.Debug(fmt.Sprintf("image token cost persist failed: %v", err))
	}
	slog.Info(fmt.Sprintf("Image token cost calibrated from provider usage: %d images priced %s tokens each (learned %s for %s)",
		nImages, groupThousands(perImage), groupThousands(learned), key))
	return ctx, learned, true
}
