package config

const (
	Key = "profiles"
	// Profile in use, and the one icebath starts with
	Active = Key + ".active"
	// Variable that picks the profile for one run, over the one in use; -p sets it, and every process icebath starts carries it
	ActiveEnv = "ICEBATH_PROFILE"
	// Variable a launcher pins the profile of the processes it starts with, over ICEBATH_PROFILE
	PinnedEnv = "ICEBATH_PROFILE_NAME"
)
