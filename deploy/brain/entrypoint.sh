#!/usr/bin/env bash
set -euo pipefail

OC_HOME="${OC_HOME:-/home/claw/.openclaw}"
OC_SKEL="${OC_SKEL:-/opt/openclaw-skel}"
OC_GW_HOST="${OC_GW_HOST:-127.0.0.1}"  
OC_GW_PORT="${OC_GW_PORT:-18789}"
GATEWAY_PORT="${GATEWAY_PORT:-4000}"

mkdir -p "$OC_HOME"
if [ ! -f "$OC_HOME/openclaw.json" ]; then
  echo "[entrypoint] ~/.openclaw kosong → semai dari skeleton image"
  cp -a "$OC_SKEL/." "$OC_HOME/" 2>/dev/null || true
else
  echo "[entrypoint] state OpenClaw persisten terdeteksi — dipakai apa adanya"
fi
for d in bin tools node_modules; do
  if [ -d "$OC_SKEL/$d" ]; then
    mkdir -p "$OC_HOME/$d"
    cp -a "$OC_SKEL/$d/." "$OC_HOME/$d/" 2>/dev/null || true
  fi
done

if ! openclaw --version >/dev/null 2>&1; then
  echo "[entrypoint] FATAL: 'openclaw' tak bisa dijalankan — runtime (bin/tools) tak lengkap di $OC_HOME" >&2
  echo "[entrypoint]        isi: $(ls "$OC_HOME" 2>/dev/null | tr '\n' ' ')" >&2
  exit 1
fi

echo "[entrypoint] versi runtime: openclaw=$(openclaw --version 2>&1 | head -1) claude=$(claude --version 2>&1 | head -1)"

CLAUDE_HOME="${CLAUDE_HOME:-/home/claw/.claude}"
sudo mkdir -p "$CLAUDE_HOME"
if [ ! -w "$CLAUDE_HOME" ]; then
  sudo chown -R claw:claw "$CLAUDE_HOME" || true
fi

if [ -z "$(ls -A "$CLAUDE_HOME" 2>/dev/null || true)" ]; then
  echo "[entrypoint] PERINGATAN: $CLAUDE_HOME kosong — Claude Code belum login."
  echo "[entrypoint]   Setiap turn agent akan gagal: 'FailoverError: write EPIPE'."
  echo "[entrypoint]   Login SEKALI:  docker exec -it pa_ai_brain claude  (ikuti alur OAuth)"
fi

# ── 2) Nyalakan OpenClaw gateway (latar) ──────────────────────────────────────
echo "[entrypoint] menyalakan openclaw gateway di ${OC_GW_HOST}:${OC_GW_PORT}"
HOST="$OC_GW_HOST" openclaw gateway &
OC_PID=$!

# ── 3) Tunggu gateway siap (TCP) ──────────────────────────────────────────────
ready=0
for _ in $(seq 1 60); do
  if (exec 3<>"/dev/tcp/${OC_GW_HOST}/${OC_GW_PORT}") 2>/dev/null; then
    exec 3>&- 3<&- || true
    ready=1
    break
  fi
  if ! kill -0 "$OC_PID" 2>/dev/null; then
    echo "[entrypoint] FATAL: openclaw gateway mati saat start" >&2
    exit 1
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "[entrypoint] PERINGATAN: gateway belum menerima TCP dalam 60d — lanjut (api-gateway akan retry per-turn)"
else
  echo "[entrypoint] openclaw gateway siap"
fi

# ── 4) Nyalakan api-gateway (latar) ───────────────────────────────────────────
echo "[entrypoint] menyalakan api-gateway di :${GATEWAY_PORT}"
cd /app/gateway
./api-gateway &
AG_PID=$!

# ── 5) Supervisor: matikan keduanya bila salah satu berhenti / sinyal ─────────
terminate() {
  echo "[entrypoint] sinyal berhenti — mematikan proses"
  kill -TERM "$AG_PID" "$OC_PID" 2>/dev/null || true
}
trap terminate TERM INT

wait -n "$OC_PID" "$AG_PID"
code=$?
echo "[entrypoint] satu proses berhenti (code=${code}) — mematikan sisanya"
kill -TERM "$AG_PID" "$OC_PID" 2>/dev/null || true
wait || true
exit "$code"
