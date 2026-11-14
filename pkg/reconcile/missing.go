package reconcile

import (
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Messages of a recorded object that is gone (MISSING), as a user reads
// them.
const (
	missingDeleted = "deleted"
	missingOutside = "deleted outside the platform; it is created again unless its cloud ID is assigned by AWS"
)

// ExplainMissing is views with each MISSING object's message saying what it
// means: while the owner is deleted the object is simply deleted; else it
// was deleted outside the platform. Other views are returned as they are;
// the input is not changed.
func ExplainMissing(views []*cloudv1.ManagedResource, deleting bool) []*cloudv1.ManagedResource {
	msg := missingOutside
	if deleting {
		msg = missingDeleted
	}
	out := make([]*cloudv1.ManagedResource, len(views))
	for i, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			out[i] = v
			continue
		}
		c := proto.Clone(v).(*cloudv1.ManagedResource)
		c.Message = msg
		out[i] = c
	}
	return out
}
