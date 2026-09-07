package guest

import "strings"

type Capabilities struct {
	PersistentExec     bool
	StreamingExec      bool
	TTY                bool
	ResizeTTY          bool
	Signals            bool
	CopyIn             bool
	CopyOut            bool
	ArchiveExtract     bool
	Network            bool
	DNS                bool
	PackageManager     string
	DynamicShares      bool
	ShareTransports    []string
	PortForward        bool
	AlternateImageExec bool
	RootSnapshot       bool
	ImageSnapshot      bool
	WritableRootBlock  bool
}

type Profile struct {
	Name      string
	Canonical string
	Aliases   []string
	Caps      Capabilities
}

func (p Profile) Match(image string) bool {
	image = strings.ToLower(strings.TrimSpace(image))
	if image == "" {
		return false
	}
	if image == strings.ToLower(strings.TrimSpace(p.Canonical)) {
		return true
	}
	for _, alias := range p.Aliases {
		if image == strings.ToLower(strings.TrimSpace(alias)) {
			return true
		}
	}
	return false
}
