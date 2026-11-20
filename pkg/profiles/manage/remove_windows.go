package manage

// Reports a lock as held on Windows, where a sharing violation is a live holder
func lockUnheldReadOnly(string) (bool, error) { return false, nil }
