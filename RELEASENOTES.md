# CrumbleCracker v0.10.0

This release adds a headless NeurodeskAppX backend, improves SquadVM graphics and
mouse control on macOS, and adds native Linux ARM64 downloads for both apps.

## NeurodeskAppX integration

- `--headless` starts an authenticated HTTP API without opening a window. It
  reports its loopback address and secure token on stdout.
- The API supports virtualization checks, image and kernel pulls, configurable
  VM startup, desktop window creation, and graceful shutdown.
- Pull progress identifies the current downloads and reports bytes, download
  rates, and estimated time remaining, including kernel preparation.
- See the [headless API specification](https://github.com/tinyrange/crumblecracker/blob/v0.10.0/docs/headless.md)
  for request and response formats and lifecycle behavior. The existing
  Neurodesk desktop image remains compatible; no image update is required for
  the headless API.

## Desktop improvements

- SquadVM's updated image enables experimental OpenGL acceleration for the
  normal desktop user on Apple Silicon macOS and includes the gedit editor.
  Enable **Experimental GPU acceleration** in Advanced settings before starting.
- Relative mouse input improves games such as Cube2. Moving through a display
  edge releases the pointer to the host; the toolbar can force mouse capture
  when a game needs continuous movement.
- Both apps have consistently sized controls on the right of the top bar,
  including a button to open the configured shared folder on the host.
- Windows shared-folder permissions allow guest users other than root to write
  files. Windows clipboard handling is also improved.

## Downloads and upgrading

Both apps are available for macOS Apple Silicon, Windows x64, Linux x64, and
Linux ARM64. Linux requires KVM; Windows requires Windows Hypervisor Platform.
macOS downloads are signed and notarized.

Close the app before replacing it. Preserve its settings, shared folders, and
persistent home data. Existing VM names, homes, and data locations are unchanged.
Update the SquadVM desktop image to receive its GPU permissions and editor fixes.
The tested image is also available as `ghcr.io/tinyrange/squadvm:v0.10.0` and
`ghcr.io/tinyrange/squadvm:v0.10.0-estargz`.

Download from the [CrumbleCracker website](https://tinyrange.github.io/crumblecracker/)
and verify the original download filenames against `checksums.txt` below.
