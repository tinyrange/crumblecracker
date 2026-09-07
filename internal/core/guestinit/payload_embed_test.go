package guestinit

import "testing"

func TestReleasePayloadsAreEmbedded(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		if err := validateGuestInitPayload(arch, embeddedPayload(arch)); err != nil {
			t.Errorf("%s: %v", arch, err)
		}
	}
}
