// Package memory ports the icebath memory system, the MEMORY.md and USER.md stores, providers, curator and journey
package memory

// Registers the memory tool, the bundled memory providers and the provider install steps
func init() {
	registerMemoryTool()
	registerByteRoverProvider()
	registerRetainDBProvider()
	registerMemoryProviderMigration()
}
