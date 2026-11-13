package main

import (
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
)

// Exit status of the hidden verb the Nix modules run at build and activation time, false when argv names none
func runNixModuleVerb(argv []string, stdout, stderr io.Writer) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	switch argv[0] {
	case "nix-config-render":
		if len(argv) != 2 {
			fmt.Fprintln(stderr, "usage: icebath nix-config-render SETTINGS_JSON")
			return 2, true
		}
		data, err := os.ReadFile(argv[1])
		if err == nil {
			var text string
			if text, err = config.NixRenderSettings(string(data)); err == nil {
				_, err = io.WriteString(stdout, text)
			}
		}
		return nixVerbStatus(err, stderr), true
	case "nix-config-merge":
		if len(argv) != 3 {
			fmt.Fprintln(stderr, "usage: icebath nix-config-merge NIX_JSON CONFIG_YAML")
			return 2, true
		}
		return nixVerbStatus(config.NixMergeConfig(argv[1], argv[2]), stderr), true
	}
	return 0, false
}

// Zero for success, otherwise the error on stderr and status one
func nixVerbStatus(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err)
	return 1
}
