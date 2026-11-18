package pm

// One entry point an installed distribution declares, with the distribution it came from
type EntryPoint struct {
	Name        string
	Value       string
	Group       string
	DistName    string
	DistVersion string
	DistSummary string
}
