package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Session reading only the given environment, IMDS off unless the map turns it on
func awsTestSession(t *testing.T, env map[string]string) *awsSession {
	dir := t.TempDir()
	full := map[string]string{"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED": "true", "HOME": dir}
	for k, v := range env {
		full[k] = v
	}
	s := newAWSSession(awsSessionOptions{})
	s.lookup = func(k string) (string, bool) { v, ok := full[k]; return v, ok }
	return s
}

func awsWriteFile(t *testing.T, path, body string) string {
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Resolves and retrieves the chain's credentials
func awsResolve(t *testing.T, s *awsSession) (awsCredentials, error) {
	src, err := resolveAWSCredentialSource(context.Background(), s)
	if err != nil {
		return awsCredentials{}, err
	}
	if src == nil {
		return awsCredentials{}, ErrNoAWSCredentials
	}
	return src.Retrieve(context.Background())
}

func TestAWSChainEnvironmentKeys(t *testing.T) {
	creds, err := awsResolve(t, awsTestSession(t, map[string]string{"AWS_ACCESS_KEY_ID": "AKENV", "AWS_SECRET_ACCESS_KEY": "SENV", "AWS_SESSION_TOKEN": "TENV"}))
	if err != nil || creds.AccessKeyID != "AKENV" || creds.SecretAccessKey != "SENV" || creds.SessionToken != "TENV" {
		t.Fatalf("env creds %#v %v", creds, err)
	}
}

func TestAWSChainSharedFilesAndProfiles(t *testing.T) {
	s := awsTestSession(t, nil)
	awsWriteFile(t, s.env("AWS_SHARED_CREDENTIALS_FILE"), "[default]\naws_access_key_id = AKDEF\naws_secret_access_key = SDEF\n\n[dev]\naws_access_key_id = AKDEV\naws_secret_access_key = SDEV\n")
	awsWriteFile(t, s.env("AWS_CONFIG_FILE"), "[profile cfgonly]\naws_access_key_id = AKCFG\naws_secret_access_key = SCFG\nregion = eu-west-3\n")
	if creds, err := awsResolve(t, s); err != nil || creds.AccessKeyID != "AKDEF" {
		t.Fatalf("default profile %#v %v", creds, err)
	}
	dev := awsTestSession(t, map[string]string{"AWS_PROFILE": "dev", "AWS_SHARED_CREDENTIALS_FILE": s.env("AWS_SHARED_CREDENTIALS_FILE"), "AWS_CONFIG_FILE": s.env("AWS_CONFIG_FILE")})
	if creds, err := awsResolve(t, dev); err != nil || creds.AccessKeyID != "AKDEV" {
		t.Fatalf("dev profile %#v %v", creds, err)
	}
	cfg := awsTestSession(t, map[string]string{"AWS_PROFILE": "cfgonly", "AWS_SHARED_CREDENTIALS_FILE": s.env("AWS_SHARED_CREDENTIALS_FILE"), "AWS_CONFIG_FILE": s.env("AWS_CONFIG_FILE")})
	if creds, err := awsResolve(t, cfg); err != nil || creds.AccessKeyID != "AKCFG" || cfg.region() != "eu-west-3" {
		t.Fatalf("config file keys %#v %v region %q", creds, err, cfg.region())
	}
}

func TestAWSChainCredentialProcess(t *testing.T) {
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Fatal(err)
	}
	s := awsTestSession(t, nil)
	awsWriteFile(t, s.env("AWS_CONFIG_FILE"), "[default]\ncredential_process = "+printf+` '{"Version": 1, "AccessKeyId": "AKPROC", "SecretAccessKey": "SPROC", "SessionToken": "TPROC", "Expiration": "2099-01-01T00:00:00Z"}'`+"\n")
	creds, err := awsResolve(t, s)
	if err != nil || creds.AccessKeyID != "AKPROC" || creds.SessionToken != "TPROC" || creds.Expires.Year() != 2099 {
		t.Fatalf("process creds %#v %v", creds, err)
	}
	bad := awsTestSession(t, nil)
	awsWriteFile(t, bad.env("AWS_CONFIG_FILE"), "[default]\ncredential_process = "+printf+` '{"Version": 2, "AccessKeyId": "A", "SecretAccessKey": "S"}'`+"\n")
	if _, err := awsResolve(t, bad); err == nil || !strings.Contains(err.Error(), "Unsupported version") {
		t.Fatalf("unsupported version accepted: %v", err)
	}
}

func TestAWSChainContainerCredentials(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]string{"AccessKeyId": "AKCTR", "SecretAccessKey": "SCTR", "Token": "TCTR", "Expiration": "2099-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	s := awsTestSession(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL + "/creds", "AWS_CONTAINER_AUTHORIZATION_TOKEN": "ctr-token"})
	creds, err := awsResolve(t, s)
	if err != nil || creds.AccessKeyID != "AKCTR" || creds.SessionToken != "TCTR" || auth.Load() != "ctr-token" {
		t.Fatalf("container creds %#v %v auth %v", creds, err, auth.Load())
	}
	remote := awsTestSession(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://example.com/creds"})
	var ce *AWSCredentialError
	if _, err := awsResolve(t, remote); !errors.As(err, &ce) || !strings.Contains(ce.Message, "Unsupported host") {
		t.Fatalf("non loopback host accepted: %v", err)
	}
}

func TestAWSChainWebIdentity(t *testing.T) {
	var form atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form.Store(r.Form.Encode())
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>AKWEB</AccessKeyId><SecretAccessKey>SWEB</SecretAccessKey><SessionToken>TWEB</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	token := awsWriteFile(t, filepath.Join(dir, "token"), "jwt-token-value")
	s := awsTestSession(t, map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": token, "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/web",
		"AWS_ROLE_SESSION_NAME": "sess", "AWS_ENDPOINT_URL_STS": srv.URL, "AWS_REGION": "us-east-1"})
	creds, err := awsResolve(t, s)
	if err != nil || creds.AccessKeyID != "AKWEB" || creds.SessionToken != "TWEB" {
		t.Fatalf("web identity creds %#v %v", creds, err)
	}
	got, _ := form.Load().(string)
	for _, want := range []string{"Action=AssumeRoleWithWebIdentity", "WebIdentityToken=jwt-token-value", "RoleSessionName=sess"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sts form %q lacks %q", got, want)
		}
	}
}

func TestAWSChainIMDSv2AndV1Fallback(t *testing.T) {
	for _, v1 := range []bool{false, true} {
		var sawToken atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
				if v1 {
					w.WriteHeader(405)
					return
				}
				if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "21600" {
					w.WriteHeader(400)
					return
				}
				_, _ = w.Write([]byte("imds-token"))
			case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
				sawToken.Store(r.Header.Get("X-aws-ec2-metadata-token") == "imds-token")
				_, _ = w.Write([]byte("instance-role"))
			case r.URL.Path == "/latest/meta-data/iam/security-credentials/instance-role":
				_ = json.NewEncoder(w).Encode(map[string]string{"Code": "Success", "AccessKeyId": "AKIMDS", "SecretAccessKey": "SIMDS", "Token": "TIMDS", "Expiration": "2099-01-01T00:00:00Z"})
			default:
				w.WriteHeader(404)
			}
		}))
		s := awsTestSession(t, map[string]string{"AWS_EC2_METADATA_DISABLED": "false", "AWS_EC2_METADATA_SERVICE_ENDPOINT": srv.URL})
		creds, err := awsResolve(t, s)
		srv.Close()
		if err != nil || creds.AccessKeyID != "AKIMDS" || creds.SessionToken != "TIMDS" {
			t.Fatalf("v1=%v imds creds %#v %v", v1, creds, err)
		}
		if sawToken.Load() == v1 {
			t.Fatalf("v1=%v token header presence %v", v1, sawToken.Load())
		}
	}
	off := awsTestSession(t, nil)
	if _, err := awsResolve(t, off); err == nil {
		t.Fatal("empty chain produced credentials")
	}
}

func TestBedrockCredentialProbeWalksChainOnce(t *testing.T) {
	bedrockIsolatedEnv(t)
	var walks []bool
	prev := awsChainHasCredentialsFn
	awsChainHasCredentialsFn = func(instanceMetadata bool) bool { walks = append(walks, instanceMetadata); return false }
	t.Cleanup(func() { awsChainHasCredentialsFn = prev })
	if hasAWSCredentials(true) {
		t.Fatal("credentials reported with an empty chain")
	}
	if len(walks) != 1 {
		t.Fatalf("chain walked %d times", len(walks))
	}
	walks = nil
	if hasAWSCredentials(false) || len(walks) != 1 || walks[0] {
		t.Fatalf("desktop probe walks %v", walks)
	}
}
