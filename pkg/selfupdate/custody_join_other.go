//go:build !windows && !icebath_core && !icebath_noextras

package selfupdate

// No update job exists outside Windows, so the join launcher refuses every command
func newJoinAPI() joinAPI { return nil }
