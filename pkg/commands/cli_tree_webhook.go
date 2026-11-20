package commands

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/spf13/cobra"
)

// Binds every icebath webhook path to pkg/gatewaycli
func (c *CLI) bindWebhook() {
	for _, path := range []string{"webhook", "webhook subscribe", "webhook list", "webhook remove", "webhook test"} {
		action := strings.TrimPrefix(strings.TrimPrefix(path, "webhook"), " ")
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runWebhook(cmd, action, args) })
	}
}

// Parses one webhook command line into WebhookArgs and runs it, a nonzero status becoming an ExitError
func runWebhook(cmd *cobra.Command, action string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f := cmd.Flags()
	str := func(name string) string {
		v, _ := f.GetString(name)
		return v
	}
	a := gatewaycli.WebhookArgs{Action: action}
	if len(args) > 0 {
		a.Name = args[0]
	}
	switch action {
	case "subscribe":
		a.Prompt, a.Events, a.Description, a.Skills = str("prompt"), str("events"), str("description"), str("skills")
		a.Deliver, a.DeliverChatID, a.Secret = str("deliver"), str("deliver-chat-id"), str("secret")
		a.Script, a.CronJob = str("script"), str("cron-job")
		if f.Changed("route-profile") {
			profile := str("route-profile")
			a.RouteProfile = &profile
		}
		a.DeliverOnly, _ = f.GetBool("deliver-only")
		a.MirrorToSession, _ = f.GetBool("mirror-to-session")
	case "test":
		a.Payload = str("payload")
	}
	code, err := gatewaycli.WebhookCommand(ctx, cmd.OutOrStdout(), a)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
