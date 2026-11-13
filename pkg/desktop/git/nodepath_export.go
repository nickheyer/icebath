package git

// NodeNormalize is path.normalize of Node on the running platform
func NodeNormalize(p string) string { return hostPath.normalize(p) }

// NodeJoin is path.join of Node on the running platform
func NodeJoin(parts ...string) string { return hostPath.join(parts...) }

// NodeDirname is path.dirname of Node on the running platform
func NodeDirname(p string) string { return hostPath.dirname(p) }
