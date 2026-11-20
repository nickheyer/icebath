package provider

import "github.com/nickheyer/icebath/pkg/skills"

// Wires skill scaffolds into the cache boundary registry and skill readiness into the scoped secret reader
func init() {
	skills.RegisterStablePrefixSink(RegisterStablePrefix)
	skills.RegisterSecretResolver(GetSecret)
}
