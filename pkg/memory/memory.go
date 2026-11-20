// Package memory ports the icebath memory system, the MEMORY.md and USER.md stores, providers, curator and journey
package memory

// Registers the memory tool and the memory, curator and journey commands once every module below exists
func init() {
	registerMemoryStore()
	registerMemoryTool()
	registerWriteApprovalGate()
	registerMemoryContextFencing()
	registerMemoryCallerScope()
	registerMemoryProviderRegistry()
	registerByteRoverProvider()
	registerRetainDBProvider()
	registerMemoryPromptBlocks()
	registerMemorySetupCommands()
	registerMemoryProviderMigration()
	registerProfileMemoryConfigClone()
	registerMemoryTrim()
	registerLearningGraph()
	registerLearningGraphRender()
	registerLearningMutations()
	registerLearnPrompt()
	registerInsightsEngine()
	registerOnboardingFacts()
	registerSkillUsageSidecar()
	registerSkillLedger()
	registerCurator()
	registerCuratorBackup()
	registerCuratorCommands()
}

// Onboarding facts are read by pkg/tools ReadOnboardingFacts over pkg/hostinfo
func registerOnboardingFacts() {}
