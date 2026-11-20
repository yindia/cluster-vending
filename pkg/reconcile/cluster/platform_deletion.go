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
	// recovered are the components whose stuck Release was recovered in
	// this pass, by name: their counts are kept in the status.
	recovered map[string]*v1.PlatformComponent
}

func withPlatformDeletion(ctx context.Context) context.Context {
	return context.WithValue(ctx, platformDeletionKey{}, &platformDeletion{})
}

// recoveryComponent is the component a recovery during the deletion counts
// on: as the status recorded it (a recovery is limited per component), or
// a new one.
func recoveryComponent(ctx context.Context, row *model.Cluster, name string) *v1.PlatformComponent {
	if holder, ok := ctx.Value(platformDeletionKey{}).(*platformDeletion); ok {
		if c := holder.recovered[name]; c != nil {
			return c
		}
	}
	for _, c := range storedPlatform(row).components {
		if c.GetName() == name {
			return proto.CloneOf(c)
		}
	}
	return &v1.PlatformComponent{Name: name}
}

// noteRecovery keeps a recovery's counts for the status of this pass.
func noteRecovery(ctx context.Context, c *v1.PlatformComponent) {
	holder, ok := ctx.Value(platformDeletionKey{}).(*platformDeletion)
	if !ok {
		return
	}
	if holder.recovered == nil {
		holder.recovered = map[string]*v1.PlatformComponent{}
	}
	holder.recovered[c.GetName()] = c
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
		if r := holder.recovered[current.GetName()]; r != nil {
			current.Recoveries, current.LastRecovery, current.LastRecoveryAt = r.GetRecoveries(), r.GetLastRecovery(), r.GetLastRecoveryAt()
		}
		next.components = append(next.components, current)
	}
	holder.seen, holder.state = true, next
}
