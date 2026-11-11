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
