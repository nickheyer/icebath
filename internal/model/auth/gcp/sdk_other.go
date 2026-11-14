//go:build !windows

package gcp

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

const sdkCommand = "gcloud"

func sdkConfigDir() (string, error) {
	if dir, ok := os.LookupEnv("CLOUDSDK_CONFIG"); ok {
		return dir, nil
	}
	userHome, err := home.UserHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".config", "gcloud"), nil
}

func computeProductName() bool {
	data, err := readBounded("/sys/class/dmi/id/product_name")
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), "Google")
}
