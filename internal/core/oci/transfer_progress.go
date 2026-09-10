package oci

import (
	"context"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

type metadataProgressKey struct{}

func metadataProgress(ctx context.Context, id, kind, state string, completed, total, network int64) {
	if report, ok := ctx.Value(metadataProgressKey{}).(func(client.ProgressEvent)); ok && report != nil {
		report(client.ProgressEvent{Transfer: &client.TransferProgress{ID: id, Kind: kind, State: state, Completed: completed, Total: total, NetworkBytes: network}})
	}
}
