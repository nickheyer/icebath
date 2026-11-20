package app

import "errors"

// os.ttyname does not exist on Windows, where every call raises
func ttyNameOS(fd int) (string, error) { return "", errors.New("ttyname: not supported on Windows") }
