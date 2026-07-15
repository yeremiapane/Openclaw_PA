# PA AI System — Dokumentasi Arsitektur & Workflow Development

> Dokumen ini merangkum seluruh diskusi arsitektur untuk membangun sistem
> **Personal Assistant AI berbasis WhatsApp** menggunakan OpenClaw + Claude Sonnet + WAHA.
> Gunakan sebagai referensi utama selama proses development.

---

## Daftar Isi

1. [Overview Sistem](#1-overview-sistem)
2. [Technology Stack](#2-technology-stack)
3. [Arsitektur Keamanan (Layered)](#3-arsitektur-keamanan-layered)
4. [Microservice Architecture](#4-microservice-architecture)
5. [Multi-Agent Design (OpenClaw)](#5-multi-agent-design-openclaw)
6. [Alur Data &amp; Komunikasi](#6-alur-data--komunikasi)
7. [Memory Architecture](#7-memory-architecture)
8. [SOUL.md Templates](#8-soulmd-templates)
9. [Lobster Workflow (YAML)](#9-lobster-workflow-yaml)
10. [Conversation State Machine](#10-conversation-state-machine)
11. [Docker Compose](#11-docker-compose)
12. [API Contracts](#12-api-contracts)
13. [Langkah-Langkah Development](#13-langkah-langkah-development)
14. [Catatan Implementasi Penting](#14-catatan-implementasi-penting)

---

## 1. Overview Sistem

### Deskripsi

Sistem PA AI adalah platform yang menempatkan AI sebagai perantara komunikasi antara **Super User (SU)** dengan **pihak eksternal** dan **support user**. AI berperan sebagai Personal Assistant yang mampu:

- Menerima perintah dari SU untuk mengatur meeting via WhatsApp
- Menghubungi pihak eksternal secara otomatis dan menegosiasikan jadwal
- Meminta approval dari SU sebelum melakukan konfirmasi
- Menghubungi Support User (Bu Nova) untuk reservasi venue
- Membuat Google Calendar event dan mengirim RSVP via email
- Mengingat seluruh histori percakapan per kontak secara persisten

### Tiga Aktor Utama

| Aktor             | Role                                          | Trust Level         | WA Channel      |
| ----------------- | --------------------------------------------- | ------------------- | --------------- |
| Pak Sudianto (SU) | Super User – pemberi perintah & approver     | Trusted             | WA Session SU   |
| Pihak Eksternal   | External Contact – penerima undangan meeting | Untrusted / Unknown | WA Session Ext  |
| Bu Nova           | Support User – booking venue                 | Semi-trusted        | WA Session Nova |

### Dua Skenario Utama

**Skenario A — Inisiasi dari SU:**

```
SU: "Hubungi Pak Andrew PT Marteux 08123456789,
     meeting offline bahas konsolidasi AI"
  ↓
Orchestrator Agent
  → PA Communicator → kontak Pak Andrew via WA
  → negosiasi jadwal
  → lapor ke SU, minta approval
  → (jika offline & belum ada venue) Support Agent → Bu Nova
  → Buat Calendar Event + kirim RSVP email
  → Konfirmasi ke SU + Pak Andrew
  → Reminder 3 jam sebelum meeting
```

**Skenario B — Inisiasi dari Pihak Eksternal:**

```
Pihak Eksternal → WA ke bot
  ↓
PA Communicator Agent
  → Perkenalan diri
  → Kumpulkan: Nama, PT, Email, Keperluan
  → Lapor ke Orchestrator
  → Orchestrator notifikasi SU untuk approval + minta jadwal
  → (jika offline) cek venue ke Bu Nova
  → Konfirmasi + detail ke semua pihak
```

### Tipe Meeting & Indikator

| Tipe                 | Indikator Minimum                                | Extra                             |
| -------------------- | ------------------------------------------------ | --------------------------------- |
| Online               | Tanggal + Waktu + Platform (Zoom/GMeet/MS Teams) | Link meeting                      |
| Offline – Venue SU  | Tanggal + Waktu + Nama Venue                     | Konfirmasi ke Nova                |
| Offline – Venue TBD | Tanggal + Waktu                                  | Eskalasi ke Nova untuk cari venue |

---

## 2. Technology Stack

| Komponen         | Teknologi               | Versi                 | Alasan Pemilihan                                              |
| ---------------- | ----------------------- | --------------------- | ------------------------------------------------------------- |
| WhatsApp Gateway | WAHA (Docker)           | Latest                | Self-hosted, 87%+ API coverage, production-grade stability    |
| LLM              | Claude Sonnet           | `claude-sonnet-4-6` | Best reasoning, cost-efficient, multi-agent support           |
| Agent Framework  | OpenClaw                | Latest                | SOUL.md, Lobster workflow, Skills (ClawHub), Memory isolation |
| API Gateway      | Golang GIN              | Terbaru               | High performance, middleware support                          |
| Message Broker   | Redis Streams           | Redis 7+              | Native queue + Pub/Sub untuk event bus antar agent            |
| Primary Database | PostgreSQL              | 16                    | Relational, full audit trail, permanent storage               |
| Session Cache    | Redis                   | Redis 7+              | Fast session state, TTL management, rate limiting             |
| Calendar         | Google Calendar API     | v3                    | RSVP, attendee management, reminder                           |
| Email            | SendGrid                | Latest                | Transactional email, HTML templates                           |
| Monitoring       | Prometheus + Grafana    | Latest                | Metrics, dashboard, alerting                                  |
| Container        | Docker + Docker Compose | Latest                | Local dev dan deployment awal                                 |

---

## 3. Arsitektur Keamanan (Layered)

### Mengapa Bukan Direct Connection

**Direct connection (WAHA ↔ OpenClaw langsung) DILARANG** karena:

- Tidak ada filter input sebelum masuk ke LLM → **prompt injection risk**
- Tidak ada kontrol siapa yang boleh berinteraksi dengan bot
- Tidak ada audit trail – tidak bisa debug jika ada masalah
- State tidak persistent jika OpenClaw restart → percakapan hilang
- Tidak ada approval gate sebelum pesan keluar → bot bisa kirim pesan salah

### Layer Stack (Top-Down)

```
┌─────────────────────────────────────────────────────┐
│       WhatsApp Users (Ext / SU / Nova)              │
└─────────────────────┬───────────────────────────────┘
                      │ WhatsApp Protocol
┌─────────────────────▼───────────────────────────────┐
│              WAHA (Docker :3000)                    │
│       WhatsApp Web bridge – multi-session           │
└─────────────────────┬───────────────────────────────┘
                      │ HTTPS webhook POST
┌─────────────────────▼───────────────────────────────┐
│           API GATEWAY (:4000)                       │
│  ┌─────────────────────────────────────────────┐   │
│  │ ① Auth + Contact Whitelist (PostgreSQL)     │   │
│  │ ② Rate Limiting per-contact (Redis counter) │   │
│  │ ③ Input Sanitization (regex + keyword)      │   │
│  │ ④ Prompt Injection Filter                   │   │
│  │ ⑤ Audit Logging (PostgreSQL messages table) │   │
│  └─────────────────────────────────────────────┘   │
└─────────────────────┬───────────────────────────────┘
                      │ Redis XADD → async
┌─────────────────────▼───────────────────────────────┐
│         MESSAGE QUEUE (Redis Streams)               │
│   inbound_queue | outbound_queue | dead_letter      │
└─────────────────────┬───────────────────────────────┘
                      │ POST /api/agents/{id}/inject
┌─────────────────────▼───────────────────────────────┐
│          OPENCLAW GATEWAY (:5173)                   │
│  ┌──────────────────────────────────────────────┐  │
│  │           Orchestrator Agent                 │  │
│  │    (SOUL.md · Lobster · Approval flow)       │  │
│  ├──────────────────┬───────────────────────────┤  │
│  │  PA Communicator │     Support Agent         │  │
│  │  (External comms)│   (Venue handler)         │  │
│  └──────────────────┴───────────────────────────┘  │
│     All agents → Claude API (api.anthropic.com)     │
└─────────────────────┬───────────────────────────────┘
                      │ Webhook POST (output)
┌─────────────────────▼───────────────────────────────┐
│       API GATEWAY – APPROVAL GATE (Outbound)        │
│   Auto-approve: percakapan biasa, pertanyaan        │
│   Manual review: konfirmasi meeting, RSVP, booking  │
└─────────────────────┬───────────────────────────────┘
                      │ POST /api/sendMessage
┌─────────────────────▼───────────────────────────────┐
│              WAHA → WhatsApp                        │
└─────────────────────────────────────────────────────┘
```

### Security Controls

| Control                 | Implementasi                         | Keterangan                                  |
| ----------------------- | ------------------------------------ | ------------------------------------------- |
| Contact Whitelist       | PostgreSQL `contacts.trust_level`  | Hanya nomor terdaftar yang diproses         |
| Rate Limiting           | Redis counter `ratelimit:{phone}`  | Max 20 msg/menit per kontak                 |
| Input Sanitization      | Regex pattern matching               | Strip karakter berbahaya                    |
| Prompt Injection Filter | Keyword blacklist + LLM pre-check    | Deteksi "ignore previous instructions" dll. |
| Audit Logging           | PostgreSQL `messages` table        | Full trace semua pesan in + out             |
| Approval Gate           | Middleware di API Gateway (outbound) | Review sebelum kirim ke WA                  |
| Agent Isolation         | Separate OpenClaw workspace          | Setiap agent punya memory sendiri           |

---

## 4. Microservice Architecture

### Service Registry Lengkap

| Service          | Stack                        | Port                  | Tanggung Jawab                             | Protocol I/O        |
| ---------------- | ---------------------------- | --------------------- | ------------------------------------------ | ------------------- |
| WAHA             | Docker · Node.js · Baileys | `:3000`             | WhatsApp Web bridge, multi-session         | Webhook + REST      |
| API Gateway      | GIN                          | `:4000`             | Single entry/exit, semua security controls | REST (in + out)     |
| Message Queue    | Redis 7+ Streams             | `:6379`             | Async buffering, retry, dead letter        | Redis Streams       |
| OpenClaw Gateway | OpenClaw                     | `:5173`             | AI agent orchestration, reasoning          | REST API inject     |
| Calendar Service | Node.js + gCal API           | `:4010`             | Google Calendar, O365, RSVP, reminders     | REST → gCal v3     |
| Email Service    | Node.js + SendGrid           | `:4020`             | Undangan meeting, konfirmasi               | REST → Credentials |
| PostgreSQL       | PostgreSQL 16                | `:5432`             | Primary DB – permanent storage            | SQL / TCP           |
| Redis            | Redis 7+                     | `:6379`             | Cache + Queue + Pub/Sub                    | Redis Protocol      |
| Monitoring       | Prometheus + Grafana         | `:9090` / `:3001` | Metrics, dashboard, alerting               | HTTP Scrape         |

### Tiga Pola Komunikasi

**① Synchronous REST** — untuk operasi yang butuh respons segera:

```
WAHA       → API Gateway (webhook push)
API Gateway → OpenClaw  (message inject)
API Gateway → Calendar Service
API Gateway → Email Service
```

**② Asynchronous Queue (Redis Streams)** — untuk decoupling dan buffering:

```
API Gateway  →[XADD]→ stream:inbound  →[XREAD]→ OpenClaw consumer
OpenClaw     →[XADD]→ stream:outbound →[XREAD]→ API Gateway consumer
```

Ini mencegah hilangnya pesan jika OpenClaw sedang lambat atau restart. Claude bisa butuh 2–8 detik; queue memungkinkan API Gateway langsung menerima pesan berikutnya.

**③ Event Bus (Redis Pub/Sub)** — untuk komunikasi antar agent:

```
PA Comm     →[PUBLISH pa_comm:events]→ Orchestrator subscriber
Support     →[PUBLISH support:events]→ Orchestrator subscriber
```

### Message Format di Redis Streams

**Inbound Queue (`stream:inbound`):**

```json
{
  "messageId":   "msg_1750267200_628123456789",
  "convId":      "pa_comm:628123456789",
  "from":        "+628123456789",
  "text":        "Saya bisa Rabu jam 14.00",
  "agentTarget": "pa_communicator",
  "sessionType": "meeting_negotiation",
  "timestamp":   1750267200
}
```

**Outbound Queue (`stream:outbound`):**

```json
{
  "messageId":        "msg_out_1750267210",
  "convId":           "pa_comm:628123456789",
  "to":               "+628123456789",
  "text":             "Terima kasih Pak Andrew...",
  "requiresApproval": false,
  "approvedBy":       "auto",
  "timestamp":        1750267210
}
```

---

## 5. Multi-Agent Design (OpenClaw)

### Tiga Agent dan Tanggung Jawabnya

#### 🟡 Agent 1: Orchestrator Agent

```
Listens to : WA Session SU (Pak Sudianto)
Role       : Pusat koordinasi seluruh sistem

Tanggung jawab:
  - Terima & parse perintah dari SU
  - Ekstrak: nama, nomor, topik, tipe meeting
  - Spawn PA Communicator via Lobster workflow
  - Kelola approval flow (minta konfirmasi SU)
  - Spawn Support Agent jika butuh venue
  - Trigger Calendar Service + Email Service setelah venue confirmed
  - Kirim konfirmasi final ke SU dan External

Isolation: Workspace & memory terpisah dari agent lain
```

#### 🔵 Agent 2: PA Communicator Agent

```
Listens to : WA Session External (nomor bot publik)
Role       : Handle semua komunikasi dengan pihak eksternal

Tanggung jawab:
  - Perkenalan diri kepada pihak eksternal
  - Jika inisiasi eksternal: kumpulkan nama, PT, email, keperluan
  - Negosiasi jadwal meeting
  - Kirim summary ke Orchestrator setelah jadwal disepakati
  - Kirim konfirmasi + detail meeting ke external
  - Handle pertanyaan lanjutan dari external

Isolation: Tidak bisa baca memory Orchestrator. Tidak punya akses ke
           data SU. Prompt injection di sini tidak bisa bocorkan info internal.
```

#### 🟢 Agent 3: Support Agent

```
Listens to : WA Session Nova (Bu Nova)
Role       : Handle komunikasi venue booking dengan Bu Nova

Tanggung jawab:
  - Tanyakan ketersediaan venue dari Bu Nova
  - Kumpulkan: nama venue, kapasitas, harga, alamat, ketersediaan
  - Kirim hasil ke Orchestrator via Redis event

Scope terbatas: Hanya bisa akses venue-related actions.
                Tidak bisa kirim pesan ke kontak lain.
```

### OpenClaw Konfigurasi (`openclaw.json`)

```json
{
  "model": "claude-sonnet-4-6",
  "agents": {
    "defaults": {
      "workspace": "~/.openclaw/workspace"
    },
    "orchestrator": {
      "agentDir": "~/.openclaw/agents/orchestrator"
    },
    "pa_communicator": {
      "agentDir": "~/.openclaw/agents/pa_comm"
    },
    "support": {
      "agentDir": "~/.openclaw/agents/support"
    }
  },
  "channels": {
    "whatsapp": { "enabled": false },
    "waha":     { "enabled": false }
  },
  "tools": {
    "alsoAllow": ["message", "lobster"]
  },
  "hooks": {
    "onAgentReply": "http://api-gateway:4000/webhook/openclaw-output"
  }
}
```

> **⚠️ PENTING:** `"waha": { "enabled": false }` wajib diset.
> OpenClaw **tidak boleh** terhubung langsung ke WAHA.
> Semua messaging dikendalikan oleh API Gateway kita.

### Dua Titik Koneksi I/O

**Inbound — API Gateway → OpenClaw:**

```http
POST http://openclaw:5173/api/agents/{agentId}/inject
Content-Type: application/json

{
  "conversationId": "pa_comm:628123456789",
  "from": "+628123456789",
  "text": "Baik, kami bisa meeting Rabu jam 14.00",
  "metadata": {
    "contactProfile": {
      "name": "Pak Andrew",
      "company": "PT Marteux",
      "email": "andrew@ptmarteux.com",
      "trust_level": "external"
    },
    "conversationHistory": [
      { "role": "assistant", "text": "Selamat pagi Pak Andrew..." },
      { "role": "user",      "text": "Halo, saya Andrew dari PT Marteux" }
    ],
    "currentState": "AWAITING_SCHEDULE",
    "isReturningContact": true
  }
}
```

**Outbound — OpenClaw → API Gateway (webhook):**

```http
POST http://api-gateway:4000/webhook/openclaw-output
Content-Type: application/json

{
  "agentId":        "pa_communicator",
  "conversationId": "pa_comm:628123456789",
  "targetContact":  "+628123456789",
  "response":       "Terima kasih Pak Andrew. Saya konfirmasi...",
  "actions": [
    { "type": "UPDATE_STATE",       "newState": "SCHEDULE_AGREED" },
    { "type": "NOTIFY_ORCHESTRATOR","payload":  { "datetime": "2026-06-25T14:00:00+07:00" }}
  ],
  "newFacts": [
    "Pak Andrew bisa meeting Rabu sore mulai jam 14.00",
    "Pak Andrew email: andrew@ptmarteux.com"
  ],
  "requiresApproval": false
}
```

---

## 6. Alur Data & Komunikasi

### Alur Lengkap Skenario A (SU Minta Arrange Meeting)

```
Step 1  : SU → WA → WAHA :3000
Step 2  : WAHA → POST /webhook → API Gateway :4000
Step 3  : API GW: whitelist check → PostgreSQL contacts
Step 4  : API GW: rate limit check → Redis counter
Step 5  : API GW: input sanitize + prompt injection filter
Step 6  : API GW: audit log → PostgreSQL messages
Step 7  : API GW: XADD → Redis stream:inbound
Step 8  : API GW: POST /api/agents/orchestrator/inject → OpenClaw
Step 9  : Orchestrator: parse perintah SU
          → ekstrak: nama, nomor, topik, meeting_type
          → trigger Lobster workflow: meeting_arrangement_flow
Step 10 : Lobster: spawn PA Communicator
Step 11 : PA Comm: build greeting → webhook output → API GW
Step 12 : API GW: approval check (auto-approve intro)
Step 13 : API GW → POST /api/sendMessage → WAHA
Step 14 : WAHA → External Party WA

          ─── [External Party replies] ───

Step 15 : External WA → WAHA → API GW (webhook)
Step 16 : API GW: security checks → inject ke PA Comm
Step 17 : PA Comm + Claude: proses reply, negosiasi jadwal
Step 18 : Jadwal disepakati → PA Comm emit event ke Orchestrator
Step 19 : Orchestrator: notify SU via WA + approval request

          ─── [SU approve] ───

Step 20 : SU approve → Orchestrator
Step 21 : [Jika offline meeting & belum ada venue]:
          Orchestrator: spawn Support Agent
          → Support → Bu Nova: tanyakan venue
          → Nova reply → Support: kirim hasil ke Orchestrator
Step 22 : Orchestrator → POST /calendar/events → Calendar Service :4010
          → Google Calendar: buat event + tambah attendees + reminder 3 jam
Step 23 : Orchestrator → POST /email/rsvp → Email Service :4020
          → SendGrid: kirim undangan HTML + RSVP link ke External
Step 24 : Orchestrator → notify SU: konfirmasi final
Step 25 : PA Comm → notify External: detail lengkap meeting

          ─── [3 jam sebelum meeting] ───

Step 26 : Calendar Service: trigger reminder
Step 27 : Orchestrator → WA reminder ke SU
Step 28 : PA Comm → WA reminder ke External
```

### Alur Lengkap Skenario B (External Party Inisiasi)

```
Step 1  : External → WA ke bot → WAHA → API GW
Step 2  : API GW: security checks (unknown contact → whitelist check)
          → Jika nomor tidak ada di contacts: create entry sementara dengan
            trust_level = 'external_unknown'
Step 3  : API GW: inject ke PA Comm
Step 4  : PA Comm: pesan pertama → perkenalan diri
          "Selamat pagi, perkenalkan saya asisten Pak Sudianto dari [perusahaan]..."
Step 5  : PA Comm: kumpulkan data (nama, PT, email, keperluan)
          → Multi-turn conversation sampai semua data terkumpul
Step 6  : PA Comm: emit event ke Orchestrator dengan full contact info
Step 7  : Orchestrator → WA ke SU:
          "Ada Pak Andrew dari PT Marteux ingin meeting membahas [topik].
           Apakah Bapak berkenan? Jika iya, kapan jadwal yang cocok?"
Step 8  : SU reply dengan jadwal → Orchestrator
Step 9  : Orchestrator cek: online atau offline?
          → Online: langsung ke Step 11
          → Offline: cek venue
            - Jika SU sudah tentukan venue: langsung ke Step 11
            - Jika belum: spawn Support Agent → ke Bu Nova
Step 10 : Support Agent ↔ Bu Nova: konfirmasi venue
Step 11 : Orchestrator: buat Calendar Event + kirim RSVP email
Step 12 : PA Comm: konfirmasi ke External dengan detail lengkap
```

---

## 7. Memory Architecture

### Empat Lapisan Memori

```
┌─────────────────────────────────────────────────────────┐
│  Layer 1: Context Window (Claude) — VOLATILE            │
│  Dirakit ulang setiap request via /api/inject           │
│  • Last 20 messages (assembled)                         │
│  • Contact profile snapshot                             │
│  • Current conversation state                           │
│  • isReturningContact: true/false                       │
└──────────────────────────┬──────────────────────────────┘
                           │ dibangun dari ↓
┌──────────────────────────▼──────────────────────────────┐
│  Layer 2: Session Cache (Redis) — TTL 48 jam            │
│  Buffer cepat untuk percakapan aktif                    │
│  • conv:{id}:state  → "AWAITING_SCHEDULE"               │
│  • conv:{id}:msgs   → [...last 20 messages]             │
│  • contact:{phone}  → {name, company, email}            │
│  • ratelimit:{phone}→ counter (resets per minute)       │
└──────────────────────────┬──────────────────────────────┘
                           │ fallback jika Redis kosong
┌──────────────────────────▼──────────────────────────────┐
│  Layer 3: Conversation DB (PostgreSQL) — PERMANENT      │
│  SUMBER KEBENARAN UTAMA – tidak boleh hilang            │
│  • conversations (state machine per contact)            │
│  • messages      (semua pesan in + out)                 │
│  • contacts      (whitelist + profil)                   │
│  • contact_facts (fakta diekstrak dari percakapan)      │
└──────────────────────────┬──────────────────────────────┘
                           │ query dan update oleh agent
┌──────────────────────────▼──────────────────────────────┐
│  Layer 4: Entity Memory (OpenClaw) — PERMANENT          │
│  Fakta lintas-sesi, vector embeddings, semantic search  │
│  • "Pak Andrew = Direktur PT Marteux"                   │
│  • "Prefers morning meetings, central Jakarta"          │
│  • "Last meeting: 2026-03-12, topik AI"                 │
│  • "Email: andrew@ptmarteux.com"                        │
└─────────────────────────────────────────────────────────┘
```

### ConversationId Strategy

ConversationId adalah kunci yang menghubungkan semua lapisan memori. Format wajib konsisten:

```javascript
// Format: {agentType}:{normalizedPhone}
function getConversationId(agentType, contactPhone) {
  const normalized = contactPhone.replace(/\D/g, ''); // "+628123" → "628123"
  return `${agentType}:${normalized}`;
}

// Contoh hasil:
// "orchestrator:628XXXXXXXXX"  → SU ↔ Orchestrator
// "pa_comm:628111222333"       → Pak Andrew ↔ PA Comm
// "support:628444555666"       → Bu Nova ↔ Support
```

### Context Assembly (API Gateway)

```javascript
// services/api-gateway/src/contextAssembler.js

async function assembleContext(inboundMsg) {
  const contactId = normalizePhone(inboundMsg.from);
  const convId    = getConversationId('pa_comm', contactId);

  // Step 1: Cek Redis dulu (fast path, ~1ms)
  let recentMsgs = await redis.get(`conv:${convId}:msgs`);
  let state      = await redis.get(`conv:${convId}:state`);

  // Step 2: Fallback ke PostgreSQL jika Redis kosong
  if (!recentMsgs) {
    const rows = await db.query(`
      SELECT role, text, created_at
      FROM   messages
      WHERE  conversation_id = $1
      ORDER  BY created_at DESC
      LIMIT  20
    `, [convId]);

    recentMsgs = rows.rows.reverse();
    const stateRow = await db.query(
      `SELECT state FROM conversations WHERE id = $1`, [convId]
    );
    state = stateRow.rows[0]?.state || 'NEW_CONTACT';

    // Warm up Redis agar request berikutnya cepat
    await redis.setex(`conv:${convId}:msgs`,  172800, JSON.stringify(recentMsgs));
    await redis.setex(`conv:${convId}:state`, 172800, state);
  }

  // Step 3: Ambil profil kontak dari PostgreSQL
  const contactRow = await db.query(
    `SELECT name, company, email, trust_level, notes
     FROM   contacts
     WHERE  phone = $1`, [contactId]
  );
  const contact = contactRow.rows[0] ?? { trust_level: 'external_unknown' };

  // Step 4: Build inject payload untuk OpenClaw
  return {
    conversationId: convId,
    from:           inboundMsg.from,
    text:           inboundMsg.text,
    metadata: {
      contactProfile:      contact,
      conversationHistory: JSON.parse(recentMsgs || '[]'),
      currentState:        state,
      isReturningContact:  JSON.parse(recentMsgs || '[]').length > 0
    }
  };
}
```

### Write-back Memory (Setelah Response)

```javascript
// services/api-gateway/src/memoryWriter.js

async function writeMemory(convId, contactId, agentResponse) {
  const { response, actions, newFacts } = agentResponse;

  // A: Simpan ke PostgreSQL (permanent)
  await db.query(`
    INSERT INTO messages (conversation_id, role, text, agent_id, created_at)
    VALUES ($1, 'assistant', $2, $3, NOW())
  `, [convId, response, agentResponse.agentId]);

  // B: Update Redis (append + sliding window of 20 + refresh TTL)
  const existing = JSON.parse(await redis.get(`conv:${convId}:msgs`) || '[]');
  existing.push({ role: 'assistant', text: response, ts: Date.now() });
  if (existing.length > 20) existing.shift();
  await redis.setex(`conv:${convId}:msgs`, 172800, JSON.stringify(existing));

  // Handle state change dari agent response
  const stateChange = actions?.find(a => a.type === 'UPDATE_STATE');
  if (stateChange) {
    await redis.setex(`conv:${convId}:state`, 172800, stateChange.newState);
    await db.query(
      `UPDATE conversations SET state = $1, updated_at = NOW() WHERE id = $2`,
      [stateChange.newState, convId]
    );
  }

  // C: Simpan entity facts baru ke PostgreSQL & OpenClaw memory
  for (const fact of (newFacts || [])) {
    await db.query(`
      INSERT INTO contact_facts (contact_id, fact, source, extracted_at)
      VALUES ($1, $2, $3, NOW())
      ON CONFLICT (contact_id, fact) DO UPDATE SET extracted_at = NOW()
    `, [contactId, fact, agentResponse.agentId]);
  }
}
```

### Ketahanan Memory per Failure Scenario

| Skenario           | Redis     | PostgreSQL | Dampak ke Sistem                                           |
| ------------------ | --------- | ---------- | ---------------------------------------------------------- |
| OpenClaw restart   | ✅ Aman   | ✅ Aman    | Tidak ada – context di-inject ulang dari Redis/PG         |
| Redis restart      | ❌ Hilang | ✅ Aman    | Request pertama sedikit lambat (load dari PG), lalu normal |
| PostgreSQL restart | ✅ Aman   | ✅ Aman    | Redis buffer masih aktif selama TTL                        |
| Server mati total  | ❌ Hilang | ✅ Aman    | Redis warm up ulang dari PG saat system start              |

**Kesimpulan**: PostgreSQL adalah satu-satunya komponen yang tidak boleh kehilangan data. Pastikan menggunakan persistent volume dan backup terjadwal.

---

## 8. SOUL.md Templates

### Orchestrator Agent (`~/.openclaw/agents/orchestrator/SOUL.md`)

```markdown
# SOUL.md — Orchestrator Agent

Kamu adalah asisten utama Pak Sudianto. Tugasmu adalah mengelola
seluruh alur pengaturan meeting dan berkoordinasi dengan agen lain.

## Kepribadian
- Profesional, ringkas, efisien
- Selalu konfirmasi ke Pak Sudianto sebelum mengambil action penting
- Bahasa Indonesia yang formal namun ramah

## Alur Kerja

### Saat Pak Sudianto Minta Arrange Meeting
1. Ekstrak informasi: nama, nomor HP, topik, tipe meeting (online/offline)
2. Jika belum lengkap, tanyakan kekurangannya
3. Spawn PA Communicator untuk menghubungi pihak eksternal
4. Tunggu laporan dari PA Communicator
5. Sajikan ringkasan ke SU dan minta approval

### Format Approval Request ke SU
Selalu sertakan:
- Nama & perusahaan pihak eksternal
- Jadwal yang disepakati (hari, tanggal, jam)
- Tipe meeting (online/offline)
- Platform atau venue
- Opsi: [✅ Setuju] [❌ Tolak] [📅 Ubah Jadwal]

### Setelah SU Approve
1. Jika offline & belum ada venue: spawn Support Agent ke Bu Nova
2. Setelah venue confirmed: buat Calendar Event
3. Trigger Email Service untuk kirim RSVP
4. Konfirmasi ke SU dan External

## Format Response (JSON)
Selalu sertakan dalam response:
{
  "response": "pesan ke SU",
  "actions": [
    { "type": "UPDATE_STATE", "newState": "AWAITING_EXTERNAL_REPLY" },
    { "type": "SPAWN_AGENT",  "agent": "pa_communicator", "task": "..." }
  ],
  "newFacts": ["fakta baru yang relevan"],
  "requiresApproval": true
}
```

### PA Communicator Agent (`~/.openclaw/agents/pa_comm/SOUL.md`)

```markdown
# SOUL.md — PA Communicator Agent

Kamu adalah asisten profesional yang menghubungi pihak eksternal
atas nama Pak Sudianto dari Hypernet Technologies.

## Kepribadian
- Sangat sopan, profesional, hangat
- Selalu perkenalkan diri di pesan pertama, jika sudah pernah melakukan komunikasi sebelumnya maka tidak perlu memperkenalkan kembali.
- Tidak menyebutkan nama lengkap Pak Sudianto tanpa izin
- Efisien – tidak bertele-tele

## Pesan Perkenalan (Inisiasi dari SU)
"Selamat [pagi/siang/sore], perkenalkan saya [nama Anda], asisten dari
Hypernet Technologies. Kami ingin menanyakan apakah Bapak/Ibu [nama]
berkenan untuk bertemu dalam waktu dekat [sebutkan dengan detail untuk waktunya] untuk membahas mengenai [topik].
Apakah ada waktu yang cocok untuk Bapak/Ibu?"

## Skenario Inisiasi dari Pihak Eksternal
1. Sambut dengan ramah
2. Tanyakan secara bertahap (jangan semua sekaligus):
   - Nama lengkap dan perusahaan (Jika pertama kali dalam melakukan komunikasi)
   - Keperluan / topik yang ingin dibahas
   - Email untuk konfirmasi
   - Waktu dan Tempat (Online/Offline)
3. Konfirmasi pemahaman Anda
4. Informasikan bahwa Anda akan menghubungi kembali setelah mengecek jadwal

## Memory & Context
- Selalu periksa `conversationHistory` sebelum menanyakan info yang sudah ada
- Jika `isReturningContact: true`, sapa secara hangat dan referensikan
  percakapan sebelumnya: "Selamat pagi Pak Andrew, senang mendengar kabar
  dari Bapak. Melanjutkan pembicaraan kita sebelumnya..."
- Gunakan `contactProfile.name` dan `contactProfile.company` jika tersedia

## Format Response (JSON)
{
  "response": "pesan ke kontak",
  "actions": [
    { "type": "UPDATE_STATE", "newState": "SCHEDULE_AGREED" },
    { "type": "NOTIFY_ORCHESTRATOR", "payload": {
        "datetime": "2026-06-25T14:00:00+07:00",
        "contact_email": "andrew@ptmarteux.com"
    }}
  ],
  "newFacts": [
    "Pak Andrew bisa meeting Rabu sore mulai jam 14.00",
    "Pak Andrew email: andrew@ptmarteux.com"
  ],
  "requiresApproval": false
}
```

### Support Agent (`~/.openclaw/agents/support/SOUL.md`)

```markdown
# SOUL.md — Support Agent

Kamu adalah asisten internal yang berkoordinasi dengan Bu Nova
untuk keperluan booking venue meeting.

## Kepribadian
- Ringkas dan to-the-point
- Profesional dalam komunikasi internal

## Informasi yang Dikumpulkan dari Bu Nova
1. Nama venue (hotel/cafe/meeting room)
2. Ketersediaan di tanggal dan jam yang diminta
3. Kapasitas ruangan
4. Estimasi harga
5. Alamat lengkap

## Template Pesan ke Nova
"Halo Nova, Pak Sudianto butuh venue untuk meeting offline pada
[tanggal] jam [waktu], untuk sekitar [jumlah] orang.
Apakah ada rekomendasi venue yang tersedia?"

## Format Response ke Orchestrator (JSON)
{
  "response": "pesan ke Nova",
  "actions": [
    { "type": "UPDATE_STATE", "newState": "VENUE_CONFIRMED" },
    { "type": "NOTIFY_ORCHESTRATOR", "payload": {
        "type": "VENUE_CONFIRMED",
        "venue": {
          "name": "Hotel XYZ – Ruang Anggrek",
          "address": "Jl. Sudirman No.1, Jakarta",
          "capacity": 10,
          "price_estimate": "Rp 1.500.000 / 3 jam",
          "booked_for": "2026-06-25T14:00:00+07:00"
        }
    }}
  ],
  "newFacts": ["Venue konfirmasi: Hotel XYZ untuk meeting Rabu 25 Juni"],
  "requiresApproval": false
}
```

---

## 9. Lobster Workflow (YAML)

```yaml
# ~/.openclaw/workspace/workflows/meeting_arrangement_flow.yaml

name: meeting_arrangement_flow
version: "1.0"
description: >
  Workflow lengkap pengaturan meeting dari perintah SU hingga
  Calendar Event dibuat dan RSVP dikirim.

trigger:
  agent: orchestrator
  event: su_requests_meeting

steps:
  # ─── STEP 1: Hubungi Pihak Eksternal ────────────────────────────
  - id: contact_external
    name: Hubungi pihak eksternal
    agent: pa_communicator
    action: initiate_contact
    input:
      contact_name:  "{{trigger.contact_name}}"
      contact_phone: "{{trigger.contact_phone}}"
      topic:         "{{trigger.topic}}"
      meeting_type:  "{{trigger.meeting_type}}"
    await: schedule_agreed
    timeout: 24h
    on_timeout:
      action: notify_orchestrator
      message: "Pihak eksternal belum merespons setelah 24 jam."

  # ─── STEP 2: Minta Approval SU ──────────────────────────────────
  - id: get_su_approval
    name: Minta approval dari SU
    agent: orchestrator
    action: request_su_approval
    input:
      proposed_datetime: "{{steps.contact_external.result.datetime}}"
      external_name:     "{{trigger.contact_name}}"
      external_company:  "{{trigger.contact_company}}"
      meeting_type:      "{{trigger.meeting_type}}"
    await: su_approved
    timeout: 48h

  # ─── STEP 3: Cek Kebutuhan Venue (jika offline) ─────────────────
  - id: check_venue_needed
    name: Cek apakah perlu venue
    condition: >
      {{trigger.meeting_type == 'offline'
        && !steps.get_su_approval.result.venue}}
    if_true:
      - id: get_venue
        name: Minta venue dari Bu Nova
        agent: support
        action: request_venue_from_nova
        input:
          datetime:   "{{steps.contact_external.result.datetime}}"
          area:       "{{steps.get_su_approval.result.preferred_area}}"
          attendees:  "{{trigger.attendee_count | default: 4}}"
        await: venue_confirmed
        timeout: 4h

  # ─── STEP 4: Buat Calendar Event ────────────────────────────────
  - id: create_calendar
    name: Buat Google Calendar event
    agent: orchestrator
    action: create_calendar_event
    input:
      title: >
        Meeting {{trigger.contact_name}} ({{trigger.contact_company}})
        – {{trigger.topic}}
      datetime:  "{{steps.contact_external.result.datetime}}"
      venue: >
        {{steps.get_venue.result.venue.address
          | steps.get_su_approval.result.venue
          | 'Online'}}
      attendees:
        - "{{trigger.su_email}}"
        - "{{steps.contact_external.result.contact_email}}"
      reminder_minutes: 180

  # ─── STEP 5: Kirim RSVP & Konfirmasi ────────────────────────────
  - id: send_rsvp
    name: Kirim RSVP dan konfirmasi
    agent: pa_communicator
    action: send_rsvp_and_confirmation
    input:
      external_contact:  "{{trigger.contact_phone}}"
      calendar_link:     "{{steps.create_calendar.result.calendar_link}}"
      rsvp_link:         "{{steps.create_calendar.result.rsvp_link}}"
      meeting_details:
        datetime:  "{{steps.contact_external.result.datetime}}"
        venue:     "{{steps.create_calendar.input.venue}}"
        topic:     "{{trigger.topic}}"
```

---

## 10. Conversation State Machine

```
                  NEW_CONTACT
                      │
                      ▼
                INFO_COLLECTED
           (nama, PT, email, keperluan)
                      │
                      ▼
             AWAITING_SCHEDULE
         (menunggu jadwal dari external)
                      │
                      ▼
              SCHEDULE_AGREED
           (jadwal disepakati bersama)
                      │
                      ▼
          AWAITING_SU_APPROVAL
         (menunggu konfirmasi SU)
                      │
            ┌─────────┴─────────┐
            │                   │
        (offline)           (online)
            │                   │
            ▼                   │
      AWAITING_VENUE            │
   (menunggu info Nova)         │
            │                   │
            ▼                   │
      VENUE_CONFIRMED ──────────┘
                      │
                      ▼
          CALENDAR_CREATED
       (event dibuat, RSVP dikirim)
                      │
                      ▼
                 COMPLETED

  [Dari state manapun] → CANCELLED
```

---

## 11. Docker Compose

```yaml
version: "3.9"

services:

  # ─── Transport ─────────────────────────────────────────────
  waha:
    image: devlikeapro/waha:noweb
    container_name: pa_ai_waha
    ports:
      - "3000:3000"
    volumes:
      - ./.waha:/app/.sessions
    environment:
      WAHA_API_KEY:     ${WAHA_API_KEY}
      WHATSAPP_HOOK_URL: http://api-gateway:4000/webhook/waha
      WAHA_LOG_LEVEL:   info
    restart: unless-stopped

  # ─── API Gateway ───────────────────────────────────────────
  api-gateway:
    build: ./services/api-gateway
    container_name: pa_ai_gateway
    ports:
      - "4000:4000"
    depends_on:
      - redis
      - postgres
      - openclaw
    environment:
      REDIS_URL:          redis://redis:6379
      DATABASE_URL:       postgresql://pa_ai:${DB_PASS}@postgres:5432/pa_ai
      WAHA_URL:           http://waha:3000
      WAHA_API_KEY:       ${WAHA_API_KEY}
      OPENCLAW_URL:       http://openclaw:5173
      WHITELIST_PHONES:   "${SU_PHONE},${NOVA_PHONE}"
      OPENCLAW_WEBHOOK:   http://api-gateway:4000/webhook/openclaw-output
      MAX_MSG_PER_MINUTE: "20"
    restart: unless-stopped

  # ─── OpenClaw ──────────────────────────────────────────────
  openclaw:
    image: ghcr.io/openclaw/openclaw:latest
    container_name: pa_ai_openclaw
    ports:
      - "5173:5173"
    volumes:
      - ~/.openclaw:/root/.openclaw
    depends_on:
      - redis
    environment:
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
    restart: unless-stopped

  # ─── Integration Services ──────────────────────────────────
  calendar-service:
    build: ./services/calendar
    container_name: pa_ai_calendar
    ports:
      - "4010:4010"
    environment:
      GOOGLE_CLIENT_ID:     ${GOOGLE_CLIENT_ID}
      GOOGLE_CLIENT_SECRET: ${GOOGLE_CLIENT_SECRET}
      GOOGLE_REFRESH_TOKEN: ${GOOGLE_REFRESH_TOKEN}
    restart: unless-stopped

  email-service:
    build: ./services/email
    container_name: pa_ai_email
    ports:
      - "4020:4020"
    environment:
      SENDGRID_API_KEY: ${SENDGRID_API_KEY}
      FROM_EMAIL:       ${FROM_EMAIL}
      FROM_NAME:        "PA Asisten"
    restart: unless-stopped

  # ─── Storage ───────────────────────────────────────────────
  redis:
    image: redis:7-alpine
    container_name: pa_ai_redis
    ports:
      - "6379:6379"
    volumes:
      - ./data/redis:/data
    command: redis-server --appendonly yes --maxmemory 512mb --maxmemory-policy allkeys-lru
    restart: unless-stopped

  postgres:
    image: postgres:16-alpine
    container_name: pa_ai_postgres
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB:       pa_ai
      POSTGRES_USER:     pa_ai
      POSTGRES_PASSWORD: ${DB_PASS}
    volumes:
      - ./data/postgres:/var/lib/postgresql/data
      - ./sql/schema.sql:/docker-entrypoint-initdb.d/01_schema.sql
    restart: unless-stopped

  # ─── Monitoring ────────────────────────────────────────────
  prometheus:
    image: prom/prometheus:latest
    container_name: pa_ai_prometheus
    ports:
      - "9090:9090"
    volumes:
      - ./monitoring/prometheus.yml:/etc/prometheus/prometheus.yml
    restart: unless-stopped

  grafana:
    image: grafana/grafana:latest
    container_name: pa_ai_grafana
    ports:
      - "3001:3000"
    depends_on:
      - prometheus
    volumes:
      - ./data/grafana:/var/lib/grafana
    restart: unless-stopped
```

### Environment Variables (`.env`)

```bash
# ─── WAHA ───────────────────────────────────────
WAHA_API_KEY=your_waha_api_key_here

# ─── Database ───────────────────────────────────
DB_PASS=your_strong_db_password

# ─── Anthropic / Claude ─────────────────────────
ANTHROPIC_API_KEY=sk-ant-xxxxxxxxxxxx

# ─── Google Calendar ────────────────────────────
GOOGLE_CLIENT_ID=xxxx.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=GOCSPX-xxxxxxxxxx
GOOGLE_REFRESH_TOKEN=1//xxxxxxxxxxxxxxxx

# ─── Email ──────────────────────────────────────
SENDGRID_API_KEY=SG.xxxxxxxxxxxxxxxxxx
FROM_EMAIL=pa@yourdomain.com

# ─── Kontak Trusted ─────────────────────────────
SU_PHONE=+628XXXXXXXXX
NOVA_PHONE=+628YYYYYYYYY


# ─── O365 ─────────────────────────────
MS_GRAPH_TENANT_ID=761xxxxxxxxxxxxxxxx
MS_GRAPH_CLIENT_ID=63cxxxxxxxxxxxxxxxx
MS_GRAPH_CLIENT_SECRET=T1xxxxxxxxxxxxxxxxxxx
MS_GRAPH_USER_UPN=pa@hypernet.co.id
```

---

## 12. API Contracts

### API Gateway — Endpoints

```
# Inbound (dari WAHA)
POST   /webhook/waha                ← Webhook dari WAHA (inbound message)

# Outbound (dari OpenClaw)
POST   /webhook/openclaw-output     ← Output dari agent setelah proses

# Management
GET    /health                      ← Health check semua service
GET    /conversations/:id           ← Detail conversation + history
GET    /contacts                    ← List whitelisted contacts
POST   /contacts                    ← Tambah kontak ke whitelist
PATCH  /contacts/:phone             ← Update trust level / profil
GET    /meetings                    ← List meeting requests
GET    /metrics                     ← Prometheus metrics endpoint
```

### OpenClaw — Inject Endpoint

```
POST /api/agents/{agentId}/inject
  agentId: orchestrator | pa_communicator | support

Body: {
  conversationId: string,          // format: {agent}:{phone}
  from:           string,          // nomor pengirim
  text:           string,          // isi pesan
  metadata: {
    contactProfile:      object,   // dari PostgreSQL contacts
    conversationHistory: array,    // last 20 messages
    currentState:        string,   // dari state machine
    isReturningContact:  boolean   // true jika pernah chat sebelumnya
  }
}
```

### Calendar Service

```
POST   /calendar/events             ← Buat Calendar event + invite attendees
GET    /calendar/availability       ← Cek ketersediaan jadwal SU
DELETE /calendar/events/:id         ← Cancel/hapus event
POST   /calendar/reminders/:id      ← Set/update reminder
```

### Email Service

```
POST   /email/rsvp                  ← Kirim RSVP invitation ke external
POST   /email/confirmation          ← Kirim konfirmasi meeting
POST   /email/reminder              ← Kirim reminder email
POST   /email/cancellation          ← Kirim notifikasi pembatalan
```

---

## 13. Langkah-Langkah Development

> Ikuti urutan ini secara berurutan. Setiap fase memiliki **verifikasi** yang harus
> lulus sebelum lanjut ke fase berikutnya. Jangan skip fase demi menjaga kemudahan debugging.

---

### FASE 1 — Environment Setup

**Estimasi: Hari 1–2**
**Target: Semua service dasar berjalan, project structure siap.**

#### Step 1.1 — Buat Struktur Direktori

Buat direktori root project dan seluruh sub-direktori yang diperlukan:

- `services/api-gateway/src/` dengan sub-folder `routes/` dan `middleware/`
- `services/calendar/src/` dan `services/email/src/`
- `sql/` untuk schema database
- `monitoring/` untuk konfigurasi Prometheus
- `data/postgres/`, `data/redis/`, `data/grafana/` sebagai Docker volume
- `.waha/` untuk menyimpan session WhatsApp
- `.openclaw/agents/orchestrator/`, `.openclaw/agents/pa_comm/`, `.openclaw/agents/support/`
- `.openclaw/workspace/workflows/` untuk Lobster workflow YAML

#### Step 1.2 — Buat File `.env`

Buat file `.env` di root project berisi semua environment variables. Gunakan template dari **Section 11** (Environment Variables). Pastikan `WAHA_API_KEY`, `DB_PASS`, `ANTHROPIC_API_KEY`, `SU_PHONE`, dan `NOVA_PHONE` diisi sebelum menjalankan service apapun.

Jangan pernah commit file `.env` ke repository. Buat juga file `.env.example` dengan nilai kosong sebagai template untuk anggota tim lain.

#### Step 1.3 — Buat `docker-compose.yml` Minimal

Untuk fase awal, hanya jalankan tiga service di Docker: **Redis**, **PostgreSQL**, dan **WAHA**. API Gateway dan OpenClaw masih dijalankan secara lokal (bukan di dalam Docker) agar lebih mudah di-debug.

Konfigurasi WAHA agar `WHATSAPP_HOOK_URL` mengarah ke `http://host.docker.internal:4000/webhook/waha` supaya WAHA di dalam Docker dapat memanggil API Gateway yang berjalan di host. Aktifkan juga `extra_hosts: host.docker.internal:host-gateway` di konfigurasi WAHA container.

#### Step 1.4 — Jalankan Service Dasar

Jalankan ketiga service (Redis, PostgreSQL, WAHA) menggunakan Docker Compose dan pastikan semua container berstatus `Up`.

#### ✅ Verifikasi Fase 1

- [ ] Redis merespons perintah `ping` dengan `PONG`
- [ ] PostgreSQL dapat diakses dan menampilkan informasi versi
- [ ] WAHA health check endpoint (`/api/health`) mengembalikan status `ok`

---

### FASE 2 — WAHA: Connect WhatsApp

**Estimasi: Hari 2–3**
**Target: Bot WhatsApp terhubung dan bisa kirim/terima pesan.**

#### Step 2.1 — Buat Session WAHA

Buat session baru di WAHA bernama `pa_bot` melalui endpoint API WAHA (`POST /api/sessions`). Konfigurasi session agar otomatis mengirimkan event bertipe `message` ke webhook URL API Gateway (`http://host.docker.internal:4000/webhook/waha`).

#### Step 2.2 — Scan QR Code

Akses QR code melalui WAHA dashboard di browser (`http://localhost:3000/dashboard`) atau melalui endpoint API (`/api/pa_bot/auth/qr`). Scan QR code menggunakan WhatsApp di HP yang akan digunakan sebagai nomor bot.

#### Step 2.3 — Verifikasi Koneksi

Cek status session melalui endpoint API WAHA (`GET /api/sessions/pa_bot`). Session harus menunjukkan status `WORKING`. Lakukan test kirim pesan ke nomor HP sendiri menggunakan endpoint `POST /api/sendText` untuk memastikan pengiriman berfungsi.

#### ✅ Verifikasi Fase 2

- [ ] Session WAHA berstatus `WORKING`
- [ ] Pesan test berhasil diterima di HP tujuan
- [ ] QR code tidak perlu di-scan ulang setelah restart Docker

---

### FASE 3 — API Gateway: Webhook Receiver

**Estimasi: Hari 3–6**
**Target: API Gateway menerima pesan dari WAHA dan bisa mengirim balik.**

#### Step 3.1 — Inisialisasi Project API Gateway

Inisialisasi project Go dengan framework GIN. Struktur utama yang diperlukan:

- Entry point di `main.go`
- Route handler di `routes/webhook.go`
- Middleware di `middleware/`
- Konfigurasi dibaca dari environment variables (`.env`)

#### Step 3.2 — Buat Webhook Receiver

Buat endpoint `POST /webhook/waha` yang:

1. Menerima payload JSON dari WAHA
2. Menyaring hanya event bertipe `message` — abaikan event lain seperti `ack`, `presence`, `typing`, dll.
3. Mengekstrak: nomor pengirim (`from`), isi pesan (`body`), nama session, dan timestamp
4. Mencatat (log) pesan yang masuk untuk keperluan debugging
5. Mengembalikan respons `200 OK` agar WAHA tidak melakukan retry pengiriman webhook

Buat juga endpoint `POST /webhook/openclaw-output` untuk menerima hasil dari OpenClaw (diimplementasikan penuh di Fase 6).

#### Step 3.3 — Buat Entry Point

Entry point API Gateway harus:

- Menjalankan HTTP server di port `4000`
- Mendaftarkan semua route di bawah prefix `/webhook`
- Menyediakan endpoint `GET /health` untuk health check dan monitoring

#### Step 3.4 — Jalankan API Gateway

Jalankan API Gateway secara lokal dan pastikan berjalan di port 4000. Cek endpoint `/health` untuk memastikan server aktif dan siap menerima request.

#### Step 3.5 — Buat Fungsi Send ke WAHA

Buat fungsi helper yang bertanggung jawab mengirim pesan keluar ke WhatsApp melalui WAHA. Fungsi ini menerima nomor tujuan dan teks pesan, lalu:

1. Memformat nomor ke format WAHA: `628xxx@c.us` (hilangkan semua karakter non-digit, tambahkan suffix `@c.us`)
2. Memanggil endpoint `POST /api/sendText` milik WAHA dengan header `X-Api-Key` yang sesuai
3. Menyertakan nama session (`pa_bot`) di body request

#### ✅ Verifikasi Fase 3

Kirim pesan dari HP ke nomor bot. Di log terminal API Gateway harus muncul isi pesan, nomor pengirim, dan nama session yang diterima dari WAHA.

---

### FASE 4 — Security Layer

**Estimasi: Hari 6–9**
**Target: Hanya nomor yang tidak diblokir yang bisa masuk, rate limit aktif, input tersanitasi.**

#### Step 4.1 — Buat Tabel Dasar di PostgreSQL

Buat tabel `contacts` dengan kolom: `id`, `phone` (unique), `name`, `company`, `email`, `trust_level` (default: `external`), dan `created_at`.

Setelah tabel terbuat, masukkan dua record awal:

- Nomor SU dengan `trust_level = 'su'`
- Nomor Nova dengan `trust_level = 'semi_trusted'`

#### Step 4.2 — Buat Auth Middleware

Buat middleware yang berjalan sebelum handler webhook:

1. Ekstrak nomor pengirim dari payload WAHA dan normalisasi ke format internasional
2. Query tabel `contacts` di PostgreSQL berdasarkan nomor tersebut
3. Jika nomor tidak ditemukan: log `[BLOCKED]` dan kembalikan `200 OK` dengan status `blocked` — jangan kembalikan error agar WAHA tidak melakukan retry
4. Jika ditemukan: simpan data kontak ke request context supaya tersedia di middleware dan handler berikutnya tanpa query ulang

#### Step 4.3 — Buat Rate Limiter Middleware

Buat middleware yang membatasi frekuensi pesan per kontak menggunakan Redis:

1. Buat Redis key `ratelimit:{phone}` sebagai counter per nomor
2. Increment counter setiap pesan masuk; set TTL 60 detik jika key baru dibuat
3. Jika counter melebihi batas (20 pesan/menit): log `[RATE LIMITED]` dan blok pesan
4. Jika masih dalam batas: lanjutkan ke middleware berikutnya

#### Step 4.4 — Buat Input Sanitizer Middleware

Buat middleware yang memeriksa konten pesan:

1. Deteksi pola prompt injection menggunakan regex atau string matching. Pola yang perlu dideteksi mencakup: `ignore previous instructions`, `disregard your previous`, `you are now`, `pretend to be`, `system prompt`, `[INST]`, `<|system|>`, dan variasinya
2. Jika terdeteksi: log `[INJECTION BLOCKED]` dan blok pesan dengan status `blocked_injection`
3. Batasi panjang pesan maksimum 2000 karakter — jika melebihi, potong tanpa error
4. Lanjutkan jika pesan aman

#### Step 4.5 — Terapkan Middleware ke Route

Terapkan ketiga middleware secara berurutan ke semua route `/webhook`:

> **auth → rate limiter → sanitizer → handler**

Urutan ini kritis: auth harus dijalankan paling awal agar data kontak tersedia untuk middleware berikutnya.

#### ✅ Verifikasi Fase 4

- [ ] Test 1 — Nomor yang diblokir: Kirim WA dari nomor yang TIDAK ada di tabel contacts. Log harus menampilkan `[BLOCKED]`
- [ ] Test 2 — Nomor terdaftar dan tidak terblokir: Kirim WA dari nomor SU. Pesan harus lolos dan log menampilkan `[INBOUND]`
- [ ] Test 3 — Rate limit: Kirim lebih dari 20 pesan dalam 1 menit dari nomor yang sama. Log harus menampilkan `[RATE LIMITED]`
- [ ] Test 4 — Prompt injection: Kirim teks `ignore previous instructions`. Log harus menampilkan `[INJECTION BLOCKED]`

---

### FASE 5 — OpenClaw Setup

**Estimasi: Hari 9–11**
**Target: OpenClaw berjalan dengan satu agent (PA Communicator) dan bisa menerima inject.**

#### Step 5.1 — Install OpenClaw

Install OpenClaw menggunakan CLI (`npm install -g @openclaw/cli`) atau pull Docker image (`ghcr.io/openclaw/openclaw:latest`). Ikuti instruksi resmi di dokumentasi OpenClaw untuk langkah instalasi yang sesuai environment.

#### Step 5.2 — Buat `openclaw.json`

Buat file konfigurasi di `~/.openclaw/openclaw.json`. Pengaturan wajib:

- `model`: `claude-sonnet-4-6`
- `agents.pa_communicator.agentDir`: path ke direktori agent PA Communicator
- `channels.whatsapp.enabled` dan `channels.waha.enabled`: keduanya **wajib** `false` — OpenClaw tidak boleh terhubung langsung ke WAHA
- `hooks.onAgentReply`: URL webhook API Gateway untuk menerima output agent (`http://localhost:4000/webhook/openclaw-output`)

Lihat konfigurasi lengkap di **Section 5** dokumen ini.

#### Step 5.3 — Buat SOUL.md untuk PA Communicator (Versi Minimal)

Buat file `SOUL.md` di direktori agent PA Communicator. Versi minimal untuk fase ini cukup berisi:

- Deskripsi peran: asisten profesional Hypernet Technologies yang membalas pesan secara sopan dalam Bahasa Indonesia
- Format response JSON yang harus dikembalikan: field `response`, `actions` (array kosong untuk sementara), `newFacts` (array kosong), dan `requiresApproval` (false)

SOUL.md lengkap dengan instruksi negosiasi meeting akan diganti di Fase 8.

#### Step 5.4 — Set API Key Anthropic

Pastikan `ANTHROPIC_API_KEY` tersedia sebagai environment variable sebelum menjalankan OpenClaw. Set langsung di terminal sesi aktif atau tambahkan ke profil shell (`~/.bashrc` atau `~/.zshrc`) agar persisten.

#### Step 5.5 — Jalankan OpenClaw

Jalankan dengan perintah `openclaw start`. Pastikan berjalan di port `5173` dan tidak ada error koneksi ke Anthropic API.

#### Step 5.6 — Test Inject Manual

Lakukan test inject menggunakan HTTP client (curl, Postman, atau sejenisnya) langsung ke endpoint OpenClaw:

- URL: `POST http://localhost:5173/api/agents/pa_communicator/inject`
- Body: `conversationId`, `from`, `text`, dan `metadata` berisi `contactProfile` (minimal dengan `name` dan `trust_level`), `conversationHistory` kosong, `currentState: NEW_CONTACT`, dan `isReturningContact: false`

#### ✅ Verifikasi Fase 5

- [ ] OpenClaw berjalan di port 5173 tanpa error
- [ ] Inject endpoint merespons tanpa error 404 atau 500
- [ ] Webhook `onAgentReply` terpanggil dan log muncul di terminal API Gateway
- [ ] Isi response dari Claude masuk akal dan sesuai instruksi SOUL.md minimal

---

### FASE 6 — Full Loop: WA → API GW → OpenClaw → WA

**Estimasi: Hari 11–14**
**Target: Bot benar-benar membalas pesan WhatsApp. Ini milestone pertama yang terlihat.**

#### Step 6.1 — Tambahkan Inject Call di Webhook Handler

Setelah semua security middleware lulus, tambahkan logika di handler `POST /webhook/waha`:

1. Tentukan agent target: untuk sementara selalu gunakan `pa_communicator` (routing multi-agent diimplementasikan di Fase 8)
2. Bentuk `conversationId` dari tipe agent dan nomor HP yang dinormalisasi: format `pa_comm:{normalizedPhone}`
3. Panggil endpoint inject OpenClaw dengan payload: `conversationId`, `from`, `text`, dan `metadata` yang sementara berisi data kontak dari request context, `conversationHistory` kosong, dan `currentState: NEW_CONTACT`

#### Step 6.2 — Handle Output OpenClaw dan Kirim ke WAHA

Lengkapi handler `POST /webhook/openclaw-output`:

1. Ekstrak field `targetContact` (nomor tujuan) dan `response` (teks pesan) dari payload OpenClaw
2. Validasi keduanya tidak kosong; kembalikan error 400 jika ada yang kurang
3. Panggil fungsi helper `sendToWhatsApp` untuk mengirim response ke nomor WhatsApp tujuan
4. Untuk Fase 6, semua output langsung dikirim tanpa review — Approval Gate diimplementasikan di Fase 8

#### Step 6.3 — Tambahkan Environment Variable

Tambahkan `OPENCLAW_URL=http://localhost:5173` ke file `.env` dan pastikan API Gateway membaca nilainya saat startup.

#### ✅ Verifikasi Fase 6 — MILESTONE PERTAMA

1. Kirim pesan dari HP ke nomor bot
2. API Gateway: log menunjukkan pesan masuk dan berhasil di-inject ke OpenClaw
3. OpenClaw: Claude memproses pesan dan menghasilkan response
4. API Gateway: log menunjukkan output dari OpenClaw dan pengiriman ke WA
5. HP menerima balasan dari bot ✅

**Bot sudah bisa membalas pesan WhatsApp secara end-to-end!**

---

### FASE 7 — Memory Layer

**Estimasi: Hari 14–18**
**Target: Bot ingat seluruh history percakapan, bahkan setelah restart.**

#### Step 7.1 — Buat Tabel Tambahan di PostgreSQL

Buat dua tabel tambahan:

**Tabel `conversations`** — menyimpan state setiap percakapan:

- `id` (primary key, format `{agent}:{phone}`)
- `contact_id` (foreign key ke tabel `contacts`)
- `agent_id` — agent yang menangani percakapan ini
- `state` (default: `NEW_CONTACT`)
- `created_at` dan `updated_at`

**Tabel `messages`** — menyimpan setiap pesan secara individual:

- `id` (auto-increment)
- `conversation_id` (foreign key ke `conversations`)
- `role` — nilai `user` atau `assistant`
- `text` — isi pesan
- `agent_id` dan `created_at`

Tambahkan index pada kolom `conversation_id` dan `created_at` (descending) di tabel `messages` untuk mempercepat query pengambilan history.

#### Step 7.2 — Install Driver Database

Tambahkan dependency driver PostgreSQL dan Redis ke project API Gateway.

#### Step 7.3 — Buat Modul `contextAssembler`

Buat modul yang merakit context lengkap sebelum inject ke OpenClaw. Urutan operasinya:

1. **Normalisasi nomor HP** dari format internasional ke format tanpa simbol
2. **Bentuk conversationId** dengan format `{agentType}:{normalizedPhone}`
3. **Cek Redis** untuk recent messages dan state percakapan — ini adalah fast path (~1ms)
4. **Fallback ke PostgreSQL** jika Redis kosong: query 20 pesan terakhir dari tabel `messages` (urutan chronological), ambil state dari tabel `conversations`. Setelah berhasil, simpan kembali ke Redis dengan TTL 48 jam (warm up cache untuk request berikutnya)
5. **Ambil profil kontak** dari tabel `contacts` berdasarkan nomor HP
6. **Kembalikan struct payload** berisi: `conversationId`, `contactProfile`, `conversationHistory`, `currentState`, dan `isReturningContact` (true jika history tidak kosong)

#### Step 7.4 — Buat Modul `memoryWriter`

Buat modul yang menyimpan hasil percakapan ke semua lapisan memori setelah menerima response dari OpenClaw:

1. **Pastikan conversation ada** di PostgreSQL — insert record jika belum ada, gunakan `ON CONFLICT DO NOTHING`
2. **Simpan pesan user** ke tabel `messages` dengan role `user`
3. **Simpan response agent** ke tabel `messages` dengan role `assistant`
4. **Update Redis cache**: append kedua pesan ke list recent messages, pertahankan sliding window maksimal 40 pesan, refresh TTL 48 jam
5. **Handle state change**: jika response agent mengandung action `UPDATE_STATE`, update nilai state di Redis dan di tabel `conversations`
6. **Simpan newFacts**: jika response agent mengandung array `newFacts`, simpan setiap fakta ke tabel `contact_facts` dengan `ON CONFLICT DO UPDATE` untuk menghindari duplikasi

#### Step 7.5 — Integrasikan di Webhook Handler

Update kedua handler webhook:

- **`POST /webhook/waha`**: panggil `assembleContext` sebelum inject ke OpenClaw, sertakan hasilnya sebagai field `metadata` di payload inject
- **`POST /webhook/openclaw-output`**: panggil `writeMemory` setelah menerima response dari OpenClaw, kemudian kirim pesan ke WhatsApp

#### ✅ Verifikasi Fase 7

- [ ] Test 1 — Multi-turn: Kirim 3 pesan berturut-turut. Pesan ke-3 ("Apakah nama saya sudah tercatat?") harus dijawab dengan menyebut nama dan perusahaan yang disebutkan di pesan 1 dan 2
- [ ] Test 2 — Tahan restart: Restart OpenClaw, kirim pesan. Bot harus tetap ingat konteks dari percakapan sebelumnya karena context di-load ulang dari PostgreSQL
- [ ] Test 3 — Cek database: Verifikasi tabel `messages` memiliki record pesan masuk dan keluar yang tersimpan dengan benar

---

### FASE 8 — Multi-Agent + Routing

**Estimasi: Hari 18–25**
**Target: Tiga agent berjalan, routing benar, Lobster workflow aktif.**

#### Step 8.1 — Tambahkan Orchestrator dan Support Agent ke `openclaw.json`

Update `openclaw.json` untuk mendaftarkan dua agent tambahan:

- `orchestrator` dengan `agentDir` mengarah ke `~/.openclaw/agents/orchestrator`
- `support` dengan `agentDir` mengarah ke `~/.openclaw/agents/support`

Tambahkan juga `lobster` ke daftar `tools.alsoAllow` agar Orchestrator dapat menjalankan Lobster workflow.

#### Step 8.2 — Tulis SOUL.md Lengkap untuk Semua Agent

Salin SOUL.md lengkap dari **Section 8** dokumen ini ke masing-masing direktori agent. Ganti SOUL.md minimal PA Communicator dari Fase 5 dengan versi lengkap yang berisi instruksi negosiasi jadwal dan format response JSON.

#### Step 8.3 — Implementasikan Routing di API Gateway

Update handler webhook untuk menentukan agent berdasarkan `trust_level` kontak yang sudah tersimpan di request context (dari auth middleware — tidak perlu query database tambahan):

| trust_level               | Agent Target        |
| ------------------------- | ------------------- |
| `su`                    | `orchestrator`    |
| `semi_trusted`          | `support`         |
| `external` atau lainnya | `pa_communicator` |

#### Step 8.4 — Buat Lobster Workflow YAML (Deprecated/Dijalankan Oleh Go)

Buat file `~/.openclaw/workspace/workflows/meeting_arrangement_flow.yaml` dengan isi lengkap dari **Section 9** dokumen ini. Workflow ini mendefinisikan alur meeting end-to-end: dari kontak eksternal hingga Calendar Event dibuat dan RSVP dikirim.

#### Step 8.5 — Implementasikan Approval Gate

Update handler `POST /webhook/openclaw-output` dengan logika percabangan approval:

**Jika `requiresApproval: true`:**

1. Simpan pesan ke tabel `approval_pending` di database
2. Kirim notifikasi ke nomor SU berisi: nomor tujuan, isi pesan yang menunggu persetujuan, dan instruksi cara approve atau reject
3. Kembalikan status `pending_approval` — jangan kirim pesan ke pihak eksternal sebelum SU menyetujui

**Jika `requiresApproval: false`:**

1. Langsung panggil `writeMemory`
2. Kirim pesan ke WhatsApp via `sendToWhatsApp`

#### ✅ Verifikasi Fase 8

- [ ] Test routing: pesan dari nomor SU → masuk ke Orchestrator; dari nomor External → masuk ke PA Comm; dari nomor Nova → masuk ke Support
- [ ] Test Skenario A parsial: SU kirim perintah meeting → PA Comm mengirim WA ke nomor eksternal → External balas → PA Comm negosiasi → Orchestrator notif SU
- [ ] Test Approval Gate: pesan dengan `requiresApproval: true` → SU mendapat notifikasi WA untuk review sebelum pesan dikirim

---

### FASE 9 — Calendar & Email & Teams Integration

**Target: Meeting masuk ke O365, RSVP email terkirim, dan dibuatkan link meeting (Teams) jika permintaan adalah online dari Super User.**

#### Step 9.1 — Setup O365 using API Key (App Registrations)

1. Sudah dibuatkan pada *.env* untuk API key, client id, client secret dan tenant id
2. Nantinya seluruh kalendar akan menggunakan

#### Step 9.3 — Buat Link Teams

Buat service terpisah di `service/teams` yang berjalan di port `4020`. Service ini bertugas untuk membuat link microsoft teams yang akan dilampirkan untuk meeting nantinya jika type meeting adalah Online.


#### Step 9.2 — Buat Calendar Service

Buat service terpisah di `services/calendar/` yang berjalan di port `4010`. Dua endpoint utama:

**`POST /calendar/events`** — Membuat O365 Calendar event:

- Input: `title`, `datetime`, `venue`, daftar `attendees` (array email), `reminderMinutes` (default 180 menit = 3 jam)
- Buat event di primary calendar SU dengan attendees, reminder email, dan reminder popup 30 menit
- Set `sendUpdates: all` agar O365 otomatis mengirim email invite ke semua attendees
- Output: `eventId`, `calendarLink`, dan `hangoutLink`

**`GET /calendar/availability`** — Mengecek ketersediaan jadwal SU:

- Input: `date` sebagai query parameter
- Output: semua event di kalender SU pada tanggal tersebut

#### Step 9.3 — Setup Email Service

1. Buatkan format email Undangan Meeting, Balasan Email, Konfirmasi Undangan (Approve, Rejected), dan Follow Up
2. Gunakan signature yang telah tersedia pada `services\email\src\signature.html`
3. Lampirkan RSVP jika itu adalah undangan meeting dari SU.

Buat service terpisah di `services/email/` yang berjalan di port `4020`. Endpoint utama:

**`POST /email/rsvp`** — Mengirim undangan meeting:

- Input: `to`, `toName`, `fromName`, `title`, `datetime`, `venue`, `calendarLink`
- Kirim email HTML dengan detail meeting yang terformat: agenda, waktu dalam timezone Asia/Jakarta, lokasi, dan tombol/link "Tambahkan ke O365/Google Calendar" dan lampirkan file RSVP.

#### ✅ Verifikasi Fase 9

- [ ] Test Calendar: buat event via endpoint `POST /calendar/events` → event muncul di O365 → email invite diterima oleh attendees
- [ ] Test Teams: buat link teams via endpoint yang telah dibuatkan
- [ ] Test Email: kirim request ke `POST /email/rsvp` → email HTML undangan diterima dengan format yang benar dan link Calendar berfungsi (Test email melalui `MS_GRAPH_USER_UPN`  yang ada di `.env` dan kirimkan ke yeremia.yosefan@hypernet.co.id)

---

### FASE 10 — End-to-End Test

**Estimasi: Hari 30–33**
**Target: Kedua skenario berjalan penuh tanpa error.**

#### Step 10.1 — Skenario A: SU Inisiasi Meeting

**Nomor HP:** Nomor SU (terdaftar di contacts dengan trust_level `su`)

**Pesan ke bot:** "Tolong hubungi Pak Andrew dari PT Marteux, nomor 628111222333, untuk meeting offline membahas konsolidasi AI"

**Expected flow:**

- ✅ API GW: auth passed (nomor SU dikenal)
- ✅ Route ke Orchestrator
- ✅ Orchestrator: parse perintah, spawn PA Comm via Lobster
- ✅ PA Comm: WA ke Pak Andrew
- ✅ Pak Andrew reply → PA Comm negosiasi jadwal
- ✅ PA Comm: jadwal disepakati → notif Orchestrator
- ✅ Orchestrator: notif SU + minta approval
- ✅ SU approve → Orchestrator
- ✅ Support Agent → tanya Bu Nova venue
- ✅ Nova reply dengan info venue → Support → Orchestrator
- ✅ Calendar event dibuat di Google Calendar
- ✅ RSVP email terkirim ke Pak Andrew
- ✅ Konfirmasi WA ke SU dan Pak Andrew
- ✅ Reminder dijadwalkan 3 jam sebelum meeting

#### Step 10.2 — Skenario B: External Inisiasi

**Nomor HP:** Nomor yang BUKAN SU dan BUKAN Nova

**Pesan ke bot:** "Halo, saya dari PT ABC ingin bertemu"

**Expected flow:**

- ✅ API GW: nomor belum dikenal → masuk sebagai external
- ✅ Route ke PA Communicator
- ✅ PA Comm: perkenalan diri
- ✅ PA Comm: kumpulkan nama, PT, email, keperluan secara bertahap
- ✅ PA Comm: notif Orchestrator dengan detail lengkap
- ✅ Orchestrator: notif SU untuk approval
- ✅ SU reply dengan jadwal → proses venue → calendar → RSVP

#### Step 10.3 — Checklist Sebelum Production

**Infrastructure:**

- [ ] Semua service berjalan stabil selama 24 jam tanpa restart
- [ ] PostgreSQL menggunakan persistent volume
- [ ] Redis menggunakan appendonly (AOF persistence)
- [ ] WAHA session tidak putus setelah 12 jam

**Security:**

- [ ] Test prompt injection dari 5+ pola berbeda → semua diblok
- [ ] Test nomor tidak terdaftar → diblok
- [ ] Test rate limit → aktif setelah 20 msg/menit
- [ ] API keys tidak ada di code (hanya di .env)

**Fungsional:**

- [ ] Skenario A berjalan end-to-end tanpa intervensi manual
- [ ] Skenario B berjalan end-to-end
- [ ] Memory terjaga setelah restart OpenClaw
- [ ] Memory terjaga setelah restart Redis (load dari PG)
- [ ] Calendar event muncul di kedua pihak (SU + External)
- [ ] Email RSVP diterima dengan format yang benar

**Edge Cases:**

- [ ] Pihak eksternal tidak balas dalam 24 jam → sistem timeout dengan notif ke SU
- [ ] SU menolak (reject) → PA Comm notif external dengan sopan
- [ ] Nova tidak tersedia → Orchestrator notif SU untuk input venue manual

---

### FASE 11 — Monitoring Setup

**Estimasi: Hari 33–36**
**Target: Semua service termonitor, alert aktif.**

#### Step 11.1 — Tambahkan Metrics ke API Gateway

Tambahkan library Prometheus client ke API Gateway. Definisikan dua metric utama:

- **Counter `pa_messages_total`**: menghitung total pesan yang diproses dengan label `direction` (inbound/outbound), `agent`, dan `status` (processed/blocked/rate_limited/injection_blocked)
- **Histogram `pa_agent_latency_seconds`**: mengukur waktu respons agent dari saat inject hingga webhook output diterima kembali, dengan label `agent`

Ekspos semua metrics melalui endpoint `GET /metrics` dalam format teks yang dapat di-scrape oleh Prometheus.

#### Step 11.2 — Konfigurasi Prometheus

Buat file `monitoring/prometheus.yml` yang mengkonfigurasi Prometheus untuk scrape dua target:

- API Gateway di `host.docker.internal:4000` (path `/metrics`)
- WAHA container di `pa_waha:3000`

Set scrape interval 15 detik.

#### Step 11.3 — Jalankan Full Docker Compose

Setelah semua service berjalan secara lokal dan seluruh verifikasi fase sebelumnya lulus, pindahkan ke konfigurasi Docker Compose penuh seperti yang ada di **Section 11** dokumen ini. Jalankan semua container sekaligus dan verifikasi semuanya berstatus `Up`.

#### ✅ Verifikasi Fase 11 — MILESTONE AKHIR

- [ ] Prometheus dapat scrape semua target dengan status `up`
- [ ] Grafana dapat diakses di browser di port 3001
- [ ] Dashboard Grafana menampilkan data metric `pa_messages_total` dan `pa_agent_latency_seconds` secara real-time
- [ ] Semua container Docker berstatus `Up` dan stabil selama minimal 1 jam

## 14. Catatan Implementasi Penting

### ❌ Jangan Dilakukan

| No. | Larangan                                       | Alasan                                             |
| --- | ---------------------------------------------- | -------------------------------------------------- |
| 1   | Hubungkan OpenClaw langsung ke WAHA            | Tidak ada security layer, prompt injection risk    |
| 2   | Simpan API keys di dalam code                  | Gunakan `.env` file dan Docker secrets           |
| 3   | Share workspace/memory antar agent             | Isolasi harus dijaga untuk keamanan                |
| 4   | Skip Approval Gate                             | Setiap pesan keluar harus bisa dikendalikan        |
| 5   | Build semua fase sekaligus                     | Ikuti urutan fase untuk debugging yang lebih mudah |
| 6   | Gunakan satu nomor WA untuk testing semua role | Pakai nomor terpisah untuk SU, Ext, Nova           |

### ✅ Yang Wajib Diperhatikan

| No. | Best Practice                                     | Keterangan                                 |
| --- | ------------------------------------------------- | ------------------------------------------ |
| 1   | ConversationId harus konsisten                    | Format:`{agentType}:{normalizedPhone}`   |
| 2   | PostgreSQL adalah ground truth                    | Redis boleh hilang, PG tidak boleh         |
| 3   | Setiap agent punya SOUL.md dan workspace terpisah | Jangan share memory antar agent            |
| 4   | Gunakan 3 nomor WA berbeda saat testing           | SU, External, Nova harus nomor berbeda     |
| 5   | WAHA session bisa disconnect                      | Implementasi health check + auto-reconnect |
| 6   | Monitor queue depth secara berkala                | Backlog > 100 biasanya tanda ada masalah   |
| 7   | Test prompt injection dari nomor unknown          | Pastikan filter bekerja sebelum production |

### Monitoring Metrics Prioritas

| Metric                         | Alert Threshold       | Keterangan                                      |
| ------------------------------ | --------------------- | ----------------------------------------------- |
| `waha_session_status`        | == 0 (disconnected)   | WA session putus → bot tidak bisa kirim/terima |
| `queue_depth_inbound`        | > 100                 | Backlog menumpuk → agent overload              |
| `agent_response_latency_p95` | > 10 detik            | Claude terlalu lambat / timeout                 |
| `approval_rate_auto`         | < 80%                 | Terlalu banyak pesan butuh manual review        |
| `message_error_rate`         | > 5%                  | Error tinggi → perlu investigasi               |
| `redis_memory_usage`         | > 80%                 | Redis hampir penuh → data bisa hilang          |
| `postgres_connections`       | > 80% max_connections | Pool koneksi database habis                     |

### Direktori Struktur Proyek

```
pa-ai-system/
├── services/
│   ├── api-gateway/          # Go GIN
│   │   ├── src/
│   │   │   ├── routes/       # webhook handlers
│   │   │   ├── middleware/   # auth, rate limit, sanitize
│   │   │   ├── contextAssembler.js
│   │   │   └── memoryWriter.js
│   │   └── Dockerfile
│   ├── calendar/             # Google Calendar & O365 wrapper
│   ├── email/                # SendGrid wrapper
│   └── monitoring/
│       └── prometheus.yml
├── sql/
│   └── schema.sql            # DB schema lengkap
├── .openclaw/
│   └── agents/
│       ├── orchestrator/
│       │   └── SOUL.md
│       ├── pa_comm/
│       │   └── SOUL.md
│       └── support/
│           └── SOUL.md
├── .openclaw/workspace/
│   └── workflows/
│       └── meeting_arrangement_flow.yaml
├── data/                     # Docker volumes (gitignore)
│   ├── postgres/
│   ├── redis/
│   └── grafana/
├── .waha/                    # WAHA sessions (gitignore)
├── .env                      # Environment variables (gitignore)
├── .env.example              # Template env vars
├── docker-compose.yml
└── README.md
```

---

## Referensi

- [OpenClaw Documentation](https://openclaw.dev/docs)
- [WAHA Documentation](https://waha.devlike.pro)
- [Anthropic API – Claude](https://docs.anthropic.com)
- [Google Calendar API v3](https://developers.google.com/calendar/api)
- [Redis Streams Guide](https://redis.io/docs/data-types/streams/)
- [SendGrid Node.js SDK](https://github.com/sendgrid/sendgrid-nodejs)

---

*Dokumen ini dihasilkan berdasarkan sesi diskusi arsitektur PA AI System.*
*Versi: 1.0 | Tanggal: Juni 2026*
