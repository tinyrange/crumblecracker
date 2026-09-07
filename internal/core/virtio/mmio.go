package virtio

// MMIODevice is the transport boundary used by hypervisors to route a guest
// MMIO exit. Keeping dispatch generic means optional devices do not expand
// every VM run-loop signature and switch.
type MMIODevice interface {
	Contains(addr uint64, size int) bool
	Read(addr uint64, size int) (uint64, error)
	Write(addr uint64, size int, value uint64) error
}
