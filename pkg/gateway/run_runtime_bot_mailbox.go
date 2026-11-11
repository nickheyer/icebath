package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Owner startup Bot Chat recovery, skipped with a warning when this token cannot use the mailbox directory
func RecoverBotMailbox(ctx context.Context, a *SessionAuthority) error {
	err := recoverBotDeliveries(ctx, a)
	var unusable *MailboxUnusable
	if errors.As(err, &unusable) {
		slog.Warn(fmt.Sprintf("Bot Chat recovery skipped for %s: %s", a.DB.Path(), unusable.Error()))
		return nil
	}
	return err
}
