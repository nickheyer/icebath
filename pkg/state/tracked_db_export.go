package state

// Opens a connection tracked for its descriptor life through the patchable session store connector
func ConnectTrackedDB(path string, opts ...TrackedOption) (*TrackedConn, error) {
	return connectTrackedDB(path, opts...)
}
