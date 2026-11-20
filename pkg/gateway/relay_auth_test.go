package gateway

import "testing"

const relayAuthSecret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

const relayAuthConnToken = "Z3ctaW5zdGFuY2UtMTowOjM3YWE3YjE0NWU4NzY0ZDQwM2JhOWM2MzlmMjMwZGQ2M2RlOGVkOTliODhmZWQzNmFhMDI2MjVhOGE3ZTM1NjQ"

func TestUpgradeTokenIsMakeTokenOfGatewayID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got, want := RelayMakeUpgradeToken("gw-1", relayAuthSecret, 0), RelayMakeToken("gw-1", relayAuthSecret, 0); got != want {
		t.Fatalf("upgrade token %q != make token %q", got, want)
	}
}

func TestPythonMakeTokenMatchesConnectorByteForByte(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := RelayMakeToken("gw-instance-1", relayAuthSecret, 0); got != relayAuthConnToken {
		t.Fatalf("token %q != connector vector %q", got, relayAuthConnToken)
	}
}
