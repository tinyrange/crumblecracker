# CrumbleCracker

The in-process Linux VM runtime behind SquadVM and NeurodeskAppX.

The production repository currently lives at `tinyrange/vmsh`; its rename to
CrumbleCracker is separate from this source split. The release feed stays on
`tinyrange/vmsh` until that rename.

This is an independent production codebase, condensed from cc. It contains the
two desktop apps, their shared frontend, and the runtime needed to boot Linux
images, preserve user storage, share files, run guest commands, and present the
desktop. Apple Silicon GPU acceleration uses the first-party VirGL renderer and
shared OpenGL textures.

The experimental runtime, full daemon and worker system, interactive vmsh shell,
and other frontends remain in [cc](https://github.com/tinyrange/cc). This repository
has no cc submodule or dependency. There is no synchronization policy or promise
of source compatibility. A standalone daemon and Python frontend are future work.

## Build

Install the Go version specified in `go.mod`, then run:

```sh
go run ./tools/build.go
```

This builds `build/SquadVM` and `build/NeurodeskAppX` (with `.exe` on Windows),
including Linux guest init payloads. macOS development binaries are ad-hoc signed
with the hypervisor entitlement. Supported desktop hosts are Apple Silicon macOS,
Linux amd64, and Windows amd64. Linux requires KVM; Windows requires Windows
Hypervisor Platform.

For isolated development, pass `-cache-dir` and `-storage` pointing to development
directories. Existing product settings, shared folders, persistent homes, bundle
identifiers, and image names are preserved during this source split.

## Checks and releases

Commit CI runs focused product/runtime tests, native Darwin GPU tests, and builds
for the three supported hosts. It does not download guest images or boot VMs.
Image publishing and signed/notarized desktop releases are explicit workflows.
Release signing credentials must be configured in the destination repository.

Before releasing a runtime change, smoke-test both apps with an isolated cache:
fresh boot, a guest command, shared-folder writes, restart with persistent data,
startup cancellation/retry, and window resize/close. Check GPU and software display
when changing presentation. Use the existing GPU fixtures for renderer changes;
long CTS runs are not part of routine commit CI.

## Source origin

Derived from vmsh `934c6888481dd1e58e8623f09641a34d6735e9e8` and cc
`c923785e542861318c4feac15e9b3a5a0bc8d183`. Original license text is retained in
`LICENSE` and `LICENSE-CC`. Gowin is a pinned Go module dependency.

A repeatable headless smoke check is also available:

```sh
go build -o build/product-smoke ./tools/smoke
# On macOS, first sign with tools/entitlements.xml, as tools/build.go does.
build/product-smoke -cache-dir build/smoke/cache -storage build/smoke/shared -image testdata/alpine-arm64.simg
```

Use `testdata/alpine.simg` on amd64 hosts. The check boots a named VM, verifies
guest command deadlines and exit status, writes through the host share, then
stops and restarts the VM and checks the saved contents.
