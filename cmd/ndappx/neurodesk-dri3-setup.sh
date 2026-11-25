# The image wrapper remains the compatibility path. Replace only its known
# launcher links in this guest's writable image layer.
runtime=/usr/local/libexec/neurodesktop-container-runtime
dir=/run/neurodesktop/crumblecracker-dri3
install -d -m 755 "$dir" "$dir/direct" "$dir/fallback"
cat > "$dir/direct/runtime" <<'CC_DRI3_RUNTIME'
#!/bin/bash
set -eu

runtime=$0
runtime="${runtime##*/}"
dir=/run/neurodesktop/crumblecracker-dri3
fallback="$dir/fallback/$runtime"
manifest=/etc/neurodesktop/gpu-containers.tsv
args=("$@")
operation=0
case "${args[0]:-}" in --silent|--debug) operation=1;; esac
if [ "${args[$operation]:-}" != exec ] || [ -z "${DISPLAY:-}" ] ||
   [ "${NEURODESKTOP_GPU_MANIFEST:-$manifest}" != "$manifest" ] ||
   [ ! -c /dev/dri/renderD128 ]; then
    exec "$fallback" "$@"
fi

case ":${LIBGL_DRI3_DISABLE:-}:${LIBGL_ALWAYS_SOFTWARE:-}:${LIBGL_ALWAYS_INDIRECT:-}:" in
    *:1:*|*:true:*|*:yes:*|*:on:*) exec "$fallback" "$@";;
esac
case "${NEURODESKTOP_GPU_ACCELERATION:-}" in
    1|true|yes|on) ;;
    *) [ -e "${NEURODESKTOP_GPU_MARKER:-/run/neurodesktop/gpu-enabled}" ] || exec "$fallback" "$@";;
esac

for ((i=operation+1; i<${#args[@]}; i++)); do
    case "${args[i]}" in
        *.sif|*.simg) break;;
    esac
done
image="${args[i]:-}"
case "$image" in
    /cvmfs/neurodesk.ardc.edu.au/containers/*/*.sif|/cvmfs/neurodesk.ardc.edu.au/containers/*/*.simg) ;;
    *) exec "$fallback" "$@";;
esac
container="${image##*/}"
stem="${container%.*}"
if ! awk -v stem="$stem" '
    NF >= 2 && $1 !~ /^#/ {
        key = $1 "_" $2
        if (stem == key || index(stem, key "_") == 1) found=1
    }
    END { exit !found }
' "$manifest"; then
    exec "$fallback" "$@"
fi

before_image=("${args[@]:0:i}" --bind /dev/dri)
# Remember a successful test for the same image, display, and launch options for
# this login. A failed test is retried the next time the tool is launched.
cached_check=
display_number="${DISPLAY#unix:}"
display_number="${display_number#:}"
display_number="${display_number%%.*}"
if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -O "$XDG_RUNTIME_DIR" ] &&
   identity=$(stat -Lc '%d:%i:%s:%Y' "$image") &&
   display_socket=$(stat -Lc '%d:%i:%Y' "/tmp/.X11-unix/X$display_number" 2>/dev/null) &&
   mkdir -p -m 700 "$XDG_RUNTIME_DIR/crumblecracker-dri3" &&
   key=$(printf '%s\0' "$image" "$identity" "$runtime" "$DISPLAY" "$display_socket" "${XAUTHORITY:-}" \
         "${APPTAINER_BINDPATH:-}" "${SINGULARITY_BINDPATH:-}" "${before_image[@]}" | sha256sum); then
    cached_check="$XDG_RUNTIME_DIR/crumblecracker-dri3/${key%% *}"
fi

# Use the stock runtime for both invocations. An empty allowlist suppresses the
# relay for this launch only, including when the desktop GPU marker exists.
if { [ -n "$cached_check" ] && [ -f "$cached_check" ]; } ||
    NEURODESKTOP_GPU_MANIFEST=/dev/null timeout --kill-after 2s 20s \
    "$fallback" "${before_image[@]}" \
    --bind "$dir/probe.py:/tmp/crumblecracker-dri3.py:ro" \
    "$image" python3 /tmp/crumblecracker-dri3.py >/dev/null 2>&1; then
    if [ -n "$cached_check" ]; then
        touch "$cached_check" || true
    fi
    export NEURODESKTOP_GPU_MANIFEST=/dev/null
    exec "$fallback" "${before_image[@]}" "${args[@]:i}"
fi
echo "neurodesktop: direct DRI3 is unavailable for $container; using the image's graphics launcher" >&2
exec "$fallback" "$@"
CC_DRI3_RUNTIME

cat > "$dir/probe.py" <<'CC_DRI3_PROBE'
"""Verify hardware GLX using the application container's graphics libraries."""

import ctypes as c
import sys


def supports_direct_virgl():
    x11 = c.CDLL("libX11.so.6")
    gl = c.CDLL("libGL.so.1")
    pointer, integer, xid = c.c_void_p, c.c_int, c.c_ulong

    def bind(library, name, result, *args):
        function = getattr(library, name)
        function.restype = result
        function.argtypes = args
        return function

    display = bind(x11, "XOpenDisplay", pointer, c.c_char_p)(None)
    if not display:
        return False
    screen = bind(x11, "XDefaultScreen", integer, pointer)(display)
    query = bind(x11, "XQueryExtension", integer, pointer, c.c_char_p, *([c.POINTER(integer)] * 3))
    if not query(display, b"DRI3", c.byref(integer()), c.byref(integer()), c.byref(integer())):
        return False

    # GLX_DRAWABLE_TYPE = GLX_PBUFFER_BIT, GLX_RENDER_TYPE = GLX_RGBA_BIT.
    config_attributes = (integer * 5)(0x8010, 0x4, 0x8011, 0x1, 0)
    count = integer()
    choose = bind(gl, "glXChooseFBConfig", c.POINTER(pointer),
                  pointer, integer, c.POINTER(integer), c.POINTER(integer))
    configs = choose(display, screen, config_attributes, c.byref(count))
    if not configs or not count.value:
        return False
    config = configs[0]
    create_context = bind(gl, "glXCreateNewContext", pointer, pointer, pointer, integer, pointer, integer)
    context = create_context(display, config, 0x8014, None, 1)  # GLX_RGBA_TYPE
    direct = bind(gl, "glXIsDirect", integer, pointer, pointer)
    if not context or not direct(display, context):
        return False

    attributes = (integer * 5)(0x8040, 16, 0x8041, 16, 0)  # height, width
    create_buffer = bind(gl, "glXCreatePbuffer", xid, pointer, pointer, c.POINTER(integer))
    buffer = create_buffer(display, config, attributes)
    make_current = bind(gl, "glXMakeContextCurrent", integer, pointer, xid, xid, pointer)
    if not buffer or not make_current(display, buffer, buffer, context):
        return False

    string = bind(gl, "glGetString", c.c_char_p, c.c_uint)
    renderer = (string(0x1F01) or b"").lower()  # GL_RENDERER
    return renderer.startswith(b"virgl (") and not renderer.startswith(b"virgl (virgl (")


sys.exit(0 if supports_direct_virgl() else 1)
CC_DRI3_PROBE

chmod 755 "$dir/direct/runtime"
chmod 644 "$dir/probe.py"
for name in apptainer singularity; do
    ln -sfn "$runtime" "$dir/fallback/$name"
done
for name in apptainer singularity; do
    launcher=/usr/local/bin/$name
    if [ -L "$launcher" ] && [ "$(readlink -f "$launcher")" = "$runtime" ]; then
        ln -sfn "$dir/direct/runtime" "$launcher"
    fi
done
