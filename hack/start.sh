#!/usr/bin/env bash
# Bootstraps a local management cluster for cluster-vending:
#
#   1. a kind cluster (KIND_CLUSTER, default cluster-vending)
#   2. Crossplane (CROSSPLANE_VERSION) from the crossplane-stable Helm chart
#
# That is all. What reconciles creates the rest (the controller; or the
# server with hack/config.local.yaml, whose database store makes it run the
# background loop): on startup the Crossplane AWS providers (ec2, eks, iam,
# cloudwatchlogs, kms), provider-helm and, with a management role, the AWS
# providers' IRSA DeploymentRuntimeConfig, when missing
# (--install-providers); tenant namespaces, ProviderConfigs and managed
# resources as it reconciles.
#
# Safe to re-run: every step is idempotent. Delete the cluster with
# hack/stop.sh.
set -euo pipefail

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
# ~300 CRDs and starves a laptop kind cluster. The server activates only the
# kinds it uses (ManagedResourceActivationPolicy "cluster-vending").
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

  make run      run the server against this cluster

MSG
