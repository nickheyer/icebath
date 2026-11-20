package tools

// Definition with the dataclass defaults for every optional field
func NewPlatformAppDef(appID, osFamily, presence string, locations ...PlatformAppLocation) PlatformAppDef {
	return PlatformAppDef{AppID: appID, OSFamily: osFamily, Presence: presence, Locations: locations,
		VersionKind: "none", LivenessKind: "none", LivenessPIDKey: "pid", LivenessURLKey: "http",
		LivenessTokenKey: "token", EndpointPath: "/mcp"}
}
