//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	"github.com/yindia/cluster-vending/pkg/cli"
)

// env is what every scenario shares: the configuration, the API clients
// (the CLI's own), and the ids of what the run created.
type env struct {
	cfg    config
	api    cli.Clients
	parent *testing.T // the whole run: cleanups registered on it run after every scenario
	runID  string     // random: prefix of every name the run creates
	st     state

	awsOnce sync.Once
	awsOK   bool
}

// state is what the scenarios pass on to the next one.
type state struct {
	accountID      string // the bootstrap id
	createdAccount bool   // the run onboarded it (so it deletes it)

	networkID, networkName string
	clusterID, clusterName string
	allClusterIDs          []string // every cluster the run created, for the leak check
	allNetworkIDs          []string
	upgradedTo             string

	importNetworkID, importClusterID string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := loadConfig(t)
	var opts []cli.ClientOption
	if cfg.Token != "" {
		token := cfg.Token
		opts = append(opts, cli.WithToken(func(context.Context) (string, error) { return token, nil }))
	}
	return &env{
		cfg:   cfg,
		api:   cli.NewClients(cfg.Server, nil, opts...),
		runID: "cv-e2e-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6],
	}
}

// rpc makes one API call bounded by the request timeout.
func rpc[Req, Resp any](e *env, fn func(context.Context, *Req) (*Resp, error), req *Req) (*Resp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.RequestTimeout)
	defer cancel()
	return fn(ctx, req)
}

func isCode(err error, code connect.Code) bool {
	return err != nil && connect.CodeOf(err) == code
}

// transient errors are retried by waitFor instead of failing a wait.
func isTransient(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeAborted, connect.CodeResourceExhausted:
		return true
	default:
		return false
	}
}

// maxTransient is how many failed reads in a row end a wait.
const maxTransient = 6

// waitFor polls step until it reports done, with a deadline, logging each
// change of its message. step returns an error to end the wait at once.
func (e *env) waitFor(t *testing.T, what string, timeout time.Duration, step func() (done bool, msg string, err error)) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last, transient := "", 0
	start := time.Now()
	for {
		done, msg, err := step()
		switch {
		case err != nil && isTransient(err) && transient < maxTransient:
			transient++
			t.Logf("%s: %v (retrying)", what, err)
		case err != nil:
			return fmt.Errorf("%s: %w", what, err)
		default:
			transient = 0
			if msg != last {
				t.Logf("[%s] %s: %s", time.Since(start).Round(time.Second), what, msg)
				last = msg
			}
			if done {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: not done after %s; last: %s", what, timeout, last)
		}
		time.Sleep(e.cfg.Poll)
	}
}

// need skips the scenario when what it depends on was not created.
func need(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Skipf("skipped: %s", what)
	}
}

// --- local tools: kubectl for the management cluster, aws for the account ---

// run executes a local command; its stderr is part of the error.
func run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (e *env) haveKube() bool {
	if e.cfg.KubeContext == "" {
		return false
	}
	_, err := exec.LookPath("kubectl")
	return err == nil
}

// kubectl runs kubectl against the management cluster.
func (e *env) kubectl(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--context", e.cfg.KubeContext}, args...)...)
}

// kubeJSON is `kubectl get ... -o json` of the management cluster.
func (e *env) kubeJSON(ctx context.Context, args ...string) (map[string]any, error) {
	out, err := e.kubectl(ctx, "", append([]string{"get"}, append(args, "-o", "json")...)...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(out, &m)
	return m, err
}

// haveAWS says whether the aws CLI is there with credentials of the
// sandbox account. Anything else (no CLI, no credentials, another account)
// turns the checks that need it into logged skips.
func (e *env) haveAWS(t *testing.T) bool {
	t.Helper()
	e.awsOnce.Do(func() {
		if _, err := exec.LookPath("aws"); err != nil {
			t.Log("aws CLI not found: AWS-side checks are skipped")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := run(ctx, "", "aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
		switch got := strings.TrimSpace(string(out)); {
		case err != nil:
			t.Logf("no usable AWS credentials (%v): AWS-side checks are skipped", err)
		case got != e.cfg.AccountID:
			t.Logf("AWS credentials are for account %s, not %s: AWS-side checks are skipped", got, e.cfg.AccountID)
		default:
			e.awsOK = true
		}
	})
	return e.awsOK
}

// awsJSON runs `aws <args> --region <region> --output json` and decodes it.
func (e *env) awsJSON(ctx context.Context, region string, args ...string) (map[string]any, error) {
	out, err := run(ctx, "", "aws", append(args, "--region", region, "--output", "json")...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if len(bytes.TrimSpace(out)) == 0 {
		return m, nil
	}
	err = json.Unmarshal(out, &m)
	return m, err
}

// awsNotFound is an aws CLI error for something that does not exist.
func awsNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "ResourceNotFoundException") || strings.Contains(s, "NoSuchBucket") ||
		strings.Contains(s, "Not Found") || strings.Contains(s, "NotFound") || strings.Contains(s, "404")
}

// --- tiny readers for generic JSON (custom resources, aws output) ---

// at walks nested maps; a list of one element is read as that element
// (Crossplane's nested blocks are lists in some provider generations).
func at(v any, path ...string) any {
	for _, p := range path {
		switch m := v.(type) {
		case map[string]any:
			v = m[p]
		case []any:
			if len(m) == 0 {
				return nil
			}
			first, ok := m[0].(map[string]any)
			if !ok {
				return nil
			}
			v = first[p]
		default:
			return nil
		}
	}
	return v
}

func asList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case nil:
		return nil
	default:
		return []any{l}
	}
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(s)
	}
}
