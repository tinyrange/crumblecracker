package guest

const LinuxImageName = "linux"

var LinuxProfile = Profile{
	Name:      "Linux",
	Canonical: LinuxImageName,
	Aliases:   []string{"alpine", "ubuntu", "debian"},
	Caps: Capabilities{
		PersistentExec:     true,
		StreamingExec:      true,
		TTY:                true,
		ResizeTTY:          true,
		Signals:            true,
		CopyIn:             true,
		CopyOut:            true,
		ArchiveExtract:     true,
		Network:            true,
		DNS:                true,
		DynamicShares:      true,
		ShareTransports:    []string{"virtio-fs"},
		PortForward:        true,
		AlternateImageExec: true,
		RootSnapshot:       true,
		ImageSnapshot:      true,
		WritableRootBlock:  true,
	},
}
