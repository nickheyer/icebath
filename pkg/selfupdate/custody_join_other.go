//go:build !windows

package selfupdate

// No update job exists outside Windows, so the join launcher refuses every command
func newJoinAPI() joinAPI { return nil }
