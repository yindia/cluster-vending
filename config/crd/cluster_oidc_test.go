package crd_test

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// The Cluster kind takes an OIDC identity provider as Microsoft Entra ID
// (tenant and client alone) or as any provider's issuer and client, never
// a mix of the two.
func TestClusterOIDCRules(t *testing.T) {
	v := newValidator(t, "Cluster")
	sample := sampleDocuments(t, "cloud_v1alpha1_cluster.yaml")[0]
	const anchor = "    adminPrincipals: []\n"
	if !bytes.Contains(sample, []byte(anchor)) {
		t.Fatalf("the cluster sample has no %q to put spec.access.oidc after", anchor)
	}
	const entra = "entra: {tenantID: 833ced3d-cb2e-41de-92f1-29e2af035ddc, clientID: 00001111-aaaa-2222-bbbb-3333cccc4444}"
	for name, tt := range map[string]struct {
		oidc    string
		wantErr bool
	}{
		"generic":                    {"{name: okta, issuerURL: 'https://example.okta.com', clientID: kubernetes, usernameClaim: email, groupsClaim: groups, groupsPrefix: 'oidc:'}", false},
		"entra":                      {"{" + entra + "}", false},
		"entra with a name":          {"{name: corp, " + entra + ", requiredClaims: {tid: 833ced3d-cb2e-41de-92f1-29e2af035ddc}}", false},
		"nothing":                    {"{}", true},
		"generic without a name":     {"{issuerURL: 'https://example.okta.com', clientID: kubernetes}", true},
		"generic without an issuer":  {"{name: okta, clientID: kubernetes}", true},
		"generic without a client":   {"{name: okta, issuerURL: 'https://example.okta.com'}", true},
		"http issuer":                {"{name: okta, issuerURL: 'http://example.okta.com', clientID: kubernetes}", true},
		"entra and an issuer":        {"{" + entra + ", issuerURL: 'https://login.microsoftonline.com/common/v2.0'}", true},
		"entra and a client":         {"{" + entra + ", clientID: kubernetes}", true},
		"entra and a user claim":     {"{" + entra + ", usernameClaim: upn}", true},
		"entra and a groups prefix":  {"{" + entra + ", groupsPrefix: 'aad:'}", true},
		"entra tenant is not a GUID": {"{entra: {tenantID: contoso.onmicrosoft.com, clientID: 00001111-aaaa-2222-bbbb-3333cccc4444}}", true},
		"entra without a client":     {"{entra: {tenantID: 833ced3d-cb2e-41de-92f1-29e2af035ddc}}", true},
		"system: prefix":             {"{name: okta, issuerURL: 'https://example.okta.com', clientID: kubernetes, groupsPrefix: 'system:'}", true},
	} {
		t.Run(name, func(t *testing.T) {
			doc := bytes.Replace(sample, []byte(anchor), []byte(anchor+"    oidc: "+tt.oidc+"\n"), 1)

			// Only what the provider is refused for: the test validator
			// reads YAML integers as floats, which other rules of the
			// kind (the node pool's sizes) do not take.
			var errs field.ErrorList
			for _, e := range v.validate(t, doc) {
				if strings.HasPrefix(e.Field, "spec.access.oidc") {
					errs = append(errs, e)
				}
			}

			if tt.wantErr && len(errs) == 0 {
				t.Error("accepted")
			}
			if !tt.wantErr && len(errs) > 0 {
				t.Error(errs.ToAggregate())
			}
		})
	}
}
