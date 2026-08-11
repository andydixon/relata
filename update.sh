#!/usr/bin/env bash

# Build and deploy the current checkout to the production Relata stack.

set -Eeuo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly COMPOSE_FILE="${SCRIPT_DIR}/deploy/docker-compose.prod.yml"
readonly READY_URL="http://127.0.0.1:8477/health/ready"

usage() {
	cat <<'EOF'
Usage: ./update.sh [version]

Run the project checks, build a versioned production image, point the latest
tag at it, restart the production stack, and verify that Relata is ready.

If version is omitted, a unique version is generated from the current Git
revision and UTC time. Example: ./update.sh 1.0.3
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi

if (( $# > 1 )); then
	usage >&2
	exit 2
fi

cd "${SCRIPT_DIR}"

git_revision="$(git describe --tags --always 2>/dev/null || git rev-parse --short HEAD)"
default_version="${git_revision}-$(date -u +%Y%m%d%H%M%S)"
version="${1:-${default_version}}"

if [[ ! "${version}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
	echo "Invalid Docker image version: ${version}" >&2
	echo "Use 1-128 letters, digits, periods, underscores, or hyphens." >&2
	exit 2
fi

echo "==> Running project checks"
make check

echo "==> Building relata:${version}"
export RELATA_VERSION="${version}"
sudo -E docker compose -f "${COMPOSE_FILE}" build app

echo "==> Tagging relata:${version} as relata:latest"
sudo docker tag "relata:${version}" relata:latest

echo "==> Restarting the production stack"
sudo systemctl restart relata

echo "==> Waiting for ${READY_URL}"
readonly READY_DEADLINE=$((SECONDS + 60))
while (( SECONDS < READY_DEADLINE )); do
	if readiness="$(curl -fsS --max-time 5 "${READY_URL}" 2>/dev/null)"; then
		echo "${readiness}"
		echo "==> Deployment complete: relata:${version}"
		exit 0
	fi

	if (( SECONDS < READY_DEADLINE )); then
		sleep 2
	fi
done

echo "Relata did not become ready within 60 seconds." >&2
echo "==> Service status" >&2
sudo systemctl status relata --no-pager || true
echo "==> Recent app logs" >&2
sudo docker compose -f "${COMPOSE_FILE}" logs --tail=100 app || true
exit 1
