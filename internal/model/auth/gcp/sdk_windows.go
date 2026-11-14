package gcp

import (
	"os"
	"path/filepath"
)

const sdkCommand = "gcloud.cmd"

func sdkConfigDir() (string, error) {
	if dir, ok := os.LookupEnv("CLOUDSDK_CONFIG"); ok {
		return dir, nil
	}
	if appData, ok := os.LookupEnv("APPDATA"); ok {
		return filepath.Join(appData, "gcloud"), nil
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	return filepath.Join(drive+`\`, "gcloud"), nil
}

func computeProductName() bool { return false }
