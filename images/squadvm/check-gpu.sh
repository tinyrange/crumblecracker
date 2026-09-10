#!/bin/sh
# Manual release check: run from a SquadVM desktop terminal with GPU enabled.
set -eu

if [ "$(id -u)" = 0 ]; then
    echo "Run this check as the desktop user, without sudo." >&2
    exit 1
fi

render_node=
for node in /dev/dri/renderD*; do
    if [ -r "$node" ] && [ -w "$node" ]; then
        render_node=$node
        break
    fi
done
if [ -z "$render_node" ]; then
    echo "The desktop user cannot read and write a DRM render node." >&2
    exit 1
fi

glx=$(glxinfo -B)
printf '%s\n' "$glx"
printf '%s\n' "$glx" | grep -Eq '^OpenGL renderer string: .*virgl'
printf '%s\n' "$glx" | grep -Eq 'Accelerated: yes'

# Unlike X11, surfaceless EGL must open the render node itself. This catches
# accidental software fallback caused by missing desktop session groups.
egl=$(eglinfo -B -p surfaceless)
printf '%s\n' "$egl"
printf '%s\n' "$egl" | grep -Eq '^OpenGL core profile renderer: .*virgl'
echo "PASS: non-root DRM access, accelerated GLX, and accelerated surfaceless EGL"
