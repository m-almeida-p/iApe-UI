#!/usr/bin/env bash
# Starts the Yape agent for the Web UI to talk to: an ADK REST agent (cmd/yape-agent)
# that answers with the models of the Yape AI gateway and reads the conversation
# history through the history service's MCP server.
#
# Usage: scripts/yape-agent.sh [--env <file>] [start|stop|status|check]     (default: start)
#
#   --env <file>  read the settings from this file (default: .env)
#   check         asks for a token and lists the MCP tools, without starting anything
#                 or sending a word to a model
#
# Settings are in .env, written down in .env.example (section "Yape agent"). The
# credentials are NOT: the gateway, its headers and the anonymous-token credentials
# are read from the Yape agent service's own .env (YAPE_AGENT_BS_ENV) each time this
# script runs, and handed to the agent in its environment only. They are not copied
# into this repository, not written to a file here, and never printed. The agent
# fetches its own bearer token with them and renews it before it expires.
#
# It builds the agent into bin/yape-agent, starts it in the background and waits until
# it lists its application. The process id is kept in logs/yape-agent.pid and the
# output goes to logs/yape-agent-<time>.log. The agent listens on this computer only
# (the ADK launcher from 2.5 on); a container reaches it as host.docker.internal, a name
# the launcher refuses ("host not allowed", its guard against DNS rebinding) unless it is
# allowed: YAPE_AGENT_ALLOWED_HOST, which opens that one host name and nothing else.
#
# The MCP server (YAPE_MCP_URL, default http://localhost:3200/mcp) is the history
# service's: start it with its scripts/run_mcp.sh. It has no authentication.
#
# Then point the Web UI at it, in .env:
#   AGENT_BASE_URL=http://localhost:9095/api   AGENT_APP_NAME=yape_agent   AGENT_REQUEST_TIMEOUT=600s
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

ROOT="$PWD"
BS_ENV="$(required_setting YAPE_AGENT_BS_ENV)"
PORT="$(required_setting YAPE_AGENT_PORT)"
SSE_TIMEOUT="$(required_setting YAPE_AGENT_SSE_TIMEOUT)"
ALLOWED_HOST="$(setting YAPE_AGENT_ALLOWED_HOST)"
MODEL="$(setting YAPE_AGENT_MODEL)"
MCP_URL="$(setting YAPE_MCP_URL)"
GO="$(setting GO)"
[ -n "$GO" ] || GO="go"

# Absolute, because the agent is built and run from other directories.
absolute() { case "$1" in /*) echo "$1" ;; *) echo "$ROOT/$1" ;; esac; }
# Two steps each: inside a nested substitution, a failure of required_setting would
# not stop the script.
BIN_DIR="$(required_setting BIN_DIR)"
BIN_DIR="$(absolute "$BIN_DIR")"
LOG_DIR="$(required_setting LOG_DIR)"
LOG_DIR="$(absolute "$LOG_DIR")"
BS_ENV="$(absolute "$BS_ENV")"
BIN="$BIN_DIR/yape-agent"
PID_FILE="$LOG_DIR/yape-agent.pid"
URL="http://127.0.0.1:${PORT}/api"
APP="yape_agent"

# The pid may have been given to another process since the file was written.
running() { [ -f "$PID_FILE" ] && is_process "$(cat "$PID_FILE")" yape-agent; }

stop_agent() {
  local pid
  pid="$(cat "$PID_FILE")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  rm -f "$PID_FILE"
}

case "${1:-start}" in
  status)
    if running; then echo "The agent is running (pid $(cat "$PID_FILE")) at $URL"; else echo "The agent is not running."; fi
    exit 0
    ;;
  stop)
    if ! running; then echo "The agent is not running."; rm -f "$PID_FILE"; exit 0; fi
    stop_agent
    echo "The agent is stopped."
    exit 0
    ;;
  start | check) ;;
  *) echo "Unknown command: $1 (try --help)" >&2; exit 2 ;;
esac
COMMAND="${1:-start}"

if [ "$COMMAND" = start ] && running; then
  echo "The agent is already running (pid $(cat "$PID_FILE")) at $URL" >&2
  exit 1
fi
if [ ! -r "$BS_ENV" ]; then
  echo "Cannot read $BS_ENV (YAPE_AGENT_BS_ENV): it is the Yape agent service's .env, where the gateway and its credentials are." >&2
  exit 1
fi

# bs_value <NAME>: the value the Yape agent service's .env gives NAME, without the quotes
# around it, or nothing. The file is read as text, not run as a script.
bs_value() {
  { grep -E "^$1=" "$BS_ENV" || true; } | head -1 | cut -d= -f2- | sed -E "s/^'(.*)'\$/\\1/; s/^\"(.*)\"\$/\\1/"
}

# What the agent is handed in its environment (not its arguments, which any user of
# this machine can read with ps): the credentials, and the two optional settings.
agent_env=()
for name in YAPETECH_AI_GATEWAY LLM_GATEWAY_DOMAIN_ID LLM_GATEWAY_APP_ID LLM_GATEWAY_PROVIDER_BS \
  YAPETECH_ANONYMOUS_TOKEN_BASE_URL YAPETECH_ANONYMOUS_TOKEN_PATH YAPETECH_ANONYMOUS_TOKEN_USERNAME \
  YAPETECH_ANONYMOUS_TOKEN_PASSWORD YAPETECH_ANONYMOUS_TOKEN_CLIENT_ID YAPETECH_ANONYMOUS_TOKEN_GRANT_TYPE \
  YAPETECH_ANONYMOUS_TOKEN_SCOPE YAPETECH_ANONYMOUS_TOKEN_TIMEOUT_SECONDS; do
  value="$(bs_value "$name")"
  if [ -n "$value" ]; then agent_env+=("$name=$value"); fi
done
if [ -n "$MODEL" ]; then agent_env+=("YAPE_AGENT_MODEL=$MODEL"); fi
if [ -n "$MCP_URL" ]; then agent_env+=("YAPE_MCP_URL=$MCP_URL"); fi

mkdir -p "$LOG_DIR" "$BIN_DIR"
echo "Building the agent into $BIN ..."
"$GO" build -trimpath -o "$BIN" ./cmd/yape-agent

if [ "$COMMAND" = check ]; then
  exec env ${agent_env[@]+"${agent_env[@]}"} "$BIN" check
fi

log="$LOG_DIR/yape-agent-$(date +%Y%m%d-%H%M%S).log"
(
  # The agent reads a .env of its own directory too; it has none of these settings.
  web_flags=(--port="$PORT")
  if [ -n "$ALLOWED_HOST" ]; then web_flags+=(--allow_origins="$ALLOWED_HOST"); fi
  env ${agent_env[@]+"${agent_env[@]}"} nohup "$BIN" web "${web_flags[@]}" api --sse-write-timeout="$SSE_TIMEOUT" >"$log" 2>&1 &
  echo $! >"$PID_FILE"
)

for _ in $(seq 1 100); do
  if ! running; then
    echo "The agent stopped while starting. Its log ($log), last lines:" >&2
    tail -n 20 "$log" >&2
    rm -f "$PID_FILE"
    exit 1
  fi
  if curl -fsS -m 2 "$URL/list-apps" 2>/dev/null | grep -q "\"$APP\""; then
    scope="$(listening_scope "$(cat "$PID_FILE")" "$PORT")"
    if [ "$scope" != loopback ]; then
      stop_agent
      echo "The agent does not listen on this computer only (where: $scope), so anyone who can reach this machine" >&2
      echo "could use the gateway's models through port $PORT. It has been stopped." >&2
      exit 1
    fi
    echo "The agent is ready (pid $(cat "$PID_FILE")) at $URL, serving \"$APP\"."
    sed -n 's/^yape-agent: /  /p' "$log" | head -1
    echo "Log: $log"
    exit 0
  fi
  sleep 0.3
done
echo "The agent did not list \"$APP\" within 30 seconds. Its log ($log), last lines:" >&2
tail -n 20 "$log" >&2
exit 1
