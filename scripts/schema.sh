#!/usr/bin/env bash
# Puts the web UI's tables in the database DB_URL points at. The web server creates none: they are
# defined in the repository ms-ml-conv-history-migrations (migrations/web_ui/chat, schema web_ui).
#
# Usage: scripts/schema.sh [--env <file>] [--db-url <url>]
#
#   --db-url  the database to use instead of DB_URL of the settings file. It goes in the arguments, which
#             ps shows, so give one without a password (the cluster's PostgreSQL asks for none).
#
# MIGRATIONS_REPO (in the settings file) says where that repository is checked out, and it needs the migrate CLI (`make install` there). Applying the tables again
# changes nothing. The database must exist: compose creates the one of this repository's file,
# and scripts/cluster.sh creates the cluster's.
#
# DB_URL is read from the settings file (.env). Its search_path is left out here: the version table
# of the migrations stays in public, as that repository says.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=lib/process.sh
. scripts/lib/process.sh

ENV_FILE=".env"
DB_URL_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --env) use_settings_file "${2:?--env needs a file name}"; shift 2 ;;
    --db-url) DB_URL_ARG="${2:?--db-url needs a URL}"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

MIGRATIONS_REPO="$(required_setting MIGRATIONS_REPO)"
if [ ! -d "$MIGRATIONS_REPO/migrations/web_ui/chat" ]; then
  echo "The tables are defined in ms-ml-conv-history-migrations (migrations/web_ui/chat), which is not at $MIGRATIONS_REPO." >&2
  echo "Check it out there, or set MIGRATIONS_REPO to where it is, and run this again." >&2
  exit 1
fi

# The URL without search_path, and handed over in the environment (not in the arguments, which ps shows).
if [ -n "$DB_URL_ARG" ]; then url="$DB_URL_ARG"; else url="$(required_setting DB_URL)"; fi
dsn="$(printf '%s' "$url" | sed -E 's/([?&])search_path=[^&]*&?/\1/; s/[?&]$//')"
MIGRATIONS_DB_DSN="$dsn" make -C "$MIGRATIONS_REPO" migrations/up SERVICE=web_ui >/dev/null
echo "The web UI's tables are in place (schema web_ui)."
