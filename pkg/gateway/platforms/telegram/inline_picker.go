package telegram

import (
	"context"
	"log/slog"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	PageSize         = 50
	CacheTimeSeconds = 10
)

// Inline picker catalog row, a dispatchable command name and its description
type CatalogItem struct {
	Name        string
	Description string
}

// One inline query article, offset scoped id with the command title and the message it sends
type InlineResult struct {
	ID          string
	Title       string
	Description string
	MessageText string
}

// Command registry reads of the inline picker, maxSlots below zero meaning no cap, installed by the process wiring
type InlineCommandSource interface {
	TelegramBotCommands(ctx context.Context) ([]CatalogItem, error)
	GatewaySkillEntries(ctx context.Context, platform string, maxSlots int, reserved map[string]bool, descLimit int,
		sanitize func(string) string) ([]CatalogItem, error)
	SanitizeTelegramName(raw string) string
}

// Registry surface the process wiring installs from pkg/commands over commands.Default and the raw config
var InlineCommands InlineCommandSource

// Every dispatchable command once, core gateway commands first, then uncapped plugin and skill commands
func CollectInlineCatalog(ctx context.Context) []CatalogItem {
	if InlineCommands == nil {
		panic("telegram.InlineCommands unassigned: the process wiring installs the pkg/commands telegram menu readers")
	}
	var catalog []CatalogItem
	seen := map[string]bool{}
	add := func(name, desc string) {
		if name != "" && !seen[name] {
			seen[name] = true
			catalog = append(catalog, CatalogItem{Name: name, Description: desc})
		}
	}
	if core, err := InlineCommands.TelegramBotCommands(ctx); err != nil {
		slog.Debug("inline picker: core command collection failed", "error", err)
	} else {
		for _, item := range core {
			add(item.Name, item.Description)
		}
	}
	reserved := make(map[string]bool, len(seen))
	for name := range seen {
		reserved[name] = true
	}
	entries, err := InlineCommands.GatewaySkillEntries(ctx, "telegram", -1, reserved, 100, InlineCommands.SanitizeTelegramName)
	if err != nil {
		slog.Debug("inline picker: skill/plugin collection failed", "error", err)
		return catalog
	}
	for _, entry := range entries {
		add(entry.Name, entry.Description)
	}
	return catalog
}

// Catalog ranked against term by name prefix, then name substring, then description, everything for an empty term
func FilterCatalog(catalog []CatalogItem, term string) []CatalogItem {
	term = pystr.LstripChars(pystr.Lower(pystr.Strip(term)), "/")
	if term == "" {
		return append([]CatalogItem{}, catalog...)
	}
	var prefix, nameSub, descSub []CatalogItem
	normTerm := strings.ReplaceAll(term, "-", "_")
	for _, item := range catalog {
		normName := strings.ReplaceAll(pystr.Lower(item.Name), "-", "_")
		switch {
		case strings.HasPrefix(normName, normTerm):
			prefix = append(prefix, item)
		case strings.Contains(normName, normTerm):
			nameSub = append(nameSub, item)
		case strings.Contains(pystr.Lower(item.Description), term):
			descSub = append(descSub, item)
		}
	}
	return append(append(append([]CatalogItem{}, prefix...), nameSub...), descSub...)
}

// First code points of s, all of s when it is shorter
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// One page of inline results whose first query token filters the catalog and whose rest becomes the command argument
func BuildInlineResults(ctx context.Context, query, offset string, pageSize int) ([]InlineResult, string) {
	parts := pystr.SplitWhitespace(pystr.Strip(query), 1)
	term, args := "", ""
	if len(parts) > 0 {
		term = parts[0]
	}
	if len(parts) > 1 {
		args = pystr.Strip(parts[1])
	}
	matches := FilterCatalog(CollectInlineCatalog(ctx), term)
	start := new(big.Int)
	if offset != "" {
		if parsed, ok := pyint.Parse(offset); ok && parsed.Sign() > 0 {
			start = parsed
		}
	}
	end := new(big.Int).Add(start, big.NewInt(int64(pageSize)))
	count := big.NewInt(int64(len(matches)))
	nextOffset := ""
	if count.Cmp(end) > 0 {
		nextOffset = end.String()
	}
	lo, hi := clampIndex(start, len(matches)), clampIndex(end, len(matches))
	var results []InlineResult
	for _, item := range matches[lo:max(lo, hi)] {
		messageText := "/" + item.Name
		if args != "" {
			messageText += " " + args
		}
		results = append(results, InlineResult{
			ID:          headRunes(start.String()+":"+item.Name, 64),
			Title:       "/" + item.Name,
			Description: headRunes(item.Description, 100),
			MessageText: headRunes(messageText, 4096),
		})
	}
	return results, nextOffset
}

// Python slice bound of a nonnegative or negative big index over a sequence of length n
func clampIndex(i *big.Int, n int) int {
	if i.Sign() < 0 {
		if i.Cmp(big.NewInt(int64(-n))) <= 0 {
			return 0
		}
		return n + int(i.Int64())
	}
	if i.Cmp(big.NewInt(int64(n))) >= 0 {
		return n
	}
	return int(i.Int64())
}
