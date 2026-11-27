package packages

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
)

const indexTimeout = 60 * time.Second

var (
	versionPart = regexp.MustCompile(`\p{Nd}+|[A-Za-z]+`)
	nodeRelease = regexp.MustCompile(`^\p{Nd}+\.\p{Nd}+\.\p{Nd}+$`)
	pbsTag      = regexp.MustCompile(`^[0-9]{8}$`)
)

// Answers of upstream indexes shared within one operation, so a package resolved for nine targets asks once
var indexCache struct {
	sync.Mutex
	responses map[string]any
}

// Runs an operation whose index reads are remembered until it ends
func remembering[T any](operation func() (T, error)) (T, error) {
	indexCache.Lock()
	previous := indexCache.responses
	indexCache.responses = map[string]any{}
	indexCache.Unlock()
	defer func() {
		indexCache.Lock()
		indexCache.responses = previous
		indexCache.Unlock()
	}()
	return operation()
}

// Body of an index address, remembered within the operation; GitHub asked with the token of the profile's environment, which lifts its
// anonymous rate limit
func fetch(cfg *config.Config, rawURL string) ([]byte, error) {
	indexCache.Lock()
	if cached, ok := indexCache.responses[rawURL]; ok {
		indexCache.Unlock()
		return cached.([]byte), nil
	}
	indexCache.Unlock()
	headers := map[string]string{"User-Agent": "icebath-pm"}
	if token := cmp.Or(cfg.Getenv("GH_TOKEN"), cfg.Getenv("GITHUB_TOKEN")); token != "" && strings.HasPrefix(rawURL, "https://api.github.com/") {
		headers["Authorization"] = "Bearer " + token
	}
	body, err := retry(context.Background(), func() ([]byte, error) {
		resp, err := open(context.Background(), rawURL, headers, indexTimeout)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}, retrying{})
	if err != nil {
		return nil, err
	}
	indexCache.Lock()
	if indexCache.responses != nil {
		indexCache.responses[rawURL] = body
	}
	indexCache.Unlock()
	return body, nil
}

// JSON of an index address
func fetchJSON(cfg *config.Config, rawURL string, into any) error {
	body, err := fetch(cfg, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s did not answer with the JSON expected: %w", rawURL, err)
	}
	return nil
}

// One release of a GitHub releases listing
type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Releases of up to pages pages of a GitHub repository, stopping at a short page or when each says so
func releasePages(cfg *config.Config, repo string, pages int, each func(release) bool) error {
	for page := 1; page <= pages; page++ {
		var listed []release
		if err := fetchJSON(cfg, fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=30&page=%d", repo, page), &listed); err != nil {
			return err
		}
		for _, r := range listed {
			if each(r) {
				return nil
			}
		}
		if len(listed) < 30 {
			return nil
		}
	}
	return nil
}

// Newest first version tags of a GitHub repository's releases, a prefix stripped, drafts and prereleases left out
func GitHubTags(cfg *config.Config, repo, strip string) ([]string, error) {
	tags := []string{}
	err := releasePages(cfg, repo, 3, func(r release) bool {
		if r.Draft || r.Prerelease || r.Tag == "latest" {
			return false
		}
		tag := strings.TrimPrefix(r.Tag, strip)
		if first, _ := utf8.DecodeRuneInString(tag); tag != "" && unicode.IsDigit(first) {
			tags = append(tags, tag)
		}
		return false
	})
	return tags, err
}

// Dist tags of an npm package on the registry the user set
func npmDistTags(cfg *config.Config, name string) (map[string]string, error) {
	tags := map[string]string{}
	err := fetchJSON(cfg, npmRegistry()+"-/package/"+name+"/dist-tags", &tags)
	return tags, err
}

// Newest first Node versions of nodejs.org's index
func nodeVersions(cfg *config.Config) ([]string, error) {
	var index []struct {
		Version string `json:"version"`
	}
	if err := fetchJSON(cfg, "https://nodejs.org/dist/index.json", &index); err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range index {
		if v := strings.TrimPrefix(entry.Version, "v"); nodeRelease.MatchString(v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// The interpreter of the newest python-build-standalone release that builds the minor for the triple, as release+tag
func standaloneVersions(cfg *config.Config, minor, triple string) ([]string, error) {
	var found []string
	err := releasePages(cfg, "astral-sh/python-build-standalone", 2, func(r release) bool {
		if r.Draft || r.Prerelease || !pbsTag.MatchString(r.Tag) {
			return false
		}
		pattern := regexp.MustCompile(`^cpython-(` + regexp.QuoteMeta(minor) + `\.[0-9]+\+` + r.Tag + `)-` + regexp.QuoteMeta(triple) + `-install_only\.tar\.gz$`)
		for _, asset := range r.Assets {
			if m := pattern.FindStringSubmatch(asset.Name); m != nil {
				found = []string{m[1]}
				return true
			}
		}
		return false
	})
	return found, err
}

// One numeric or alphabetic segment of a version's sort key
type keyPart struct {
	alpha bool
	num   *big.Int
	text  string
}

// Sort key of a version, numbers before words so a prerelease never beats its release
func versionKey(version string) []keyPart {
	var key []keyPart
	for _, chunk := range strings.Split(strings.ReplaceAll(version, "-", "."), ".") {
		for _, m := range versionPart.FindAllString(chunk, -1) {
			if r, _ := utf8.DecodeRuneInString(m); unicode.Is(unicode.Nd, r) {
				n, _ := new(big.Int).SetString(m, 10)
				if n == nil {
					n = new(big.Int)
				}
				key = append(key, keyPart{num: n})
			} else {
				key = append(key, keyPart{alpha: true, text: m})
			}
		}
	}
	return key
}

// Order of two versions by their sort keys
func compareVersions(a, b string) int {
	ka, kb := versionKey(a), versionKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		x, y := ka[i], kb[i]
		switch {
		case x.alpha != y.alpha && !x.alpha:
			return -1
		case x.alpha != y.alpha:
			return 1
		case x.alpha:
			if c := strings.Compare(x.text, y.text); c != 0 {
				return c
			}
		default:
			if c := x.num.Cmp(y.num); c != 0 {
				return c
			}
		}
	}
	return len(ka) - len(kb)
}

// Major and minor of a version, false when it has fewer than two numbers
func minorOf(version string) (string, bool) {
	var nums []string
	for _, m := range versionPart.FindAllString(version, -1) {
		if r, _ := utf8.DecodeRuneInString(m); unicode.Is(unicode.Nd, r) {
			n, _ := new(big.Int).SetString(m, 10)
			nums = append(nums, n.String())
		}
	}
	if len(nums) < 2 {
		return "", false
	}
	return nums[0] + "." + nums[1], true
}

// What pm update decided for one package
type decision struct {
	name   string
	locked *string
	style  string
	// Version to pin, nil when none
	version *string
	// Version each target takes
	perTarget []targetVersion
	reason    string
	// Targets whose archives of the same minor moved, with their addresses
	artifacts map[string][]string
}

type targetVersion struct{ target, version string }

// Whether the decision moves the pins
func (d *decision) changed() bool {
	switch {
	case d.version == nil:
		return false
	case d.locked == nil:
		return true
	case d.style == "minor":
		lockedMinor, _ := minorOf(*d.locked)
		versionMinor, _ := minorOf(*d.version)
		return len(d.artifacts) > 0 || lockedMinor != versionMinor
	}
	return compareVersions(*d.version, *d.locked) > 0
}

// Intersects each target's candidates into one decision, a target without candidates left out
func resolveBest(name string, candidates []targetCandidates, locked *string, style string) *decision {
	d := &decision{name: name, locked: locked, style: style, artifacts: map[string][]string{}}
	var present []targetCandidates
	for _, c := range candidates {
		if len(c.versions) > 0 {
			present = append(present, c)
		}
	}
	if len(present) == 0 {
		d.reason = "no source"
		return d
	}
	if style == "minor" {
		var common map[string]bool
		for _, c := range present {
			set := map[string]bool{}
			for _, v := range c.versions {
				if m, ok := minorOf(v); ok {
					set[m] = true
				}
			}
			if common == nil {
				common = set
				continue
			}
			maps.DeleteFunc(common, func(m string, _ bool) bool { return !set[m] })
		}
		if len(common) == 0 {
			d.reason = "no shared minor"
			return d
		}
		shared := slices.MaxFunc(slices.Collect(maps.Keys(common)), compareVersions)
		for _, c := range present {
			best := ""
			for _, v := range c.versions {
				if m, _ := minorOf(v); m == shared && (best == "" || compareVersions(v, best) > 0) {
					best = v
				}
			}
			d.perTarget = append(d.perTarget, targetVersion{c.target, best})
		}
		d.version = &shared
		return d
	}
	common := map[string]bool{}
	for _, v := range present[0].versions {
		common[v] = true
	}
	for _, c := range present[1:] {
		maps.DeleteFunc(common, func(v string, _ bool) bool { return !slices.Contains(c.versions, v) })
	}
	if len(common) == 0 {
		d.reason = "no shared version"
		return d
	}
	best := slices.MaxFunc(slices.Collect(maps.Keys(common)), compareVersions)
	d.version = &best
	for _, c := range present {
		d.perTarget = append(d.perTarget, targetVersion{c.target, best})
	}
	return d
}

type targetCandidates struct {
	target   string
	versions []string
}

// Latest versions of a package for its targets, and within a shared minor whether a target's archives moved
func resolvePackage(cfg *config.Config, p *Package, targets []string, locked *string, pinned map[string]Archives) (*decision, error) {
	return remembering(func() (*decision, error) {
		lockedText := ""
		if locked != nil {
			lockedText = *locked
		}
		var candidates []targetCandidates
		for _, t := range targets {
			var versions []string
			if p.Latest != nil {
				var err error
				if versions, err = p.Latest(cfg, t, lockedText); err != nil {
					return nil, err
				}
			}
			candidates = append(candidates, targetCandidates{t, versions})
		}
		d := resolveBest(p.Name, candidates, locked, p.style())
		if d.style == "minor" && d.version != nil {
			for _, tv := range d.perTarget {
				urls, err := p.urls(tv.version, tv.target)
				if err != nil {
					return nil, err
				}
				current := pinned[tv.target]
				if len(current) == 0 {
					current = pinned["any"]
				}
				var existing []string
				for _, a := range current {
					existing = append(existing, a.URL)
				}
				if !slices.Equal(urls, existing) {
					d.artifacts[tv.target] = urls
				}
			}
			if locked != nil {
				if lockedMinor, _ := minorOf(*locked); lockedMinor == *d.version {
					d.version = locked
				}
			}
		}
		return d, nil
	})
}

// Archives of a decision, targets it did not resolve kept and digests reused for addresses that did not change
func pinArtifacts(ctx context.Context, p *Package, d *decision, current map[string]Archives) (map[string]Archives, error) {
	return remembering(func() (map[string]Archives, error) {
		perTarget := d.perTarget
		if len(perTarget) == 0 {
			for _, t := range Targets {
				perTarget = append(perTarget, targetVersion{t, *d.version})
			}
		}
		artifacts := maps.Clone(current)
		if artifacts == nil {
			artifacts = map[string]Archives{}
		}
		hashes := map[string]string{}
		for _, tv := range perTarget {
			if p.Missing(tv.target) != "" {
				continue
			}
			old := current[tv.target]
			if len(old) == 0 {
				old = current["any"]
			}
			known := map[string]string{}
			var existing []string
			for _, a := range old {
				known[a.URL] = a.SHA256
				existing = append(existing, a.URL)
			}
			urls, moved := d.artifacts[tv.target]
			if !moved {
				var err error
				if urls, err = p.urls(tv.version, tv.target); err != nil {
					return nil, err
				}
			}
			if slices.Equal(urls, existing) {
				continue
			}
			var pinned Archives
			for _, u := range urls {
				digest := cmp.Or(known[u], hashes[u])
				if digest == "" {
					var err error
					if p.Digest != nil {
						if digest, err = p.Digest(tv.version, u); err != nil {
							return nil, err
						}
					}
					if digest == "" {
						if digest, err = hashURL(ctx, u); err != nil {
							return nil, err
						}
					}
					hashes[u] = digest
				}
				pinned = append(pinned, Artifact{URL: u, SHA256: digest})
			}
			artifacts[tv.target] = pinned
		}
		return artifacts, nil
	})
}

// Archives of a version for every target it builds for, each hashed or taken from upstream's digest, the same for all becoming "any"
func (h *Packages) pinVersion(ctx context.Context, p *Package, version string, r Reporter) (map[string]Archives, error) {
	type planned struct {
		target string
		urls   []string
	}
	var plan []planned
	distinct := map[string]bool{}
	for _, target := range Targets {
		if p.Missing(target) != "" {
			continue
		}
		urls, err := p.urls(version, target)
		if err != nil {
			return nil, err
		}
		plan = append(plan, planned{target, urls})
		distinct[strings.Join(urls, "\x00")] = true
	}
	pin := func(urls []string) (Archives, error) {
		var out Archives
		for _, u := range urls {
			r.Line("    " + u)
			digest := ""
			var err error
			if p.Digest != nil {
				if digest, err = p.Digest(version, u); err != nil {
					return nil, err
				}
			}
			if digest == "" {
				if digest, err = hashURL(ctx, u); err != nil {
					return nil, err
				}
			}
			r.Line("      sha256 " + digest)
			out = append(out, Artifact{URL: u, SHA256: digest})
		}
		return out, nil
	}
	artifacts := map[string]Archives{}
	if len(plan) > 0 && len(distinct) == 1 {
		r.Line("  any:")
		pinned, err := pin(plan[0].urls)
		if err != nil {
			return nil, err
		}
		artifacts["any"] = pinned
		return artifacts, nil
	}
	for _, step := range plan {
		r.Line("  " + step.target + ":")
		pinned, err := pin(step.urls)
		if err != nil {
			return nil, err
		}
		artifacts[step.target] = pinned
	}
	return artifacts, nil
}

// An archive of the Termux pool as its index lists it now
type poolPackage struct {
	name, version, url, sha256 string
}

// Every package the Termux pool serves now, by name
func termuxIndex(cfg *config.Config) (map[string]poolPackage, error) {
	body, err := fetch(cfg, TermuxPool+"dists/stable/main/binary-aarch64/Packages")
	if err != nil {
		return nil, err
	}
	pool := map[string]poolPackage{}
	fields := map[string]string{}
	flush := func() {
		if fields["Package"] != "" && fields["Filename"] != "" && fields["SHA256"] != "" {
			pool[fields["Package"]] = poolPackage{name: fields["Package"], version: fields["Version"], url: TermuxPool + fields["Filename"], sha256: fields["SHA256"]}
		}
		fields = map[string]string{}
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if first, _ := utf8.DecodeRuneInString(line); unicode.IsSpace(first) {
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	flush()
	return pool, nil
}

// A Termux pin whose archive the pool retired, with what the pool serves in its place, nil when it carries the package no more
type retiredPin struct {
	name, target, version, url string
	replacement                *poolPackage
}

// Pins of the lock pointing at Termux pool archives the pool no longer serves
func retiredPins(l *lockfile, pool map[string]poolPackage) []retiredPin {
	var out []retiredPin
	for _, name := range l.names() {
		pin := l.pins[name]
		for _, target := range slices.Sorted(maps.Keys(pin.Artifacts)) {
			for _, a := range pin.Artifacts[target] {
				pkg := termuxPackage(a.URL)
				if pkg == "" {
					continue
				}
				entry, ok := pool[pkg]
				switch {
				case !ok:
					out = append(out, retiredPin{name: name, target: target, version: pin.Version, url: a.URL})
				case path.Base(entry.url) != path.Base(a.URL):
					e := entry
					out = append(out, retiredPin{name: name, target: target, version: pin.Version, url: a.URL, replacement: &e})
				}
			}
		}
	}
	return out
}
