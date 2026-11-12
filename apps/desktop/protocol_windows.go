//go:build windows

package main

import (
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

type protocolRegistrar struct{}

// SetAsDefaultProtocolClient registers the scheme under HKCU Software Classes as Electron does
func (protocolRegistrar) SetAsDefaultProtocolClient(protocol, execPath string, args []string) error {
	exe := execPath
	if exe == "" {
		var err error
		if exe, err = executablePath(); err != nil {
			return err
		}
	}
	quoted := []string{strconv.Quote(exe)}
	for _, a := range args {
		quoted = append(quoted, strconv.Quote(a))
	}
	quoted = append(quoted, `"%1"`)
	root, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\`+protocol, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.SetStringValue("", "URL:"+protocol); err != nil {
		return err
	}
	if err := root.SetStringValue("URL Protocol", ""); err != nil {
		return err
	}
	cmd, _, err := registry.CreateKey(root, `shell\open\command`, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer cmd.Close()
	return cmd.SetStringValue("", strings.Join(quoted, " "))
}
