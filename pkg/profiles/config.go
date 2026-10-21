package profiles

import (
	"errors"
	"os"
	"path/filepath"
)

// Environment variable naming the directory every icebath configuration file lives in
const ConfigDirEnv = "ICEBATH_CONFIG_DIR"

// Directory holding icebath configuration, the environment override, else icebath under the user configuration directory
func ConfigDir() (string, error) {
	if dir := os.Getenv(ConfigDirEnv); dir != "" {
		return filepath.Abs(dir)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "icebath"), nil
}

// Path of the user profile list inside the configuration directory
func UserPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles.json"), nil
}

// User file in any shape Read accepts, empty when the file does not exist
func ReadUser() (Document, error) {
	path, err := UserPath()
	if err != nil {
		return Document{}, err
	}
	f, err := ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Document{}, nil
	}
	return f, err
}

// Profiles from the user file, none when the file does not exist
func LoadUser() ([]Profile, error) {
	f, err := ReadUser()
	return f.Profiles, err
}
