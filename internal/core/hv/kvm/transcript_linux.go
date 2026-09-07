//go:build linux && (amd64 || arm64)

package kvm

const managedTranscriptErrorLimit = 32 << 10

func boundedManagedTranscript(text string) string {
	if len(text) <= managedTranscriptErrorLimit {
		return text
	}
	return "[earlier transcript omitted]\n" + text[len(text)-managedTranscriptErrorLimit:]
}
