package config

const (
	Key = "profiles"
	// Profile in use
	Active = Key + ".active"
	// Variable that picks the profile for one run, over the one in use
	ActiveEnv = "ICEBATH_PROFILE"
)
