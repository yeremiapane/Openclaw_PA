# syntax=docker/dockerfile:1
#
# Image "brain" : OpenClaw (CLI + gateway) + api-gateway (Go) dalam SATU
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

ARG OPENCLAW_VERSION=""
RUN OPENCLAW_VERSION="${OPENCLAW_VERSION}" curl -fsSL https://openclaw.ai/install-cli.sh | bash \
    && openclaw --version

RUN sudo mkdir -p /opt/openclaw-skel \
    && sudo cp -a /home/claw/.openclaw/. /opt/openclaw-skel/ \
    && sudo chown -R claw:claw /opt/openclaw-skel

ARG CLAUDE_CODE_VERSION=""
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- ${CLAUDE_CODE_VERSION:+"$CLAUDE_CODE_VERSION"} \
    && claude --version

RUN sudo apt-get update && sudo apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv \
      libpango-1.0-0 libpangoft2-1.0-0 libcairo2 libgdk-pixbuf-2.0-0 \
      fonts-dejavu-core \
    && sudo rm -rf /var/lib/apt/lists/*

ENV PA_VENV=/opt/pa-tools
RUN sudo mkdir -p ${PA_VENV} && sudo chown claw:claw ${PA_VENV} \
    && python3 -m venv ${PA_VENV} \
    && ${PA_VENV}/bin/pip install --no-cache-dir --upgrade pip \
    && ${PA_VENV}/bin/pip install --no-cache-dir \
         openpyxl XlsxWriter \
         python-docx python-pptx \
         weasyprint reportlab \
         matplotlib pandas Pillow \
    && ${PA_VENV}/bin/python -c "import openpyxl, docx, pptx, weasyprint, reportlab, matplotlib, pandas, PIL; print('perkakas dokumen OK')"
ENV PATH="${PA_VENV}/bin:${PATH}"

WORKDIR /app/gateway
COPY --chown=claw:claw --from=gobuild /out/api-gateway ./api-gateway
COPY --chown=claw:claw services/api-gateway/assets ./assets

COPY --chown=claw:claw deploy/brain/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN sudo chmod +x /usr/local/bin/entrypoint.sh

ENV GATEWAY_PORT=4000
EXPOSE 4000

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
