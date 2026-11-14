package i18n

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/locales"
)

const envBundledLocales = "ICEBATH_BUNDLED_LOCALES"

var (
	ErrInvalidLanguage = errors.New("invalid language id")
	ErrUnknownSurface  = errors.New("unknown locale surface")
)

type Pack struct {
	lang     string
	surface  Surface
	messages map[string]string
	source   string
	endonym  string
	rtl      bool
}

type packInfo struct {
	endonym string
	rtl     bool
	source  string
}

type views struct {
	supported map[string][]string
	merged    map[[2]string]map[string]string
	packs     map[[2]string]map[string]string
	overlay   map[[3]string]map[string]string
	scans     map[string][]File
	bundled   map[string][]File
}

func newViews() views {
	return views{
		supported: map[string][]string{},
		merged:    map[[2]string]map[string]string{},
		packs:     map[[2]string]map[string]string{},
		overlay:   map[[3]string]map[string]string{},
		scans:     map[string][]File{},
		bundled:   map[string][]File{},
	}
}

type Catalog struct {
	homes  *home.Resolver
	prefer func(ctx context.Context) (string, error)

	mu    sync.Mutex
	gen   uint64
	packs []*Pack
	views views
}

func New(homes *home.Resolver, prefer func(ctx context.Context) (string, error)) *Catalog {
	return &Catalog{homes: homes, prefer: prefer, views: newViews()}
}

func (c *Catalog) Reset() {
	c.mu.Lock()
	c.gen++
	c.views = newViews()
	c.mu.Unlock()
}

func (c *Catalog) RegisterPack(lang string, surface Surface, messages map[string]string, source, endonym string, rtl bool) (*Pack, error) {
	id := NormalizeID(lang)
	if !languageIDPattern.MatchString(id) {
		return nil, fmt.Errorf("%w %s (expected e.g. 'pl', 'pt-br', 'zh-hant')", ErrInvalidLanguage, quote(lang, false))
	}
	if !surface.Valid() {
		return nil, fmt.Errorf("%w %s (expected one of %s)", ErrUnknownSurface, quote(string(surface), false), surfaceList)
	}
	p := &Pack{lang: id, surface: surface, messages: maps.Clone(messages), source: source, endonym: endonym, rtl: rtl}
	if p.messages == nil {
		p.messages = map[string]string{}
	}
	c.mu.Lock()
	c.packs = append(c.packs, p)
	c.gen++
	c.views = newViews()
	c.mu.Unlock()
	slog.Debug(fmt.Sprintf("i18n pack registered: %s/%s from %s (%d keys)", id, surface, source, len(p.messages)))
	return p, nil
}

func (c *Catalog) UnregisterPack(p *Pack) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, q := range c.packs {
		if q == p {
			c.packs = append(c.packs[:i:i], c.packs[i+1:]...)
			c.gen++
			c.views = newViews()
			return true
		}
	}
	return false
}

func (c *Catalog) T(ctx context.Context, key string, args Args) string {
	return c.In(ctx, "", key, args)
}

func (c *Catalog) In(ctx context.Context, lang, key string, args Args) string {
	h := c.homeOrNone(ctx)
	var target string
	if lang != "" {
		target = c.normalize(h, lang)
	} else {
		target = c.active(ctx, h)
	}
	value, ok := c.merged(h, target)[key]
	if !ok && target != DefaultLanguage {
		value, ok = c.merged(h, DefaultLanguage)[key]
	}
	if !ok {
		slog.Debug(fmt.Sprintf("i18n miss: key=%s lang=%s", quote(key, false), quote(target, false)))
		value = key
	}
	if len(args) == 0 {
		return value
	}
	out, err := render(value, args, 0)
	if err != nil {
		slog.Warn(fmt.Sprintf("i18n format failed for key=%s lang=%s kwargs=%s: %v", quote(key, false), quote(target, false), argsText(args), err))
		return value
	}
	return out
}

func (c *Catalog) Translator(ctx context.Context) func(key string, args map[string]string) string {
	return func(key string, args map[string]string) string {
		a := make(Args, len(args))
		for k, v := range args {
			a[k] = v
		}
		return c.T(ctx, key, a)
	}
}

func (c *Catalog) Language(ctx context.Context) string {
	return c.active(ctx, c.homeOrNone(ctx))
}

func (c *Catalog) Resolve(ctx context.Context, value string) (string, bool) {
	h, err := c.homes.Home(ctx)
	if err != nil {
		return "", false
	}
	id, ok, err := c.resolve(h, value)
	if err != nil {
		return "", false
	}
	return id, ok
}

func (c *Catalog) Supported(ctx context.Context) ([]string, error) {
	h, err := c.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := c.supported(h)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), ids...), nil
}

func (c *Catalog) Languages(ctx context.Context) ([]Option, error) {
	h, err := c.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	files, err := c.overlayFiles(h)
	if err != nil {
		return nil, err
	}
	overlay := map[string]bool{}
	for _, f := range files {
		overlay[f.Lang] = true
	}
	surfaces, err := c.surfaces(h)
	if err != nil {
		return nil, err
	}
	ids, err := c.supported(h)
	if err != nil {
		return nil, err
	}
	opts := make([]Option, 0, len(ids))
	for _, id := range ids {
		opts = append(opts, describe(id, c.packInfo(id), overlay[id], surfaces[id]))
	}
	return opts, nil
}

func (c *Catalog) SurfaceCatalog(ctx context.Context, lang string, surface Surface) (string, map[string]string, error) {
	if !surface.Valid() {
		return "", nil, fmt.Errorf("%w %s (expected one of %s)", ErrUnknownSurface, quote(string(surface), false), surfaceList)
	}
	h, err := c.homes.Home(ctx)
	if err != nil {
		return "", nil, err
	}
	id, ok, err := c.resolve(h, lang)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		id = DefaultLanguage
	}
	messages := map[string]string{}
	if surface != Core {
		maps.Copy(messages, loadBundled(id+"."+string(surface)))
	}
	maps.Copy(messages, c.overlayLayer(h, id, surface))
	maps.Copy(messages, c.packLayer(id, surface))
	return id, messages, nil
}

func (c *Catalog) homeOrNone(ctx context.Context) string {
	h, err := c.homes.Home(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("i18n: no home for the user overlay: %v", err))
		return ""
	}
	return h
}

func (c *Catalog) active(ctx context.Context, h string) string {
	pref, err := c.prefer(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read display.language from config: %v", err))
		return DefaultLanguage
	}
	if pref == "" {
		return DefaultLanguage
	}
	return c.normalize(h, pref)
}

func (c *Catalog) normalize(h, value string) string {
	id, ok, err := c.resolve(h, value)
	if err != nil {
		slog.Warn(fmt.Sprintf("i18n: overlay languages of %s unavailable: %v", h, err))
		id, ok, _ = c.resolve("", value)
	}
	if !ok {
		return DefaultLanguage
	}
	return id
}

func (c *Catalog) resolve(h, value string) (string, bool, error) {
	key := NormalizeID(value)
	if key == "" {
		return "", false, nil
	}
	ids, err := c.supported(h)
	if err != nil {
		return "", false, err
	}
	has := func(id string) bool {
		for _, s := range ids {
			if s == id {
				return true
			}
		}
		return false
	}
	if has(key) {
		return key, true, nil
	}
	if alias, ok := languageAliases[key]; ok && has(alias) {
		return alias, true, nil
	}
	base, _, _ := strings.Cut(key, "-")
	if has(base) {
		return base, true, nil
	}
	return "", false, nil
}

func (c *Catalog) supported(h string) ([]string, error) {
	c.mu.Lock()
	if ids, ok := c.views.supported[h]; ok {
		c.mu.Unlock()
		return ids, nil
	}
	gen := c.gen
	c.mu.Unlock()
	files, err := c.overlayFiles(h)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, id := range bundledLanguages {
		set[id] = true
	}
	for _, f := range files {
		set[f.Lang] = true
	}
	c.mu.Lock()
	for _, p := range c.packs {
		set[p.lang] = true
	}
	c.mu.Unlock()
	delete(set, DefaultLanguage)
	rest := make([]string, 0, len(set))
	for id := range set {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	ids := append([]string{DefaultLanguage}, rest...)
	c.mu.Lock()
	if c.gen == gen {
		c.views.supported[h] = ids
	}
	c.mu.Unlock()
	return ids, nil
}

func (c *Catalog) surfaces(h string) (map[string][]Surface, error) {
	files, err := c.overlayFiles(h)
	if err != nil {
		return nil, err
	}
	candidates := map[[2]string]bool{}
	for _, f := range files {
		candidates[[2]string{f.Lang, string(f.Surface)}] = true
	}
	c.mu.Lock()
	for _, p := range c.packs {
		candidates[[2]string{p.lang, string(p.surface)}] = true
	}
	c.mu.Unlock()
	pairs := map[[2]string]bool{}
	for pair := range candidates {
		s := Surface(pair[1])
		if len(c.overlayLayer(h, pair[0], s)) > 0 || len(c.packLayer(pair[0], s)) > 0 {
			pairs[pair] = true
		}
	}
	bundled, err := c.bundledFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range bundled {
		pairs[[2]string{f.Lang, string(f.Surface)}] = true
	}
	found := map[string][]Surface{}
	for _, s := range Surfaces() {
		for pair := range pairs {
			if pair[1] == string(s) {
				found[pair[0]] = append(found[pair[0]], s)
			}
		}
	}
	return found, nil
}

func (c *Catalog) packInfo(lang string) *packInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entries []*Pack
	for _, p := range c.packs {
		if p.lang == lang {
			entries = append(entries, p)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	last := entries[len(entries)-1]
	info := &packInfo{rtl: last.rtl, source: last.source}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].endonym != "" {
			info.endonym, info.rtl = entries[i].endonym, entries[i].rtl
			break
		}
	}
	return info
}

func (c *Catalog) packLayer(lang string, s Surface) map[string]string {
	key := [2]string{lang, string(s)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.views.packs[key]; ok {
		return m
	}
	merged := map[string]string{}
	for _, p := range c.packs {
		if p.lang == lang && p.surface == s {
			maps.Copy(merged, p.messages)
		}
	}
	c.views.packs[key] = merged
	return merged
}

func (c *Catalog) overlayFiles(h string) ([]File, error) {
	if h == "" {
		return nil, nil
	}
	c.mu.Lock()
	if files, ok := c.views.scans[h]; ok {
		c.mu.Unlock()
		return files, nil
	}
	gen := c.gen
	c.mu.Unlock()
	files, err := ScanDir(home.Locales.In(h))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.gen == gen {
		c.views.scans[h] = files
	}
	c.mu.Unlock()
	return files, nil
}

func (c *Catalog) overlayLayer(h, lang string, s Surface) map[string]string {
	if h == "" {
		return map[string]string{}
	}
	key := [3]string{h, lang, string(s)}
	c.mu.Lock()
	if m, ok := c.views.overlay[key]; ok {
		c.mu.Unlock()
		return m
	}
	gen := c.gen
	c.mu.Unlock()
	name := lang + ".yaml"
	if s != Core {
		name = lang + "." + string(s) + ".yaml"
	}
	path := filepath.Join(home.Locales.In(h), name)
	flat := map[string]string{}
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.Mode().IsRegular():
		loc, err := ReadLocale(path)
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to load i18n overlay %s: %v", path, err))
		} else {
			flat = loc.Messages
		}
	case err != nil && !missing(err):
		slog.Warn(fmt.Sprintf("Failed to load i18n overlay %s: %v", path, err))
	}
	c.mu.Lock()
	if c.gen == gen {
		c.views.overlay[key] = flat
	}
	c.mu.Unlock()
	return flat
}

func (c *Catalog) merged(h, lang string) map[string]string {
	key := [2]string{h, lang}
	c.mu.Lock()
	if m, ok := c.views.merged[key]; ok {
		c.mu.Unlock()
		return m
	}
	gen := c.gen
	c.mu.Unlock()
	m := loadBundled(lang)
	maps.Copy(m, c.overlayLayer(h, lang, Core))
	maps.Copy(m, c.packLayer(lang, Core))
	c.mu.Lock()
	if c.gen == gen {
		c.views.merged[key] = m
	}
	c.mu.Unlock()
	return m
}

func (c *Catalog) bundledFiles() ([]File, error) {
	fsys, dir := bundledSource()
	c.mu.Lock()
	if files, ok := c.views.bundled[dir]; ok {
		c.mu.Unlock()
		return files, nil
	}
	gen := c.gen
	c.mu.Unlock()
	files, err := scanFS(fsys, dir)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.gen == gen {
		c.views.bundled[dir] = files
	}
	c.mu.Unlock()
	return files, nil
}

func bundledSource() (fs.FS, string) {
	dir := strings.TrimSpace(os.Getenv(envBundledLocales))
	if dir == "" {
		return locales.FS(), ""
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return os.DirFS(dir), dir
	}
	slog.Warn(fmt.Sprintf("%s points to a non-directory path (%s), using the catalogs built into the binary", envBundledLocales, dir))
	return locales.FS(), ""
}

func loadBundled(stem string) map[string]string {
	fsys, dir := bundledSource()
	name := stem + ".yaml"
	path := name
	if dir != "" {
		path = filepath.Join(dir, name)
	}
	fi, err := fs.Stat(fsys, name)
	if err != nil || !fi.Mode().IsRegular() {
		if err != nil && !missing(err) {
			slog.Warn(fmt.Sprintf("Failed to load i18n catalog %s: %v", path, err))
		} else {
			slog.Debug(fmt.Sprintf("i18n catalog missing for %s at %s", stem, path))
		}
		return map[string]string{}
	}
	data, err := fs.ReadFile(fsys, name)
	if err == nil {
		var loc Locale
		if loc, err = parseLocale(path, data); err == nil {
			return loc.Messages
		}
	}
	slog.Warn(fmt.Sprintf("Failed to load i18n catalog %s: %v", path, err))
	return map[string]string{}
}

func argsText(args Args) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, quote(k, false)+": "+valueOf(args[k]).repr(false))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
