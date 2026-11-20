package pyfs

// Ten character mode string of a raw st_mode like stat.filemode from the C _stat module
func Filemode(mode uint32) string {
	b := make([]byte, 10)
	switch mode & 0o170000 {
	case 0o100000:
		b[0] = '-'
	case 0o040000:
		b[0] = 'd'
	case 0o120000:
		b[0] = 'l'
	case 0o060000:
		b[0] = 'b'
	case 0o020000:
		b[0] = 'c'
	case 0o010000:
		b[0] = 'p'
	case 0o140000:
		b[0] = 's'
	default:
		b[0] = '?'
	}
	perm := func(i int, r, w, x, special uint32, set, unset byte) {
		b[i], b[i+1], b[i+2] = '-', '-', '-'
		if mode&r != 0 {
			b[i] = 'r'
		}
		if mode&w != 0 {
			b[i+1] = 'w'
		}
		switch {
		case mode&special != 0 && mode&x != 0:
			b[i+2] = set
		case mode&special != 0:
			b[i+2] = unset
		case mode&x != 0:
			b[i+2] = 'x'
		}
	}
	perm(1, 0o400, 0o200, 0o100, 0o4000, 's', 'S')
	perm(4, 0o040, 0o020, 0o010, 0o2000, 's', 'S')
	perm(7, 0o004, 0o002, 0o001, 0o1000, 't', 'T')
	return string(b)
}
