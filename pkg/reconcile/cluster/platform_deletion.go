package cluster

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

type platformDeletionKey struct{}
type platformDeletion struct {
	seen  bool
	state *platform
}

func withPlatformDeletion(ctx context.Context) context.Context {
	return context.WithValue(ctx, platformDeletionKey{}, &platformDeletion{})
}

// Keep deletion phases in the pass until remember persists them. This also
// handles provider objects that disappear before a second observation.
func recordPlatformDeletion(ctx context.Context, row *model.Cluster, gone bool) {
	holder, ok := ctx.Value(platformDeletionKey{}).(*platformDeletion)
	if !ok {
		return
	}
	previous := storedPlatform(row)
	next := &platform{release: previous.release}
	for _, component := range previous.components {
		if gone {
			continue
		}
		current := proto.CloneOf(component)
		current.Phase = v1.ComponentPhase_COMPONENT_PHASE_DELETING
		current.Message = removingMessage
		next.components = append(next.components, current)
	}
	holder.seen, holder.state = true, next
}
