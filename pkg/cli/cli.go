// Package cli is the API client behind the bootstrap, network and cluster
// commands: it builds requests from flags and Kubernetes-style manifests
// (manifest.go), calls
// the cloud.v1 API, and prints the response. cmd only wires cobra flags to
// Options.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// DefaultRequestTimeout bounds one API call unless Options.RequestTimeout
// says otherwise. A watch and --wait are not bounded by it.
const DefaultRequestTimeout = 2 * time.Minute

// requestTimeoutKey carries Options.RequestTimeout to call.
type requestTimeoutKey struct{}

// requestTimeoutOf is the timeout of one API call under ctx.
func requestTimeoutOf(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(requestTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return DefaultRequestTimeout
}

// newHTTPClient is the client of the API. It has no total timeout: that
// would also cut a watch (a response read for as long as it is watched)
// off; each call is bounded by its context instead (call).
func newHTTPClient() *http.Client { return &http.Client{} }

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
	// ID is the resource ID: the argument of network and cluster
	// commands, --id of bootstrap get (the bootstrap ID, CloudAccount.id).
	ID string
	// Input is -f: the manifest of a create, update or import (Manifest).
	Input string
	// Wait is --wait: create, update, import, takeover and delete return
	// once the resource is ready (or gone), polling it; Timeout bounds it.
	Wait    bool
	Timeout time.Duration
	// RequestTimeout is --request-timeout: how long one API call may take.
	// Zero: DefaultRequestTimeout.
	RequestTimeout time.Duration
	// Progress receives --wait's state changes (stderr).
	Progress io.Writer
	// Out and Output are where and how (--output) watch prints its stream.
	Out    io.Writer
	Output string
	// ETag is --etag, for optimistic concurrency on update and delete.
	ETag string
	// UpdateMask is --update-mask: the fields an update changes.
	UpdateMask []string
	// Plan is --plan of cluster update: only show what the update would do
	// (validate_only); PlanID is --plan-id: apply only if that plan still
	// holds.
	Plan   bool
	PlanID string
	// Force is --force of cluster delete: tear down even if the cluster's
	// API is unreachable.
	Force bool
	// Cluster is --cluster of admin pause / resume: one cluster only.
	Cluster string
	// AcknowledgeNotImported is --acknowledge-not-imported of network
	// takeover.
	AcknowledgeNotImported bool
	// PageSize and PageToken page list results.
	PageSize  int32
	PageToken string
	// OnlyRemoved is --only-removed of cluster deprecations.
	OnlyRemoved bool
	// OIDC is --oidc of cluster kubeconfig: the user signs in through the
	// cluster's OIDC identity provider.
	OIDC bool
	// Print is --print of cluster kubeconfig: print the kubeconfig instead
	// of installing it with the AWS CLI.
	Print bool
	// KubeconfigFile is --kubeconfig-file of cluster kubeconfig: the file
	// the AWS CLI writes to. Empty: its default (~/.kube/config).
	KubeconfigFile string
	// Exec runs a local command with its output to w. Nil: the real one
	// (os/exec, with the caller's stdin, stdout and stderr).
	Exec func(ctx context.Context, w io.Writer, name string, args ...string) error
}

func (o Options) requireTenant() error {
	if o.Tenant == "" {
		return errors.New("--tenant is required")
	}
	return nil
}

// Clients are the cloud.v1 API clients.
type Clients struct {
	Tenant  cloudv1connect.TenantServiceClient
	Network cloudv1connect.NetworkServiceClient
	Cluster cloudv1connect.ClusterServiceClient
	Admin   cloudv1connect.AdminServiceClient
}

// NewClients returns clients of the API at server (a base URL) over
// httpClient; nil uses newHTTPClient. With WithToken every
// call carries a bearer token (auth.go).
func NewClients(server string, httpClient *http.Client, opts ...ClientOption) Clients {
	if httpClient == nil {
		httpClient = newHTTPClient()
	}
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}
	var interceptors []connect.ClientInterceptor
	if o.token != nil {
		interceptors = append(interceptors, bearerToken(server, o.token))
	}
	c := connect.NewClient(connecthttp.NewTransport(httpClient, server), interceptors...)
	return Clients{
		Tenant:  cloudv1connect.NewTenantServiceClient(c),
		Network: cloudv1connect.NewNetworkServiceClient(c),
		Cluster: cloudv1connect.NewClusterServiceClient(c),
		Admin:   cloudv1connect.NewAdminServiceClient(c),
	}
}

// Command is one API call: it builds its request from Options and returns
// the response.
type Command func(ctx context.Context, c Clients, o Options) (proto.Message, error)

// Run runs cmd and prints its response to w in format. Each API call is
// bounded by o.RequestTimeout (DefaultRequestTimeout when unset); --wait
// by o.Timeout.
func Run(ctx context.Context, cmd Command, c Clients, o Options, w io.Writer, format string) error {
	o.Out, o.Output = w, format
	if o.RequestTimeout > 0 {
		ctx = context.WithValue(ctx, requestTimeoutKey{}, o.RequestTimeout)
	}
	res, err := cmd(ctx, c, o)
	if err != nil || res == nil { // watch prints as it goes
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
	ctx, cancel := context.WithTimeout(ctx, requestTimeoutOf(ctx))
	defer cancel()
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
