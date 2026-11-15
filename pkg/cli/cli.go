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
	// Out and Output are where and how (--output) watch prints its stream.
	Out    io.Writer
	Output string
	// ETag is --etag, for optimistic concurrency on update and delete.
	ETag string
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
	// Reason is --reason of admin pause: why, shown with the pause.
	Reason string
	// Channel is --channel of admin rollout get, pause and resume (alpha,
	// beta, stable); Tier its --tier (0, 1, 2; empty: all).
	Channel, Tier string
	// Only is --only of admin pause and resume ("upgrades": automatic
	// upgrades only); Until --until of admin pause.
	Only, Until string
	// Regions is --region of account verify: regions to check besides
	// those of the account's networks.
	Regions []string
	// CreateRole is account create's --create-role: how the platform's
	// role is created in the AWS account (CreateRoleTerminal or
	// CreateRoleSkip; empty: asked at a terminal).
	// Profile is --profile, the AWS profile of the account's credentials;
	// StackRegion --region, the region of the role's stack (empty: the
	// server's default).
	CreateRole, Profile, StackRegion string
	// DeleteRole is account delete's --delete-role: delete the stack of
	// the account's role too, when account create made it.
	DeleteRole bool
	// RoleStacks opens the account's CloudFormation with the caller's AWS
	// credentials; nil: the AWS SDK. PollInterval is how often account
	// create looks at the stack and the access check; zero: 5s.
	RoleStacks   func(ctx context.Context, profile, region string) (RoleStacks, error)
	PollInterval time.Duration
	// Server is the API's base URL (--server): where the account role
	// templates are served too.
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
	// OnlyWarnings is --only-warnings of cluster events and network events.
	OnlyWarnings bool
	// OIDC is --oidc of cluster kubeconfig: the user signs in through the
	// cluster's OIDC identity provider.
	OIDC bool
	// PauseNetwork is --network of admin pause and resume: only this
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
