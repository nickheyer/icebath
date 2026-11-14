package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	contextCacheLockTimeout = 5 * time.Second
	bedrockConfirmedSource  = "bedrock-confirmed-v1"
	contextLengthsSection   = "context_lengths"
	bedrockConfirmedSection = "bedrock_confirmed_v1"
	bedrockCacheURL         = "bedrock://"
)

type contextDocument struct {
	lengths   *yaml.Node
	confirmed *yaml.Node
}

func emptyMapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func mappingChild(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mappingDelete(m *yaml.Node, key string) bool {
	removed := false
	for i := 0; i+1 < len(m.Content); {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			removed = true
			continue
		}
		i += 2
	}
	return removed
}

func intNode(value int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
}

func mappingSet(m *yaml.Node, key string, value int) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = intNode(value)
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, intNode(value))
}

func presentNode(n *yaml.Node) bool {
	return n != nil && !(n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

func nodeInt(n *yaml.Node) (int, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64)
	if err != nil {
		return 0, false
	}
	return int(parsed), true
}

func nodeEquals(n *yaml.Node, value int) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	switch n.ShortTag() {
	case "!!int":
		got, ok := nodeInt(n)
		return ok && got == value
	case "!!float":
		got, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
		return err == nil && got == float64(value)
	}
	return false
}

func mappingSnapshot(m *yaml.Node) map[string]string {
	out := make(map[string]string, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out[m.Content[i].Value] = m.Content[i+1].ShortTag() + "\x00" + m.Content[i+1].Value
	}
	return out
}

func (c *Catalog) contextCachePath() string { return home.ContextLengthCache.In(c.home) }

func (c *Catalog) loadContextDocument() contextDocument {
	doc := contextDocument{lengths: emptyMapping(), confirmed: emptyMapping()}
	data, err := os.ReadFile(c.contextCachePath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("failed to load context length cache", "error", err)
		}
		return doc
	}
	var root yaml.Node
	if err := config.LoadYAML(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &root); err != nil {
		slog.Debug("failed to load context length cache", "error", err)
		return doc
	}
	top := &root
	if top.Kind == yaml.DocumentNode && len(top.Content) > 0 {
		top = top.Content[0]
	}
	if top.Kind != yaml.MappingNode {
		return doc
	}
	if section := mappingChild(top, contextLengthsSection); section != nil && section.Kind == yaml.MappingNode {
		doc.lengths = section
	}
	if section := mappingChild(top, bedrockConfirmedSection); section != nil && section.Kind == yaml.MappingNode {
		doc.confirmed = section
	}
	return doc
}

func (c *Catalog) writeContextDocument(ctx context.Context, doc contextDocument) error {
	top := emptyMapping()
	top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: contextLengthsSection}, doc.lengths)
	if len(doc.confirmed.Content) > 0 {
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: bedrockConfirmedSection}, doc.confirmed)
	}
	data, err := config.DumpYAML(top)
	if err != nil {
		return err
	}
	return fileutil.WriteFile(ctx, c.contextCachePath(), data)
}

func (c *Catalog) lockedContextCache(ctx context.Context, fn func(ctx context.Context)) {
	lockPath := home.ContextLengthCacheLock.In(c.home)
	wait, cancel := context.WithTimeout(ctx, contextCacheLockTimeout)
	defer cancel()
	lk, err := fileutil.OpenLock(wait, lockPath, fileutil.RecordHolder(), fileutil.Range(0, 1),
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for the context length cache lock (%s)", lockPath)))
	if err != nil {
		slog.Debug("skipping context length cache update", "error", err)
		return
	}
	defer func() { _ = lk.Release() }()
	fn(lk.Context(ctx))
}

func contextCacheKey(model, baseURL string) string {
	return model + "@" + strings.TrimRight(baseURL, "/")
}

func contextCacheAliases(model, baseURL string) []string {
	key := contextCacheKey(model, baseURL)
	return []string{key, model + "@" + baseURL, key + "/"}
}

func groupedInt(n int) string {
	text := strconv.Itoa(n)
	sign := ""
	if n < 0 {
		sign, text = "-", text[1:]
	}
	var b strings.Builder
	for i, r := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

func (c *Catalog) saveContextLength(ctx context.Context, model, baseURL string, length int, source string) {
	c.lockedContextCache(ctx, func(ctx context.Context) {
		if length <= 0 {
			slog.Warn("refusing to cache a context length that is not positive", "key", model+"@"+shownURL(baseURL), "tokens", length)
			return
		}
		key := contextCacheKey(model, baseURL)
		doc := c.loadContextDocument()
		before := mappingSnapshot(doc.confirmed)
		for _, alias := range contextCacheAliases(model, baseURL) {
			mappingDelete(doc.confirmed, alias)
		}
		if source == bedrockConfirmedSource {
			mappingSet(doc.confirmed, key, length)
		}
		if nodeEquals(mappingChild(doc.lengths, key), length) && maps.Equal(before, mappingSnapshot(doc.confirmed)) {
			return
		}
		mappingSet(doc.lengths, key, length)
		if err := c.writeContextDocument(ctx, doc); err != nil {
			slog.Debug("failed to save context length cache", "error", err)
			return
		}
		slog.Info("cached context length", "key", contextCacheKey(model, shownURL(baseURL)), "tokens", groupedInt(length))
	})
}

func (c *Catalog) SaveProviderContextLength(ctx context.Context, model, baseURL string, length int, provider string) {
	ctx = c.scoped(ctx)
	if isBedrockContext(baseURL, provider) {
		if baseURL == "" {
			baseURL = bedrockCacheURL
		}
		c.saveContextLength(ctx, model, baseURL, length, bedrockConfirmedSource)
		return
	}
	c.saveContextLength(ctx, model, baseURL, length, "")
}

func (c *Catalog) cachedContextLength(model, baseURL string, bedrockConfirmed bool) (int, bool) {
	doc := c.loadContextDocument()
	matched := ""
	var node *yaml.Node
	for _, alias := range contextCacheAliases(model, baseURL) {
		if n := mappingChild(doc.lengths, alias); presentNode(n) {
			matched, node = alias, n
			break
		}
	}
	length, ok := nodeInt(node)
	if !ok {
		return 0, false
	}
	if bedrockConfirmed {
		marker, ok := nodeInt(mappingChild(doc.confirmed, matched))
		if !ok || marker != length {
			return 0, false
		}
	}
	return length, true
}

func (c *Catalog) CachedContextLength(model, baseURL string, bedrockConfirmed bool) (int, bool) {
	return c.cachedContextLength(model, baseURL, bedrockConfirmed)
}

func (c *Catalog) dropContextCacheRows(ctx context.Context, keys []string, label string) {
	c.lockedContextCache(ctx, func(ctx context.Context) {
		doc := c.loadContextDocument()
		found := false
		for _, key := range keys {
			if mappingChild(doc.lengths, key) != nil || mappingChild(doc.confirmed, key) != nil {
				found = true
			}
		}
		if !found {
			return
		}
		for _, key := range keys {
			mappingDelete(doc.lengths, key)
			mappingDelete(doc.confirmed, key)
		}
		if err := c.writeContextDocument(ctx, doc); err != nil {
			slog.Debug("failed to invalidate a context length cache entry", "key", label, "error", err)
		}
	})
}

func (c *Catalog) invalidateCachedContextLength(ctx context.Context, model, baseURL string) {
	bare, stripped := c.StripProviderPrefix(ctx, model), strings.TrimRight(baseURL, "/")
	c.mu.Lock()
	delete(c.localProbes, localProbeKey{model: bare, baseURL: stripped})
	delete(c.localProbes, localProbeKey{kind: "ollama_show", model: bare, baseURL: stripped})
	for key := range c.bedrockMiss {
		if key.baseURL == stripped && (key.model == model || key.model == bare) {
			delete(c.bedrockMiss, key)
		}
	}
	c.mu.Unlock()
	c.dropContextCacheRows(ctx, contextCacheAliases(model, baseURL), contextCacheKey(model, shownURL(baseURL)))
}
