# NeurodeskAppX headless backend

`NeurodeskAppX --headless [--cache-dir /absolute/cache]` exposes the local
control API for a selectable backend in `neurodesk/neurodesk-app`. The executable
owns one active VM and one native Glass window. It does not initially open a
window, display settings, launch a browser, pull an image, or boot a VM.

The machine-readable contract is [OpenAPI 3.1](headless.openapi.json).
The originating design is [issue #275](https://github.com/tinyrange/crumblecracker/issues/275).
The Electron selector/adapter lives in the upstream repository and is a separate
integration change; this repository supplies the executable and API.

## Launch and ownership

Spawn the executable directly, using an argument array and piped stdin/stdout/
stderr. Keep stdin open for the session. Parse a complete newline from stdout;
pipe reads can split or combine data arbitrarily. The sole stdout record is:

```json
{"event":"ready","protocol":"ndappx","api_version":1,"base_url":"http://127.0.0.1:49152","token":"<43-character-base64url-secret>","pid":12345}
```

Validate the protocol, version and loopback URL before using them. Startup errors
produce a nonzero exit and stderr diagnostics, without a ready record. Every
process generates a fresh 256-bit token and binds one OS-assigned IPv4 loopback
port. The token belongs in `Authorization: Bearer <token>` on **every** request.
Keep it in Electron's main process, out of renderer IPC, URLs, settings, and logs.
The actual bound address/port must be the request Host. Browser Origin requests,
queries, redirects and CORS access are not supported.

JSON bodies are limited to 1 MiB and must be objects without unknown fields,
duplicate keys, or null values. Mutations require an `Idempotency-Key` UUID.
Repeating the same key/method/path/JSON replays the original response; changing
its request returns 409. A disconnected client does not cancel accepted work.
Retain the key until the caller has reconciled the result. Failed shutdown retries
use a new key. IDs and replay/operation records are scoped to the child lifetime.

## Session sequence

1. `GET /v1/info` and `GET /v1/virtualization`. A negative virtualization check is
   a successful HTTP response with `accessible:false` and a machine-readable reason.
2. `POST /v1/images/pull` with an OCI tag or sha256 digest reference. Image and
   kernel preparation run concurrently, with kernel packages/modules prepared
   before success. `refresh` resolves a tag again; boot uses its immutable revision.
3. Poll the returned `Location` (`GET /v1/operations/{operation_id}`) every 500 ms.
   A 202 acceptance is not completion. Wait for `succeeded`, `failed`, or `cancelled`.
4. `POST /v1/vms` using the returned `image_id` and explicit saved session options.
   Wait for its operation to succeed. This boots guest control without Glass.
5. `POST /v1/vms/{vm_id}/glass` starts the guest desktop and opens the executable's
   native window on the platform UI thread. Its operation succeeds after a desktop
   frame has actually been presented. It returns a native-window session, not a
   Jupyter URL. Poll VM status for window closure or guest failure.
6. Closing Glass detaches the window and releases its frame/input state. The VM
   remains running; another spawn reopens the same desktop. Stop via
   `POST /v1/vms/{vm_id}/stop`, or shut down the entire child using
   `POST /v1/shutdown`.

Example Start VM body (substitute your prepared image ID and host paths):

```json
{
  "image_id":"img_1",
  "name":"neurodesk",
  "memory_mib":8192,
  "cpus":4,
  "user":"jovyan",
  "home":{"mode":"persistent","id":"neurodesk"},
  "storage":{"host_path":"/Users/example/neurodesktop-storage","create":true},
  "shares":[{"host_path":"/Users/example/project","guest_path":"/data","read_only":false}],
  "network":{"enabled":true,"allow_internet":true},
  "display":{"width":1440,"height":900,"gpu_acceleration":false},
  "cvmfs":{"enabled":true,"mirror":"auto","cache_limit_bytes":5368709120},
  "boot_timeout_seconds":600,
  "env":{"NEURODESKTOP_VERSION":"2026-07-11","GRANT_SUDO":"true"}
}
```

Values in `env` are strings, including booleans and dates. They reach guest
startup services through transient systemd environment overrides, and are passed
as structured values without shell interpolation. `CCX3_` and `VMSH_` prefixes
are reserved for runtime control. Environment values are returned in authenticated
VM configuration; do not log or forward that configuration indiscriminately.
Setting a variable cannot add a behavior that an image does not implement.

Host-managed CVMFS (`enabled:true`) supplies `CVMFS_DISABLE="true"` and
`NEURODESKTOP_CVMFS_STARTUP_MODE="external"`. Explicit conflicting values fail.
With `enabled:false`, omit host mirror/cache options; guest-managed settings
belong in `env`, and eager startup needs networking and a compatible image.
Existing Glass image scripts may mount CVMFS independently of Jupyter's startup
mode; use host-managed CVMFS for the current product configuration.

Primary storage keeps the product's `/vmsh-neurodesktop-storage` mapping. Additional
host folders use `shares`, including `/data`. Persistent homes keep the existing
identity, migration and ownership rules; session overrides do not write GUI
settings or reset existing data. Home mode `ephemeral` discards that session's
home changes only. Existing Docker volumes are not automatically migrated.

## Progress

Pull operations expose a complete snapshot with labelled artifacts, compressed
source bytes completed/total, rates in bytes/second, and download ETA in seconds.
Image layers and kernel packages have independent states and rates. Repository
metadata/dependencies are included. Cache entries remain visible as `cached`
and contribute zero new transfer bytes. Rates use a rolling five-second window;
no received bytes for five seconds produces a zero rate and unknown ETA.

Totals and ETA are null while dependency discovery or sizes are incomplete, or
when there is no usable throughput estimate. Aggregate download ETA divides
remaining bytes by aggregate throughput, rather than adding parallel ETAs.
It excludes verification, indexing, decompression and guest boot. Download ETA
can reach zero while the operation remains `preparing`; only `succeeded` means
both the image and kernel are ready. Polling recomputes rates even during stalls.

The initial implementation uses full layer preparation for new downloads and
reuses cached indexed/enhanced layers. It does not claim that future CVMFS content
has already been downloaded. GPU acceleration is currently advertised as false:
the accelerated renderer requires a window share context during boot. Requesting
it fails explicitly without creating a hidden window or silently downgrading.

## Shutdown and failures

VM stop closes/detaches Glass before asking systemd to power off the guest. It
waits for guest exit before releasing runtime/storage resources. Failure leaves
ownership and status visible for a retry; no force flag or cache deletion is part
of the API. Failed boot attempts clean up only their own resources.

`POST /v1/shutdown` accepts `{"timeout_seconds":30}` (1–300). It drains/cancels
jobs, gracefully stops the guest, and flushes `{"state":"stopped"}` before
closing HTTP and exiting 0. Confirm process exit as well as the response. A 504
means cleanup is incomplete: reads remain available, new work is rejected, and
shutdown can be retried. Connection loss alone is not proof of graceful shutdown.

Stdin EOF or SIGINT/SIGTERM triggers bounded cleanup. On Windows use HTTP for
normal shutdown; headless startup preserves inherited pipes instead of attaching
the parent console. An orphan cleanup failure exits nonzero. Forced termination
or host failure cannot guarantee guest-graceful persistence.

## Image and application releases

Core headless boot/Glass support does **not** require rebuilding the existing
Neurodesktop Glass image. The application embeds guest init, which creates a
transient `ccx3-headless.target` and environment drop-ins under `/run`. It then
explicitly starts the image's existing `neurodesktop-glass.service` on demand.
Ship an updated application build, including regenerated guest-init payloads.
Images must contain systemd and the compatible Glass service; requests cannot
make an arbitrary container image desktop-compatible. Changes to guest scripts'
interpretation of additional environment variables need their own image release.

For isolated real-VM verification, compile the desktopapp test binary, sign it
with `tools/entitlements.xml` on macOS, and run `TestHeadlessGuestIntegration` with
`NDAPPX_HEADLESS_TEST_CACHE` and `NDAPPX_HEADLESS_TEST_IMAGE` pointing to a prepared
**development-only** cache/image. It checks windowless boot, exact guest environment
values, `/data` writes, guest-graceful shutdown and persistent home across restart.
Never point that test at the user's live cache or storage. Commit CI uses fast
controller/progress/filesystem tests, without VM downloads or boot.

Set `NDAPPX_HEADLESS_TEST_DESKTOP=1` to also start the guest desktop and check
its startup environment. Set `NDAPPX_HEADLESS_TEST_EPHEMERAL=1` to check that home
changes are discarded across restarts instead of retained.
