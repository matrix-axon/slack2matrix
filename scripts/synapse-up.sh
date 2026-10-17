#!/usr/bin/env bash
# Start a fresh, throwaway Synapse for testing slack2matrix.
#
#   scripts/synapse-up.sh <registration.yaml>
#
# Everything under $DATA is deleted first. Test accounts admin, steve and
# jamie are created with password "test-<name>-pw" (admin is a server admin).
# Set POSTGRES=1 to back Synapse with PostgreSQL (much faster for full-size
# imports) instead of SQLite.
set -euo pipefail

REG=${1:?usage: $0 <appservice registration file>}
NAME=${NAME:-s2m-synapse-test}
PORT=${PORT:-8009}
DATA=${DATA:?set DATA to a directory for the test server (it will be wiped)}
SERVER_NAME=${SERVER_NAME:-test.local}
IMAGE=${IMAGE:-matrixdotorg/synapse:v1.163.0}

POSTGRES=${POSTGRES:-}
NET=$NAME-net
PG=$NAME-postgres

docker rm -f "$NAME" "$PG" >/dev/null 2>&1 || true
rm -rf "$DATA" "$DATA-postgres"
mkdir -p "$DATA"
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

# Run as the invoking user so the generated files stay editable.
docker run --rm -v "$DATA:/data" -e UID="$(id -u)" -e GID="$(id -g)" \
	-e SYNAPSE_SERVER_NAME="$SERVER_NAME" -e SYNAPSE_REPORT_STATS=no "$IMAGE" generate >/dev/null

if [ -n "$POSTGRES" ]; then
	# synchronous_commit=off avoids waiting on fsync for every event, which
	# dominates import time on slow disks; acceptable for a test server.
	# Keep the database next to $DATA (not in Docker's own storage) and owned
	# by the invoking user so it can be wiped without root.
	mkdir -p "$DATA-postgres"
	docker run -d --name "$PG" --network "$NET" --user "$(id -u):$(id -g)" \
		-v "$DATA-postgres:/var/lib/postgresql/data" \
		-e POSTGRES_USER=synapse -e POSTGRES_PASSWORD=synapse \
		-e POSTGRES_INITDB_ARGS="--encoding=UTF8 --lc-collate=C --lc-ctype=C" \
		postgres:16 -c synchronous_commit=off >/dev/null
	for _ in $(seq 1 60); do
		docker exec "$PG" pg_isready -U synapse >/dev/null 2>&1 && break
		sleep 1
	done
	# Swap the generated SQLite database section for PostgreSQL.
	python3 - "$DATA/homeserver.yaml" "$PG" <<'PY'
import re, sys
path, host = sys.argv[1], sys.argv[2]
s = open(path).read()
s = re.sub(r"database:\n(?:  .*\n)+", f"""database:
  name: psycopg2
  args:
    user: synapse
    password: synapse
    database: synapse
    host: {host}
    cp_min: 5
    cp_max: 20
""", s, count=1)
open(path, "w").write(s)
PY
fi

cp "$REG" "$DATA/slack-import-registration.yaml"
cat >>"$DATA/homeserver.yaml" <<YAML

app_service_config_files:
  - /data/slack-import-registration.yaml
max_upload_size: 1G
enable_authenticated_media: true
# Test clients log in repeatedly.
rc_login:
  address: {per_second: 100, burst_count: 1000}
  account: {per_second: 100, burst_count: 1000}
  failed_attempts: {per_second: 100, burst_count: 1000}
YAML

docker run -d --name "$NAME" --network "$NET" -p "$PORT:8008" -v "$DATA:/data" \
	-e UID="$(id -u)" -e GID="$(id -g)" "$IMAGE" >/dev/null

# First start creates the database schema, which can take minutes on slow disks.
for _ in $(seq 1 600); do
	curl -sf "http://localhost:$PORT/health" >/dev/null && break
	sleep 1
done
curl -sf "http://localhost:$PORT/health" >/dev/null || { docker logs --tail 30 "$NAME"; exit 1; }

for u in admin steve jamie; do
	flag=--no-admin
	[ "$u" = admin ] && flag=--admin
	docker exec "$NAME" register_new_matrix_user -u "$u" -p "test-$u-pw" $flag \
		-c /data/homeserver.yaml "http://localhost:8008" >/dev/null
done
echo "Synapse $SERVER_NAME is up on http://localhost:$PORT (container $NAME, data $DATA)"
