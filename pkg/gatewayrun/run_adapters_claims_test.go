package gatewayrun

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter stand in holding Python attributes by name
type attrFake map[string]any

func (f attrFake) AdapterAttr(name string) any { return f[name] }

// Adapter whose credential lives on its platform config
func configTokenAdapter(token any) gateway.PlatformAdapter {
	config := gateway.NewPlatformConfig()
	config.Token = token
	return gateway.NewBasePlatformAdapter(config, gateway.PlatformDiscord, nil).Self()
}

func TestCredentialFingerprintNoneWithoutToken(t *testing.T) {
	if fp := adapterCredentialFingerprint(attrFake{"token": nil}); fp != "" {
		t.Fatalf("fingerprint = %q", fp)
	}
}

func TestCredentialFingerprintClaimsNeverRaise(t *testing.T) {
	digest := sha256.Sum256([]byte("hermes-mux:tok"))
	if fp := adapterCredentialFingerprint(attrFake{"token": "tok"}); fp != hex.EncodeToString(digest[:])[:16] {
		t.Fatalf("fingerprint = %q", fp)
	}
	if fp := adapterCredentialFingerprint(attrFake{"token": "tok\xed\xa0\x80"}); fp == "" {
		t.Fatal("a lone surrogate token must still hash")
	}
	photon := attrFake{"_sidecar_bind": "0.0.0.0", "_sidecar_port": math.Inf(1)}
	if claim := adapterListenerClaim("photon", photon); claim != nil {
		t.Fatalf("claim = %v", claim)
	}
}

func TestCredentialFingerprintReadsPhotonProjectSecret(t *testing.T) {
	fp1 := adapterCredentialFingerprint(attrFake{"_project_secret": "shared-project-secret"})
	fp2 := adapterCredentialFingerprint(attrFake{"_project_secret": "shared-project-secret"})
	if fp1 != fp2 || fp1 == "" || strings.Contains(fp1, "shared-project-secret") {
		t.Fatalf("fingerprints %q %q", fp1, fp2)
	}
}

func TestCredentialFingerprintReadsFeishuAppID(t *testing.T) {
	fp1 := adapterCredentialFingerprint(attrFake{"_app_id": "cli_a1b2c3", "_app_secret": "top-secret"})
	fp2 := adapterCredentialFingerprint(attrFake{"_app_id": "cli_a1b2c3", "_app_secret": "top-secret"})
	if fp1 == "" || fp1 != fp2 || strings.Contains(fp1, "cli_a1b2c3") {
		t.Fatalf("fingerprints %q %q", fp1, fp2)
	}
}

func TestCredentialFingerprintDistinctFeishuAppIDsDistinctFp(t *testing.T) {
	a := adapterCredentialFingerprint(attrFake{"_app_id": "app-A", "_app_secret": "s"})
	b := adapterCredentialFingerprint(attrFake{"_app_id": "app-B", "_app_secret": "s"})
	if a == "" || b == "" || a == b {
		t.Fatalf("fingerprints %q %q", a, b)
	}
}

func TestCredentialFingerprintReadsAppStyleIDsTeamsWecom(t *testing.T) {
	for _, attr := range []string{"_client_id", "_bot_id"} {
		t.Run(attr, func(t *testing.T) {
			a := adapterCredentialFingerprint(attrFake{attr: "app-1"})
			b := adapterCredentialFingerprint(attrFake{attr: "app-1"})
			c := adapterCredentialFingerprint(attrFake{attr: "app-2"})
			if a == "" || a != b || a == c || strings.Contains(a, "app-1") {
				t.Fatalf("fingerprints %q %q %q", a, b, c)
			}
		})
	}
}

func TestCredentialFingerprintReadsConfigToken(t *testing.T) {
	fp := adapterCredentialFingerprint(configTokenAdapter("discord-bot-token"))
	if fp == "" || strings.Contains(fp, "discord-bot-token") || len(fp) != 16 {
		t.Fatalf("fingerprint = %q", fp)
	}
}

func TestCredentialFingerprintDistinctConfigTokensDistinctFp(t *testing.T) {
	a := adapterCredentialFingerprint(configTokenAdapter("tok-A"))
	b := adapterCredentialFingerprint(configTokenAdapter("tok-B"))
	if a == "" || b == "" || a == b {
		t.Fatalf("fingerprints %q %q", a, b)
	}
}
