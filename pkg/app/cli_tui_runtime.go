package app

import (
	"os"
)

// Environment value, the fallback when unset
func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
