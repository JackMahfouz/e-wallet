# E-Wallet Micro-Service System Design

Nov 9, 2026 · @CRM Backend Team

## Executive Summary

This document defines the architecture, service boundaries, data models, and deployment strategy for a production-grade e-wallet system built as a micro-service mesh. The system supports peer-to-peer transfers, card top-ups, merchant payments, transaction history, and compliance reporting.

**Scope**

- 9 bounded micro-services, each owning its own database
- Event-driven inter-service communication over a message broker (Kafka)
- REST APIs exposed to clients via an API Gateway
- PCI-DSS Level 1 compliance posture for card data handling
- Multi-region active-passive deployment targeting 99.99% uptime

**Out of scope:** crypto wallets, lending, investment products.

## Architecture Overview

&#91;embedded content: E-Wallet architecture · 9 services, Kafka bus, external partners\]

All services publish and consume events through Kafka; synchronous REST calls are limited to client-facing reads and the API Gateway. No service calls another service's database directly.

## Service Catalog

Nine bounded services — each owns its database, deploys independently, and communicates via Kafka for writes and REST for reads.

| Service | Responsibility | Language | Database | Key Kafka topics |
| --- | --- | --- | --- | --- |
| **Identity** | Registration, login, JWT issuance, MFA, OAuth2 clients, session revocation | PHP (Laravel) | PostgreSQL | `auth.events` |
| **Wallet** | Balance management, holds/locks, spending limits, multi-currency accounts | Go | PostgreSQL | `wallet.events` |
| **Payment** | Card top-ups, P2P transfers, QR payments, merchant payouts, refunds | Go | PostgreSQL | `payment.events` |
| **Ledger** | Double-entry accounting, debit/credit records, reconciliation | Go | PostgreSQL | `ledger.entries` |
| **Fee Engine** | Fee rule evaluation, FX rate lookup, fee schedule versioning | Go | Redis | `fee.computed` |
| **KYC / AML** | Identity verification, risk scoring, sanctions screening, document storage | Python | PostgreSQL | `kyc.events` |
| **Notification** | Multi-channel dispatch (push, SMS, email), retry queue, preference management | Node.js | PostgreSQL + Redis | `notification.triggers` |
| **Audit** | Immutable event log, regulatory export, tamper detection | Python | ClickHouse | `audit.log` |
| **Reporting** | OLAP queries, compliance reports, merchant dashboards | Python | ClickHouse | — (reads Audit) |

## File Structure

The mono-repo root holds shared infrastructure config; each service is a self-contained directory. Go services share one layout (shown under `wallet/`); Identity (Laravel), the Python services (KYC, Audit, Reporting; FastAPI) and Notification (Express) use their framework's idiomatic layout with the same layering intent.

```
e-wallet/
├── infra/                        # Terraform, Helm charts, Kubernetes manifests
│   ├── terraform/
│   │   ├── eks.tf
│   │   ├── rds.tf
│   │   ├── elasticache.tf
│   │   └── kafka.tf
│   ├── helm/
│   │   └── <service>/            # One chart per service
│   └── k8s/
│       ├── namespace.yaml
│       └── istio-mesh.yaml
│
├── libs/                         # Shared Go/Node packages (not services); Identity (Laravel) and the Python services have their own equivalents
│   ├── auth/                     # JWT validation middleware
│   ├── events/                   # Kafka producer/consumer wrappers
│   ├── observability/            # OTEL tracer, logger, metrics
│   └── pagination/               # Cursor pagination helpers
│
├── services/
│   ├── identity/                 # PHP: Laravel; standard Laravel layout instead of cmd/ + internal/
│   │   ├── app/
│   │   │   ├── Models/           # Eloquent models (persistence + entities)
│   │   │   │   ├── User.php
│   │   │   │   └── Session.php
│   │   │   ├── Services/         # Business logic (no HTTP types)
│   │   │   │   ├── AuthService.php
│   │   │   │   └── MfaService.php
│   │   │   ├── Http/
│   │   │   │   ├── Controllers/  # REST handlers (thin: validate, call a service, respond)
│   │   │   │   │   ├── AuthController.php
│   │   │   │   │   └── UserController.php
│   │   │   │   ├── Requests/     # Form request validation
│   │   │   │   └── Middleware/
│   │   │   ├── Events/           # Domain events (UserRegistered, SessionRevoked, ...)
│   │   │   ├── Listeners/        # Publish domain events to auth.events
│   │   │   ├── Kafka/            # Producer + consumer wrappers (php-rdkafka)
│   │   │   │   ├── Producer.php
│   │   │   │   └── Consumer.php
│   │   │   ├── Console/
│   │   │   │   └── Commands/     # Long-running Kafka consumer workers (kafka:consume)
│   │   │   └── Providers/
│   │   ├── config/
│   │   ├── database/
│   │   │   ├── migrations/       # Laravel migrations
│   │   │   │   ├── 0001_create_users_table.php
│   │   │   │   └── 0002_create_sessions_table.php
│   │   │   └── factories/
│   │   ├── routes/
│   │   │   └── api.php
│   │   ├── tests/
│   │   ├── Dockerfile
│   │   ├── composer.json
│   │   └── composer.lock
│   │
│   ├── wallet/                   # Go: reference layout for all Go services
│   │   ├── cmd/
│   │   │   └── server/
│   │   │       └── main.go
│   │   ├── internal/
│   │   │   ├── domain/           # Entities, value objects, domain errors
│   │   │   │   ├── wallet.go
│   │   │   │   └── hold.go
│   │   │   ├── service/          # Business logic (no HTTP, no DB)
│   │   │   │   ├── wallet_service.go
│   │   │   │   └── limit_service.go
│   │   │   ├── handler/          # HTTP handlers (REST)
│   │   │   │   └── wallet_handler.go
│   │   │   ├── store/            # DB queries (sqlc-generated + wrappers)
│   │   │   │   ├── queries/
│   │   │   │   └── wallet_store.go
│   │   │   ├── events/           # Kafka producers and consumers
│   │   │   │   ├── producer.go
│   │   │   │   └── consumer.go
│   │   │   └── config/
│   │   │       └── config.go
│   │   ├── migrations/           # SQL migration files (goose)
│   │   │   ├── 001_create_wallets.sql
│   │   │   └── 002_create_holds.sql
│   │   ├── Dockerfile
│   │   ├── go.mod
│   │   └── go.sum
│   │
│   ├── payment/                  # Same Go layout as wallet/
│   ├── ledger/
│   ├── fee-engine/
│   ├── kyc/                      # Python: FastAPI; src/ instead of internal/
│   │   ├── src/
│   │   │   ├── domain/
│   │   │   ├── service/
│   │   │   ├── api/
│   │   │   ├── store/
│   │   │   └── events/
│   │   ├── migrations/
│   │   ├── requirements.txt
│   │   └── Dockerfile
│   ├── notification/             # Node.js: Express
│   │   ├── src/
│   │   │   ├── domain/
│   │   │   ├── service/
│   │   │   ├── routes/
│   │   │   ├── store/
│   │   │   └── workers/          # Bull queue workers
│   │   ├── package.json
│   │   └── Dockerfile
│   ├── audit/                    # Python: FastAPI; same layout as kyc/
│   └── reporting/                # Python: FastAPI; same layout as kyc/
│
├── gateway/                      # API Gateway (Envoy config + custom plugins)
│   ├── envoy.yaml
│   ├── rate-limit/
│   └── auth-filter/
│
├── docker-compose.yml            # Local dev: all services + Kafka + DBs
├── Makefile                      # dev, test, lint, migrate targets
└── .github/
    └── workflows/
        ├── ci.yml
        └── deploy.yml
```

**Convention rules**

Go services:

- `domain/` has zero infrastructure imports — testable in isolation
- `service/` depends only on `domain/` and `store/` interfaces — no HTTP types
- `handler/` and `events/` are the only layers that touch transport

Identity (Laravel):

- Eloquent models are Active Record, so the "no infrastructure imports in `domain/`" rule does not apply; business logic lives in `Services/` and is tested against a real test database
- `Services/` takes plain values or DTOs — no `Request`/`Response` types
- Controllers stay thin (validate, call a service, respond); controllers, listeners and `Console/Commands/` are the only layers that touch transport

All services:

- Each service exports a health endpoint at `GET /health` and metrics at `GET /metrics` (Identity maps `/health` over Laravel's default `/up` route)

## Data Models

Each service owns its schema. Foreign key references across service boundaries are soft references (UUID stored, no DB-level constraint).

**Identity service**

```sql
users          (id uuid PK, phone varchar UNIQUE, email varchar UNIQUE,
                password_hash text, status enum, created_at, updated_at)
sessions       (id uuid PK, user_id uuid, device_id text, token_hash text,
                expires_at timestamptz, revoked_at timestamptz)
oauth_clients  (id uuid PK, name text, secret_hash text, scopes text[])
```

**Wallet service**

```sql
wallets        (id uuid PK, user_id uuid UNIQUE, currency char(3),
                balance numeric(18,6), hold_balance numeric(18,6),
                daily_limit numeric(18,6), status enum, created_at)
holds          (id uuid PK, wallet_id uuid, amount numeric(18,6),
                reason text, expires_at timestamptz, released_at timestamptz)
```

**Payment service**

```sql
transactions   (id uuid PK, type enum, status enum, sender_wallet_id uuid,
                receiver_wallet_id uuid, amount numeric(18,6), currency char(3),
                fee_amount numeric(18,6), external_ref text,
                idempotency_key uuid UNIQUE, created_at, settled_at)
payment_methods (id uuid PK, user_id uuid, type enum, stripe_pm_id text,
                 last4 char(4), brand text, is_default bool)
```

**Ledger service**

```sql
ledger_entries (id uuid PK, transaction_id uuid, account_id text,
                direction enum CHECK IN ('debit','credit'),
                amount numeric(18,6), currency char(3),
                balance_after numeric(18,6), created_at)
-- Invariant: SUM(debit) = SUM(credit) per transaction_id
```

**KYC service**

```sql
kyc_profiles   (id uuid PK, user_id uuid UNIQUE, tier enum,
                status enum, risk_score int, verified_at timestamptz)
documents      (id uuid PK, kyc_profile_id uuid, type enum,
                provider_ref text, result enum, checked_at timestamptz)
```

**Audit service (ClickHouse)**

```sql
CREATE TABLE audit_log (
  event_id    UUID,
  service     LowCardinality(String),
  actor_id    UUID,
  action      String,
  resource_id UUID,
  payload     String,   -- JSON blob
  created_at  DateTime64(3, 'UTC')
) ENGINE = ReplacingMergeTree
  ORDER BY (created_at, service, event_id);
```

## API Contracts & Communication

**Client-facing REST (via API Gateway)**

| Method | Path | Service | Description |
| --- | --- | --- | --- |
| POST | `/v1/auth/register` | Identity | Phone + email registration |
| POST | `/v1/auth/login` | Identity | Returns `access_token`, `refresh_token` |
| GET | `/v1/wallets/me` | Wallet | Current balance + limits |
| POST | `/v1/payments/top-up` | Payment | Card → wallet |
| POST | `/v1/payments/transfer` | Payment | P2P transfer (idempotency\_key required) |
| POST | `/v1/payments/pay` | Payment | Wallet → merchant |
| GET | `/v1/transactions` | Payment | Paginated history (cursor-based) |
| GET | `/v1/transactions/:id` | Payment | Single transaction detail |
| POST | `/v1/kyc/submit` | KYC | Document upload trigger |
| GET | `/v1/notifications/preferences` | Notification | User channel prefs |

All endpoints require `Authorization: Bearer <JWT>`. The API Gateway validates the token signature and injects `X-User-ID` before forwarding.

**Inter-service Kafka events**

```jsonc
// wallet.events — emitted by Wallet service
{
  "event_id": "uuid",
  "type": "balance.updated" | "hold.created" | "hold.released",
  "wallet_id": "uuid",
  "user_id": "uuid",
  "delta": "decimal",
  "balance_after": "decimal",
  "occurred_at": "ISO8601"
}

// payment.events — emitted by Payment service
{
  "event_id": "uuid",
  "type": "transaction.initiated" | "transaction.settled" | "transaction.failed",
  "transaction_id": "uuid",
  "sender_wallet_id": "uuid",
  "receiver_wallet_id": "uuid",
  "amount": "decimal",
  "currency": "USD",
  "occurred_at": "ISO8601"
}
```

**Idempotency** — every mutating endpoint accepts `Idempotency-Key: <uuid>` header. The Payment service deduplicates by key for 24 hours using a Redis SET.

**Saga pattern for transfers** — a P2P transfer runs as a choreography saga across Payment → Wallet (debit sender) → Fee Engine → Wallet (credit receiver) → Ledger. Each step publishes its result event; a compensating event triggers rollback if any step fails.

## Security & Compliance

**Authentication & authorization**

- Short-lived access tokens (15 min), long-lived refresh tokens (30 days) stored server-side and revocable
- All tokens are RS256-signed JWTs; public key distributed to all services via Vault PKI
- MFA enforced for high-value actions (transfer > $500, new device login)
- Scoped OAuth2 for third-party clients — no client ever sees the user's wallet directly

**Transport & storage encryption**

- TLS 1.3 everywhere — client to gateway, service to service (mTLS via Istio)
- Database encryption at rest (AWS KMS, AES-256); column-level encryption for card data
- Card numbers never stored — Stripe tokenizes them; only `stripe_pm_id` and `last4` are stored

**PCI-DSS posture**

- Payment service runs in a dedicated PCI-scoped network segment
- No raw PAN ever traverses the application layer
- Quarterly ASV scans, annual QSA assessment
- All Stripe communication uses Stripe-signed webhooks (signature verified before processing)

**Fraud & AML controls**

| Check | Where | Trigger |
| --- | --- | --- |
| Velocity limit | Wallet service | > N transactions per hour per wallet |
| Sanctions screening | KYC service | On registration and on each outbound payout |
| Risk score threshold | KYC service | Blocks transfer if score ≥ HIGH |
| Duplicate detection | Payment service | Idempotency key + amount + recipient match |
| Large transaction flag | Audit service | > $10,000 in a single transfer (SAR trigger) |

**Secrets management**

- All credentials sourced from Vault at boot; no secrets in environment variables or Docker images
- Database credentials rotated every 24 hours via Vault dynamic secrets
- Kafka mTLS certs rotated every 7 days

## Deployment & Infrastructure

**Container orchestration**

- All services run on EKS (Kubernetes 1.30+) across 3 availability zones
- Istio service mesh handles mTLS, circuit breaking, retries, and traffic shaping
- Each service has a `Deployment` (min 2 replicas, HPA up to 20), a `Service`, and a `PodDisruptionBudget`
- Resource requests/limits set per service; Payment and Ledger get dedicated node groups

**Databases**

| Service | Engine | Config |
| --- | --- | --- |
| Identity, Wallet, Payment, Ledger, KYC, Notification | RDS PostgreSQL 16 | Multi-AZ, encrypted, 7-day PITR |
| Fee Engine | ElastiCache Redis 7 (cluster mode) | 3 shards, TLS, AUTH token |
| Audit, Reporting | ClickHouse (self-hosted on EKS) | 3 shards, RF=2, zookeeper-free |

**Message broker**

- MSK (Managed Kafka) with 6 brokers, 3 AZs, 7-day retention, topic-level encryption
- Consumers use consumer groups per service; `auto.offset.reset = earliest`; dead-letter topics for failed events

**CI/CD pipeline**

```
PR opened
  → unit tests (per service)
  → lint + static analysis (go vet; ruff + mypy for Python; Pint + PHPStan for Identity)
  → integration tests (docker-compose)
  → SAST (Semgrep)
  → SCA (Trivy)

Merge to main
  → build Docker image → push to ECR (tagged with git SHA)
  → deploy to staging via Helm
  → smoke tests
  → manual approval gate (Payment + Ledger services)
  → deploy to production (rolling update, 25% per wave)
  → Datadog synthetic monitors verify health
```

**Observability**

- Distributed tracing: OTEL → Datadog APM (trace ID propagated via `traceparent` header)
- Structured JSON logs (service, trace\_id, user\_id, level, message) → Datadog Log Management
- Prometheus metrics scraped by Datadog Agent; dashboards and SLO alerts per service
- On-call rotation via PagerDuty; P1 auto-pages within 2 minutes of SLO breach

## Non-Functional Requirements & Trade-offs

**Targets**

| Dimension | Target | Mechanism |
| --- | --- | --- |
| Availability | 99.99% (< 52 min/year downtime) | Multi-AZ, PDB, rolling deploys |
| P99 latency (transfer API) | < 500 ms | Pre-validated holds, async Ledger write via Kafka |
| P99 latency (read APIs) | < 100 ms | Redis cache for balances; read replicas for history |
| Throughput | 5,000 TPS peak | Kafka partitioning, HPA, connection pooling (PgBouncer) |
| Data durability | 99.999999999% | RDS Multi-AZ + PITR; Kafka RF=3; ClickHouse RF=2 |
| RTO | < 15 min | Automated failover (RDS, MSK); runbooks for ClickHouse |
| RPO | < 1 min | Kafka as source of truth; DB replication lag < 30 s |

**Key design decisions and trade-offs**

- **Choreography saga vs. orchestration saga** — choreography keeps services decoupled and avoids a central orchestrator SPOF, at the cost of harder observability. Mitigated by correlating all events on `transaction_id` in the Audit service.
- **ClickHouse for Audit and Reporting** — write-optimized columnar store handles high-velocity append-only event streams and complex aggregation queries cheaply. Trade-off: not suitable for point updates; fine here because audit events are immutable by design.
- **Separate Fee Engine as a service** — fee rules change frequently and independently of transaction flow. Extracting it avoids redeploying Payment for every pricing change. Trade-off: one extra network hop per transaction; acceptable given the sub-millisecond Redis lookup.
- **Go for core financial services** — deterministic GC pause, low memory footprint, and strong concurrency primitives make it the right fit for latency-sensitive services that move money (Wallet, Payment, Ledger, Fee Engine). Python is used where ML libraries, model deployment or rapid iteration matter (KYC, Audit, Reporting).
- **Python for Audit** — Audit's detection models (e.g. anomaly scoring over the event stream) deploy in the same Python ML stack as KYC, with no separate model-serving hop or cross-language export step. Audit consumes events asynchronously and is off the request path, so Go's latency advantage matters little here. Trade-offs: lower raw ingest throughput per instance than Go, mitigated by `confluent-kafka` (librdkafka-backed), batched ClickHouse inserts, and scaling consumers horizontally up to the topic's partition count; and the Go shared libs (`libs/events`, `libs/observability`) cannot be reused, so Audit must follow the documented event schemas rather than shared code.
- **Laravel for Identity** — registration, login, MFA and OAuth2 clients are CRUD-heavy, security-sensitive flows that Laravel's ecosystem covers with little custom code (Passport for OAuth2 clients and RS256 JWT issuance, plus built-in password hashing, validation, rate limiting and queues), so it is cheaper to build and maintain than a hand-rolled Go service. Identity is off the money-movement hot path: the gateway validates JWT signatures itself, so Identity only serves register/login/refresh traffic and the transfer P99 target is unaffected. Trade-offs: a fourth runtime (PHP) to patch and scan alongside Go, Python and Node; Kafka needs the `php-rdkafka` extension in the image, and consumers run as long-lived `artisan` workers in their own Deployment rather than inside the web process; the Go shared libs (`libs/events`, `libs/observability`) cannot be reused, so event payloads for `auth.events` must follow the documented schema rather than shared code.
- **No distributed transactions (2PC)** — 2PC is a scalability ceiling and a reliability hazard. Saga + idempotency + compensating events achieve eventual consistency without locking across services.
- **Soft foreign keys across service boundaries** — no DB-level FK across service databases; referential integrity enforced by event consumers. Trade-off: a service must handle "entity not found" gracefully for events that arrive before the referenced entity is replicated.
