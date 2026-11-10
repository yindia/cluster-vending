// Package cli is the API client behind the bootstrap, network, cluster and
// nodepool commands: it builds requests from flags and an input file, calls
// the cloud.v1 API, and prints the response. cmd only wires cobra flags to
// Options.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// requestTimeout bounds one API call.
const requestTimeout = 30 * time.Second

// Output formats.
const (
	OutputYAML = "yaml"
	OutputJSON = "json"
)

// Options are the inputs of every command. Commands use the fields they
// need and ignore the others.
type Options struct {
	// Tenant is --tenant, required by every command.
	Tenant string
	// Cluster is --cluster, the parent of node pools.
	Cluster string
	// ID is the resource ID: the argument of network, cluster and nodepool
	// commands, --id of bootstrap get (the bootstrap ID, CloudAccount.id).
	ID string
	// Input is --config: a YAML (or JSON) file holding the resource of a
	// create or update, in protojson field names (camelCase).
	Input string
	// ETag is --etag, for optimistic concurrency on update and delete.
	ETag string
	// UpdateMask is --update-mask: the fields an update changes.
	UpdateMask []string
	// AcknowledgeNotImported is --acknowledge-not-imported of network
	// takeover.
	AcknowledgeNotImported bool
	// PageSize and PageToken page list results.
	PageSize  int32
	PageToken string
}

func (o Options) requireTenant() error {
	if o.Tenant == "" {
		return errors.New("--tenant is required")
	}
	return nil
}

// Clients are the cloud.v1 API clients.
type Clients struct {
	Tenant   cloudv1connect.TenantServiceClient
	Network  cloudv1connect.NetworkServiceClient
	Cluster  cloudv1connect.ClusterServiceClient
	NodePool cloudv1connect.NodePoolServiceClient
}

// NewClients returns clients of the API at server (a base URL) over
// httpClient; nil uses a client with requestTimeout.
func NewClients(server string, httpClient *http.Client) Clients {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	c := connect.NewClient(connecthttp.NewTransport(httpClient, server))
	return Clients{
		Tenant:   cloudv1connect.NewTenantServiceClient(c),
		Network:  cloudv1connect.NewNetworkServiceClient(c),
		Cluster:  cloudv1connect.NewClusterServiceClient(c),
		NodePool: cloudv1connect.NewNodePoolServiceClient(c),
	}
}

// Command is one API call: it builds its request from Options and returns
// the response.
type Command func(ctx context.Context, c Clients, o Options) (proto.Message, error)

// Run runs cmd and prints its response to w in format.
func Run(ctx context.Context, cmd Command, c Clients, o Options, w io.Writer, format string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := cmd(ctx, c, o)
	if err != nil {
		return err
	}
	return Print(w, res, format)
}

// call validates req with its protovalidate rules (the buf.validate
// options in idl/, the same the server checks) and sends it, so a bad
// input fails before any network call, with the field that is wrong.
func call[Req, Resp proto.Message](ctx context.Context, req Req, send func(context.Context, Req) (Resp, error)) (proto.Message, error) {
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	return send(ctx, req)
}

// requestNamespace scopes the request IDs the CLI derives.
var requestNamespace = uuid.MustParse("5f2c8a8e-6b1d-4c0e-9a1b-7c3d2e1f0a9b")

// requestID derives a request ID (UUIDv5) from the tenant and the request
// as built from flags and --config: running the same input again repeats
// the same request, and the server returns what the first one created
// instead of creating it again.
func requestID(tenant string, req proto.Message) (string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return "", err
	}
	return uuid.NewSHA1(requestNamespace, append([]byte(tenant+"\x00"), raw...)).String(), nil
}

// LoadInput reads a YAML or JSON file into msg (protojson field names).
// An empty path leaves msg as it is. Unknown fields are errors, so typos do
// not pass silently.
func LoadInput(path string, msg proto.Message) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("--config: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("--config %s: %w", path, err)
	}
	if err := protojson.Unmarshal(js, msg); err != nil {
		return fmt.Errorf("--config %s: %w", path, err)
	}
	return nil
}

// Print writes msg as YAML or JSON.
func Print(w io.Writer, msg proto.Message, format string) error {
	js, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		return err
	}
	switch format {
	case OutputJSON:
		_, err = fmt.Fprintln(w, string(js))
		return err
	case OutputYAML, "":
		y, err := yaml.JSONToYAML(js)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	default:
		return fmt.Errorf("--output must be %s or %s, got %q", OutputYAML, OutputJSON, format)
	}
}
