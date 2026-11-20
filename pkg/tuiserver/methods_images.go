package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var imagesRegistry = &handlerRegistry{}

// Registers the inline image method, a decode heavy enough for the worker pool
func init() {
	imagesRegistry.method("image.render", imageRender)
	imagesRegistry.install()
	addLongHandlers("image.render")
}

// Lays out a local image inside the caller's cell budget in the form display.inline_images and the caller's terminal allow
func imageRender(ctx context.Context, rid any, params map[string]any) map[string]any {
	detected := "unicode"
	if params["terminal"] == "kitty" {
		detected = "kitty"
	}
	configured, present := displayCfg(ctx)["inline_images"]
	if !present {
		configured = "auto"
	}
	graphics, _ := params["graphics"].(bool)
	mode := resolveInlineMode(configured, graphics, detected)
	budget := [2]int{}
	for i, key := range []string{"cols", "rows"} {
		if raw := params[key]; truthy(raw) {
			n, err := pyint.Int64(raw)
			if err != nil {
				return rpcErr(rid, 4072, "cols and rows must be integers", nil)
			}
			budget[i] = int(min(n, inlineMaxCols+inlineMaxRows))
		}
	}
	path := tuigateway.LocalImagePath(params["path"])
	var img *inlineImage
	if path != "" {
		img = renderInlineImage(path, mode, min(budget[0], inlineMaxCols), min(budget[1], inlineMaxRows))
	}
	switch {
	case img == nil:
		return ok(rid, map[string]any{"mode": "off"})
	case img.mode == "kitty":
		return ok(rid, map[string]any{"mode": "kitty", "cols": img.cols, "rows": img.rows, "color": img.color,
			"transmit": img.transmit, "placeholder": img.placeholder})
	}
	return ok(rid, map[string]any{"mode": "unicode", "cols": img.cols, "rows": img.rows, "cells": img.cells})
}
