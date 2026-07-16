# syntax=docker/dockerfile:1
#
# Image "brain" (Opsi A): OpenClaw (CLI + gateway) + api-gateway (Go) dalam SATU
# container.
# Build context = ROOT repo (butuh services/api-gateway + deploy/brain/entrypoint.sh).

ARG GO_IMAGE=golang:1.26-bookworm

# ── Stage 1: build binary api-gateway ───────────────────────────────────────
FROM ${GO_IMAGE} AS gobuild
WORKDIR /src
COPY services/api-gateway/go.mod services/api-gateway/go.sum ./
RUN go mod download
COPY services/api-gateway/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/api-gateway .

# ── Stage 2: runtime ────────────────────────────────────────────────────────
FROM ubuntu:22.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates sudo tini \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash -u 1000 claw \
    && echo "claw ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

USER claw
WORKDIR /home/claw
ENV PATH="/home/claw/.local/bin:/home/claw/.openclaw/bin:/home/claw/bin:${PATH}"

# Kosong = selalu tarik versi TERBARU. Ini pilihan sadar (lihat ADR di README):
# tiap `--build` ikut meng-upgrade OpenClaw, walau rebuild-nya untuk alasan lain.
# Penawarnya: entrypoint mencatat versi tiap start, jadi upgrade yang memecahkan
# sesuatu bisa dilacak lewat log. Untuk menyematkan versi tertentu (mis. saat
# membisect regresi), TANPA mengubah berkas ini:
#   docker compose ... build --build-arg OPENCLAW_VERSION=2026.7.1 brain
ARG OPENCLAW_VERSION=""
RUN OPENCLAW_VERSION="${OPENCLAW_VERSION}" curl -fsSL https://openclaw.ai/install-cli.sh | bash \
    && openclaw --version

RUN sudo mkdir -p /opt/openclaw-skel \
    && sudo cp -a /home/claw/.openclaw/. /opt/openclaw-skel/ \
    && sudo chown -R claw:claw /opt/openclaw-skel

# Claude Code CLI — WAJIB. OpenClaw dikonfigurasi dengan provider=claude-cli, artinya
# ia menjalankan binary `claude` sebagai subprocess dan menulis prompt ke stdin-nya.
# Tanpa binary ini setiap turn mati dalam ~3ms dengan "FailoverError: write EPIPE"
# (OpenClaw menulis ke pipa yang ujungnya tak pernah ada) — pesan gagal TANPA
# menyebut `claude` sama sekali, jadi sangat menyesatkan. Di dev Windows ini lolos
# karena Claude Code memang terpasang di mesin; di container harus dipasang eksplisit.
# Terpasang ke /home/claw/.local/bin (sudah ada di PATH, lihat ENV PATH di atas).
# Kosong = versi terbaru, alasan & cara pin ad-hoc sama spt OPENCLAW_VERSION di atas.
ARG CLAUDE_CODE_VERSION=""
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- ${CLAUDE_CODE_VERSION:+"$CLAUDE_CODE_VERSION"} \
    && claude --version

WORKDIR /app/gateway
COPY --chown=claw:claw --from=gobuild /out/api-gateway ./api-gateway
COPY --chown=claw:claw services/api-gateway/assets ./assets

COPY --chown=claw:claw deploy/brain/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN sudo chmod +x /usr/local/bin/entrypoint.sh

ENV GATEWAY_PORT=4000
EXPOSE 4000

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
