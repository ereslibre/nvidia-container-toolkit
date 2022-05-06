/**
# Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package modifier

import (
	"fmt"
	"os"
	"strings"

	"github.com/NVIDIA/nvidia-container-toolkit/internal/config"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/config/image"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/cuda"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/discover"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/discover/csv"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/edits"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/oci"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/requirements"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

// experiemental represents the modifications required by the experimental runtime
type experimental struct {
	logger     *logrus.Logger
	discoverer discover.Discover
}

const (
	visibleDevicesEnvvar = "NVIDIA_VISIBLE_DEVICES"
	visibleDevicesVoid   = "void"

	nvidiaRequireJetpackEnvvar = "NVIDIA_REQUIRE_JETPACK"
)

// NewExperimentalModifier creates a modifier that applies the experimental
// modications to an OCI spec if required by the runtime wrapper.
func NewExperimentalModifier(logger *logrus.Logger, cfg *config.Config, ociSpec oci.Spec) (oci.SpecModifier, error) {
	rawSpec, err := ociSpec.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load OCI spec: %v", err)
	}

	// In experimental mode, we check whether a modification is required at all and return the lowlevelRuntime directly
	// if no modification is required.
	visibleDevices, exists := ociSpec.LookupEnv(visibleDevicesEnvvar)
	if !exists || visibleDevices == "" || visibleDevices == visibleDevicesVoid {
		logger.Infof("No modification required: %v=%v (exists=%v)", visibleDevicesEnvvar, visibleDevices, exists)
		return nil, nil
	}
	logger.Infof("Constructing modifier from config: %+v", cfg)

	config := &discover.Config{
		Root:                                    cfg.NVIDIAContainerCLIConfig.Root,
		NVIDIAContainerToolkitCLIExecutablePath: cfg.NVIDIACTKConfig.Path,
	}

	var d discover.Discover

	switch resolveAutoDiscoverMode(logger, cfg.NVIDIAContainerRuntimeConfig.DiscoverMode) {
	case "legacy":
		legacyDiscoverer, err := discover.NewLegacyDiscoverer(logger, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create legacy discoverer: %v", err)
		}
		d = legacyDiscoverer
	case "csv":
		// TODO: Once the devices have been encapsulated in the CUDA image, this can be moved to before the
		// visible devices are checked.
		image, err := image.NewCUDAImageFromSpec(rawSpec)
		if err != nil {
			return nil, err
		}

		if err := checkRequirements(logger, &image); err != nil {
			return nil, fmt.Errorf("requirements not met: %v", err)
		}

		csvFiles, err := csv.GetFileList(csv.DefaultMountSpecPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get list of CSV files: %v", err)
		}

		nvidiaRequireJetpack, _ := ociSpec.LookupEnv(nvidiaRequireJetpackEnvvar)
		if nvidiaRequireJetpack != "csv-mounts=all" {
			csvFiles = csv.BaseFilesOnly(csvFiles)
		}

		csvDiscoverer, err := discover.NewFromCSVFiles(logger, csvFiles, config.Root)
		if err != nil {
			return nil, fmt.Errorf("failed to create CSV discoverer: %v", err)
		}

		ldcacheUpdateHook, err := discover.NewLDCacheUpdateHook(logger, csvDiscoverer, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create ldcach update hook discoverer: %v", err)
		}

		createSymlinksHook, err := discover.NewCreateSymlinksHook(logger, csvFiles, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create symlink hook discoverer: %v", err)
		}

		d = discover.NewList(csvDiscoverer, ldcacheUpdateHook, createSymlinksHook)
	default:
		return nil, fmt.Errorf("invalid discover mode: %v", cfg.NVIDIAContainerRuntimeConfig.DiscoverMode)
	}

	return newExperimentalModifierFromDiscoverer(logger, d)
}

// newExperimentalModifierFromDiscoverer created a modifier that aplies the discovered
// modifications to an OCI spec if require by the runtime wrapper.
func newExperimentalModifierFromDiscoverer(logger *logrus.Logger, d discover.Discover) (oci.SpecModifier, error) {
	m := experimental{
		logger:     logger,
		discoverer: d,
	}
	return &m, nil
}

// Modify applies the required modifications to the incomming OCI spec. These modifications
// are applied in-place.
func (m experimental) Modify(spec *specs.Spec) error {
	err := nvidiaContainerRuntimeHookRemover{m.logger}.Modify(spec)
	if err != nil {
		return fmt.Errorf("failed to remove existing hooks: %v", err)
	}

	specEdits, err := edits.NewSpecEdits(m.logger, m.discoverer)
	if err != nil {
		return fmt.Errorf("failed to get required container edits: %v", err)
	}

	return specEdits.Modify(spec)
}

func checkRequirements(logger *logrus.Logger, image *image.CUDA) error {
	if image.HasDisableRequire() {
		// TODO: We could print the real value here instead
		logger.Debugf("NVIDIA_DISABLE_REQUIRE=%v; skipping requirement checks", true)
		return nil
	}

	imageRequirements, err := image.GetRequirements()
	if err != nil {
		//  TODO: Should we treat this as a failure, or just issue a warning?
		return fmt.Errorf("failed to get image requirements: %v", err)
	}

	r := requirements.New(logger, imageRequirements)

	cudaVersion, err := cuda.Version()
	if err != nil {
		logger.Warnf("Failed to get CUDA version: %v", err)
	} else {
		r.AddVersionProperty(requirements.CUDA, cudaVersion)
	}

	compteCapability, err := cuda.ComputeCapability(0)
	if err != nil {
		logger.Warnf("Failed to get CUDA Compute Capability: %v", err)
	} else {
		r.AddVersionProperty(requirements.ARCH, compteCapability)
	}

	return r.Assert()
}

// resolveAutoDiscoverMode determines the correct discover mode for the specified platform if set to "auto"
func resolveAutoDiscoverMode(logger *logrus.Logger, mode string) (rmode string) {
	if mode != "auto" {
		return mode
	}
	defer func() {
		logger.Infof("Auto-detected discover mode as '%v'", rmode)
	}()

	isTegra, reason := isTegraSystem()
	logger.Debugf("Is Tegra-based system? %v: %v", isTegra, reason)

	if isTegra {
		return "csv"
	}

	return "legacy"
}

// isTegraSystem returns true if the system is detected as a Tegra-based system
func isTegraSystem() (bool, string) {
	const tegraReleaseFile = "/etc/nv_tegra_release"
	const tegraFamilyFile = "/sys/devices/soc0/family"

	if info, err := os.Stat(tegraReleaseFile); err == nil && !info.IsDir() {
		return true, fmt.Sprintf("%v found", tegraReleaseFile)
	}

	if info, err := os.Stat(tegraFamilyFile); err != nil || !info.IsDir() {
		return false, fmt.Sprintf("%v not found", tegraFamilyFile)
	}

	contents, err := os.ReadFile(tegraFamilyFile)
	if err != nil {
		return false, fmt.Sprintf("could not read %v", tegraFamilyFile)
	}

	if strings.HasPrefix(strings.ToLower(string(contents)), "tegra") {
		return true, fmt.Sprintf("%v has 'tegra' prefix", tegraFamilyFile)
	}

	return false, fmt.Sprintf("%v has no 'tegra' prefix", tegraFamilyFile)
}