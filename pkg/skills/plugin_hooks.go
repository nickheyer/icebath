package skills

// Copy of the skills hub source id alias table the shared metrics field normalizer reads
func SourceIDAliases() map[string]string {
	out := make(map[string]string, len(sourceIDAliases))
	for k, v := range sourceIDAliases {
		out[k] = v
	}
	return out
}
