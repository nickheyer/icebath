package connections

// RosterSourceStatus is whether a roster source answered, with its error and sign in need
type RosterSourceStatus struct {
	Reachable   bool   `json:"reachable"`
	Error       string `json:"error,omitempty"`
	NeedsSignIn bool   `json:"needsSignIn,omitempty"`
}

// RosterSourceStatusOf reports a source reachable when it listed profiles or waits for a dial on demand
func RosterSourceStatusOf(profiles []string, err string, needsSignIn bool) RosterSourceStatus {
	return RosterSourceStatus{Reachable: profiles != nil && (err == "" || err == "connect-on-demand"), Error: err, NeedsSignIn: needsSignIn}
}

// FetchRosterSourceData reads a source profile listing and its install id at once
func FetchRosterSourceData(fetchProfiles func() (any, error), fetchInstallID func() string) (any, string, error) {
	var installID string
	done := make(chan struct{})
	go func() {
		installID = fetchInstallID()
		close(done)
	}()
	body, err := fetchProfiles()
	if err != nil {
		return nil, "", err
	}
	<-done
	return body, installID, nil
}
