package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	nameUsernamePassword = "UsernamePasswordCredential"
	passwordDeprecation  = "UsernamePasswordCredential is deprecated, as it doesn't support multifactor authentication (MFA). For more details, see https://aka.ms/azsdk/identity/mfa."
	unexpectedEntraReply = "Unexpected response from Microsoft Entra ID"
	recordFailure        = "Failed to build AuthenticationRecord from unexpected identity token"
	federationAudience   = "urn:federation:MicrosoftOnline"
	mexMissingEndpoint   = "Unable to find wstrust endpoint from MEX. This typically happens when attempting MSA accounts. More details available here. https://github.com/AzureAD/microsoft-authentication-library-for-python/wiki/Username-Password-Authentication"
	wsTrust13Action      = "http://docs.oasis-open.org/ws-sx/ws-trust/200512/RST/Issue"
	wsTrust2005Action    = "http://schemas.xmlsoap.org/ws/2005/02/trust/RST/Issue"
	soapHTTPTransport    = "http://schemas.xmlsoap.org/soap/http"
	nsWSDL               = "http://schemas.xmlsoap.org/wsdl/"
	nsSecurityPolicy     = "http://docs.oasis-open.org/ws-sx/ws-securitypolicy/200702"
	nsSecurityPolicy2005 = "http://schemas.xmlsoap.org/ws/2005/07/securitypolicy"
	nsUtility            = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
	nsAddressing         = "http://www.w3.org/2005/08/addressing"
	nsSOAP12Binding      = "http://schemas.xmlsoap.org/wsdl/soap12/"
	nsPolicy             = "http://schemas.xmlsoap.org/ws/2004/09/policy"
	nsEnvelope           = "http://www.w3.org/2003/05/soap-envelope"
	nsTrust13            = "http://docs.oasis-open.org/ws-sx/ws-trust/200512"
	nsTrust2005          = "http://schemas.xmlsoap.org/ws/2005/02/trust"
	samlV1               = "urn:oasis:names:tc:SAML:1.0:assertion"
	samlV2               = "urn:oasis:names:tc:SAML:2.0:assertion"
	samlProfileV11       = "http://docs.oasis-open.org/wss/oasis-wss-saml-token-profile-1.1#SAMLV1.1"
	samlProfileV2        = "http://docs.oasis-open.org/wss/oasis-wss-saml-token-profile-1.1#SAMLV2.0"
	saml11Grant          = "urn:ietf:params:oauth:grant-type:saml1_1-bearer"
	saml2Grant           = "urn:ietf:params:oauth:grant-type:saml2-bearer"
	wsuTimeLayout        = "2006-01-02T15:04:05Z"
	wsTrustLogLimit      = 4096
)

var (
	rstrPattern          = regexp.MustCompile(`(?s)<(?:wst:|trust:)?RequestSecurityTokenResponse.*?>.*?</(?:wst:|trust:)?RequestSecurityTokenResponse>`)
	tokenTypePattern     = regexp.MustCompile(`(?s)<(?:\w+:)?TokenType(?:[^>]*)>(.*)</(?:\w+:)?TokenType`)
	securityTokenPattern = regexp.MustCompile(`(?s)<(?:\w+:)?RequestedSecurityToken(?:[^>]*)>(.*)</(?:\w+:)?RequestedSecurityToken`)
	passwordEscaper      = strings.NewReplacer("&", "&amp;", `"`, "&quot;", "'", "&apos;", "<", "&lt;", ">", "&gt;")
)

type passwordUser struct {
	c         *client
	tenant    string
	clientID  string
	authority string
	username  string
	password  auth.Secret

	mu      sync.Mutex
	account string
	family  bool
	renewal auth.Secret
	tokens  map[string]accessToken
}

func (s *passwordUser) name() string { return nameUsernamePassword }

func (s *passwordUser) String() string { return nameUsernamePassword + " for client " + s.clientID }

func (s *passwordUser) GoString() string { return s.String() }

func (s *passwordUser) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (c *client) usernamePassword(tenant, clientID, username, password, authority string) (source, error) {
	slog.Debug(passwordDeprecation)
	if tenant == "" {
		tenant = "organizations"
	}
	if !validTenant(tenant) {
		return nil, &AuthError{Credential: nameUsernamePassword, Message: invalidTenantText}
	}
	return &passwordUser{c: c, tenant: tenant, clientID: clientID, authority: authority, username: username,
		password: auth.NewSecret(password), tokens: map[string]accessToken{}}, nil
}

func (s *passwordUser) token(ctx context.Context, scope string, _ bool) (accessToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.acquire(ctx, scope)
	if err != nil {
		slog.Warn(nameUsernamePassword + ".get_token failed: " + err.Error())
		return accessToken{}, err
	}
	slog.Info(nameUsernamePassword + ".get_token succeeded")
	return token, nil
}

func (s *passwordUser) fail(status int, code, message string, err error) error {
	return &AuthError{Credential: nameUsernamePassword, Status: status, Code: code, Message: message, Err: err}
}

func (s *passwordUser) acquire(ctx context.Context, scope string) (accessToken, error) {
	now := time.Now().Unix()
	cached, ok := s.tokens[scope]
	aging := false
	if ok && cached.expiresOn-now >= refreshOffset {
		if cached.refreshOn == 0 || cached.refreshOn >= now {
			return cached, nil
		}
		aging = true
	}
	found, err := s.c.discover(ctx, nameUsernamePassword, s.authority, s.tenant)
	if err != nil {
		return accessToken{}, err
	}
	if s.account != "" && !s.renewal.Empty() {
		refreshed, settled, err := s.refresh(ctx, found, scope)
		switch {
		case err != nil:
			return accessToken{}, err
		case settled:
			return refreshed, nil
		case aging:
			return cached, nil
		}
	}
	return s.signIn(ctx, found, scope)
}

func (s *passwordUser) refresh(ctx context.Context, found endpoints, scope string) (accessToken, bool, error) {
	attempts := 1
	if s.family {
		attempts = 2
	}
	header := http.Header{"X-AnchorMailbox": {"Oid:" + strings.ReplaceAll(s.account, ".", "@")}}
	for range attempts {
		requestTime := time.Now().Unix()
		form := []pair{{"client_id", s.clientID}, {"grant_type", "refresh_token"}, {"client_info", "1"},
			{"refresh_token", s.renewal.Reveal()}, {"scope", scope + browserScopes}}
		status, reply, err := s.redeem(ctx, found.token, header, form)
		if err != nil {
			return accessToken{}, false, err
		}
		if reply.text("error") != "" {
			slog.Debug("Refresh failed. " + reply.text("error") + ": " + reply.text("error_description"))
			continue
		}
		token, err := s.keep(status, reply, scope, requestTime)
		if err != nil {
			return accessToken{}, false, err
		}
		return token, true, nil
	}
	return accessToken{}, false, nil
}

func (s *passwordUser) redeem(ctx context.Context, endpoint string, extra http.Header, form []pair) (int, document, error) {
	header := http.Header{"Content-Type": {formType}, "Accept": {jsonType}}
	for name, values := range extra {
		header[name] = values
	}
	status, _, data, err := s.c.call(ctx, http.MethodPost, endpoint, header, encodeForm(form))
	if err != nil {
		return 0, nil, s.fail(0, "", "Authentication failed: "+httpclient.Message(err), err)
	}
	if status >= 500 {
		return status, nil, entraStatusError(nameUsernamePassword, status, data)
	}
	reply, ok := parseDocument(data)
	if !ok {
		return status, nil, s.fail(status, "", "Authentication failed: token endpoint reply is not a JSON object", nil)
	}
	return status, reply, nil
}

func entraStatusError(credential string, status int, data []byte) error {
	reply, ok := parseDocument(data)
	_, hasError := reply["error"]
	_, hasDescription := reply["error_description"]
	if ok && (hasError || hasDescription) {
		detail := reply.text("error_description")
		if detail == "" {
			detail = reply.text("error")
		}
		return &AuthError{Credential: credential, Status: status, Code: reply.text("error"), Message: "Authentication failed: " + detail}
	}
	return &AuthError{Credential: credential, Status: status, Message: unexpectedEntraReply}
}

func (s *passwordUser) keep(status int, reply document, scope string, requestTime int64) (accessToken, error) {
	value := reply.text("access_token")
	if value == "" {
		detail := reply.text("error_description")
		if detail == "" {
			detail = reply.text("error")
		}
		if detail == "" {
			detail = "token endpoint reply carried no access token"
		}
		return accessToken{}, s.fail(status, reply.text("error"), "Authentication failed: "+detail, nil)
	}
	lifetime, ok := reply.integer("expires_in")
	if !ok {
		return accessToken{}, s.fail(status, "", "Authentication failed: token endpoint reply carried no lifetime", nil)
	}
	stored := accessToken{value: value, expiresOn: requestTime + lifetime}
	if in, ok := reply.integer("refresh_in"); ok {
		stored.refreshOn = requestTime + in
	}
	if next := reply.text("refresh_token"); next != "" {
		s.renewal = auth.NewSecret(next)
	}
	if _, ok := reply["foci"]; ok {
		s.family = reply.text("foci") != ""
	}
	s.tokens[scope] = stored
	return stored, nil
}

func (s *passwordUser) signIn(ctx context.Context, found endpoints, scope string) (accessToken, error) {
	requestTime := time.Now().Unix()
	instance, adfs, err := authorityInstance(s.authority, found.token)
	if err != nil {
		return accessToken{}, s.fail(0, "", "Authentication failed: "+err.Error(), err)
	}
	var reply document
	status := 0
	if !adfs {
		realm, err := s.c.userRealm(ctx, instance, s.username)
		if err != nil {
			return accessToken{}, err
		}
		if realm.text("account_type") == "Federated" {
			if status, reply, err = s.federated(ctx, realm, found, scope); err != nil {
				return accessToken{}, err
			}
		}
	}
	if reply == nil {
		form := []pair{{"client_id", s.clientID}, {"grant_type", "password"}, {"client_info", "1"},
			{"username", s.username}, {"password", s.password.Reveal()}, {"scope", scope + browserScopes}}
		if status, reply, err = s.redeem(ctx, found.token, http.Header{"X-AnchorMailbox": {"upn:" + s.username}}, form); err != nil {
			return accessToken{}, err
		}
	}
	if reply.text("access_token") == "" {
		_, err := s.keep(status, reply, scope, requestTime)
		return accessToken{}, err
	}
	account, err := recordAccount(reply)
	if err != nil {
		return accessToken{}, s.fail(status, "", recordFailure, err)
	}
	token, err := s.keep(status, reply, scope, requestTime)
	if err != nil {
		return accessToken{}, err
	}
	s.account = account
	token.refreshOn = 0
	return token, nil
}

func authorityInstance(authority, tokenEndpoint string) (string, bool, error) {
	base, err := url.Parse(authority)
	if err != nil || base.Hostname() == "" {
		return "", false, errors.New("the authority " + authority + " is not an https URL with a host")
	}
	endpoint, err := url.Parse(tokenEndpoint)
	if err != nil {
		return "", false, errors.New("the token endpoint is not a URL")
	}
	parts := strings.Split(endpoint.Path, "/")
	adfs := len(parts) > 1 && strings.EqualFold(parts[1], "adfs")
	return strings.ToLower(base.Hostname()), adfs, nil
}

func decodeSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(segment, "="))
}

func recordAccount(reply document) (string, error) {
	parts := strings.Split(reply.text("id_token"), ".")
	if len(parts) < 2 {
		return "", errors.New("the reply carries no identity token")
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return "", err
	}
	claims, ok := parseDocument(payload)
	if !ok {
		return "", errors.New("the identity token claims are not a JSON object")
	}
	var account string
	if _, present := reply["client_info"]; present {
		raw, err := decodeSegment(reply.text("client_info"))
		if err != nil {
			return "", err
		}
		var info struct {
			UID  *string `json:"uid"`
			UTID *string `json:"utid"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return "", err
		}
		if info.UID == nil || info.UTID == nil {
			return "", errors.New("client_info carries no uid or utid")
		}
		account = *info.UID + "." + *info.UTID
	} else {
		if _, present := claims["sub"]; !present {
			return "", errors.New("the identity token carries no subject")
		}
		account = claims.text("sub")
	}
	for _, required := range []string{"iss", "aud"} {
		if _, present := claims[required]; !present {
			return "", errors.New("the identity token carries no " + required)
		}
	}
	if claims.text("preferred_username") == "" {
		if _, present := claims["upn"]; !present {
			return "", errors.New("the identity token carries no username")
		}
	}
	return account, nil
}

func (c *client) userRealm(ctx context.Context, instance, username string) (document, error) {
	c.mu.Lock()
	skip := c.realmless[instance]
	c.mu.Unlock()
	if skip {
		return document{}, nil
	}
	target := "https://" + instance + "/common/userrealm/" + url.PathEscape(username) + "?api-version=1.0"
	status, _, data, err := c.call(ctx, http.MethodGet, target, http.Header{"Accept": {jsonType}}, nil)
	if err != nil {
		return nil, &AuthError{Credential: nameUsernamePassword, Message: "Authentication failed: " + httpclient.Message(err), Err: err}
	}
	if status == http.StatusNotFound {
		c.mu.Lock()
		c.realmless[instance] = true
		c.mu.Unlock()
		return document{}, nil
	}
	if status >= 400 {
		return nil, entraStatusError(nameUsernamePassword, status, data)
	}
	reply, ok := parseDocument(data)
	if !ok {
		return nil, &AuthError{Credential: nameUsernamePassword, Status: status, Message: "Authentication failed: user realm discovery reply is not a JSON object"}
	}
	return reply, nil
}

type wsTrustEndpoint struct {
	address string
	action  string
}

func (s *passwordUser) federated(ctx context.Context, realm document, found endpoints, scope string) (int, document, error) {
	address, action := realm.text("federation_active_auth_url"), ""
	if metadata := realm.text("federation_metadata_url"); metadata != "" {
		status, _, data, err := s.c.call(ctx, http.MethodGet, metadata, nil, nil)
		if err != nil {
			return 0, nil, s.fail(0, "", "Authentication failed: "+httpclient.Message(err), err)
		}
		if status >= 400 {
			return 0, nil, s.fail(status, "", unexpectedEntraReply, nil)
		}
		endpoint, err := mexEndpoint(data)
		if err != nil {
			return 0, nil, s.fail(status, "", "Authentication failed: "+err.Error(), err)
		}
		if endpoint == nil {
			return 0, nil, s.fail(0, "", "Authentication failed: "+mexMissingEndpoint, nil)
		}
		address, action = endpoint.address, endpoint.action
	}
	slog.Debug("wstrust_endpoint", "address", address, "action", action)
	audience := federationAudience
	if _, present := realm["cloud_audience_urn"]; present {
		audience = realm.text("cloud_audience_urn")
	}
	assertion, tokenType, err := s.wsTrust(ctx, address, action, audience)
	if err != nil {
		return 0, nil, err
	}
	grant := ""
	switch tokenType {
	case samlV1, samlProfileV11:
		grant = saml11Grant
	case samlV2, samlProfileV2:
		grant = saml2Grant
	default:
		return 0, nil, s.fail(0, "", "Authentication failed: RSTR returned unknown token type: "+tokenType, nil)
	}
	form := []pair{{"client_id", s.clientID}, {"grant_type", grant}, {"client_info", "1"}, {"scope", scope + browserScopes},
		{"assertion", base64.RawURLEncoding.EncodeToString(assertion)}}
	return s.redeem(ctx, found.token, nil, form)
}

func (s *passwordUser) wsTrust(ctx context.Context, address, action, audience string) ([]byte, string, error) {
	if address == "" {
		return nil, "", s.fail(0, "", "Authentication failed: WsTrust endpoint address can not be empty", nil)
	}
	if action == "" {
		switch {
		case strings.Contains(address, "/trust/2005/usernamemixed"):
			action = wsTrust2005Action
		case strings.Contains(address, "/trust/13/usernamemixed"):
			action = wsTrust13Action
		}
	}
	if action != wsTrust13Action && action != wsTrust2005Action {
		return nil, "", s.fail(0, "", "Authentication failed: Unsupported soap action: "+action+". Contact your administrator to check your ADFS's MEX settings.", nil)
	}
	body := buildRST(s.username, s.password.Reveal(), audience, address, action, time.Now().UTC())
	header := http.Header{"Content-Type": {"application/soap+xml; charset=utf-8"}, "SOAPAction": {action}}
	status, _, data, err := s.c.call(ctx, http.MethodPost, address, header, []byte(body))
	if err != nil {
		return nil, "", s.fail(0, "", "Authentication failed: "+httpclient.Message(err), err)
	}
	if status >= 400 {
		excerpt := data
		if len(excerpt) > wsTrustLogLimit {
			excerpt = excerpt[:wsTrustLogLimit]
		}
		slog.Debug("Unsuccessful WsTrust request receives: " + string(excerpt))
	}
	text := string(data)
	for _, rstr := range rstrPattern.FindAllString(text, -1) {
		tokenType := tokenTypePattern.FindStringSubmatch(rstr)
		token := securityTokenPattern.FindStringSubmatch(rstr)
		if tokenType != nil && token != nil {
			return []byte(token[1]), tokenType[1], nil
		}
	}
	root, err := parseXMLTree(data)
	if err != nil {
		return nil, "", s.fail(status, "", "Authentication failed: "+err.Error(), err)
	}
	reason := root.find(xml.Name{Space: nsEnvelope, Local: "Body"}, xml.Name{Space: nsEnvelope, Local: "Fault"},
		xml.Name{Space: nsEnvelope, Local: "Reason"}, xml.Name{Space: nsEnvelope, Local: "Text"})
	code := root.find(xml.Name{Space: nsEnvelope, Local: "Body"}, xml.Name{Space: nsEnvelope, Local: "Fault"},
		xml.Name{Space: nsEnvelope, Local: "Code"}, xml.Name{Space: nsEnvelope, Local: "Subcode"}, xml.Name{Space: nsEnvelope, Local: "Value"})
	detail := ""
	switch {
	case reason != nil || code != nil:
		parts := []string{}
		if reason != nil {
			parts = append(parts, "reason: "+reason.text)
		}
		if code != nil {
			parts = append(parts, "code: "+code.text)
		}
		detail = strings.Join(parts, ", ")
	default:
		detail = text
		if len(detail) > wsTrustLogLimit {
			detail = detail[:wsTrustLogLimit]
		}
	}
	return nil, "", s.fail(status, "", "Authentication failed: WsTrust server returned error in RSTR: "+detail, nil)
}

func buildRST(username, password, audience, address, action string, now time.Time) string {
	trust := nsTrust13
	keyType := "http://docs.oasis-open.org/ws-sx/ws-trust/200512/Bearer"
	requestType := "http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue"
	if action != wsTrust13Action {
		trust = nsTrust2005
		keyType = "http://schemas.xmlsoap.org/ws/2005/05/identity/NoProofKey"
		requestType = "http://schemas.xmlsoap.org/ws/2005/02/trust/Issue"
	}
	var b strings.Builder
	b.WriteString("<s:Envelope xmlns:s='" + nsEnvelope + "' xmlns:wsa='" + nsAddressing + "' xmlns:wsu='" + nsUtility + "'>\n")
	b.WriteString("        <s:Header>\n")
	b.WriteString("            <wsa:Action s:mustUnderstand='1'>" + action + "</wsa:Action>\n")
	b.WriteString("            <wsa:MessageID>urn:uuid:" + uuid.NewString() + "</wsa:MessageID>\n")
	b.WriteString("            <wsa:ReplyTo>\n")
	b.WriteString("                <wsa:Address>http://www.w3.org/2005/08/addressing/anonymous</wsa:Address>\n")
	b.WriteString("            </wsa:ReplyTo>\n")
	b.WriteString("            <wsa:To s:mustUnderstand='1'>" + address + "</wsa:To>\n\n")
	b.WriteString("            <wsse:Security s:mustUnderstand='1'\n")
	b.WriteString("            xmlns:wsse='http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd'>\n")
	b.WriteString("                <wsu:Timestamp wsu:Id='_0'>\n")
	b.WriteString("                    <wsu:Created>" + now.Format(wsuTimeLayout) + "</wsu:Created>\n")
	b.WriteString("                    <wsu:Expires>" + now.Add(10*time.Minute).Format(wsuTimeLayout) + "</wsu:Expires>\n")
	b.WriteString("                </wsu:Timestamp>\n")
	b.WriteString("                <wsse:UsernameToken wsu:Id='ADALUsernameToken'>\n")
	b.WriteString("                    <wsse:Username>" + username + "</wsse:Username>\n")
	b.WriteString("                    <wsse:Password>" + passwordEscaper.Replace(password) + "</wsse:Password>\n")
	b.WriteString("                </wsse:UsernameToken>\n")
	b.WriteString("            </wsse:Security>\n\n")
	b.WriteString("        </s:Header>\n")
	b.WriteString("        <s:Body>\n")
	b.WriteString("        <wst:RequestSecurityToken xmlns:wst='" + trust + "'>\n")
	b.WriteString("        <wsp:AppliesTo xmlns:wsp='" + nsPolicy + "'>\n")
	b.WriteString("            <wsa:EndpointReference>\n")
	b.WriteString("                <wsa:Address>" + audience + "</wsa:Address>\n")
	b.WriteString("            </wsa:EndpointReference>\n")
	b.WriteString("        </wsp:AppliesTo>\n")
	b.WriteString("        <wst:KeyType>" + keyType + "</wst:KeyType>\n")
	b.WriteString("            <wst:RequestType>" + requestType + "</wst:RequestType>\n")
	b.WriteString("        </wst:RequestSecurityToken>\n")
	b.WriteString("        </s:Body>\n")
	b.WriteString("        </s:Envelope>")
	return b.String()
}

type xmlNode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*xmlNode
}

func parseXMLTree(data []byte) (*xmlNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var root *xmlNode
	var stack []*xmlNode
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: t.Name, attrs: t.Copy().Attr}
			switch {
			case len(stack) > 0:
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			case root == nil:
				root = n
			default:
				return nil, errors.New("XML document has more than one root element")
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && len(stack[len(stack)-1].children) == 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("XML document has no root element")
	}
	return root, nil
}

func (n *xmlNode) attr(space, local string) (string, bool) {
	for _, a := range n.attrs {
		if a.Name.Space == space && a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

func (n *xmlNode) all(name xml.Name) []*xmlNode {
	var out []*xmlNode
	for _, child := range n.children {
		if child.name == name {
			out = append(out, child)
		}
	}
	return out
}

func (n *xmlNode) find(path ...xml.Name) *xmlNode {
	if len(path) == 0 {
		return n
	}
	for _, child := range n.all(path[0]) {
		if hit := child.find(path[1:]...); hit != nil {
			return hit
		}
	}
	return nil
}

func (n *xmlNode) descendant(name xml.Name) bool {
	for _, child := range n.children {
		if child.name == name || child.descendant(name) {
			return true
		}
	}
	return false
}

type mexBinding struct {
	policy string
	action string
}

func mexPolicies(root *xmlNode) map[string]bool {
	ids := map[string]bool{}
	policy := xml.Name{Space: nsPolicy, Local: "Policy"}
	routes := []struct {
		space   string
		support string
	}{{nsSecurityPolicy, "SignedEncryptedSupportingTokens"}, {nsSecurityPolicy2005, "SignedSupportingTokens"}}
	for _, route := range routes {
		path := []xml.Name{{Space: nsPolicy, Local: "ExactlyOne"}, {Space: nsPolicy, Local: "All"}, {Space: route.space, Local: route.support},
			policy, {Space: route.space, Local: "UsernameToken"}, policy, {Space: route.space, Local: "WssUsernameToken10"}}
		for _, candidate := range root.all(policy) {
			if candidate.find(path...) == nil || !candidate.descendant(xml.Name{Space: route.space, Local: "TransportBinding"}) {
				continue
			}
			if id, ok := candidate.attr(nsUtility, "Id"); ok {
				ids["#"+id] = true
			}
		}
	}
	return ids
}

func mexEndpoint(data []byte) (*wsTrustEndpoint, error) {
	root, err := parseXMLTree(data)
	if err != nil {
		return nil, err
	}
	policies := mexPolicies(root)
	bindings := map[string]mexBinding{}
	for _, binding := range root.all(xml.Name{Space: nsWSDL, Local: "binding"}) {
		soap := binding.find(xml.Name{Space: nsSOAP12Binding, Local: "binding"})
		if soap == nil {
			return nil, errors.New("malformed MEX document, a binding carries no soap12 binding")
		}
		if transport, _ := soap.attr("", "transport"); transport != soapHTTPTransport {
			continue
		}
		operation := binding.find(xml.Name{Space: nsWSDL, Local: "operation"}, xml.Name{Space: nsSOAP12Binding, Local: "operation"})
		if operation == nil {
			return nil, errors.New("malformed MEX document, a binding carries no soap12 operation")
		}
		action, _ := operation.attr("", "soapAction")
		name, _ := binding.attr("", "name")
		for _, reference := range binding.all(xml.Name{Space: nsPolicy, Local: "PolicyReference"}) {
			uri, _ := reference.attr("", "URI")
			bindings[name] = mexBinding{policy: uri, action: action}
		}
	}
	var endpoints []wsTrustEndpoint
	for _, service := range root.all(xml.Name{Space: nsWSDL, Local: "service"}) {
		for _, port := range service.all(xml.Name{Space: nsWSDL, Local: "port"}) {
			reference, ok := port.attr("", "binding")
			if !ok {
				return nil, errors.New("malformed MEX document, a port names no binding")
			}
			binding, ok := bindings[reference[strings.LastIndex(reference, ":")+1:]]
			if !ok || !policies[binding.policy] {
				continue
			}
			address := port.find(xml.Name{Space: nsAddressing, Local: "EndpointReference"}, xml.Name{Space: nsAddressing, Local: "Address"})
			if address != nil && strings.HasPrefix(strings.ToLower(address.text), "https://") {
				endpoints = append(endpoints, wsTrustEndpoint{address: address.text, action: binding.action})
			}
		}
	}
	for _, e := range endpoints {
		if e.action == wsTrust13Action {
			return &e, nil
		}
	}
	if len(endpoints) > 0 {
		return &endpoints[0], nil
	}
	return nil, nil
}
