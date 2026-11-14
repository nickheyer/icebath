package toolchain

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

//go:embed lock.json
var embeddedLock []byte

// Version of the lock document layout this build reads
const lockSchema = 1

// Grammar of a pinned digest, sixty four lower case hex digits
var fullSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

// One pinned download, Size zero when the pin records none
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// File name of the download, the last element of its address path
func (a Artifact) fileName() string {
	u, err := url.Parse(a.URL)
	if err != nil {
		return ""
	}
	return u.Path[strings.LastIndexByte(u.Path, '/')+1:]
}

// Artifacts of one target key, written as one object or a list of them
type artifactSet []Artifact

// Reads a single artifact object or a list of them
func (s *artifactSet) UnmarshalJSON(data []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		var list []Artifact
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*s = list
		return nil
	}
	var one Artifact
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*s = artifactSet{one}
	return nil
}

// Pinned version of one entry with its artifacts by target key
type lockedPackage struct {
	Version   string                 `json:"version"`
	Artifacts map[string]artifactSet `json:"artifacts"`
}

// Public content addressed copy of every pinned download, tried after its own address
type mirrorLayout struct {
	Origin string `json:"origin"`
	Prefix string `json:"prefix"`
}

// Lock document as stored
type lockDocument struct {
	Schema   int                      `json:"schema"`
	Mirror   mirrorLayout             `json:"mirror"`
	Packages map[string]lockedPackage `json:"packages"`
}

// Validated pins of a catalog
type lock struct {
	mirror   mirrorLayout
	packages map[string]lockedPackage
}

// Target key of an artifact row split into its target and the glibc release it needs
func splitTargetKey(key string) (string, [2]int, bool, error) {
	name, floor, floored := strings.Cut(key, "@")
	if !floored {
		return name, [2]int{}, false, nil
	}
	release, ok := strings.CutPrefix(floor, "glibc")
	parsed, valid := parseRelease(release)
	if !ok || !valid {
		return "", [2]int{}, false, fmt.Errorf("target key %q has a floor that is not glibc and a release", key)
	}
	return name, parsed, true, nil
}

// Parses and validates a lock document, refusing anything an install could not trust
func parseLock(data []byte) (*lock, error) {
	var doc lockDocument
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(data), "\xef\xbb\xbf")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("toolchain lock: %w", err)
	}
	if doc.Schema != lockSchema {
		return nil, fmt.Errorf("toolchain lock: schema %d is not the supported schema %d", doc.Schema, lockSchema)
	}
	if doc.Mirror.Origin != "" || doc.Mirror.Prefix != "" {
		if !secureURL(doc.Mirror.Origin) || !strings.HasSuffix(doc.Mirror.Prefix, "/") {
			return nil, errors.New("toolchain lock: the mirror needs an https origin and a prefix ending in a slash")
		}
	}
	for name, pkg := range doc.Packages {
		if name == "" || pkg.Version == "" {
			return nil, fmt.Errorf("toolchain lock: package %q has no name or version", name)
		}
		if len(pkg.Artifacts) == 0 {
			return nil, fmt.Errorf("toolchain lock: package %s pins no artifact", name)
		}
		for key, set := range pkg.Artifacts {
			target, _, _, err := splitTargetKey(key)
			if err != nil {
				return nil, fmt.Errorf("toolchain lock: package %s: %w", name, err)
			}
			if target != anyTarget && !Target(target).known() {
				return nil, fmt.Errorf("toolchain lock: package %s names the unknown target %q", name, key)
			}
			if len(set) == 0 {
				return nil, fmt.Errorf("toolchain lock: package %s pins nothing for %s", name, key)
			}
			for _, a := range set {
				if !secureURL(a.URL) || a.fileName() == "" {
					return nil, fmt.Errorf("toolchain lock: package %s target %s has the unusable address %q", name, key, a.URL)
				}
				if !fullSHA256.MatchString(a.SHA256) {
					return nil, fmt.Errorf("toolchain lock: package %s target %s lacks a full lower case sha256 for %s", name, key, a.URL)
				}
				if a.Size < 0 {
					return nil, fmt.Errorf("toolchain lock: package %s target %s has a negative size", name, key)
				}
			}
		}
	}
	return &lock{mirror: doc.Mirror, packages: doc.Packages}, nil
}

// Reports an https address, or a plain http one on the loopback interface
func secureURL(raw string) bool {
	if strings.HasPrefix(raw, "https://") {
		return true
	}
	return loopbackURL(raw)
}

// Reports a plain http address on the loopback interface
func loopbackURL(raw string) bool {
	for _, prefix := range []string{"http://127.0.0.1:", "http://localhost:", "http://[::1]:"} {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}
	return false
}

// Host name of an address, empty when it does not parse
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Release the lock pins for name, false when the lock does not hold it
func (l *lock) version(name string) (string, bool) {
	pkg, ok := l.packages[name]
	return pkg.Version, ok
}

// Artifacts of name for the host in extraction order, the highest glibc floor the host meets winning
func (l *lock) artifacts(name string, host hostTarget) []Artifact {
	pkg := l.packages[name]
	var best artifactSet
	bestFloor, found := [2]int{-1, -1}, false
	for key, set := range pkg.Artifacts {
		target, floor, floored, err := splitTargetKey(key)
		if err != nil || target != string(host.target) {
			continue
		}
		if floored && (!host.hasLib || !releaseAtLeast(host.glibc, floor)) {
			continue
		}
		if !floored {
			floor = [2]int{0, 0}
		}
		if !found || !releaseAtLeast(bestFloor, floor) {
			best, bestFloor, found = set, floor, true
		}
	}
	if !found {
		best = pkg.Artifacts[anyTarget]
	}
	return append([]Artifact(nil), best...)
}

// Lowest glibc release any row of name asks of target, false when no row has a floor
func (l *lock) glibcFloor(name string, target Target) ([2]int, bool) {
	var lowest [2]int
	found := false
	for key := range l.packages[name].Artifacts {
		t, floor, floored, err := splitTargetKey(key)
		if err != nil || !floored || t != string(target) {
			continue
		}
		if !found || !releaseAtLeast(floor, lowest) {
			lowest, found = floor, true
		}
	}
	return lowest, found
}

// Reports whether name has a row for target under any glibc floor, or one for every target
func (l *lock) pinsTarget(name string, target Target) bool {
	for key := range l.packages[name].Artifacts {
		if t, _, _, err := splitTargetKey(key); err == nil && (t == string(target) || t == anyTarget) {
			return true
		}
	}
	return false
}

// Names of every pinned package in sorted order
func (l *lock) names() []string {
	names := make([]string, 0, len(l.packages))
	for name := range l.packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Mirror address of a pinned digest, empty when the lock names no mirror
func (l *lock) mirrorURL(sha string) string {
	if l.mirror.Origin == "" {
		return ""
	}
	return l.mirror.Origin + "/" + l.mirror.Prefix + sha
}
