#!/usr/bin/env bash
# Build the image and (re)start the stack, starting colima first if needed.
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f .env ] || { echo "missing .env (cp .env.example .env and set TUNNEL_TOKEN)" >&2; exit 1; }

go vet ./...

if ! colima status >/dev/null 2>&1; then
	# sg picks up the kvm group even in shells opened before the group was added
	if id -nG | grep -qw kvm; then
		colima start --cpu 2 --memory 2 --disk 20
	else
		sg kvm -c "colima start --cpu 2 --memory 2 --disk 20"
	fi
fi

docker compose up -d --build
docker compose ps
