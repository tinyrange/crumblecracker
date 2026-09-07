package hvf

import "testing"

func TestExtractCommandResultAcceptsExitOnlyOneShotTranscript(t *testing.T) {
	transcript := "[    0.100000] boot noise\n" +
		"ccx3-init: +42ms changing workdir\n" +
		"stdout line\n" +
		"stderr line\n" +
		commandExitMarkerPref + "7\n" +
		"[    0.200000] reboot: Power down\n"

	code, output, ok := extractCommandResult(transcript, false)
	if !ok {
		t.Fatalf("extractCommandResult did not accept exit-only transcript")
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if output != "stdout line\nstderr line" {
		t.Fatalf("output = %q, want command output only", output)
	}
}

func TestValidateGuestUserAcceptsNamesAndNumericIDs(t *testing.T) {
	valid := []string{"", "root", "nobody", "nobody:nogroup", "65534", "65534:65534", "nobody:65534", "65534:nogroup"}
	for _, user := range valid {
		if err := validateGuestUser(user); err != nil {
			t.Errorf("validateGuestUser(%q): %v", user, err)
		}
	}
	invalid := []string{":", "nobody:", ":nogroup", "nobody:nogroup:extra", "4294967296"}
	for _, user := range invalid {
		if err := validateGuestUser(user); err == nil {
			t.Errorf("validateGuestUser(%q) accepted invalid user", user)
		}
	}
}
