package pysqlite

import (
	"os"

	"modernc.org/libc"
)

// Pushes the process TZ variable into the embedded C library and reruns its tzset, the time.tzset sqlite localtime reads
func Tzset() error {
	tls := libc.NewTLS()
	defer tls.Close()
	name, err := libc.CString("TZ")
	if err != nil {
		return err
	}
	defer libc.Xfree(tls, name)
	if value, ok := os.LookupEnv("TZ"); ok {
		cvalue, err := libc.CString(value)
		if err != nil {
			return err
		}
		defer libc.Xfree(tls, cvalue)
		libc.Xsetenv(tls, name, cvalue, 1)
	} else {
		libc.Xunsetenv(tls, name)
	}
	libc.Xtzset(tls)
	return nil
}
