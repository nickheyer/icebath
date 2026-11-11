package backup

import "testing"

func TestBackupSecretNamesIncludeVaultFiles(t *testing.T) {
	for _, name := range []string{"vault.key", "vault.json.enc"} {
		if !secretFileNames[name] {
			t.Fatalf("%s is not a backup secret file name", name)
		}
	}
}
