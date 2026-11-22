// Package cli is the API client behind the account, network and cluster
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
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// DefaultRequestTimeout bounds one API call unless Options.RequestTimeout
// says otherwise. --wait as a whole is not bounded by it.
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

// newHTTPClient is the client of the API. It has no total timeout: each
// call is bounded by its context instead (call), so --request-timeout
// decides.
func newHTTPClient() *http.Client { return &http.Client{} }

// Options are the inputs of every command. Commands use the fields they
// need and ignore the others.
type Options struct {
	// Tenant is --tenant, required by every command.
	Tenant string
	// DefaultTenant is CV_TENANT: the tenant when --tenant is not given
	// and no manifest names one (withDefaultTenant).
	DefaultTenant string
	// tenantDefaulted: Tenant is DefaultTenant, which a manifest's
	// metadata.tenant overrides.
	tenantDefaulted bool
	// ID is the resource ID: the argument of network and cluster
	// commands, --id of account get and verify (CloudAccount.id; the
	// argument is accepted too, ResolveID).
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
	// Out and Output are where and how (--output) a command that prints
	// for itself (events, tables) prints.
	Out    io.Writer
	Output string
	// ETag is --etag, for optimistic concurrency on delete (update sends
	// the etag of the object it read).
	ETag string
	// Plan is --plan of update: only show what the update would do
	// (validate_only).
	Plan   bool
	PlanID string
	// Force is --force of cluster delete: tear down even if the cluster's
	// API is unreachable.
	Force bool
	// Cluster is --cluster of admin unfreeze: one cluster only.
	Cluster string
	// Reason is --reason of admin freeze: why, shown with the pause.
	Reason          string
	ResetExceptions bool
	Platform        bool
	// Channel and Tier reject obsolete scoped freeze options.
	Channel, Tier string
	// Until is --until of admin freeze.
	Until string
	// Regions is --region of account verify: regions to check besides
	// those of the account's networks.
	Regions []string
	// PollInterval is how often a --wait looks again; zero: 5s.
	PollInterval time.Duration
	// Server is the API's base URL (--server): where the account role
	// templates are served too (account create prints their URL).
	Server string
	// AcknowledgeNotImported is --acknowledge-not-imported of network
	// takeover.
	AcknowledgeNotImported bool
	// SkipPlatformComponents is --skip-platform-components of cluster
	// takeover: the platform components are kept off the cluster.
	SkipPlatformComponents bool
	// PageSize and PageToken page list results.
	PageSize  int32
	PageToken string
	// Live is --live of get: the status read now, not the recorded one.
	Live bool
	// RetryComponents and RetryUpgrade are --component and --upgrade of
	// cluster retry: what to try again (neither: everything that failed).
	RetryComponents []string
	RetryUpgrade    bool
	// OnlyRemoved is --only-removed of cluster deprecations.
	OnlyRemoved bool
	// Namespace is --namespace of cluster recommendations: one namespace of
	// the workload cluster.
	Namespace string
	// OnlyChanges is --only-changes of cluster recommendations.
	OnlyChanges bool
	// SortBy and SortOrder are --sort and --order of the cluster lists
	// (nodes, violations, deprecations, recommendations): an enum value in
	// lower case with dashes (cpu-requested-percent), and asc or desc.
	// Empty: the server's default for the list.
	SortBy, SortOrder string
	// NodeGroup, Ready, NotReady and NameContains are --group, --ready,
	// --not-ready and --name of cluster nodes.
	NodeGroup       string
	Ready, NotReady bool
	NameContains    string
	// Manifests is --manifests of cluster nodes: the NodePool and
	// EC2NodeClass YAML of the groups instead of their nodes.
	Manifests bool
	// NodeGroupOptions is --options of cluster nodes: what node groups of
	// the tenant may use (families, types, architectures, capacity types).
	NodeGroupOptions bool
	// Engine and Events are --engine and --events of cluster violations:
	// one policy engine; the admission events of the last hour instead of
	// the violations.
	Engine string
	Events bool
	// OnlyWarnings is --only-warnings of cluster events and network events.
	OnlyWarnings bool
	// OIDC is --oidc of cluster kubeconfig: the user signs in through the
	// cluster's OIDC identity provider.
	OIDC bool
	// PauseNetwork is --network of admin unfreeze: only this
	// network (network ID). Not with Cluster.
	PauseNetwork string
	// Details is --details of component list: why each component is not
	// ready (chart, Helm state, last error, failures, recoveries).
	Details bool
	// Print is --print of cluster kubeconfig: print the kubeconfig instead
	// of installing it with the AWS CLI.
	Print bool
	// KubeconfigFile is --kubeconfig-file of cluster kubeconfig: the file
	// the AWS CLI writes to. Empty: its default (~/.kube/config).
	KubeconfigFile string
	// Component is the argument of component enable and component values:
	// the catalog name.
	Component string
	// ValidateOnly is --validate-only of component enable and update:
	// check, say what would be stored, store nothing.
	ValidateOnly bool
	// Values is --values of component get: print only the stored values.
	Values bool
	// Terminal: a person is at the terminal, so account create may ask how
	// to create the role; In is where the answer is read (nil: stdin).
	Terminal bool
	In       io.Reader
	// Exec runs a local command with its output to w. Nil: the real one
	// (os/exec, with the caller's stdin, stdout and stderr).
	Exec func(ctx context.Context, w io.Writer, name string, args ...string) error
}

func (o Options) requireTenant() error {
	if o.Tenant == "" {
		return errors.New("--tenant is required (or set " + EnvTenant + ")")
	}
	return nil
}

// Clients are the cloud.v1 API clients.
type Clients struct {
	Tenant  cloudv1connect.TenantServiceClient
	Network cloudv1connect.NetworkServiceClient
	Cluster cloudv1connect.ClusterServiceClient
	Admin   cloudv1connect.AdminServiceClient
	// Component is the component catalog and a tenant's components.
	Component cloudv1connect.ComponentServiceClient
	// SubTenant is the namespace allocations of a cluster that runs Capsule.
	SubTenant cloudv1connect.SubTenantServiceClient
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
		Tenant:    cloudv1connect.NewTenantServiceClient(c),
		Network:   cloudv1connect.NewNetworkServiceClient(c),
		Cluster:   cloudv1connect.NewClusterServiceClient(c),
		Admin:     cloudv1connect.NewAdminServiceClient(c),
		Component: cloudv1connect.NewComponentServiceClient(c),
		SubTenant: cloudv1connect.NewSubTenantServiceClient(c),
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
	o = o.withDefaultTenant()
	if o.RequestTimeout > 0 {
		ctx = context.WithValue(ctx, requestTimeoutKey{}, o.RequestTimeout)
	}
	res, err := cmd(ctx, c, o)
	if err != nil || res == nil { // the command printed for itself
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
