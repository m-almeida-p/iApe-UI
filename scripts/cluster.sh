#!/usr/bin/env bash
# Runs the Web UI in Docker on the Yape cluster: its database is in the cluster's
# yape-postgres and it joins the cluster's yape-local network.
#
# Usage: scripts/cluster.sh [--env <file>] [up|down|status]     (default: up)
#
#   up      checks the cluster, creates the database if it is missing, puts its tables in
#           place (from ms-ml-conv-history-migrations), and starts the web server (the
#           web-cluster service of docker-compose.yml)
#   down    stops that web server; the database stays in the cluster
#   status  says whether the cluster, the Yape agent and the web server are up
#
# The cluster is the one in the Yape repositories (cluster/scripts/up.sh). This script
# starts nothing of it and changes nothing in it, but one database: CLUSTER_DB_NAME
# (default agentwebui), created in yape-postgres when it is not there, with the tables the
# repository ms-ml-conv-history-migrations defines (migrations/web_ui/chat, schema web_ui;
# MIGRATIONS_REPO says where it is checked out, ../ms-ml-conv-history-migrations by
# default; it needs the migrate CLI, `make install` there). That PostgreSQL
# lets every connection of its network in without a password, so nothing here is a secret.
#
# The web server reaches the Yape agent on this computer, so start that first
# (scripts/yape-agent.sh); the agent needs the history service's MCP server for its tools
# (its scripts/run_mcp.sh). The page is on http://localhost:$WEB_PORT, 127.0.0.1 only.
# It uses the same port as the full and selftest profiles: run one at a time.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

ENV_FILE=".env"
while [ $# -gt 0 ]; do
  case "$1" in
    --env) use_settings_file "${2:?--env needs a file name}"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) break ;;
  esac
done

DB_NAME="$(required_setting CLUSTER_DB_NAME)"
AGENT_PORT="$(required_setting YAPE_AGENT_PORT)"
WEB_PORT="$(required_setting WEB_PORT)"
PG=yape-postgres

# The name goes into SQL: a plain identifier only.
case "$DB_NAME" in
  [a-z_]*) ;;
  *) echo "CLUSTER_DB_NAME must start with a lowercase letter or _." >&2; exit 2 ;;
esac
if [ -n "$(printf '%s' "$DB_NAME" | tr -d 'a-z0-9_')" ]; then
  echo "CLUSTER_DB_NAME may hold only lowercase letters, digits and _." >&2
  exit 2
fi

compose() { docker compose --env-file "$ENV_FILE" --profile cluster "$@"; }
postgres_up() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$PG" 2>/dev/null)" = healthy ]; }
network_up() { docker network inspect yape-local >/dev/null 2>&1; }
agent_up() { curl -fsS -m 2 "http://127.0.0.1:${AGENT_PORT}/api/list-apps" 2>/dev/null | grep -q yape_agent; }
web_up() { curl -fsS -m 2 -o /dev/null "http://127.0.0.1:${WEB_PORT}/healthz" 2>/dev/null; }

case "${1:-up}" in
  status)
    if postgres_up && network_up; then echo "cluster:  $PG is healthy, network yape-local exists"; else echo "cluster:  NOT up (run cluster/scripts/up.sh of the Yape repositories)"; fi
    if agent_up; then echo "agent:    yape_agent answers on :$AGENT_PORT"; else echo "agent:    not running (scripts/yape-agent.sh)"; fi
    if web_up; then echo "web:      answers on http://localhost:$WEB_PORT"; else echo "web:      not running (scripts/cluster.sh up)"; fi
    exit 0
    ;;
  down)
    compose stop web-cluster
    compose rm -f web-cluster
    echo "The web server is stopped. Its database ($DB_NAME) stays in $PG."
    exit 0
    ;;
  up) ;;
  *) echo "Unknown command: $1 (try --help)" >&2; exit 2 ;;
esac

if ! postgres_up || ! network_up; then
  echo "The Yape cluster is not up: $PG is not healthy, or the network yape-local does not exist." >&2
  echo "Start it with cluster/scripts/up.sh in the Yape repositories, and run this again." >&2
  exit 1
fi
if ! agent_up; then
  echo "Warning: the Yape agent does not answer on :$AGENT_PORT; the page will load but chats will fail." >&2
  echo "Start it with scripts/yape-agent.sh (and the history service's MCP server for its tools)." >&2
fi

# The database and its tables are not made here: ms-ml-conv-history-migrations creates the database when it is missing
# (database/create) and defines the tables (migrations/web_ui/chat); scripts/schema.sh --create runs both, and running it again
# changes nothing. The cluster trusts local connections, so the URL needs no password.
scripts/schema.sh --env "$ENV_FILE" --create --db-url "postgres://postgres@localhost:5432/$DB_NAME?sslmode=disable"

# Named, so compose does not also start the PostgreSQL of this repository's file.
compose up -d --build --wait web-cluster
echo "The web server is up on the Yape cluster: http://localhost:$WEB_PORT (the first user is ADMIN_EMAIL of $ENV_FILE)."
