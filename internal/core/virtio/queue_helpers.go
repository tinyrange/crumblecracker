package virtio

func alignVirtio(value, alignment uint64) uint64 {
	if alignment == 0 {
		return value
	}
	return (value + alignment - 1) & ^(alignment - 1)
}
