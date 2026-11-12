package codeexec

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Fails when the source text differs from the hermes constant with the icebath renames applied
func assertSourceSHA256(t *testing.T, text, want string) {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("sha256 %s, want %s", got, want)
	}
}

func TestRunnerCellSourceSHA256(t *testing.T) {
	assertSourceSHA256(t, runnerCellSource, "8fdd807c523ab21678a021630fe3be3c5d2a8a23c20ecebf25105b8e912f4a1d")
}

func TestKernelRunnerSourceSHA256(t *testing.T) {
	assertSourceSHA256(t, kernelRunnerSource, "2eae30dcf4f2d2bd1dea00f40192dd4f3e058768f7ccf676717efd7865c55235")
}

func TestRemoteKernelRunnerTemplateSHA256(t *testing.T) {
	assertSourceSHA256(t, remoteKernelRunnerTemplate, "2a00266224cbc3f4dbb9773a06b4c66e2c9f4da766c4e6bfa244b83e3f0c707c")
}

func TestCommonHelpersSHA256(t *testing.T) {
	assertSourceSHA256(t, commonHelpers, "951d29ac3723f01836efa80b082003f0f7ad79c18636344a8fc08640e133143a")
}

func TestUDSTransportHeaderSHA256(t *testing.T) {
	assertSourceSHA256(t, udsTransportHeader, "96a2aebf10a890c1197a116ca95c31ab704bb2a8f7b123148556616829d013a4")
}

func TestFileTransportHeaderSHA256(t *testing.T) {
	assertSourceSHA256(t, fileTransportHeader, "ad8e3469cbca3c7fe62dc9568f14e71fcb899548e4d25f04dc20dc760024aece")
}
