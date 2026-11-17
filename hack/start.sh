#!/usr/bin/env bash
# Bootstraps a local management cluster for cluster-vending:
#
#   1. a kind cluster (KIND_CLUSTER, default cluster-vending)
#   2. Crossplane (CROSSPLANE_VERSION) from the crossplane-stable Helm chart
#
# The controller (`make run-controller`) creates the rest: on
# startup the platform's CRDs, the Crossplane AWS providers (ec2, eks, iam,
# cloudwatchlogs, kms), provider-helm and their runtime config, when
# missing (--install-providers); tenant namespaces, ProviderConfigs and
# managed resources as it reconciles. Kind has no IRSA: the providers need
# a credentials Secret (docs/controller.md, "Local run").
#
# Safe to re-run: every step is idempotent. Delete the cluster with
# hack/stop.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

KIND_CLUSTER="${KIND_CLUSTER:-cluster-vending}"
CROSSPLANE_VERSION="${CROSSPLANE_VERSION:-2.4.3}"
CONTEXT="kind-${KIND_CLUSTER}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

for tool in docker kind kubectl helm; do
  command -v "$tool" >/dev/null || die "$tool is not installed"
done
docker info >/dev/null 2>&1 || die "docker is not running"

if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  log "kind cluster $KIND_CLUSTER exists"
else
  log "creating kind cluster $KIND_CLUSTER"
  kind create cluster --name "$KIND_CLUSTER" --wait 120s
fi

# provider.defaultActivations=null: no catch-all activation policy. The AWS
# providers define ~280 managed resource kinds; activating all of them makes
# ~300 CRDs and starves a laptop kind cluster. The controller activates only
# the kinds it uses (ManagedResourceActivationPolicy "cluster-vending").
log "installing Crossplane $CROSSPLANE_VERSION"
helm repo add crossplane-stable https://charts.crossplane.io/stable --force-update >/dev/null
helm upgrade --install crossplane crossplane-stable/crossplane \
  --kube-context "$CONTEXT" \
  --namespace crossplane-system --create-namespace \
  --version "$CROSSPLANE_VERSION" \
  --set provider.defaultActivations=null \
  --wait --timeout 5m

kubectl --context "$CONTEXT" wait crd/providers.pkg.crossplane.io \
  --for=condition=Established --timeout=2m >/dev/null

cat <<MSG

Ready. kubectl context: $CONTEXT

  1. create the providers' credentials Secret (docs/controller.md, "Local run")
  2. make run-controller   the controller (AWS keys exported in its shell)
  3. make run             API (OIDC off by default)
  4. make run-ui          UI (same auth mode as the API)

Optional login: run make keycloak separately, then uncomment the auth section
in hack/config.local.yaml and restart the API and UI. See docs/local-keycloak.md.

MSG
