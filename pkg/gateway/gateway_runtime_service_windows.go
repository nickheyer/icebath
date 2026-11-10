package gateway

import (
	"context"
	"encoding/csv"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() { serviceBackends["windows"] = windowsExistingService }

// Probe attributes of a service manager command, a hidden console on Windows
func hiddenServiceProbeAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000}
}

// Rows of CSV text read leniently like the excel dialect
func lenientCSVRows(text string) ([][]string, error) {
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord, reader.LazyQuotes = -1, true
	return reader.ReadAll()
}

// Registered scheduled task owning home, verified against its definition and this account
func windowsExistingService(ctx context.Context, h string, deadline float64) (*ExistingService, error) {
	suffix, err := ServiceSuffix(ctx, h)
	if err != nil {
		return nil, err
	}
	name := "Icebath_Gateway"
	if suffix != "" {
		name += "_" + suffix
	}
	query := func(refusal string, argv ...string) (string, error) {
		res, _, err := runServiceProbe(ctx, argv, deadline, false)
		if err != nil {
			return "", err
		}
		if res.Returncode != 0 {
			return "", runtimeStartError(refusal)
		}
		return tools.DecodeSchtasksBytes(res.Stdout)
	}
	listing, err := query("service_manager_unavailable", "schtasks.exe", "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return nil, err
	}
	rows, err := lenientCSVRows(listing)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(row) != 3 || strings.TrimLeft(row[0], "\\") != name {
			continue
		}
		unverified := "service_identity_unverified"
		definition, err := query(unverified, "schtasks.exe", "/Query", "/TN", name, "/XML")
		if err != nil {
			return nil, err
		}
		whoami, err := query(unverified, "whoami.exe", "/USER", "/FO", "CSV", "/NH")
		if err != nil {
			return nil, err
		}
		accounts, err := lenientCSVRows(whoami)
		if err != nil {
			return nil, err
		}
		if len(accounts) != 1 || len(accounts[0]) != 2 {
			return nil, runtimeStartError("service_identity_unverified")
		}
		if err := verifyServiceBinding(VerifyWindowsTask(definition, h, accounts[0][0], accounts[0][1]), "profile_mismatch", "service_account_mismatch", "service_disabled"); err != nil {
			return nil, err
		}
		return &ExistingService{Backend: "windows", StartArgv: []string{"schtasks.exe", "/Run", "/TN", name}}, nil
	}
	return nil, nil
}
