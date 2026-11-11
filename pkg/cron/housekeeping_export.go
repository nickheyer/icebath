package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Sends the queued worker results of the ambient home through the live adapters, returning how many went out
func DrainDeliveryQueue(ctx context.Context, adapters, loop any) (int, error) {
	return cronStoreFrom(ctx).drainDeliveryQueue(ctx, adapters, loop)
}

// Fires jobs whose external fire never arrived once the misfire grace passed, returning how many dispatched
func FireOverdueJobs(ctx context.Context, p CronScheduler, adapters, loop any) (int, error) {
	return fireOverdueJobs(ctx, p, FireOptions{Adapters: adapters, Loop: loop}, nil)
}

// Enabled primary gateway profile routes that target the served profile, none for the primary home
func PrimaryProfileRoutesForCurrentHome(ctx context.Context) []*gateway.ProfileRoute {
	return primaryProfileRoutesForCurrentHome(ctx)
}

// Read only adapter map of a credentialless satellite over the primary's adapters and its routes
func NewSharedRouteAdapters(adapters any, routes []*gateway.ProfileRoute) *SharedRouteAdapters {
	return newSharedRouteAdapters(adapters, routes)
}
