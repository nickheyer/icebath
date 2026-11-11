package plugins

// Local provenance verdict of one plugin under the update checker's admission rules, no network
func CheckLocalProvenance(prov Provenance) *CheckResult { return checkLocalProvenance(prov) }
