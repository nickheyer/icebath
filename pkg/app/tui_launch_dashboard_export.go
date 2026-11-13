package app

import "os"

// Command line that runs the terminal UI of this binary, for the dashboard chat tab
func TUIArgv() ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{self, "--tui"}, nil
}
