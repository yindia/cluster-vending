#!/usr/bin/env bash
# Local development only. No credentials are printed. Do not run with bash -x.
set +x
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
STATE="$ROOT/hack/.keycloak"
IMAGE="quay.io/keycloak/keycloak:26.8.2"
CONTAINER="cluster-vending-keycloak"
VOLUME="cluster-vending-keycloak-data"
ISSUER="http://localhost:18080/realms/cluster-vending"
ACTION="${1:-start}"
[[ "$ACTION" == prepare || "$ACTION" == start ]] || { printf 'usage: hack/keycloak.sh [prepare|start]; use make run and make run-ui for the applications\n' >&2; exit 1; }

die() { printf 'keycloak: %s\n' "$*" >&2; exit 1; }
log() { printf 'keycloak: %s\n' "$*"; }
command -v go >/dev/null || die 'go is not installed'
mkdir -p "$STATE"
chmod 700 "$STATE"
mkdir "$STATE/lock" 2>/dev/null || die 'another bootstrap is active (remove hack/.keycloak/lock only if that process exited)'
trap 'rmdir "$STATE/lock"' EXIT

if command -v docker >/dev/null && \
   docker volume inspect "$VOLUME" >/dev/null 2>&1 && [[ ! -f "$STATE/secrets.env" ]]; then
  die 'Keycloak data already exists but private credentials are missing; restore them or follow the documented reset procedure'
fi

# A dedicated helper uses the repo's YAML parser, preserving personal settings
# in a generated copy. No edits to config.local.yaml.
GOTOOLCHAIN=auto go build -o "$STATE/configure" ./hack/keycloak
"$STATE/configure" --base "${KEYCLOAK_BASE_CONFIG:-hack/config.local.yaml}" \
  --state "$STATE" --cli-redirect "${KEYCLOAK_CLI_REDIRECT:-http://localhost:18081/callback}"
if [[ "$ACTION" == prepare ]]; then
  log 'private settings prepared in hack/.keycloak/{secrets.env,ui.env,server.yaml}'
  exit 0
fi
command -v docker >/dev/null || die 'docker is not installed'
command -v curl >/dev/null || die 'curl is not installed'
docker info >/dev/null 2>&1 || die 'docker is not running'

offline_migration=0
if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
  if ! cmp -s "$STATE/import/cluster-vending-realm.json" "$STATE/applied-realm.json"; then
    "$STATE/configure" --state "$STATE" --check-offline-migration || die 'realm settings changed since import; startup does not overwrite existing realms; follow the documented reset procedure'
    offline_migration=1
  fi
  [[ "$(docker inspect --format '{{index .Config.Labels "io.cluster-vending.local-keycloak"}}' "$CONTAINER")" == true ]] || die 'container name is owned by another setup'
  [[ "$(docker inspect --format '{{.Config.Image}}' "$CONTAINER")" == "$IMAGE" ]] || die 'existing container uses another version; follow the documented reset procedure'
  [[ "$(docker inspect --format '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostIp}}:{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostPort}}' "$CONTAINER")" == 127.0.0.1:18080 ]] || die 'existing container has unexpected port bindings'
  docker start "$CONTAINER" >/dev/null
else
  "$STATE/configure" --check-keycloak-port
  # The one volume holds Keycloak's own H2 development database only. Platform
  # state remains in Kubernetes. The host import is private; docker cp lets
  # the non-root Keycloak user read it without relaxing host permissions.
  docker volume create "$VOLUME" >/dev/null
  docker create --name "$CONTAINER" \
    --label io.cluster-vending.local-keycloak=true \
    --publish 127.0.0.1:18080:8080 \
    --env-file "$STATE/container.env" \
    --mount "type=volume,source=$VOLUME,target=/opt/keycloak/data" \
    "$IMAGE" start-dev --hostname http://localhost:18080 --import-realm >/dev/null
  docker cp "$STATE/import" "$CONTAINER:/opt/keycloak/data/" >/dev/null
  # Copied files retain mode 0600; only the container copy is made readable.
  docker run --rm --user 0 --entrypoint /bin/bash \
    --mount "type=volume,source=$VOLUME,target=/opt/keycloak/data" \
    "$IMAGE" -c 'chown -R 1000:0 /opt/keycloak/data/import' >/dev/null
  cp "$STATE/import/cluster-vending-realm.json" "$STATE/applied-realm.json"
  docker start "$CONTAINER" >/dev/null
fi

log 'waiting for realm discovery and signing keys'
ready=0
for ((attempt=0; attempt<120; attempt++)); do
  if curl --fail --silent --max-time 2 "$ISSUER/.well-known/openid-configuration" > "$STATE/discovery.json" 2>/dev/null && \
     curl --fail --silent --max-time 2 "$ISSUER/protocol/openid-connect/certs" > "$STATE/jwks.json" 2>/dev/null; then
    ready=1
    break
  fi
  [[ "$(docker inspect --format '{{.State.Running}}' "$CONTAINER")" == true ]] || die 'container stopped; inspect its private local logs'
  sleep 2
done
[[ "$ready" == 1 ]] || die 'realm did not become ready within four minutes'
if [[ "$offline_migration" == 1 ]]; then
  "$STATE/configure" --state "$STATE" --grant-offline-access
  cp "$STATE/import/cluster-vending-realm.json" "$STATE/applied-realm.json"
  log 'enabled local CLI offline access without resetting the realm'
fi
log "ready: $ISSUER (audience cv-api)"
