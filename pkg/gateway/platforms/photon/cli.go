package photon

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Allowlists the operator number and makes it the cron home channel, each key only when unset
func autoconfigureAccess(ctx context.Context, phone string, stdout, stderr io.Writer) {
	h := home.Dir(ctx)
	for _, row := range [][2]string{{"PHOTON_ALLOWED_USERS", "allowlisted your number"},
		{"PHOTON_HOME_CHANNEL", "set your DM as the cron home channel"}} {
		key, label := row[0], row[1]
		current, _, err := config.GetEnvValue(ctx, h, key)
		if err == nil && current != "" {
			fmt.Fprintf(stdout, "      %s already set — leaving it as-is.\n", key)
			continue
		}
		if err == nil {
			err = config.SaveEnvValueContext(ctx, h, key, phone)
		}
		if err != nil {
			fmt.Fprintf(stderr, "      could not set %s: %v\n", key, err)
			continue
		}
		fmt.Fprintf(stdout, "  ✓ %s (%s)\n", label, key)
	}
}
