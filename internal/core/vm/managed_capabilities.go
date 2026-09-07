package vm

import managedguest "github.com/tinyrange/crumblecracker/internal/core/managed/guest"

type guestCapabilities = managedguest.Capabilities

type managedCapabilityProvider interface {
	ManagedCapabilities() guestCapabilities
}

func managedCapabilitiesOf(inst Instance) guestCapabilities {
	provider, ok := inst.(managedCapabilityProvider)
	if !ok {
		return guestCapabilities{}
	}
	return provider.ManagedCapabilities()
}
