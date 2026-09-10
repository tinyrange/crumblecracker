package desktopapp

// Reads report contention separately from a genuinely empty clipboard.
// All methods run on the native window's UI thread.
type hostClipboard interface {
	ReadText() (string, error)
	SetText(string) error
	Close()
}
