#!/usr/bin/env bash
# check-versions.sh — print components where a newer upstream version exists
# than the newest tag already published to the OCI registry.
#
# Usage: OCI_REGISTRY=ghcr.io GITHUB_TOKEN=... ./scripts/check-versions.sh [repository]
#
# Exits 0 always; prints "component current→latest" lines for stale components.

set -euo pipefail

REGISTRY="${OCI_REGISTRY:-ghcr.io}"
REPOSITORY="${1:-cellebyte/k8s-sysext}"
BINARY="${BINARY:-./k8s-sysext}"

# Ensure the binary exists (build it if not).
if [[ ! -x "${BINARY}" ]]; then
  echo "building k8s-sysext…" >&2
  GOROOT=/home/a108073420/sdk/go1.26.3 GOTOOLCHAIN=local \
    /home/a108073420/sdk/go1.26.3/bin/go build -o "${BINARY}" ./cmd/k8s-sysext
fi

# Fetch all upstream versions once per component.
for COMPONENT in kubernetes eksd containerd crio k3s cni-plugins; do
  LATEST=$("${BINARY}" list "${COMPONENT}" \
    --registry "${REGISTRY}" --repository "${REPOSITORY}" 2>/dev/null | head -1) || continue

  [[ -z "${LATEST}" ]] && continue

  # Check if the immutable tag already exists in the registry.
  # oras resolve exits non-zero when the tag is absent.
  PUBLISHED=$(oras resolve "${REGISTRY}/${REPOSITORY}/${COMPONENT}:${LATEST}" \
    2>/dev/null || true)

  if [[ -z "${PUBLISHED}" ]]; then
    echo "${COMPONENT}: ${LATEST} not yet published"
  fi
done
