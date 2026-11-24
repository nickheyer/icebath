package telegram

import (
	"context"
	"errors"
	"strconv"
	"strings"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
	TelegramMenuMaxCommands(ctx context.Context) int
	TelegramMenuCommands(ctx context.Context, maxCommands int) ([]CatalogItem, int, error)
}

// Command menu readers the process wiring installed, panicking when unassigned
func menuCommands() InlineCommandSource {
	if InlineCommands == nil {
		panic("telegram.InlineCommands unassigned: the process wiring installs the pkg/commands telegram menu readers")
	}
	return InlineCommands
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
		pkgLog().Debug("inline picker: core command collection failed", "error", err)
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
		pkgLog().Debug("inline picker: skill/plugin collection failed", "error", err)
		return catalog
	}
	for _, entry := range entries {
		add(entry.Name, entry.Description)
	}
	return catalog
}

// Catalog ranked against term by name prefix, then name substring, then description, everything for an empty term
func FilterCatalog(catalog []CatalogItem, term string) []CatalogItem {
	term = strings.TrimLeft(strings.ToLower(strings.TrimSpace(term)), "/")
	if term == "" {
		return append([]CatalogItem{}, catalog...)
	}
	var prefix, nameSub, descSub []CatalogItem
	normTerm := strings.ReplaceAll(term, "-", "_")
	for _, item := range catalog {
		normName := strings.ReplaceAll(strings.ToLower(item.Name), "-", "_")
		switch {
		case strings.HasPrefix(normName, normTerm):
			prefix = append(prefix, item)
		case strings.Contains(normName, normTerm):
			nameSub = append(nameSub, item)
		case strings.Contains(strings.ToLower(item.Description), term):
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
	parts := textutil.FieldsN(strings.TrimSpace(query), 1)
	term, args := "", ""
	if len(parts) > 0 {
		term = parts[0]
	}
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	matches := FilterCatalog(CollectInlineCatalog(ctx), term)
	start := 0
	if offset != "" {
		// A positive offset, held at the end of the catalog when it lies past it or past what an int64 holds
		if parsed, err := strconv.ParseInt(strings.TrimSpace(offset), 10, 64); (err == nil || errors.Is(err, strconv.ErrRange)) && parsed > 0 {
			start = int(min(parsed, int64(len(matches))))
		}
	}
	end := start + pageSize
	nextOffset := ""
	if len(matches) > end {
		nextOffset = strconv.Itoa(end)
	}
	var results []InlineResult
	for _, item := range matches[start:max(start, min(end, len(matches)))] {
		messageText := "/" + item.Name
		if args != "" {
			messageText += " " + args
		}
		results = append(results, InlineResult{
			ID:          headRunes(strconv.Itoa(start)+":"+item.Name, 64),
			Title:       "/" + item.Name,
			Description: headRunes(item.Description, 100),
			MessageText: headRunes(messageText, 4096),
		})
	}
	return results, nextOffset
}
