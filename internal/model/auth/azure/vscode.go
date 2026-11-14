package azure

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	nameVSCode           = "VisualStudioCodeCredential"
	vscodeClientID       = "aebc6443-996d-45c2-90f0-388ff96faa56"
	vscodeRecordLimit    = 10 * 1024
	vscodeBrokerRequired = "VisualStudioCodeCredential requires the 'azure-identity-broker' package to be installed. You must also ensure you have the Azure Resources extension installed and have signed in to Azure via Visual Studio Code."
	vscodeSignedOut      = "VisualStudioCodeCredential requires the user to be signed in to Azure via Visual Studio Code. Please ensure you have the Azure Resources extension installed and have signed in."
	vscodeRecordTooLarge = "VS Code auth record file is unexpectedly large. Please check the file for corruption or unexpected content."
	vscodeRecordAdvice   = ". Please ensure you have the Azure Resources extension installed and signed in."
)

type vscodeRecordError struct{ message string }

func (e *vscodeRecordError) Error() string { return e.message }

type vscodeRecord struct {
	authority string
	clientID  string
	tenantID  string
}

type unavailableSource struct {
	label   string
	message string
}

func (u unavailableSource) name() string { return u.label }

func (u unavailableSource) token(context.Context, string, bool) (accessToken, error) {
	return accessToken{}, unavailable(u.label, u.message)
}

func loadVSCodeRecord() (*vscodeRecord, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	for _, dir := range []string{".azure", ".Azure"} {
		path := filepath.Join(home, dir, "ms-azuretools.vscode-azureresourcegroups", "authRecord.json")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() > vscodeRecordLimit {
			return nil, &vscodeRecordError{message: vscodeRecordTooLarge}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			return nil, &vscodeRecordError{message: "the authentication record is not UTF-8 text"}
		}
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, &vscodeRecordError{message: err.Error()}
		}
		fields, ok := parsed.(map[string]any)
		if !ok {
			return nil, errors.New("the Visual Studio Code authentication record " + path + " is not a JSON object")
		}
		return validateVSCodeRecord(fields)
	}
	return nil, nil
}

func validateVSCodeRecord(fields map[string]any) (*vscodeRecord, error) {
	var problems []string
	text := func(key string) (string, bool) {
		value, present := fields[key]
		if !present {
			problems = append(problems, key+" field is missing")
			return "", false
		}
		s, isText := value.(string)
		if !isText || s == "" {
			problems = append(problems, key+" must be a non-empty string")
			return "", false
		}
		return s, true
	}
	tenant, ok := text("tenantId")
	if ok && !validTenant(tenant) {
		problems = append(problems, "tenantId validation failed: "+invalidTenantText)
	}
	client, ok := text("clientId")
	if ok && client != vscodeClientID {
		problems = append(problems, "clientId must match expected VS Code Azure Resources extension client ID: "+vscodeClientID)
	}
	text("username")
	text("homeAccountId")
	authority, _ := text("authority")
	if len(problems) > 0 {
		return nil, &vscodeRecordError{message: "Authentication record validation failed: " + strings.Join(problems, "; ")}
	}
	return &vscodeRecord{authority: authority, clientID: client, tenantID: tenant}, nil
}

func normalizeAuthority(authority string) (string, error) {
	u, err := url.Parse(authority)
	if err != nil || u.Scheme == "" {
		return "https://" + strings.TrimRight(authority, " /"), nil
	}
	if u.Scheme != "https" {
		return "", errors.New("'" + authority + "' is an invalid authority. The value must be a TLS protected (https) URL.")
	}
	return strings.TrimRight(authority, " /"), nil
}

func (i *Identity) visualStudioCode(tenant string) (source, error) {
	if !brokerSupported(i.host) || i.toolchain == nil {
		return unavailableSource{label: nameVSCode, message: vscodeBrokerRequired}, nil
	}
	record, err := loadVSCodeRecord()
	var bad *vscodeRecordError
	switch {
	case errors.As(err, &bad):
		return unavailableSource{label: nameVSCode, message: "Failed to load authentication record from Visual Studio Code: " + bad.message + vscodeRecordAdvice}, nil
	case err != nil:
		return nil, &AuthError{Credential: nameVSCode, Message: err.Error(), Err: err}
	case record == nil:
		return unavailableSource{label: nameVSCode, message: vscodeSignedOut}, nil
	}
	if tenant == "" {
		tenant = record.tenantID
	}
	if !validTenant(tenant) {
		return unavailableSource{label: nameVSCode, message: "Failed to load authentication record from Visual Studio Code: " + invalidTenantText + vscodeRecordAdvice}, nil
	}
	authority, err := normalizeAuthority(record.authority)
	if err != nil {
		return unavailableSource{label: nameVSCode, message: "Failed to load authentication record from Visual Studio Code: " + err.Error() + vscodeRecordAdvice}, nil
	}
	return &broker{identity: i, label: nameVSCode, clientID: record.clientID, authority: authority + "/" + tenant,
		trusted: trustedAuthority(authority, tenant), platform: vscodeBrokerRequired, prefix: vscodeBrokerRequired + " "}, nil
}
