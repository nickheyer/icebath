package app

import "context"

// Random tip in the active language, empty when the catalog has none
func RandomTip(ctx context.Context) string { return getRandomTip(ctx) }
