package aws

// IdentityProviderAssociationName is the name of the EKS association a
// managed identity provider gets: "<name>-<hash8 of its settings>"; for
// the tests.
func IdentityProviderAssociationName(idp IdentityProviderPlan) string {
	return idp.Name + "-" + idp.hash()
}
