package vm

import (
	"fmt"
	"os"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

const customKernelFilePrefix = "file:"

type customKernelProvider struct {
	path    string
	modules *alpine.Manager
}

func customKernelPath(value string) (string, bool) {
	value = strings.TrimSpace(value)
	path, ok := strings.CutPrefix(value, customKernelFilePrefix)
	return path, ok && strings.TrimSpace(path) != ""
}

func (p customKernelProvider) ReadKernel() ([]byte, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil, fmt.Errorf("read custom kernel %s: %w", p.path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("custom kernel %s is empty", p.path)
	}
	return data, nil
}

func (p customKernelProvider) PlanModuleLoad(configVars []string, moduleMap map[string]string) ([]alpine.Module, error) {
	if p.modules == nil {
		return nil, nil
	}
	return p.modules.PlanModuleLoad(configVars, moduleMap)
}

func readRuntimeKernel(manager *alpine.Manager, flavor string) ([]byte, error) {
	if path, ok := customKernelPath(flavor); ok {
		return customKernelProvider{path: path}.ReadKernel()
	}
	return manager.ReadKernel()
}

func planRuntimeKernelModules(manager *alpine.Manager, flavor string, configVars []string, moduleMap map[string]string) ([]alpine.Module, error) {
	return manager.PlanModuleLoad(configVars, moduleMap)
}

func applyRuntimeKernelMetadata(config *vmruntime.GuestInitConfig, manager *alpine.Manager, flavor string) error {
	if _, custom := customKernelPath(flavor); custom {
		return nil
	}
	metadata, err := manager.ReadKernelMetadata()
	if err != nil {
		return err
	}
	config.KernelRelease = metadata.Release
	config.ModuleSymvers = metadata.ModuleSymvers
	return nil
}
