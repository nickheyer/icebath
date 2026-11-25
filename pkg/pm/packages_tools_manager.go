//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"
	"strings"
)

// Release tags like v2.53.0.windows.3 become lock versions like 2.53.0+3
func (p *Git) LatestVersions(target, locked string) ([]string, error) {
	tags, err := GithubReleaseTags("git-for-windows/git", "v")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tag := range tags {
		if base, build, found := strings.Cut(tag, ".windows."); found {
			out = append(out, base+"+"+build)
		}
	}
	return out, nil
}

func (p *Gh) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("cli/cli", "v")
}

// Resolves the exact advertised asset for the version and target, refusing to relabel other bytes
func (p *Ffmpeg) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		return fmt.Sprintf("https://packages.termux.dev/apt/termux-main/pool/main/f/ffmpeg/ffmpeg_%s_aarch64.deb", version), nil
	}
	if MuslTargets[target] {
		return "", NewInstallError(p.Name, fmt.Sprintf("unavailable on %s: %s", target, noneStr(p.dispatch().MissingReason(target))), "")
	}
	osname, arch, err := unpackTwo(target, "-", "target")
	if err != nil {
		return "", err
	}
	if osname == "win32" || osname == "linux" {
		index, err := BtbnIndex()
		if err != nil {
			return "", err
		}
		if artifact, ok := index[target][version]; ok {
			return fmt.Sprintf("https://github.com/BtbN/FFmpeg-Builds/releases/download/%s/%s", artifact[0], artifact[1]), nil
		}
	} else if epoch, ok := MartinRiedlIndex()[target][version]; ok {
		sourceArch := arch
		if arch == "x64" {
			sourceArch = "amd64"
		}
		return fmt.Sprintf("https://ffmpeg.martin-riedl.de/download/macos/%s/%s_%s/ffmpeg.zip", sourceArch, epoch, version), nil
	}
	return "", NewInstallError(p.Name, fmt.Sprintf("no advertised %s artifact for %s", version, target),
		"retry when the upstream index is available, or keep the existing pin")
}

func (p *Ffmpeg) LatestVersions(target, locked string) ([]string, error) {
	if MuslTargets[target] {
		return nil, nil
	}
	switch target {
	case "win32-x64", "win32-arm64", "linux-x64", "linux-arm64":
		return BtbnVersions(target)
	}
	return MartinRiedlVersions(target), nil
}

func (p *Ripgrep) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("BurntSushi/ripgrep", "")
}

func (p *CuaDriver) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("trycua/cua", "cua-driver-rs-v")
}
