package tuigateway

import (
	"encoding/json"
	"errors"
)

// Profile to create and how, a fresh profile with bundled skills unless a clone is asked for, launch keys and logins shared unless turned off
type ProfilesCreateParams struct {
	Profile           *string `json:"profile,omitzero"`
	Name              string  `json:"name"`
	Description       *string `json:"description,omitzero"`
	CloneFrom         *string `json:"clone_from,omitzero"`
	CloneAll          *bool   `json:"clone_all,omitzero"`
	CloneChannels     *bool   `json:"clone_channels,omitzero"`
	NoSkills          *bool   `json:"no_skills,omitzero"`
	NoAlias           *bool   `json:"no_alias,omitzero"`
	Soul              *string `json:"soul,omitzero"`
	Model             *string `json:"model,omitzero"`
	Provider          *string `json:"provider,omitzero"`
	ShareAuth         *bool   `json:"share_auth,omitzero"`
	MirrorCredentials *bool   `json:"mirror_credentials,omitzero"`
	CloneConfig       *bool   `json:"clone_config,omitzero"`
	SyncImports       *bool   `json:"sync_imports,omitzero"`
	Report            *bool   `json:"report,omitzero"`
}

// Whether a create mirrored one item from the launch profile, Shared standing for logins the new profile reads from the root store
type MirroredValue struct {
	Done   bool
	Shared bool
}

// Reports whether the item reached the new profile, copied or shared
func (v MirroredValue) Held() bool { return v.Done || v.Shared }

func (v *MirroredValue) UnmarshalJSON(data []byte) error {
	var done bool
	if err := json.Unmarshal(data, &done); err == nil && string(data) != "null" {
		*v = MirroredValue{Done: done}
		return nil
	}
	var word string
	if err := json.Unmarshal(data, &word); err != nil || word != "shared" {
		return errors.New(`wants true, false or "shared"`)
	}
	*v = MirroredValue{Shared: true}
	return nil
}

func (v MirroredValue) MarshalJSON() ([]byte, error) {
	if v.Shared {
		return json.Marshal("shared")
	}
	return json.Marshal(v.Done)
}

// What a create copied from the launch profile
type ProfileMirrored struct {
	Env            bool          `json:"env"`
	Auth           MirroredValue `json:"auth"`
	ModelInherited bool          `json:"model_inherited"`
	Voice          bool          `json:"voice"`
}

// What profile create prints about a new profile, read on the machine and in the profile scope that made it
type ProfileCreateReceipt struct {
	Path               string                   `json:"path"`
	DisplayPath        string                   `json:"display_path"`
	LaunchLabel        string                   `json:"launch_label"`
	SourceLabel        string                   `json:"source_label"`
	SkillsSynced       *int64                   `json:"skills_synced"`
	SkillsOptedOut     bool                     `json:"skills_opted_out"`
	AliasPath          *string                  `json:"alias_path"`
	AliasCollision     *string                  `json:"alias_collision"`
	Mirrored           map[string]MirroredValue `json:"mirrored"`
	Warnings           []string                 `json:"warnings"`
	ClonedPlugins      []string                 `json:"cloned_plugins"`
	MemoryProvider     *string                  `json:"memory_provider"`
	ChannelsLeftBehind []string                 `json:"channels_left_behind"`
	SharedChannels     []string                 `json:"shared_channels"`
	ServedNow          *bool                    `json:"served_now"`
}

// Receipt a gateway sent, nil unless every field is there with its type and the path is set, fields this build does not know dropped
func ProfileCreateReceiptFromWire(raw any) *ProfileCreateReceipt {
	data, err := wireBytes(raw)
	if err != nil {
		return nil
	}
	value, err := decodeGeneric(data)
	if err != nil {
		return nil
	}
	sent, isObject := value.(map[string]any)
	if !isObject {
		return nil
	}
	known := make(map[string]any, len(sent))
	for _, name := range WireNames[ProfileCreateReceipt]() {
		if item, present := sent[name]; present {
			known[name] = item
		}
	}
	var receipt ProfileCreateReceipt
	if Strict(known, &receipt) != nil || receipt.Path == "" {
		return nil
	}
	return &receipt
}

// Created profile with what was mirrored into it, the receipt present when the caller asked for the report
type ProfilesCreateResult struct {
	Ok          bool                  `json:"ok"`
	Name        string                `json:"name"`
	Path        string                `json:"path"`
	SoulWritten bool                  `json:"soul_written"`
	ModelSet    bool                  `json:"model_set"`
	Mirrored    ProfileMirrored       `json:"mirrored"`
	Warnings    []string              `json:"warnings"`
	Receipt     *ProfileCreateReceipt `json:"receipt,omitzero"`
}

// Creates a profile in the caller's profile scope, mirroring its launch credentials unless told not to
var ProfilesCreate = NewMethod[ProfilesCreateParams, ProfilesCreateResult]("profiles.create")
