package mcp

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Serializes webhook subscription snapshots and mutations across goroutines and processes
func webhookSubscriptionsLock(h string) (func(), error) { return webhooksubs.Lock(h) }

// One webhook subscription store snapshot as plain maps, unreadable files reading as empty
func loadWebhookSubscriptions(h string) map[string]any {
	return jsonutil.Plain(webhooksubs.LoadUnlocked(h)).(map[string]any)
}

// Locks, rereads, mutates and atomically publishes the webhook subscription store
func mutateWebhookSubscriptions(h string, mutate func(subscriptions map[string]any)) error {
	return webhooksubs.Mutate(h, func(subs *jsonutil.Object) error {
		plain := jsonutil.Plain(subs).(map[string]any)
		mutate(plain)
		webhooksubs.ApplyPlain(subs, plain)
		return nil
	})
}
