package updaterecord

import (
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
)

// ProfileHome pairs a profile name with its home directory
type ProfileHome struct {
	Profile string
	Home    string
}

// Default home plus every valid named profile dir in name order, an error when the profiles root is unreadable
func ProfileHomes() ([]ProfileHome, error) {
	var homes []ProfileHome
	defaultHome := home.Root()
	if st, err := os.Stat(defaultHome); err == nil && st.IsDir() {
		homes = append(homes, ProfileHome{Profile: "default", Home: defaultHome})
	}
	root := filepath.Join(defaultHome, home.ProfilesDir)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return homes, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if st, err := os.Stat(path); err == nil && st.IsDir() && e.Name() != "default" && home.ProfileIDRe.MatchString(e.Name()) {
			homes = append(homes, ProfileHome{Profile: e.Name(), Home: path})
		}
	}
	return homes, nil
}
