package pyjson

// utils.safe_json_loads, json.loads of a str or bytes value with def returned for any decode error or non text argument
func SafeLoads(v any, def any) any {
	var (
		out any
		err error
	)
	switch t := v.(type) {
	case string:
		out, err = Loads(t)
	case []byte:
		out, err = LoadsBytes(t)
	default:
		return def
	}
	if err != nil {
		return def
	}
	return out
}
