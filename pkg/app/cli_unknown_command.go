package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lead and pointer lines for a slash token with no handler, a close match becoming a suggestion
func unknownCommandLines(ctx context.Context, typed string, known []string) (string, string) {
	base := typed
	if tokens := pystr.SplitWhitespace(typed, -1); len(tokens) > 0 {
		base = tokens[0]
	}
	hint := ""
	if close := pydifflib.GetCloseMatches(base, known, 1, 0.6); len(close) > 0 {
		hint = " " + i18n.T(ctx, "cli.command.did_you_mean_one", "", map[string]any{"suggestion": close[0]})
	}
	return i18n.T(ctx, "cli.command.unknown_nothing_sent", "", map[string]any{"command": base, "hint": hint}),
		i18n.T(ctx, "cli.command.type_help", "", nil)
}
