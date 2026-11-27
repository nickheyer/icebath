// What speech needs on this machine: ffmpeg, which converts, trims, splits and joins audio, BtbN's builds on Windows and Linux,
// martin-riedl's on macOS and the Termux package on Android; and pilk, which reads the SILK voice notes of QQ and WeChat
package packages

import (
	_ "embed"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
)

// Names of what speech declares
const (
	FFmpeg = "ffmpeg"
	Silk   = "silk"
)

var (
	//go:embed lock.json
	lock []byte
	//go:embed extras.json
	extras []byte
)

// Declares them, the build indexes read with the settings of the profile in use, whose GitHub token lifts the anonymous rate limit
func Register(host *packages.Packages, cfg *config.Config) {
	host.Register(packages.Declared{Tools: []packages.Package{ffmpeg(packages.Pins(lock)[FFmpeg], cfg)}, Sets: packages.Extras(extras)})
}

var (
	martinRiedlBuild = regexp.MustCompile(`/download/(macos|linux)/(amd64|arm64)/(\p{Nd}+)_(\p{Nd}+\.\p{Nd}+\.\p{Nd}+)/ffmpeg\.zip`)
	btbnTag          = regexp.MustCompile(`^autobuild-(\p{Nd}{4})-(\p{Nd}{2})-\p{Nd}{2}-\p{Nd}{2}-\p{Nd}{2}$`)
	btbnAsset        = regexp.MustCompile(`^ffmpeg-n(\p{Nd}+\.\p{Nd}+\.\p{Nd}+)-.+-(win|linux)(64|arm64)-gpl-\p{Nd}+\.\p{Nd}+\.(?:zip|tar\.xz)$`)
)

func ffmpeg(pin packages.Pin, cfg *config.Config) packages.Package {
	gaps := map[string]string{}
	for target := range packages.Musl {
		gaps[target] = "BtbN Linux builds link glibc dynamically"
	}
	return packages.Package{
		Name: FFmpeg, Pin: pin, Gaps: gaps, Style: "minor",
		Binary: map[string]string{"win32": "bin/ffmpeg.exe", "linux-x64": "bin/ffmpeg", "linux-arm64": "bin/ffmpeg", "posix": "ffmpeg"},
		Probe:  []string{"-version"},
		Deb:    &packages.Deb{Main: "bin/ffmpeg"},
		// The exact asset the build index advertises for the version, never other bytes relabelled
		URLs: func(version, target string) ([]string, error) {
			switch {
			case target == packages.Bionic:
				return []string{fmt.Sprintf("https://packages.termux.dev/apt/termux-main/pool/main/f/ffmpeg/ffmpeg_%s_aarch64.deb", version)}, nil
			case packages.Musl[target]:
				return nil, packages.Failure(FFmpeg, "unavailable on "+target+": "+gaps[target], "")
			}
			index := martinRiedl(cfg)
			if !strings.HasPrefix(target, "darwin") {
				var err error
				if index, err = btbn(cfg); err != nil {
					return nil, err
				}
			}
			if url := index.where[target][version]; url != "" {
				return []string{url}, nil
			}
			return nil, packages.Failure(FFmpeg, fmt.Sprintf("no advertised %s artifact for %s", version, target),
				"retry when the upstream index is available, or keep the existing pin")
		},
		Latest: func(cfg *config.Config, target, _ string) ([]string, error) {
			switch {
			case packages.Musl[target]:
				return nil, nil
			case target == packages.Bionic:
				return nil, nil
			case strings.HasPrefix(target, "win32"), strings.HasPrefix(target, "linux"):
				builds, err := btbn(cfg)
				return builds.order[target], err
			}
			return martinRiedl(cfg).order[target], nil
		},
	}
}

// Builds an index lists for each target: their versions in the order it gives them, and where each comes from
type builds struct {
	order map[string][]string
	where map[string]map[string]string
}

func (b *builds) add(target, version, url string, replace bool) {
	if b.where[target] == nil {
		b.where[target] = map[string]string{}
	}
	_, seen := b.where[target][version]
	if !seen {
		b.order[target] = append(b.order[target], version)
	}
	if !seen || replace {
		b.where[target][version] = url
	}
}

// martin-riedl's macOS builds as its download page links them, the newest epoch of each version winning; an unreachable page reads as
// none
func martinRiedl(cfg *config.Config) builds {
	out := builds{order: map[string][]string{}, where: map[string]map[string]string{}}
	page, err := packages.Fetch(cfg, "https://ffmpeg.martin-riedl.de/")
	if err != nil {
		return out
	}
	epochs := map[string]*big.Int{}
	for _, m := range martinRiedlBuild.FindAllStringSubmatch(string(page), -1) {
		system, arch, epoch, version := m[1], m[2], m[3], m[4]
		target := "linux-"
		if system == "macos" {
			target = "darwin-"
		}
		if arch == "amd64" {
			target += "x64"
		} else {
			target += "arm64"
		}
		value, _ := new(big.Int).SetString(epoch, 10)
		key := target + "\x00" + version
		url := fmt.Sprintf("https://ffmpeg.martin-riedl.de/download/%s/%s/%s_%s/ffmpeg.zip", system, arch, epoch, version)
		newer := epochs[key] == nil || value.Cmp(epochs[key]) > 0
		out.add(target, version, url, newer)
		if newer {
			epochs[key] = value
		}
	}
	return out
}

// BtbN's static GPL builds, the first build of each month before the newest one, which BtbN keeps for good
func btbn(cfg *config.Config) (builds, error) {
	out := builds{order: map[string][]string{}, where: map[string]map[string]string{}}
	releases, err := packages.GitHubReleases(cfg, "BtbN/FFmpeg-Builds", 3)
	if err != nil {
		return out, err
	}
	slices.SortStableFunc(releases, func(a, b packages.Release) int { return strings.Compare(b.Tag, a.Tag) })
	newest, seen := "", map[string]bool{}
	for _, release := range releases {
		dated := btbnTag.FindStringSubmatch(release.Tag)
		if dated == nil {
			continue
		}
		month := dated[1] + "-" + dated[2]
		if newest == "" {
			newest = month
		}
		if month == newest || seen[month] {
			continue
		}
		seen[month] = true
		for _, asset := range release.Assets {
			m := btbnAsset.FindStringSubmatch(asset)
			if m == nil {
				continue
			}
			version, system, arch := m[1], m[2], m[3]
			target := "linux-"
			if system == "win" {
				target = "win32-"
			}
			if arch == "64" {
				target += "x64"
			} else {
				target += "arm64"
			}
			out.add(target, version, "https://github.com/BtbN/FFmpeg-Builds/releases/download/"+release.Tag+"/"+asset, false)
		}
	}
	return out, nil
}
