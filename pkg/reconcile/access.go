package reconcile

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// credentialsSourceSecret is the ProviderConfig source of static keys.
const credentialsSourceSecret = "Secret"

// AWSAccess is how the server's own AWS calls (ImportNetwork discovery)
// reach an account:
//
//  1. AWS keys in the server's environment (config.AWS.StaticKeys): those,
//     directly, for every account (local testing);
//  2. else the same way as the account's ProviderConfig, so they work
//     wherever the provider works: the keys of its Secret (credentials.source
//     Secret) or the server's own credentials (IRSA), then the last role of
//     assumeRoleChain, if any.
func (d Deps) AWSAccess(ctx context.Context, a *model.CloudAccount) (awscloud.Access, error) {
	if k := d.AWS.StaticKeys; k != nil {
		return awscloud.Access{AccountID: a.AccountID, Static: &awscloud.StaticCredentials{
			AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, SessionToken: k.SessionToken,
		}}, nil
	}
	pc := &awsv1beta1.ProviderConfig{}
	err := d.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.ProviderConfigName}, pc)
	if apierrors.IsNotFound(err) {
		return awscloud.Access{}, fault.New(fault.FailedPrecondition, fmt.Sprintf("account %s has no ProviderConfig %s/%s; bootstrap it", a.ID, a.Namespace, a.ProviderConfigName))
	}
	if err != nil {
		return awscloud.Access{}, fault.New(fault.Unavailable, "cannot read the account's ProviderConfig: "+err.Error())
	}
	access := awscloud.Access{AccountID: a.AccountID}
	if chain := pc.Spec.AssumeRoleChain; len(chain) > 0 && chain[len(chain)-1].RoleARN != nil {
		access.RoleARN = *chain[len(chain)-1].RoleARN
	}
	if pc.Spec.Credentials.Source != credentialsSourceSecret {
		return access, nil
	}
	ref := pc.Spec.Credentials.SecretRef
	if ref == nil {
		return awscloud.Access{}, fault.New(fault.FailedPrecondition, "the ProviderConfig uses a Secret but names none")
	}
	secret := &corev1.Secret{}
	if err := d.Kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return awscloud.Access{}, fault.New(fault.FailedPrecondition, fmt.Sprintf("cannot read credentials Secret %s/%s: %v", ref.Namespace, ref.Name, err))
	}
	keys, err := parseCredentials(secret.Data[ref.Key])
	if err != nil {
		return awscloud.Access{}, fault.New(fault.FailedPrecondition, fmt.Sprintf("credentials Secret %s/%s key %s: %v", ref.Namespace, ref.Name, ref.Key, err))
	}
	access.Static = keys
	return access, nil
}

// parseCredentials reads the [default] profile (or the first one) of an
// AWS shared credentials file.
func parseCredentials(raw []byte) (*awscloud.StaticCredentials, error) {
	values := map[string]string{}
	section, chosen := "", ""
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSpace(line[1 : len(line)-1])
			if chosen == "" || section == "default" {
				chosen, values = section, map[string]string{}
			}
		case section == chosen:
			if k, v, ok := strings.Cut(line, "="); ok {
				values[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	keys := &awscloud.StaticCredentials{
		AccessKeyID: values["aws_access_key_id"], SecretAccessKey: values["aws_secret_access_key"],
		SessionToken: values["aws_session_token"],
	}
	if keys.AccessKeyID == "" || keys.SecretAccessKey == "" {
		return nil, fmt.Errorf("no aws_access_key_id / aws_secret_access_key")
	}
	return keys, nil
}
