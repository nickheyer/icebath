package connections

import "context"

// CloudOrg is the trimmed portal org the renderer persists
type CloudOrg struct {
	ID         string  `json:"id"`
	Slug       *string `json:"slug"`
	Name       string  `json:"name"`
	IsPersonal bool    `json:"isPersonal"`
	Role       string  `json:"role"`
}

// CloudAgent is the trimmed portal agent row the renderer lists
type CloudAgent struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Status                string  `json:"status"`
	DashboardURL          *string `json:"dashboardUrl"`
	DashboardGatewayState string  `json:"dashboardGatewayState"`
}

// CloudAgents is a discovery answer with the agents and the org they were listed under
type CloudAgents struct {
	Agents []CloudAgent `json:"agents"`
	Org    *CloudOrg    `json:"org"`
}

// CloudOrgSelection is a discovery answer asking a multi org user to pick one
type CloudOrgSelection struct {
	NeedsOrgSelection bool       `json:"needsOrgSelection"`
	Orgs              []CloudOrg `json:"orgs"`
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

func stringOrNil(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// Org of a portal object, nil unless it has a string id
func trimCloudOrg(raw any) *CloudOrg {
	org := jsObject(raw)
	id, ok := org["id"].(string)
	if !ok {
		return nil
	}
	return &CloudOrg{ID: id, Slug: stringOrNil(org["slug"]), Name: stringOr(org["name"], id), IsPersonal: jsTruthy(org["isPersonal"]), Role: stringOr(org["role"], "MEMBER")}
}

// Org list of an org selection required error, nil when the error body is another shape
func parseOrgSelectionError(err error) []CloudOrg {
	parsed := ReadJSONErrorBody(err)
	orgs, ok := parsed["orgs"].([]any)
	if parsed["error"] != "org_selection_required" || !ok {
		return nil
	}
	out := []CloudOrg{}
	for _, item := range orgs {
		if org := trimCloudOrg(item); org != nil {
			out = append(out, *org)
		}
	}
	return out
}

// Agent rows with a string id projected to the renderer shape
func trimCloudAgents(body any) []CloudAgent {
	agents, _ := jsObject(body)["agents"].([]any)
	out := []CloudAgent{}
	for _, item := range agents {
		agent := jsObject(item)
		id, ok := agent["id"].(string)
		if !ok {
			continue
		}
		out = append(out, CloudAgent{ID: id, Name: stringOr(agent["name"], id), Status: stringOr(agent["status"], "unknown"),
			DashboardURL: stringOrNil(agent["dashboardUrl"]), DashboardGatewayState: stringOr(agent["dashboardGatewayState"], "unknown")})
	}
	return out
}

func hasStatus(err error, want int) bool {
	status, ok := readStatusCode(err)
	return ok && status == want
}

// Lists the portal agents through the portal session, renewing a lapsed access token once
func (s *Service) discoverCloudAgents(org string) (any, error) {
	portalBaseURL := ResolvePortalBaseURL()
	if !s.hasLivePortalSession() {
		return nil, &CloudLoginError{Message: "You are not signed in to Icebath Cloud. Open Settings → Gateway, choose Icebath Cloud, and sign in."}
	}
	if !s.hasPortalAccessToken() {
		s.renewPortalAccessSilently(false)
	}
	fetchAgents := func() (any, error) {
		return DiscoverWithTeamFallback(func(selectedOrg string) (any, error) {
			url := portalBaseURL + "/api/agents"
			if selectedOrg != "" {
				url += "?org=" + encodeURIComponent(selectedOrg)
			}
			return s.fetchJSONViaOauthSession(context.Background(), url, JSONOptions{Method: "GET", TimeoutMs: 15_000})
		}, org)
	}
	body, err := fetchAgents()
	if err != nil {
		if hasStatus(err, 401) && s.renewPortalAccessSilently(true) {
			body, err = fetchAgents()
		}
		if err != nil {
			if hasStatus(err, 401) {
				return nil, &CloudLoginError{Message: "Your Icebath Cloud session has expired. Open Settings → Gateway and sign in again."}
			}
			if hasStatus(err, 409) {
				if orgs := parseOrgSelectionError(err); orgs != nil {
					return CloudOrgSelection{NeedsOrgSelection: true, Orgs: orgs}, nil
				}
			}
			return nil, err
		}
	}
	return CloudAgents{Agents: trimCloudAgents(body), Org: trimCloudOrg(jsObject(body)["org"])}, nil
}

// CloudStatusResult is the portal base URL and whether a live portal session exists
type CloudStatusResult struct {
	PortalBaseURL string `json:"portalBaseUrl"`
	SignedIn      bool   `json:"signedIn"`
}

// CloudSessionResult is a finished portal login or logout and the session liveness after it
type CloudSessionResult struct {
	OK       bool `json:"ok"`
	SignedIn bool `json:"signedIn"`
}

// CloudStatus is the portal base URL and the portal session liveness
func (s *Service) CloudStatus() CloudStatusResult {
	return CloudStatusResult{PortalBaseURL: ResolvePortalBaseURL(), SignedIn: s.hasLivePortalSession()}
}

// CloudLogin opens the portal login window and answers the session liveness after it closes
func (s *Service) CloudLogin() (CloudSessionResult, error) {
	if err := s.openPortalLoginWindow(); err != nil {
		return CloudSessionResult{}, err
	}
	return CloudSessionResult{OK: true, SignedIn: s.hasLivePortalSession()}, nil
}

// CloudLogout clears the portal session cookies and answers the session liveness after
func (s *Service) CloudLogout() CloudSessionResult {
	s.clearOauthSession(ResolvePortalBaseURL())
	return CloudSessionResult{OK: true, SignedIn: s.hasLivePortalSession()}
}

// CloudDiscover lists the portal agents, scoped to org when it names one
func (s *Service) CloudDiscover(org any) (any, error) {
	selected, _ := org.(string)
	return s.discoverCloudAgents(selected)
}

// CloudAgentSignIn signs in to the agent behind a dashboard URL through the portal session
func (s *Service) CloudAgentSignIn(dashboardURL any) (CloudAgentSignIn, error) {
	return s.cloudAgentSilentSignIn(context.Background(), jsOrString(dashboardURL), false)
}
