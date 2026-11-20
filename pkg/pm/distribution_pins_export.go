//go:build !icebath_core && !icebath_noextras

package pm

import (
	"os"
	"path/filepath"
	"strings"
)

// Installed distribution as importlib metadata reports its Name and Version headers, nil when a header is absent
type DistributionPin struct {
	Name, Version *string
}

// Name and version headers of every distribution in a generation's site packages in directory order
func DistributionPins(environment string) ([]DistributionPin, error) {
	site, err := SitePackages(environment)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(site)
	if err != nil {
		return []DistributionPin{}, nil
	}
	out := []DistributionPin{}
	for _, entry := range entries {
		low := strings.ToLower(entry.Name())
		if !strings.HasSuffix(low, ".dist-info") && !strings.HasSuffix(low, ".egg-info") {
			continue
		}
		path := filepath.Join(site, entry.Name())
		text := distributionText(filepath.Join(path, "METADATA"))
		if text == "" {
			text = distributionText(filepath.Join(path, "PKG-INFO"))
		}
		if text == "" {
			text = distributionText(path)
		}
		out = append(out, DistributionPin{Name: metadataHeader(text, "Name"), Version: metadataHeader(text, "Version")})
	}
	return out, nil
}

// Header value of an RFC 822 metadata text, matched case insensitively, nil when absent
func metadataHeader(text, name string) *string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found || line[0] == ' ' || line[0] == '\t' || !strings.EqualFold(key, name) {
			continue
		}
		value = strings.TrimLeft(value, " \t")
		for _, more := range lines[i+1:] {
			if more == "" || (more[0] != ' ' && more[0] != '\t') {
				break
			}
			value += "\n" + more
		}
		return &value
	}
	return nil
}

// Readable text of a file the way importlib metadata reads it, empty when absent
func distributionText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
