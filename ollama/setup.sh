#!/usr/bin/env bash
#
# One-time host setup for the Fathom Ollama backend.
#
# Applies ollama/config.env to the host: configures the systemd unit to listen
# where the cluster VMs can reach it, pulls the model, and pins serving parameters
# into a derived model alias. Idempotent -- safe to re-run after editing config.env.
#
#   sudo ./setup.sh            full setup
#   sudo ./setup.sh --verify   check an existing install, change nothing
#   sudo ./setup.sh --judge    install the CPU-only judge instance for eval/
#
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${FATHOM_CONFIG:-${SCRIPT_DIR}/config.env}"
DROPIN_DIR="/etc/systemd/system/ollama.service.d"
DROPIN_FILE="${DROPIN_DIR}/10-fathom.conf"
JUDGE_UNIT="/etc/systemd/system/ollama-judge.service"

VERIFY_ONLY=0
JUDGE_ONLY=0
[[ "${1:-}" == "--verify" ]] && VERIFY_ONLY=1
[[ "${1:-}" == "--judge" ]] && JUDGE_ONLY=1

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

# --- preflight -------------------------------------------------------------

[[ -f "$CONFIG_FILE" ]] || die "config not found: $CONFIG_FILE"
# shellcheck source=config.env
source "$CONFIG_FILE"

: "${FATHOM_MODEL:?not set in config.env}"
: "${FATHOM_MODEL_ALIAS:?not set in config.env}"
: "${OLLAMA_BIND:?not set in config.env}"
: "${OLLAMA_HOST_IP:?not set in config.env}"
: "${OLLAMA_PORT:?not set in config.env}"

command -v ollama >/dev/null 2>&1 || die \
  "ollama not installed. Install it first: curl -fsSL https://ollama.com/install.sh | sh"

command -v systemctl >/dev/null 2>&1 || die "systemd required; this script manages ollama.service"

# Note: pipelines feeding `grep -q` are avoided throughout. Under `set -o pipefail`
# grep exits on first match, the producer takes SIGPIPE, and the pipeline reports
# failure even though the match succeeded. Capture first, match against a herestring.
UNIT_FILES="$(systemctl list-unit-files 2>/dev/null || true)"
grep -q '^ollama\.service' <<<"$UNIT_FILES" || die \
  "ollama.service not found. Reinstall via the official install script so the unit is created."

# Exact match on the first column of `ollama list`, tolerating the implicit :latest tag.
model_present() {
  local name="$1" list
  list="$(ollama list 2>/dev/null || true)"
  awk -v n="$name" 'NR > 1 && ($1 == n || $1 == n ":latest") { found = 1 } END { exit !found }' \
    <<<"$list"
}

if (( ! VERIFY_ONLY )) && [[ "$(id -u)" -ne 0 ]]; then
  die "must run as root (systemd unit + firewall changes). Try: sudo $0"
fi

# The client talks to the server over HTTP; point it at the loopback side of the
# bind so pulls work regardless of what OLLAMA_HOST is set to in the caller's shell.
export OLLAMA_HOST="127.0.0.1:${OLLAMA_PORT}"
OLLAMA_URL="http://${OLLAMA_HOST_IP}:${OLLAMA_PORT}"

wait_ready() {
  local tries="${1:-60}"
  for ((i = 0; i < tries; i++)); do
    if curl -fsS -m 2 "http://127.0.0.1:${OLLAMA_PORT}/api/version" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# --- verify mode -----------------------------------------------------------

if (( VERIFY_ONLY )); then
  log "Verifying existing setup"
  systemctl is-active --quiet ollama.service \
    && echo "  service:  active" \
    || echo "  service:  NOT active"
  [[ -f "$DROPIN_FILE" ]] \
    && echo "  drop-in:  $DROPIN_FILE" \
    || echo "  drop-in:  missing"
  wait_ready 3 \
    && echo "  loopback: reachable" \
    || echo "  loopback: NOT reachable"
  curl -fsS -m 3 "${OLLAMA_URL}/api/version" >/dev/null 2>&1 \
    && echo "  ${OLLAMA_URL}: reachable" \
    || echo "  ${OLLAMA_URL}: NOT reachable"
  if model_present "$FATHOM_MODEL_ALIAS"; then
    echo "  model:    ${FATHOM_MODEL_ALIAS} present"
  else
    echo "  model:    ${FATHOM_MODEL_ALIAS} MISSING"
  fi
  exit 0
fi

# --- judge mode ------------------------------------------------------------
#
# A second Ollama instance, pinned to the CPU, serving the model that grades the
# evaluation harness's results. It is a separate unit rather than a second model in the
# existing one because OLLAMA_MAX_LOADED_MODELS=1 means a second model would evict the
# agent's from the GPU -- and even at 2, an 8 GB card cannot hold both.
#
# CUDA_VISIBLE_DEVICES="" is the load-bearing line: without it Ollama would try the GPU,
# fail to fit a 19 GB model, and either spill unpredictably or evict the agent.

if (( JUDGE_ONLY )); then
  : "${FATHOM_JUDGE_MODEL:?not set in config.env -- add the judge block}"
  : "${FATHOM_JUDGE_MODEL_ALIAS:?not set in config.env}"
  : "${OLLAMA_JUDGE_BIND:?not set in config.env}"
  : "${OLLAMA_JUDGE_PORT:?not set in config.env}"

  OLLAMA_BIN="$(command -v ollama)"

  log "Writing ${JUDGE_UNIT}"
  cat > "$JUDGE_UNIT" <<EOF
# Managed by fathom/ollama/setup.sh --judge -- edit ollama/config.env and re-run.
#
# The CPU-only Ollama instance used by fathom-eval to grade answers. It shares the
# model store with ollama.service (reads are safe; never pull from both at once) and
# deliberately never touches the GPU.
[Unit]
Description=Ollama Service (Fathom eval judge, CPU only)
After=network-online.target

[Service]
ExecStart=${OLLAMA_BIN} serve
User=ollama
Group=ollama
Restart=always
RestartSec=3
Environment="OLLAMA_HOST=${OLLAMA_JUDGE_BIND}"
Environment="OLLAMA_KEEP_ALIVE=${OLLAMA_JUDGE_KEEP_ALIVE:-5m}"
Environment="OLLAMA_NUM_PARALLEL=1"
Environment="OLLAMA_MAX_LOADED_MODELS=1"
# The whole reason this unit exists: no GPU, ever. The agent's model owns the card.
Environment="CUDA_VISIBLE_DEVICES="
Environment="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

[Install]
WantedBy=default.target
EOF

  log "Enabling and restarting ollama-judge.service"
  systemctl daemon-reload
  systemctl enable ollama-judge.service >/dev/null
  systemctl restart ollama-judge.service

  for _ in $(seq 30); do
    curl -fsS -m 2 "http://127.0.0.1:${OLLAMA_JUDGE_PORT}/api/version" >/dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS -m 5 "http://127.0.0.1:${OLLAMA_JUDGE_PORT}/api/version" >/dev/null 2>&1 || die \
    "ollama-judge did not become ready. Check: journalctl -u ollama-judge -n 50"

  if model_present "$FATHOM_JUDGE_MODEL"; then
    log "Judge base model already present: ${FATHOM_JUDGE_MODEL}"
  else
    warn "${FATHOM_JUDGE_MODEL} is not on disk. The model store is large and the"
    warn "filesystem may be short of room -- check df before pulling several GB."
    ollama pull "$FATHOM_JUDGE_MODEL" || die "pull failed for '${FATHOM_JUDGE_MODEL}'"
  fi

  JUDGE_MODELFILE="$(mktemp)"
  trap 'rm -f "$JUDGE_MODELFILE"' EXIT
  cat > "$JUDGE_MODELFILE" <<EOF
# Generated by fathom/ollama/setup.sh --judge from config.env -- do not edit by hand.
FROM ${FATHOM_JUDGE_MODEL}
PARAMETER num_ctx ${FATHOM_JUDGE_NUM_CTX:-8192}
PARAMETER temperature ${FATHOM_JUDGE_TEMPERATURE:-0}
EOF

  log "Creating judge alias ${FATHOM_JUDGE_MODEL_ALIAS}"
  OLLAMA_HOST="127.0.0.1:${OLLAMA_JUDGE_PORT}" ollama create \
    "$FATHOM_JUDGE_MODEL_ALIAS" -f "$JUDGE_MODELFILE" >/dev/null

  JUDGE_URL="http://${OLLAMA_HOST_IP}:${OLLAMA_JUDGE_PORT}"
  if curl -fsS -m 5 "${JUDGE_URL}/api/version" >/dev/null 2>&1; then
    log "OK: ${JUDGE_URL} is reachable from the host"
  else
    warn "${JUDGE_URL} is not reachable; the eval CronJob will not be able to judge."
  fi

  if [[ "${FATHOM_FIREWALL:-0}" == "1" ]] && command -v ufw >/dev/null 2>&1; then
    log "Allowing ${FATHOM_ALLOWED_CIDR} -> ${OLLAMA_JUDGE_PORT}/tcp via ufw"
    ufw allow from "${FATHOM_ALLOWED_CIDR}" to any port "${OLLAMA_JUDGE_PORT}" proto tcp >/dev/null
  fi

  cat <<EOF

Judge setup complete.

  judge alias : ${FATHOM_JUDGE_MODEL_ALIAS}  (from ${FATHOM_JUDGE_MODEL}, CPU only)
  endpoint    : ${JUDGE_URL}

Put these in eval/helm/values.yaml:

  judge:
    url: ${JUDGE_URL}
    model: ${FATHOM_JUDGE_MODEL_ALIAS}

A judgement takes minutes, not seconds -- that is expected on CPU. The GPU is
untouched, so the agent's model stays resident throughout.
EOF
  exit 0
fi

# --- 1. systemd drop-in ----------------------------------------------------

log "Writing systemd drop-in: ${DROPIN_FILE}"
mkdir -p "$DROPIN_DIR"
cat > "$DROPIN_FILE" <<EOF
# Managed by fathom/ollama/setup.sh -- edit ollama/config.env and re-run instead.
[Service]
Environment="OLLAMA_HOST=${OLLAMA_BIND}"
Environment="OLLAMA_KEEP_ALIVE=${OLLAMA_KEEP_ALIVE:-30m}"
Environment="OLLAMA_NUM_PARALLEL=${OLLAMA_NUM_PARALLEL:-1}"
Environment="OLLAMA_MAX_LOADED_MODELS=${OLLAMA_MAX_LOADED_MODELS:-1}"
EOF

log "Enabling and restarting ollama.service"
systemctl daemon-reload
systemctl enable ollama.service >/dev/null
systemctl restart ollama.service

wait_ready 60 || die "ollama did not become ready. Check: journalctl -u ollama -n 50"
log "Server ready ($(curl -fsS "http://127.0.0.1:${OLLAMA_PORT}/api/version"))"

# --- 2. model --------------------------------------------------------------

if model_present "$FATHOM_MODEL"; then
  log "Base model already present: ${FATHOM_MODEL}"
else
  log "Pulling ${FATHOM_MODEL} (several GB, this takes a while)"
  ollama pull "$FATHOM_MODEL" || die \
    "pull failed for '${FATHOM_MODEL}'. Confirm the tag exists in the registry and that config.env is correct."
fi

# --- 3. derived alias with pinned serving parameters -----------------------

MODELFILE="$(mktemp)"
trap 'rm -f "$MODELFILE"' EXIT
cat > "$MODELFILE" <<EOF
# Generated by fathom/ollama/setup.sh from config.env -- do not edit by hand.
FROM ${FATHOM_MODEL}
PARAMETER num_ctx ${FATHOM_NUM_CTX:-16384}
PARAMETER temperature ${FATHOM_TEMPERATURE:-0.2}
PARAMETER top_p ${FATHOM_TOP_P:-0.9}
EOF

log "Creating model alias ${FATHOM_MODEL_ALIAS} (num_ctx=${FATHOM_NUM_CTX:-16384})"
ollama create "$FATHOM_MODEL_ALIAS" -f "$MODELFILE" >/dev/null

# --- 4. firewall (opt-in) --------------------------------------------------

if [[ "${FATHOM_FIREWALL:-0}" == "1" ]]; then
  if command -v ufw >/dev/null 2>&1; then
    log "Allowing ${FATHOM_ALLOWED_CIDR} -> ${OLLAMA_PORT}/tcp via ufw"
    ufw allow from "${FATHOM_ALLOWED_CIDR}" to any port "${OLLAMA_PORT}" proto tcp >/dev/null
  else
    warn "FATHOM_FIREWALL=1 but ufw not found; no rule added. Restrict port ${OLLAMA_PORT} yourself."
  fi
else
  warn "Port ${OLLAMA_PORT} is bound to ${OLLAMA_BIND} with no authentication and no firewall rule."
  warn "Set FATHOM_FIREWALL=1 in config.env to restrict it to ${FATHOM_ALLOWED_CIDR}."
fi

# --- 5. reachability from the cluster network ------------------------------

log "Checking reachability on the libvirt address"
if curl -fsS -m 5 "${OLLAMA_URL}/api/version" >/dev/null 2>&1; then
  log "OK: ${OLLAMA_URL} is reachable from the host"
else
  warn "${OLLAMA_URL} is not reachable. Is virbr0 up? (ip -brief addr show virbr0)"
  warn "Pods cannot reach the model until this address answers."
fi

cat <<EOF

Setup complete.

  model alias : ${FATHOM_MODEL_ALIAS}  (from ${FATHOM_MODEL})
  endpoint    : ${OLLAMA_URL}

Put these in helm/ values so the in-cluster agent dials the host:

  ollama:
    url:   "${OLLAMA_URL}"
    model: "${FATHOM_MODEL_ALIAS}"

Verify from inside the cluster once the VMs are up:

  kubectl run curl-test --rm -it --image=curlimages/curl --restart=Never -- \\
    curl -sS ${OLLAMA_URL}/api/version
EOF
