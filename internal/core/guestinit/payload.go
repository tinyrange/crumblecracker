package guestinit

var embeddedPayloads = map[string][]byte{}

func embeddedPayload(goarch string) []byte {
	return embeddedPayloads[goarch]
}
