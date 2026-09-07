//go:build windows && amd64

package whp

import "github.com/tinyrange/crumblecracker/internal/protocol"

func emitManagedBootStatus(onEvent func(client.BootEvent) error, message string) error {
	if onEvent == nil {
		return nil
	}
	return onEvent(client.BootEvent{Kind: "status", Message: message})
}
