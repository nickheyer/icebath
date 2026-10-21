package app

import "encoding/json"

func marshal(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
