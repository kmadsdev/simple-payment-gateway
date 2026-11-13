#!/bin/sh
# Runs the full test suite (unit + integration, race detector on) with
# only Docker installed. It starts a throwaway PostgreSQL container on a
# private network, runs `go test` in a golang container, and removes both
# containers and the network on exit, also on failure.
#
# Usage: scripts/test-in-docker.sh [go test args]   (default: -race ./...)
set -eu

GO_IMAGE="${GO_IMAGE:-golang:1.27}"
PG_IMAGE="${PG_IMAGE:-postgres:17}"
NAME="spg-test-$$"

cleanup() {
	docker rm -f "$NAME-pg" >/dev/null 2>&1 || true
	docker network rm "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker network create "$NAME" >/dev/null
docker run -d --rm --name "$NAME-pg" --network "$NAME" \
	-e POSTGRES_PASSWORD=postgres "$PG_IMAGE" >/dev/null

# Wait for PostgreSQL to accept connections.
i=0
until docker exec "$NAME-pg" pg_isready -h 127.0.0.1 -U postgres -q; do
	i=$((i + 1))
	[ "$i" -gt 60 ] && { echo "postgres did not start" >&2; exit 1; }
	sleep 1
done

if [ "$#" -eq 0 ]; then
	set -- -race ./...
fi

docker run --rm --network "$NAME" \
	-v "$(pwd)":/src -w /src \
	-v spg-gomod:/go/pkg/mod -v spg-gocache:/root/.cache/go-build \
	-e DATABASE_URL="postgres://postgres:postgres@$NAME-pg:5432/postgres?sslmode=disable" \
	-e REQUIRE_DB=1 \
	"$GO_IMAGE" go test "$@"
