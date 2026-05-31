#!/usr/bin/env bash
set -euo pipefail

CONFIG_PATH="${CONFIG_PATH:-/etc/v2node/config.json}"
ACCESS_AUDIT_ENDPOINT="${ACCESS_AUDIT_ENDPOINT:-${1:-}}"
ACCESS_AUDIT_TOKEN="${ACCESS_AUDIT_TOKEN:-${2:-}}"
ACCESS_AUDIT_ENABLED="${ACCESS_AUDIT_ENABLED:-true}"
SNTP_ACCESS="${SNTP_ACCESS:-false}"
ACCESS_AUDIT_BATCH_SIZE="${ACCESS_AUDIT_BATCH_SIZE:-1000}"
ACCESS_AUDIT_MAX_QUEUE_SIZE="${ACCESS_AUDIT_MAX_QUEUE_SIZE:-10000}"
ACCESS_AUDIT_FLUSH_INTERVAL="${ACCESS_AUDIT_FLUSH_INTERVAL:-1s}"
ACCESS_AUDIT_TIMEOUT="${ACCESS_AUDIT_TIMEOUT:-5s}"
RESTART_V2NODE="${RESTART_V2NODE:-true}"

usage() {
    cat <<'EOF'
Usage:
  ACCESS_AUDIT_ENDPOINT="https://logs.sntp.uk/api/v1/access-events" \
  ACCESS_AUDIT_TOKEN="<ingest-token>" \
  bash configure-access-audit.sh

Or:
  bash configure-access-audit.sh "https://logs.sntp.uk/api/v1/access-events" "<ingest-token>"

Optional environment variables:
  CONFIG_PATH=/etc/v2node/config.json
  ACCESS_AUDIT_ENABLED=true|false
  SNTP_ACCESS=true|false
  ACCESS_AUDIT_BATCH_SIZE=1000
  ACCESS_AUDIT_MAX_QUEUE_SIZE=10000
  ACCESS_AUDIT_FLUSH_INTERVAL=1s
  ACCESS_AUDIT_TIMEOUT=5s
  RESTART_V2NODE=true|false
EOF
}

parse_bool_for_shell() {
    case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes|y|on) return 0 ;;
        0|false|no|n|off) return 1 ;;
        *) return 2 ;;
    esac
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "python3 is required" >&2
    exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
    echo "config file not found: $CONFIG_PATH" >&2
    exit 1
fi

enabled_rc=0
parse_bool_for_shell "$ACCESS_AUDIT_ENABLED" || enabled_rc=$?
if [[ $enabled_rc -eq 2 ]]; then
    echo "ACCESS_AUDIT_ENABLED must be true or false" >&2
    exit 1
fi

sntp_rc=0
parse_bool_for_shell "$SNTP_ACCESS" || sntp_rc=$?
if [[ $sntp_rc -eq 2 ]]; then
    echo "SNTP_ACCESS must be true or false" >&2
    exit 1
fi

if [[ $enabled_rc -eq 0 ]]; then
    if [[ -z "$ACCESS_AUDIT_ENDPOINT" || -z "$ACCESS_AUDIT_TOKEN" ]]; then
        echo "ACCESS_AUDIT_ENDPOINT and ACCESS_AUDIT_TOKEN are required when ACCESS_AUDIT_ENABLED=true" >&2
        usage
        exit 1
    fi
fi

backup_path="${CONFIG_PATH}.bak.$(date +%Y%m%d%H%M%S)"
cp -a "$CONFIG_PATH" "$backup_path"

export CONFIG_PATH
export ACCESS_AUDIT_ENDPOINT
export ACCESS_AUDIT_TOKEN
export ACCESS_AUDIT_ENABLED
export SNTP_ACCESS
export ACCESS_AUDIT_BATCH_SIZE
export ACCESS_AUDIT_MAX_QUEUE_SIZE
export ACCESS_AUDIT_FLUSH_INTERVAL
export ACCESS_AUDIT_TIMEOUT

python3 <<'PY'
import json
import os
import tempfile


def parse_bool(value: str) -> bool:
    value = str(value).strip().lower()
    if value in {"1", "true", "yes", "y", "on"}:
        return True
    if value in {"0", "false", "no", "n", "off"}:
        return False
    raise ValueError(f"invalid boolean: {value}")


def positive_int(name: str, default: int) -> int:
    raw = os.environ.get(name, str(default)).strip()
    value = int(raw)
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


path = os.environ["CONFIG_PATH"]
with open(path, "r", encoding="utf-8") as fh:
    config = json.load(fh)

if not isinstance(config, dict):
    raise SystemExit("config root must be a JSON object")

log_config = config.get("Log")
if not isinstance(log_config, dict):
    log_config = {}
config["Log"] = log_config
log_config.setdefault("Level", "warning")
log_config.setdefault("Output", "")
log_config.setdefault("Access", "none")
log_config["SNTPAccess"] = parse_bool(os.environ.get("SNTP_ACCESS", "false"))

enabled = parse_bool(os.environ.get("ACCESS_AUDIT_ENABLED", "true"))
config["AccessAudit"] = {
    "Enabled": enabled,
    "Endpoint": os.environ.get("ACCESS_AUDIT_ENDPOINT", "").strip(),
    "Token": os.environ.get("ACCESS_AUDIT_TOKEN", "").strip(),
    "BatchSize": positive_int("ACCESS_AUDIT_BATCH_SIZE", 1000),
    "MaxQueueSize": positive_int("ACCESS_AUDIT_MAX_QUEUE_SIZE", 10000),
    "FlushInterval": os.environ.get("ACCESS_AUDIT_FLUSH_INTERVAL", "1s").strip() or "1s",
    "Timeout": os.environ.get("ACCESS_AUDIT_TIMEOUT", "5s").strip() or "5s",
}

directory = os.path.dirname(path) or "."
fd, tmp_path = tempfile.mkstemp(prefix=".config.json.", dir=directory)
try:
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(config, fh, ensure_ascii=False, indent=4)
        fh.write("\n")
    os.replace(tmp_path, path)
finally:
    if os.path.exists(tmp_path):
        os.unlink(tmp_path)
PY

echo "updated $CONFIG_PATH"
echo "backup saved to $backup_path"

restart_rc=0
parse_bool_for_shell "$RESTART_V2NODE" || restart_rc=$?
if [[ $restart_rc -eq 2 ]]; then
    echo "RESTART_V2NODE must be true or false" >&2
    exit 1
fi

if [[ $restart_rc -eq 0 ]]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl restart v2node
        systemctl status v2node --no-pager -l || true
    else
        service v2node restart
        service v2node status || true
    fi
fi
