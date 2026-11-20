package gatewayrun

// Update hook bar, two profiles with a standalone secondary, not converged and nothing blocking
func (m *MigrationPlan) EligibleForMigration() bool {
	return len(m.Profiles) >= 2 && len(m.StandaloneSecondaries()) > 0 && !m.AlreadyMultiplexed() && !m.Blocked()
}
