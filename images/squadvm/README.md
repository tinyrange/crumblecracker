# SquadVM image

SquadVM is a curated Kali Linux desktop for UQ Cyber Squad. It boots systemd,
Xorg, and XFCE against the Glass virtio display and input devices. The image
includes the Glass clipboard and display-resize bridges, a non-root `squad`
user with passwordless sudo, gedit for graphical text editing, and an explicit
security-tool manifest. Open **Text Editor** from the desktop or application menu,
or open a text file from the file manager.
The SSH daemon runs inside the isolated guest with password and root login
disabled. SquadVM's optional host integration installs a dedicated key,
forwards the service on host loopback, and manages the `Host squadvm` block in
`~/.ssh/config`.

The image builds the Rust rewrite of Binwalk from the pinned v3.1.0 release
rather than installing Kali's Binwalk v2 package.

On arm64, run an x86-64 program normally through binfmt:

```sh
./hello
```

Debug it through QEMU's GDB stub:

```sh
qemu-gdb ./hello
```

This opens Pwndbg with the x86-64 target connected. Set breakpoints and use
`continue`; do not use GDB's `run` command for an emulated program.

Build the image from this directory:

```sh
docker build --tag squadvm:dev .
```

Run the native SquadVM frontend from the repository root:

```sh
go run ./cmd/squadvm
```

With no arguments it pulls the host architecture from
`ghcr.io/tinyrange/squadvm:edge`, opens the Glass desktop in a native window,
persists the image home directory, and maps `~/squadvm-shared` on the host to
`/shared` in the guest. Pass another OCI reference as the final argument to
test a locally published image.

On Apple silicon Macs, enable **Experimental GPU acceleration** in SquadVM's
settings before starting the VM. The guest uses Mesa VirGL for OpenGL; the
desktop user belongs to `video` and `render`, and the session retains those
groups so both X11 and direct EGL applications can use the GPU. This requires
an updated SquadVM image. Existing home directories do not need to be reset.
Intel Macs and other hosts retain the software rendering path.

The native top bar includes **Open shared folder**, which opens the currently
configured host share in Finder, Explorer, or the host file manager. The same
control is available in NeurodeskAppX, including windows opened by its headless API.

On macOS, the native app uses a single relative mouse in supported images. Move
into the guest display to control its virtual cursor; crossing an edge releases
the pointer back to the host at that edge. **Lock mouse** keeps movement captured
for games such as Cube 2. **Ctrl + Option** releases it explicitly. Focus loss and
window close also release capture, guest mouse buttons, and held keys.

This mode needs an updated app and image. The app requests
`CCX3_RELATIVE_POINTER=1`; guest init publishes `/run/ccx3-relative-pointer`, and
SquadVM starts Xorg with `xorg-relative.conf`. The image advertises
`/run/user/1000/squadvm-relative-desktop-ready` after the device is ready. Other
hosts, VNC, and older apps retain the default input configuration. No SDL changes
are required.

For an opt-in release check, copy `check-gpu.sh` into the shared directory and
run `sh /shared/check-gpu.sh` from a guest desktop terminal, without sudo.
The image includes `glxinfo` and `eglinfo`; the check requires accelerated VirGL
through both GLX and surfaceless EGL and fails if the desktop user lacks DRM
access. With acceleration disabled, failure is expected. For rendered-frame
coverage, the [Firefox WebGL fixture](../gpu-firefox/README.md) exercises four
scenes and records pixel readbacks. These checks are manual and do not add VM
boots to commit CI.

For opt-in framebuffer capture and guest keyboard or pointer control without
host OS automation, see the shared
[desktop automation API](../../docs/desktop-automation.md).

Import and start it with `cc`:

```sh
docker save --output squadvm.docker.tar squadvm:dev
cc pull squadvm 'docker-archive:squadvm.docker.tar#squadvm:dev'
cc vm start --vnc --network --share ~/squadvm-shared:/shared \
  --display 1280x800 --init systemd --default-user root \
  --memory-mb 2048 --cpus 2 --timeout 5m squadvm squadvm
```

The VNC listener is bound to the host loopback interface. Use the
`display.vnc_address` returned by `cc vm start` from the host or through an SSH
tunnel.

The production publisher should retain the base-image digest in the Dockerfile,
tag SquadVM releases immutably, and create amd64 and arm64 images from the same
recipe.
