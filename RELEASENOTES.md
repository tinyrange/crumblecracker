# CrumbleCracker v0.9.0

NeurodeskAppX and SquadVM now use an independent, reduced Linux runtime that
runs inside each app. CrumbleCracker is the new name of the former vmsh
repository. This release contains the two desktop apps; the experimental vmsh
shell and daemon now live in [cc](https://github.com/tinyrange/cc).

## Desktop improvements

- Sharper startup, settings, and notification text on high-density displays,
  with improved startup spacing and panel widths.
- Experimental OpenGL acceleration on Apple Silicon macOS. Enable
  **Experimental GPU acceleration** under **Advanced** before starting.
  Update the desktop image as well as the app; container acceleration applies
  only to supported application versions. This does not provide CUDA support.
  Turn the option off and restart if an application renders incorrectly.
- Default VM names are `squadvm` and `neurodesk`.

## Upgrading

Download the app for your platform from the
[CrumbleCracker downloads page](https://tinyrange.github.io/crumblecracker/).
Close the running app before replacing it. Keep its data folders when upgrading.

Existing settings, shared folders, application data locations, and macOS bundle
identities are preserved. NeurodeskAppX reuses an existing `ndappx` persistent
home when no `neurodesk` home exists. It does not move or delete either home.
If both exist, the `neurodesk` home remains selected; use `--home ndappx` to
open the older home explicitly. Custom `--name` and `--home` selections are
unchanged.

Older desktop releases can still check for updates through GitHub's redirect
from `tinyrange/vmsh`. Updates open the app download in your browser; replace
the app manually. Keep the `NeurodeskAppX` and `SquadVM` download filenames when
checking release checksums. macOS downloads are signed and notarized.
