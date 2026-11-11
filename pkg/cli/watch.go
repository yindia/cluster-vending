package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NetworkWatch streams NetworkService.WatchNetwork to o.Out until the
// network is deleted or the command is interrupted.
func NetworkWatch(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	req := &cloudv1.WatchNetworkRequest{TenantId: o.Tenant, NetworkId: o.ID}
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	stream, err := c.Network.WatchNetwork(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	return nil, receive(o, stream.Receive, func(m *cloudv1.WatchNetworkResponse) watchLine {
		n := m.GetNetwork()
		return watchLine{name: "network " + o.ID, set: n != nil, state: n.GetStatus().GetState().String(),
			message: n.GetStatus().GetMessage(), events: m.GetEvents(), deleted: m.GetDeleted()}
	})
}

// ClusterWatch streams ClusterService.WatchCluster to o.Out until the
// cluster is deleted or the command is interrupted.
func ClusterWatch(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	req := &cloudv1.WatchClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	stream, err := c.Cluster.WatchCluster(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	return nil, receive(o, stream.Receive, func(m *cloudv1.WatchClusterResponse) watchLine {
		cl := m.GetCluster()
		return watchLine{name: "cluster " + o.ID, set: cl != nil, state: cl.GetStatus().GetState().String(),
			message: cl.GetStatus().GetMessage(), events: m.GetEvents(), deleted: m.GetDeleted()}
	})
}

// watchLine is what a watch message says, for the line format.
type watchLine struct {
	name           string
	set            bool // the resource's state is in the message
	state, message string
	events         []*cloudv1.ManagedResourceEvent
	deleted        bool
}

// receive prints every message of a watch stream: as JSON lines with -o
// json, else as readable lines (the resource's state when it changed, one
// line per managed resource event: + added, ~ modified, - deleted).
func receive[M proto.Message](o Options, next func() (M, error), describe func(M) watchLine) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	for {
		m, err := next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if o.Output == OutputJSON {
			js, err := protojson.Marshal(m)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out, string(js)); err != nil {
				return err
			}
			continue
		}
		if err := printLine(out, describe(m)); err != nil {
			return err
		}
	}
}

var eventMarks = map[cloudv1.ManagedResourceEvent_Type]string{
	cloudv1.ManagedResourceEvent_TYPE_ADDED: "+", cloudv1.ManagedResourceEvent_TYPE_MODIFIED: "~",
	cloudv1.ManagedResourceEvent_TYPE_DELETED: "-",
}

func printLine(w io.Writer, l watchLine) error {
	now := time.Now().Format("15:04:05")
	var b strings.Builder
	if l.set {
		fmt.Fprintf(&b, "%s %s %s", now, l.name, shortState(l.state))
		if l.message != "" {
			fmt.Fprintf(&b, ": %s", l.message)
		}
		b.WriteString("\n")
	}
	for _, e := range l.events {
		r := e.GetResource()
		fmt.Fprintf(&b, "%s   %s %s %s %s", now, eventMarks[e.GetType()], r.GetKind(), r.GetName(), shortState(r.GetState().String()))
		if r.GetExternalName() != "" {
			fmt.Fprintf(&b, " (%s)", r.GetExternalName())
		}
		if r.GetMessage() != "" {
			fmt.Fprintf(&b, ": %s", r.GetMessage())
		}
		b.WriteString("\n")
	}
	if l.deleted {
		fmt.Fprintf(&b, "%s %s deleted\n", now, l.name)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// shortState drops the enum prefix: NETWORK_STATE_READY -> READY.
func shortState(s string) string {
	for _, p := range []string{"MANAGED_RESOURCE_STATE_", "NETWORK_STATE_", "CLUSTER_STATE_"} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}
