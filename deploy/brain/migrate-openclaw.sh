#!/usr/bin/env bash
# Migrasi state OpenClaw dari mesin dev (Windows ~/.openclaw) ke bind-mount "brain"
# di server. Jalankan DI SERVER, setelah menyalin & mengekstrak arsip dari Windows
# (lihat pack-openclaw.ps1 → openclaw-dump.tar.gz → `tar -xzf` di sini).
#
# Pemakaian:
#   ./migrate-openclaw.sh <SUMBER> [TUJUAN]
#     <SUMBER> = folder hasil ekstrak (berisi openclaw.json, state/, workspaces/, ...)
#     [TUJUAN] = folder state brain (default: ./data/openclaw = OPENCLAW_STATE_DIR)
set -euo pipefail

SRC="${1:?SUMBER wajib: folder ekstrak ~/.openclaw dari Windows}"
DST="${2:-./data/openclaw}"

if [ ! -d "$SRC" ]; then
  echo "[migrate] ERROR: SUMBER '$SRC' bukan folder" >&2
  exit 1
fi

echo "[migrate] SUMBER=$SRC"
echo "[migrate] TUJUAN=$DST"
mkdir -p "$DST"

# 1) Salin sub-dir stateful. Sengaja LEWATI bin/ & node_modules/ — binary Linux
#    datang dari image (skeleton), bukan dari dump Windows. Juga lewati log/cache.
for d in openclaw.json identity devices state agents memory workspaces workspace skills plugin-skills hooks; do
  if [ -e "$SRC/$d" ]; then
    echo "[migrate] salin $d"
    cp -a "$SRC/$d" "$DST/"
  fi
done

# 2) Rewrite path Windows → Linux di openclaw.json (prefix drive + pemisah \ → /).
if [ -f "$DST/openclaw.json" ]; then
  echo "[migrate] rewrite path Windows → /home/claw/.openclaw pada openclaw.json"
  cp -a "$DST/openclaw.json" "$DST/openclaw.json.pre-migrate.bak"
  python3 - "$DST/openclaw.json" <<'PY'
import json, sys
p = sys.argv[1]
data = json.load(open(p, encoding="utf-8"))

def fix(v):
    if isinstance(v, str):
        low = v.lower()
        idx = low.find(".openclaw")
        # hanya path absolut Windows: ada 'X:\' atau 'X:/' sebelum '.openclaw'
        head = v[:idx]
        if idx != -1 and (":\\" in head or ":/" in head):
            tail = v[idx + len(".openclaw"):].replace("\\", "/")
            return "/home/claw/.openclaw" + tail
        return v
    if isinstance(v, list):
        return [fix(x) for x in v]
    if isinstance(v, dict):
        return {k: fix(x) for k, x in v.items()}
    return v

json.dump(fix(data), open(p, "w", encoding="utf-8"), indent=2, ensure_ascii=False)
print("   ok — cadangan: openclaw.json.pre-migrate.bak")
PY
else
  echo "[migrate] PERINGATAN: openclaw.json tak ditemukan di dump — lewati rewrite"
fi

# 3) Kepemilikan WAJIB uid 1000 (user 'claw' di container) agar OpenClaw bisa menulis.
echo "[migrate] chown -R 1000:1000 $DST"
sudo chown -R 1000:1000 "$DST"

cat <<EOF

[migrate] SELESAI.
Langkah lanjut:
  1) Nyalakan stack:
       docker compose -f docker-compose.yml -f deploy/brain/docker-compose.brain.yml up -d --build
  2) VERIFIKASI auth OpenClaw (claude-cli/oauth) — uji satu turn:
       docker exec -it pa_ai_brain \\
         openclaw agent --agent pa_communicator --session-key migrasi:test --message 'ping' --json
     Jika muncul error auth, login ulang DI DALAM container (lihat README → "Auth OpenClaw").
EOF
