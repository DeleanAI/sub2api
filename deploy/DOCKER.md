# Sub2API Docker Image

Sub2API is an AI API Gateway Platform for distributing and managing AI product subscription API quotas.

## Quick Start

```bash
docker run -d \
  --name sub2api \
  -p 8080:8080 \
  -v sub2api_data:/app/data \
  -e AUTO_SETUP=true \
  -e DATABASE_URL="postgres://user:pass@db-host:5432/sub2api?sslmode=disable" \
  -e REDIS_URL="redis://:redis-pass@redis-host:6379/0" \
  -e JWT_SECRET="$(openssl rand -hex 32)" \
  -e TOTP_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  weishaw/sub2api:latest
```

On the first start with `AUTO_SETUP=true` the container connects to PostgreSQL and Redis, applies the
database migrations, creates the admin account (the password is printed to the logs once when
`ADMIN_PASSWORD` is empty) and writes `config.yaml` into `/app/data`. Mount `/app/data` so that file and
the install lock survive restarts; without it every restart runs the setup again.

## Docker Compose

```yaml
services:
  sub2api:
    image: weishaw/sub2api:latest
    ports:
      - "8080:8080"
    volumes:
      - sub2api_data:/app/data
    environment:
      - AUTO_SETUP=true
      - DATABASE_URL=postgres://postgres:postgres@db:5432/sub2api?sslmode=disable
      - REDIS_URL=redis://redis:6379/0
      - JWT_SECRET=change-me-to-a-random-secret-of-32-or-more-characters
      # 64 hex characters. Generate with: openssl rand -hex 32
      # A non-hex placeholder is fatal at startup, not a warning.
      - TOTP_ENCRYPTION_KEY=0000000000000000000000000000000000000000000000000000000000000000
      - TZ=Asia/Shanghai
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_healthy

  db:
    image: postgres:18-alpine
    environment:
      # postgres:18 keeps its data under /var/lib/postgresql/18/docker by default; point PGDATA at the
      # mounted volume or the database is re-initialised on every `compose down && up`.
      - PGDATA=/var/lib/postgresql/data
      - POSTGRES_USER=postgres
      - POSTGRES_PASSWORD=postgres
      - POSTGRES_DB=sub2api
    volumes:
      - postgres_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres -d sub2api"]
      interval: 10s
      timeout: 5s
      retries: 5

  redis:
    image: redis:8-alpine
    command: redis-server --save 60 1 --appendonly yes --appendfsync everysec
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5

volumes:
  sub2api_data:
  postgres_data:
  redis_data:
```

This is the minimum that boots. The maintained Compose files with connection-pool tuning, resource
limits and security options live in the repository under `deploy/` (`docker-compose.yml` bundles
PostgreSQL and Redis, `docker-compose.standalone.yml` connects to services you run yourself).
## Startup and Database Recovery

Sub2API runs database migrations while starting. PostgreSQL may still be
recovering briefly after a host or Docker daemon restart. The application
retries transient PostgreSQL startup and connection errors with bounded
exponential backoff, then continues startup when the database is ready.
Permanent errors such as invalid credentials, migration checksum mismatches,
SQL errors, and incompatible data fail immediately.

The Compose deployment also checks PostgreSQL readiness with both `pg_isready`
and a simple SQL query. `depends_on: condition: service_healthy` helps order a
fresh Compose start, but application-level retries are still required when
Docker restores existing containers after a host restart.

## Environment Variables

Every variable below is read by the backend. `DATABASE_URL` / `REDIS_URL` and the discrete
`DATABASE_*` / `REDIS_*` forms are alternatives: when a URL is set it is the **only** source for that
connection target and the discrete connection variables are ignored (the startup log says which form
was used). Pool and timeout settings always come from the discrete variables.

Defaults are those applied by `AUTO_SETUP`; they are written to `config.yaml` on the first start.

### Database and Redis connection

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `DATABASE_URL` | PostgreSQL connection URL `postgres://user:pass@host:port/dbname?sslmode=...`. Extra libpq options such as `sslrootcert=` or `application_name=` are passed through; an unknown option name fails at startup naming the option. | One of `DATABASE_URL` or `DATABASE_HOST` | - |
| `DATABASE_HOST` | PostgreSQL host (ignored when `DATABASE_URL` is set) | One of `DATABASE_URL` or `DATABASE_HOST` | `localhost` |
| `DATABASE_PORT` | PostgreSQL port | No | `5432` |
| `DATABASE_USER` | PostgreSQL user | No | `postgres` |
| `DATABASE_PASSWORD` | PostgreSQL password | No | *(empty)* |
| `DATABASE_DBNAME` | Database name (created on first start if missing) | No | `sub2api` |
| `DATABASE_SSLMODE` | `disable`, `require`, `verify-ca` or `verify-full` | No | `disable` |
| `DATABASE_MAX_OPEN_CONNS` | Connection pool: max open connections | No | `256` |
| `DATABASE_MAX_IDLE_CONNS` | Connection pool: max idle connections | No | `128` |
| `REDIS_URL` | `redis://[user:pass@]host:port/db`, or `rediss://...` for TLS. Only the database index may be given as a query parameter; tuning stays in `REDIS_*`. | One of `REDIS_URL`, `REDIS_HOST` or `REDIS_SENTINEL_ADDRS` | - |
| `REDIS_HOST` | Redis host (ignored when `REDIS_URL` or `REDIS_SENTINEL_ADDRS` is set) | One of `REDIS_URL`, `REDIS_HOST` or `REDIS_SENTINEL_ADDRS` | `localhost` |
| `REDIS_PORT` | Redis port | No | `6379` |
| `REDIS_USERNAME` | Redis ACL user for the data nodes | No | *(empty)* |
| `REDIS_PASSWORD` | Redis password for the data nodes | No | *(empty)* |
| `REDIS_DB` | Database index | No | `0` |
| `REDIS_ENABLE_TLS` | `true` to connect with TLS (same as `rediss://`) | No | `false` |
| `REDIS_TLS_SERVER_NAME` | Name used for certificate verification. By default each connection verifies against the host it dials, which is what you want unless Redis sits behind a name that differs from the dial address. | No | *(dialed host)* |
| `REDIS_SENTINEL_ADDRS` | Comma-separated `host:port` list of Sentinels. Setting it switches to Sentinel mode (see below); `REDIS_HOST`/`REDIS_PORT` are then unused. | No | - |
| `REDIS_MASTER_NAME` | Master name to ask the Sentinels for. Required together with `REDIS_SENTINEL_ADDRS`. | No | - |
| `REDIS_SENTINEL_USERNAME` | ACL user for the Sentinel connections themselves | No | *(empty)* |
| `REDIS_SENTINEL_PASSWORD` | Password for the Sentinel connections themselves | No | *(empty)* |
| `REDIS_POOL_SIZE` | Connection pool size (applies to single-node and Sentinel mode alike) | No | `1024` |
| `REDIS_MIN_IDLE_CONNS` | Minimum idle connections | No | `128` |

### Server and first-start setup

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `AUTO_SETUP` | `true` initialises the instance from the environment on the first start instead of serving the setup wizard | Yes, for unattended deployments | `false` |
| `SERVER_HOST` | Listen address inside the container | No | `0.0.0.0` |
| `SERVER_PORT` | Listen port inside the container | No | `8080` |
| `SERVER_MODE` | `release` or `debug` | No | `release` |
| `SERVER_FRONTEND_VARIANT` | Which embedded frontend to serve (`dist/<name>`); an unknown name fails startup and lists the available variants | No | `default` |
| `ADMIN_EMAIL` | Admin account created on the first start | No | `admin@sub2api.local` |
| `ADMIN_PASSWORD` | Admin password; generated and printed to the logs once when empty | No | *(generated)* |
| `JWT_SECRET` | 32+ bytes. Generated on every start when empty, which logs all users out on restart | Recommended | *(generated)* |
| `TOTP_ENCRYPTION_KEY` | 64 hex characters (`openssl rand -hex 32`). Encrypts TOTP secrets and every other stored secret. **Startup fails when it is empty and `SERVER_MODE=release` (the default)**, and when the value is not 64 hex characters. Only `SERVER_MODE=debug` falls back to a per-process random key, which no other instance can read | **Yes** (release mode) | *(none; debug mode generates a per-process key)* |
| `TZ` | Timezone for the application and for database sessions | No | `Asia/Shanghai` |
| `DATA_DIR` | Directory for `config.yaml`, the install lock and log files | No | `/app/data` |

## Redis high availability

**Redis Sentinel is supported.** Set `REDIS_SENTINEL_ADDRS` and `REDIS_MASTER_NAME` (and
`REDIS_SENTINEL_PASSWORD` when the Sentinels require authentication). The client asks the Sentinels
for the current master and follows failovers automatically; `REDIS_USERNAME`, `REDIS_PASSWORD`,
`REDIS_DB` and `REDIS_ENABLE_TLS` apply to the data nodes. With TLS every connection verifies the
certificate against the host it dials — the Sentinels by their configured addresses, the master by the
address the Sentinels announce — so each node needs a certificate valid for its own address. Set
`REDIS_TLS_SERVER_NAME` only when all nodes present a certificate for one shared name. The startup log
reports the chosen mode (`redis.mode=sentinel master=... sentinels=N` or `redis.mode=single addr=...`).

**Redis Cluster is not supported, and this is final rather than pending.** The concurrency-slot and
scheduler Lua scripts operate on keys that hash to different slots (`acquireLiveLeaseScript` in
`internal/repository/concurrency_cache.go`, runtime-built key names in
`internal/repository/scheduler_cache.go`), which Cluster rejects with `CROSSSLOT`. Any `REDIS_CLUSTER_*`
variable makes the process refuse to start with that explanation.

## Supported Architectures

- `linux/amd64`
- `linux/arm64`

## Tags

- `latest` - Latest stable release
- `x.y.z` - Specific version
- `x.y` - Latest patch of minor version
- `x` - Latest minor of major version

## Links

- [GitHub Repository](https://github.com/weishaw/sub2api)
- [Documentation](https://github.com/weishaw/sub2api#readme)
