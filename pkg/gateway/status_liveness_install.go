package gateway

import "context"

// Installs the multiplexer liveness answer the profile listing and the cron liveness check ask for
func init() {
	NamedProfileServedByRunningMultiplexer = func(profileName string) bool {
		served, err := namedProfileServedByRunningMultiplexer(context.Background(), &profileName)
		if err != nil {
			panic(err)
		}
		return served
	}
}
