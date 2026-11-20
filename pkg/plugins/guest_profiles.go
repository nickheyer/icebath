package plugins

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Export a plugin module answers its model provider profiles through
const guestProfilesExport = "icebath_profiles"

// Profiles document of a WebAssembly plugin, read from a throwaway instance that reaches no host service
func wasmGuestProfiles(ctx context.Context, spec *guestSpec) ([]byte, error) {
	entry, err := guestEntryPath(spec)
	if err != nil {
		return nil, err
	}
	code, err := os.ReadFile(entry)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, hostExtractTimeout)
	defer cancel()
	runtime := wazero.NewRuntimeWithConfig(runCtx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(spec.memoryPages))
	defer runtime.Close(context.Background())
	if _, err := wasi_snapshot_preview1.Instantiate(runCtx, runtime); err != nil {
		return nil, err
	}
	if _, err := runtime.NewHostModuleBuilder("icebath").
		NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint32, uint32) uint64 { return 0 }).Export("host_call").
		NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint32, uint32) {}).Export("host_read").
		NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint64) uint32 { return 0 }).Export("cancelled").
		Instantiate(runCtx); err != nil {
		return nil, err
	}
	compiled, err := runtime.CompileModule(runCtx, code)
	if err != nil {
		return nil, fmt.Errorf("plugin '%s' module %s does not compile: %w", spec.name, spec.entry[0], err)
	}
	if _, ok := compiled.ExportedFunctions()[guestProfilesExport]; !ok {
		return nil, fmt.Errorf("plugin '%s' module %s exports no %s; rebuild it against the current pkg/pluginsdk", spec.name, spec.entry[0], guestProfilesExport)
	}
	module, err := runtime.InstantiateModule(runCtx, compiled, wazero.NewModuleConfig().WithName(spec.name).
		WithStartFunctions("_initialize").WithStdout(io.Discard).WithStderr(io.Discard).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().WithRandSource(rand.Reader))
	if err != nil {
		return nil, fmt.Errorf("plugin '%s' module %s failed to start: %w", spec.name, spec.entry[0], err)
	}
	packed, err := module.ExportedFunction(guestProfilesExport).Call(runCtx)
	if err != nil {
		return nil, err
	}
	if len(packed) != 1 {
		return nil, fmt.Errorf("plugin '%s' module %s answered no profiles document", spec.name, spec.entry[0])
	}
	view, ok := module.Memory().Read(uint32(packed[0]>>32), uint32(packed[0]))
	if !ok {
		return nil, fmt.Errorf("plugin '%s' module %s answered a profiles document outside its memory", spec.name, spec.entry[0])
	}
	return append([]byte(nil), view...), nil
}
