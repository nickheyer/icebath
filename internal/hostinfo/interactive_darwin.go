package hostinfo

// Reports true, since every macOS process reaches the user session of the account it runs as
func InteractiveSession() bool { return true }
