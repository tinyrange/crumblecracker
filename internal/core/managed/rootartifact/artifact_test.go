package rootartifact

import "testing"

func TestArtifactCloseRunsCleanup(t *testing.T) {
	called := false
	artifact := Artifact{Cleanup: func() error {
		called = true
		return nil
	}}
	if err := artifact.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !called {
		t.Fatalf("Close did not run cleanup")
	}
}

func TestArtifactCloseWithoutCleanup(t *testing.T) {
	if err := (Artifact{}).Close(); err != nil {
		t.Fatalf("Close without cleanup returned error: %v", err)
	}
}
