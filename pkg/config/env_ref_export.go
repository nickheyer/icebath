package config

// Variable name a reference body reads, empty for a non env source or a blank env source
func EnvRefVarName(body string) string { return envRefName(body) }
