package guestinit

import _ "embed"

//go:embed guest-init-linux-arm64
var guestInitLinuxARM64 []byte

//go:embed guest-init-linux-amd64
var guestInitLinuxAMD64 []byte

func init() {
	embeddedPayloads["arm64"] = guestInitLinuxARM64
	embeddedPayloads["amd64"] = guestInitLinuxAMD64
}
