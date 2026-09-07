package machine

type Spec struct {
	ID       string
	Guest    string
	Arch     string
	MemoryMB uint64
	CPUs     int
	Dmesg    bool
	Boot     BootSpec
	Control  ControlSpec
	Network  *NetworkSpec
	Devices  []DeviceSpec
}

type BootSpec struct {
	Kind string
}

type ControlSpec struct {
	Kind string
	Port int
}

type NetworkSpec struct {
	GuestIPv4   string
	GatewayIPv4 string
	GatewayMAC  string
	DNSIPv4     string
	MAC         string
	Hostname    string
	Interface   string
}

type DeviceSpec struct {
	Kind   string
	Name   string
	Bus    string
	Slot   uint8
	IOBase uint16
	IRQ    uint8
}
