package pm

// Hash pinned wheel of one target
type Wheel struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Locked nemo-relay wheels of the targets its marker admits, none for darwin x64 or Android
var nemoRelayWheels = map[string]Wheel{
	"darwin-arm64":     {"https://files.pythonhosted.org/packages/49/41/49e6ac6733ef92e0a7a5e84bb5cac17e75e82218b2f55d6166dc4de0e0db/nemo_relay-0.11.0-cp311-abi3-macosx_11_0_arm64.whl", "26bf5b96d23ef39971b8c7721e8d74a78f59498b3b4922ce2b137da52043dcb4"},
	"linux-arm64":      {"https://files.pythonhosted.org/packages/93/5c/eed59ecb29abe4eb0661e6a807efaf24749b3fc9a381449bfc78f94bd34d/nemo_relay-0.11.0-cp311-abi3-manylinux_2_17_aarch64.manylinux2014_aarch64.whl", "b5c4d07ecfa35e09cf64c625c970778d8b0596143473ec29c25a8b33efa5c53c"},
	"linux-x64":        {"https://files.pythonhosted.org/packages/b1/bf/9978ec3f8e5ee0dd1d2c7c1abba5f7358e0ae340d41bf1722097f3f19e1d/nemo_relay-0.11.0-cp311-abi3-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", "146aecca2cb3d9c7490084fac83f838c09588ddce9360c942dcd1f2660d23f21"},
	"linux-arm64-musl": {"https://files.pythonhosted.org/packages/0b/d5/d3c0e5551228e973735c126e74868d4c55fec54aa677aa4eff803778f4e6/nemo_relay-0.11.0-cp311-abi3-musllinux_1_2_aarch64.whl", "e30c0abbf21e010954f11acecd9f0b18c4c735103b3321c4c2834c0711592612"},
	"linux-x64-musl":   {"https://files.pythonhosted.org/packages/02/04/b6c8249a6606e5b13bc2b6b81e8e1c2bba221fd4b2499b134d8fb8b390cd/nemo_relay-0.11.0-cp311-abi3-musllinux_1_2_x86_64.whl", "a4b2e72375a262dceee49ec6996fe2b63282b8ec0b3559a5f815cc02841d710e"},
	"win32-x64":        {"https://files.pythonhosted.org/packages/de/2c/7c7502b9a8454036b11877921fbd49618827fb4e499748b39abd5bba2bac/nemo_relay-0.11.0-cp311-abi3-win_amd64.whl", "344d6fbfd61ce4c9a497fe36080f6a6f5718490b4947cdbcb13772f7c77b3e47"},
	"win32-arm64":      {"https://files.pythonhosted.org/packages/b8/29/938a5c20653f320563f14178485f63c267e0b43d37d87b832b29fe09f14b/nemo_relay-0.11.0-cp311-abi3-win_arm64.whl", "1255077e8efc759f95ee05b568f51783b9542e267c3da0a2295bea14d0021078"},
}

// Locked nemo-relay wheel of a target, false where the hermes marker leaves Relay out
func NemoRelayWheel(target string) (Wheel, bool) {
	w, ok := nemoRelayWheels[target]
	return w, ok
}
