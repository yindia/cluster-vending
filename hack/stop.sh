#!/usr/bin/env bash
# Deletes the kind cluster created by hack/start.sh.
set -euo pipefail
KIND_CLUSTER="${KIND_CLUSTER:-cluster-vending}"
kind delete cluster --name "$KIND_CLUSTER"
