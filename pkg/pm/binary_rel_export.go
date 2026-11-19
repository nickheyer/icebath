package pm

// Relative binary path of a binary package for a target, empty when the package ships none there
func (b *BinaryPackage) Rel(target string) string { return b.rel(target) }
