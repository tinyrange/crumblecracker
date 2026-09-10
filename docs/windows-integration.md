# Windows guest integration

The desktop clipboard transfers Unicode text in both directions. Temporary host
clipboard contention is retried without clearing guest text or stopping the VM.
The native Windows clipboard test runs on the disposable GitHub Actions desktop;
local runs require `CCX3_TEST_WINDOWS_CLIPBOARD=1` because they replace its contents.

Writable NTFS shared folders preserve guest permission bits and UID/GID in the
file's `crumblecracker.unix.v1` alternate data stream. This allows guest-created
files to retain their non-root owner and `chmod +x` to survive reconnects and
renames. Two checksummed JSON records preserve the last complete metadata update.
Updates are serialized across attachments/processes. Metadata follows hard links
and does not appear as another file in the guest folder.

Guest permissions do not change Windows ACLs or turn the host file read-only.
The default mapped guest owner still applies to host files without saved Unix
ownership. An explicit guest `chown` overrides that default for the file. Windows
ACL restrictions and read-only guest mounts continue to apply. Filesystems and
copy tools that do not support or preserve NTFS streams cannot retain this Unix
metadata; unsupported metadata writes return an error instead of claiming success.

Guest file handles allow concurrent access and rename/delete on Windows. The
persistent-home store's exclusive writer lock remains separate: opening the same
home in two VMs is still rejected to protect its data.

These runtime fixes need an updated application. The gedit package and Text
Editor launcher are image changes and need a rebuilt SquadVM image. Existing
user desktop/settings files are retained when the new launcher is installed.
