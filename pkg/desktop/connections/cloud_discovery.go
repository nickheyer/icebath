package connections

// Stale team codes the portal answers for an org the user no longer reaches, by status
var staleTeamResponses = map[int]string{403: "org_access_denied", 404: "org_not_found"}

// Fetches agents for org, retrying once without it when the portal says the saved team is stale
func DiscoverWithTeamFallback[T any](fetchAgents func(org string) (T, error), org string) (T, error) {
	value, err := fetchAgents(org)
	if err == nil {
		return value, nil
	}
	status, _ := readStatusCode(err)
	staleTeamCode := staleTeamResponses[status]
	if org != "" && staleTeamCode != "" && ReadJSONErrorBody(err)["error"] == staleTeamCode {
		return fetchAgents("")
	}
	var zero T
	return zero, err
}
