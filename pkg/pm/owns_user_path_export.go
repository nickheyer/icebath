package pm

// True when new consoles resolve root, so it may put its commands on the User PATH
func OwnsUserPath(root string) bool { return ownsUserPath(root) }
