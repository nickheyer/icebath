package config

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
)

// Python strftime of a time that renders the zone name with repaired surrogates instead of raising
func SafeStrftime(t time.Time, format string) string { return pydatetime.Strftime(t, format) }
