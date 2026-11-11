package gatewayrun

// Salted log safe hash of an adapter's credential, empty when none is discoverable
func AdapterCredentialFingerprint(adapter any) string { return adapterCredentialFingerprint(adapter) }
