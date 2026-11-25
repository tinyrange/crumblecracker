//go:build linux || windows

package desktopapp

import (
	"image"
	"testing"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestExperimentalGPUCheckboxControlsBootRequest(t *testing.T) {
	for _, product := range []struct {
		name    string
		mirrors []string
	}{
		{name: "SquadVM"},
		{name: "NeurodeskAppX", mirrors: []string{"https://cvmfs.example.org"}},
	} {
		t.Run(product.name, func(t *testing.T) {
			viewer := displayViewer{
				chromeEnabled: true,
				settings: startupOptions{
					GPUAccelerationAvailable: platformExperimentalGPUAccelerationAvailable(),
					CVMFSMirrors:             product.mirrors,
				},
			}
			viewer.activateStartupControl(startupControlAdvanced)
			layout := viewer.settingsLayout(960, 760)
			if layout.gpuCheckbox.Empty() {
				t.Fatal("Advanced settings did not expose the experimental GPU checkbox")
			}
			control := advancedControlAt(layout.gpuCheckbox.Min.Add(image.Pt(2, 2)), layout)
			base := client.CreateInstanceRequest{Display: &client.DisplayConfig{Width: 1440, Height: 900}}
			for index, want := range []bool{false, true, false} {
				request := applyStartupOptions(base, viewer.settings)
				if request.Display.Accelerated3D != want {
					t.Fatalf("GPU acceleration after %d toggles = %t, want %t", index, request.Display.Accelerated3D, want)
				}
				viewer.activateStartupControl(control)
			}
		})
	}
}
