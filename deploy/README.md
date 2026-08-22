# Sub2API Deployment Files

This directory contains files for deploying Sub2API on Linux servers and Apple-silicon Macs.

## Deployment Methods

| Method | Best For | Setup Wizard |
|--------|----------|--------------|
| **Docker Compose** | Quick setup, all-in-one | Not needed (auto-setup) |
| **Apple container** | Native local stack on macOS 26 | Not needed (auto-setup) |
| **Binary Install** | Production servers, systemd | Web-based wizard |

## Files

| File | Description |
|------|-------------|
| `docker-compose.yml` | Docker Compose configuration (named volumes) |
| `docker-compose.local.yml` | Docker Compose configuration (local directories, easy migration) |
| `docker-deploy.sh` | **One-click Docker deployment script (recommended)** |
| `apple-container.sh` | Native Apple `container` lifecycle script |
| `APPLE_CONTAINER.md` | Apple `container` deployment and operations guide |
| `.env.example` | Container environment variables template |
| `DOCKER.md` | Docker Hub documentation |
| `install.sh` | One-click binary installation script |
| `install-datamanagementd.sh` | datamanagementd 一键安装脚本 |
| `sub2api.service` | Systemd service unit file |
| `sub2api-datamanagementd.service` | datamanagementd systemd service unit file |
| `DATAMANAGEMENTD_CN.md` | datamanagementd 部署与联动说明（中文） |
| `config.example.yaml` | Example configuration file |
| `EDGE_SECURITY.md` | Reverse proxy, CDN/WAF, trusted proxy, and ingress hardening guide |

---

## Apple container Deployment

Apple-silicon Macs running macOS 26 can run the complete Sub2API, PostgreSQL, and Redis stack with Apple `container` 1.1.0 or newer:

```bash
./apple-container.sh init
./apple-container.sh up
./apple-container.sh status
./apple-container.sh logs app -f
```

The script uses Apple named volumes, starts dependencies in order, and performs live readiness checks. It does not provide a continuous restart supervisor; run `./apple-container.sh up` after a host reboot. Docker Compose remains the recommended production deployment path.

See [APPLE_CONTAINER.md](./APPLE_CONTAINER.md) for configuration, upgrades, persistence, networking behavior, and limitations.

---

## Docker Deployment (Recommended)

### Method 1: One-Click Deployment (Recommended)

Use the automated preparation script for the easiest setup:

```bash
# Download and run the preparation script
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh | bash

# Or download first, then run
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh -o docker-deploy.sh
chmod +x docker-deploy.sh
./docker-deploy.sh
```

**What the script does:**
- Downloads `docker-compose.local.yml` and `.env.example`
- Automatically generates secure secrets (JWT_SECRET, TOTP_ENCRYPTION_KEY, POSTGRES_PASSWORD)
- Creates `.env` file with generated secrets
- Creates necessary data directories (data/, postgres_data/, redis_data/)
- **Displays generated credentials** (POSTGRES_PASSWORD, JWT_SECRET, etc.)

**After running the script:**
```bash
# Start services
docker compose -f docker-compose.local.yml up -d

# View logs
docker compose -f docker-compose.local.yml logs -f sub2api

# If admin password was auto-generated, find it in logs:
docker compose -f docker-compose.local.yml logs sub2api | grep "admin password"

# Access Web UI
# http://localhost:8080
```

### Method 2: Manual Deployment

If you prefer manual control:

```bash
# Clone repository
git clone https://github.com/Wei-Shaw/sub2api.git
cd sub2api/deploy

# Configure environment
cp .env.example .env
chmod 600 .env
nano .env  # Set POSTGRES_PASSWORD and other required variables

# Generate secure secrets (recommended)
JWT_SECRET=$(openssl rand -hex 32)
TOTP_ENCRYPTION_KEY=$(openssl rand -hex 32)
echo "JWT_SECRET=${JWT_SECRET}" >> .env
echo "TOTP_ENCRYPTION_KEY=${TOTP_ENCRYPTION_KEY}" >> .env

# Create data directories
mkdir -p data postgres_data redis_data

# Start all services using local directory version
docker compose -f docker-compose.local.yml up -d

# View logs (check for auto-generated admin password)
docker compose -f docker-compose.local.yml logs -f sub2api

# Access Web UI
# http://localhost:8080
```

### Deployment Version Comparison

| Version | Data Storage | Migration | Best For |
|---------|-------------|-----------|----------|
| **docker-compose.local.yml** | Local directories (./data, ./postgres_data, ./redis_data) | ✅ Easy (tar entire directory) | Production, need frequent backups/migration |
| **docker-compose.yml** | Named volumes (/var/lib/docker/volumes/) | ⚠️ Requires docker commands | Simple setup, don't need migration |

**Recommendation:** Use `docker-compose.local.yml` (deployed by `docker-deploy.sh`) for easier data management and migration.

### How Auto-Setup Works

When using Docker Compose with `AUTO_SETUP=true`:

1. On first run, the system automatically:
   - Connects to PostgreSQL and Redis
   - Applies database migrations (SQL files in `backend/migrations/*.sql`) and records them in `schema_migrations`
   - Generates JWT secret (if not provided)
   - Creates admin account (password auto-generated if not provided)
   - Writes config.yaml

2. No manual Setup Wizard needed - just configure `.env` and start

3. If `ADMIN_PASSWORD` is not set, check logs for the generated password:
   ```bash
   docker compose logs sub2api | grep "admin password"
   ```

### Database Migration Notes (PostgreSQL)

- Migrations are applied in lexicographic order (e.g. `001_...sql`, `002_...sql`).
- `schema_migrations` tracks applied migrations (filename + checksum).
- Migrations are forward-only; rollback requires a DB backup restore or a manual compensating SQL script.

**Rolling updates and destructive migrations**

- Migrations run on **every start** of the `sub2api` binary (`backend/internal/repository/ent.go` → `applyMigrationsFS`), serialised across instances by a PostgreSQL advisory lock. This is not limited to the setup path.
- Some migrations are destructive (`090_drop_sora.sql`, `136_remove_ops_retry_replay.sql`, … drop tables and columns). Reverting the image does not revert the schema; the only way back from a destructive migration is restoring the backup taken before the upgrade.
- During a rolling update the old and new versions run concurrently. The moment the new version applies a destructive migration, the old replicas are still reading the schema it just removed — a hard error on every affected request, not a degradation.

Gate every upgrade with the **new** image before rolling it out. The command reads the same config/env as the server, only reads `schema_migrations`, and applies nothing:

```bash
# Docker (same .env as the service; the entrypoint needs the explicit binary path)
docker run --rm --env-file .env weishaw/sub2api:<new-tag> /app/sub2api migrate plan

# Docker Compose (service already defined; --no-deps keeps postgres/redis untouched)
docker compose run --rm --no-deps sub2api /app/sub2api migrate plan

# Binary install: run from the service working directory so the same config.yaml is found
/opt/sub2api/sub2api migrate plan
```

| Exit code | Meaning | What to do |
|-----------|---------|------------|
| `0` | nothing pending | roll as usual |
| `10` | every pending migration is `additive` or `data-rewrite` | rolling update is safe; `data-rewrite` rows are listed so you know which tables get rewritten |
| `20` | at least one pending migration is `destructive` | take a database backup, scale to **zero** replicas (or a single replica), start one instance of the new image so the migrations apply exactly once, then scale back up |
| `1` | error, or an applied migration no longer matches the checksum recorded in `schema_migrations` | the new image will refuse to start against this database; fix that first |

`migrate plan --json` prints the same report as JSON for scripts, `--quiet` prints nothing and only sets the exit code. The report lists every pending file, its kind and the statements behind the verdict, plus the number of applied migrations and the last applied filename; migrations recorded in the database but unknown to the image (you are rolling back) are printed as a warning.

The classifier only reads the `-- +goose Up` part of each file and errs on the side of flagging. In particular the fork-local repair migration `229_maycluster_repair_goose_down_side_effects.sql` is reported as destructive on databases created by upstream releases (it conditionally drops the `users.wechat` column an older runner bug resurrected), so the first upgrade to a build containing it must be a stop-the-world upgrade. The contributor-side rule (expand/contract: the release that stops using a column must not be the one that drops it) is in `backend/migrations/README.md`.

**Verify `users.allowed_groups` → `user_allowed_groups` backfill**

During the incremental GORM→Ent migration, `users.allowed_groups` (legacy `BIGINT[]`) is being replaced by a normalized join table `user_allowed_groups(user_id, group_id)`.

Run this query to compare the legacy data vs the join table:

```sql
WITH old_pairs AS (
  SELECT DISTINCT u.id AS user_id, x.group_id
  FROM users u
  CROSS JOIN LATERAL unnest(u.allowed_groups) AS x(group_id)
  WHERE u.allowed_groups IS NOT NULL
)
SELECT
  (SELECT COUNT(*) FROM old_pairs)           AS old_pair_count,
  (SELECT COUNT(*) FROM user_allowed_groups) AS new_pair_count;
```

### datamanagementd（数据管理）联动

如需启用管理后台“数据管理”功能，请额外部署宿主机 `datamanagementd`：

- 主进程固定探测 `/tmp/sub2api-datamanagement.sock`
- Docker 场景下需把宿主机 Socket 挂载到容器内同路径
- 详细步骤见：`deploy/DATAMANAGEMENTD_CN.md`

### Commands

For **local directory version** (docker-compose.local.yml):

```bash
# Start services
docker compose -f docker-compose.local.yml up -d

# Stop services
docker compose -f docker-compose.local.yml down

# View logs
docker compose -f docker-compose.local.yml logs -f sub2api

# Restart Sub2API only
docker compose -f docker-compose.local.yml restart sub2api

# Update to latest version
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d

# Remove all data (caution!)
docker compose -f docker-compose.local.yml down
rm -rf data/ postgres_data/ redis_data/
```

For **named volumes version** (docker-compose.yml):

```bash
# Start services
docker compose up -d

# Stop services
docker compose down

# View logs
docker compose logs -f sub2api

# Restart Sub2API only
docker compose restart sub2api

# Update to latest version
docker compose pull
docker compose up -d

# Remove all data (caution!)
docker compose down -v
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `POSTGRES_PASSWORD` | **Yes** | - | PostgreSQL password |
| `JWT_SECRET` | **Recommended** | *(auto-generated)* | JWT secret (fixed for persistent sessions) |
| `TOTP_ENCRYPTION_KEY` | **Yes** (release mode) | - | Encryption key for all data at rest (TOTP secrets, monitor API keys, S3 secrets, ...). Must be identical on every instance; the server refuses to start without it in release mode. |
| `TOTP_ENCRYPTION_KEY_PREVIOUS` | No | *(empty)* | Previous key(s) during rotation, comma-separated. See [Rotating the encryption key](#rotating-the-encryption-key). |
| `SERVER_PORT` | No | `8080` | Server port |
| `ADMIN_EMAIL` | No | `admin@sub2api.local` | Admin email |
| `ADMIN_PASSWORD` | No | *(auto-generated)* | Admin password |
| `TZ` | No | `Asia/Shanghai` | Timezone |
| `UPDATE_GITHUB_TOKEN` | No | *(empty)* | Token for `api.github.com` release checks only; asset downloads remain anonymous. |
| `GEMINI_OAUTH_CLIENT_ID` | No | *(builtin)* | Google OAuth client ID (Gemini OAuth). Leave empty to use the built-in Gemini CLI client. |
| `GEMINI_OAUTH_CLIENT_SECRET` | No | *(builtin)* | Google OAuth client secret (Gemini OAuth). Leave empty to use the built-in Gemini CLI client. |
| `GEMINI_OAUTH_SCOPES` | No | *(default)* | OAuth scopes (Gemini OAuth) |
| `GEMINI_QUOTA_POLICY` | No | *(empty)* | JSON overrides for Gemini local quota simulation (Code Assist only). |

See `.env.example` for all available options.

> **Note:** The `docker-deploy.sh` script automatically generates `JWT_SECRET`, `TOTP_ENCRYPTION_KEY`, and `POSTGRES_PASSWORD` for you.

### Connection Settings (external PostgreSQL / Redis)

`docker-compose.standalone.yml`, plain `docker run` (see `DOCKER.md`) and the systemd install connect to
services you operate yourself. The connection target is accepted in two forms. When the URL form is set
it is the **only** source for that target and the discrete variables are ignored — the startup log states
which form was used. Pool and timeout settings (`DATABASE_MAX_OPEN_CONNS`, `REDIS_POOL_SIZE`, ...) always
come from the discrete variables.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | No | - | `postgres://user:pass@host:port/dbname?sslmode=...`; replaces `DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE`. Extra libpq options (`sslrootcert=`, `application_name=`, ...) are passed through; an unknown option name fails at startup. |
| `REDIS_URL` | No | - | `redis://[user:pass@]host:port/db` or `rediss://...` (TLS); replaces `REDIS_HOST/PORT/USERNAME/PASSWORD/DB/ENABLE_TLS`. Only the database index may be a query parameter. |
| `REDIS_SENTINEL_ADDRS` | No | - | Comma-separated Sentinel `host:port` list. Setting it enables Sentinel mode; `REDIS_HOST`/`REDIS_PORT` are then unused. |
| `REDIS_MASTER_NAME` | No | - | Master name resolved through the Sentinels; required together with `REDIS_SENTINEL_ADDRS`. |
| `REDIS_SENTINEL_USERNAME` | No | *(empty)* | ACL user for the Sentinel connections themselves. |
| `REDIS_SENTINEL_PASSWORD` | No | *(empty)* | Password for the Sentinel connections themselves. |
| `REDIS_TLS_SERVER_NAME` | No | *(dialed host)* | Name used for TLS certificate verification when it differs from the address being dialed; applies to Sentinels and data nodes alike. |

#### Redis High Availability

**Redis Sentinel is supported.** Point `REDIS_SENTINEL_ADDRS` at the Sentinels and name the master in
`REDIS_MASTER_NAME`; the client discovers the current master and follows failovers. `REDIS_USERNAME`,
`REDIS_PASSWORD`, `REDIS_DB` and `REDIS_ENABLE_TLS` apply to the data nodes. With TLS, every connection
verifies the certificate against the host it dials (Sentinels by their configured addresses, the master by
the address the Sentinels announce); set `REDIS_TLS_SERVER_NAME` only when all nodes share one
certificate name. Check the startup log line `redis client configured redis.mode=sentinel master=...
sentinels=N`.

**Redis Cluster is not supported, and this is final rather than pending.** The concurrency-slot and
scheduler Lua scripts touch keys in different hash slots (`acquireLiveLeaseScript` in
`backend/internal/repository/concurrency_cache.go`, runtime-built key names in
`backend/internal/repository/scheduler_cache.go`), which Cluster rejects with `CROSSSLOT`. Any
`REDIS_CLUSTER_*` variable or `redis.cluster*` config key makes the process refuse to start with that
explanation.

### Rotating the encryption key

`TOTP_ENCRYPTION_KEY` is, despite its name, the key for everything Sub2API stores encrypted: user TOTP secrets, channel monitor API keys, backup and image-storage S3 secrets, Ollama web sessions, prompt-audit endpoint tokens, and the payment resume-token signing key derived from it. Every ciphertext carries the id of the key that wrote it (`v1:<key-id>:...`), so two keys can be valid at the same time and the key can be replaced without losing data:

```bash
# 1. Generate the new key.
openssl rand -hex 32

# 2. On EVERY instance: make the new key primary and keep the old one as previous, then restart.
TOTP_ENCRYPTION_KEY=<new key>
TOTP_ENCRYPTION_KEY_PREVIOUS=<old key>
docker compose up -d          # instances now read both old and new ciphertext; new writes use the new key

# 3. Re-encrypt everything that is still under the old key (idempotent; safe to re-run).
docker compose exec sub2api /app/sub2api encryption-key rotate --dry-run
docker compose exec sub2api /app/sub2api encryption-key rotate

# 4. When the report says "stored fingerprint updated", remove the previous key and restart.
TOTP_ENCRYPTION_KEY_PREVIOUS=
docker compose up -d
```

The command prints, per storage location, how many rows were rotated, were already current, or failed (with row ids). Failed rows are ciphertext no configured key can open — typically data written by an instance that ran with a different (or auto-generated) key; reset those secrets through the admin UI and re-run. Legacy payment provider configs are converted to the current plaintext format in the same pass. `--old-key`/`--new-key` override the two variables for a single run.

Sub2API records a fingerprint (SHA-256, never the key itself) of the key that encrypted the stored data in the `security_secrets` table and logs it at startup as `encryption_key.fingerprint=<8 chars>`. An instance started with a key that is neither that key nor listed in `TOTP_ENCRYPTION_KEY_PREVIOUS` refuses to start in release mode, which is how a replica with a mismatched key is caught before it encrypts data nobody else can read.

### Easy Migration (Local Directory Version)

When using `docker-compose.local.yml`, all data is stored in local directories, making migration simple:

```bash
# On source server: Stop services and create archive
cd /path/to/deployment
docker compose -f docker-compose.local.yml down
cd ..
tar czf sub2api-complete.tar.gz deployment/

# Transfer to new server
scp sub2api-complete.tar.gz user@new-server:/path/to/destination/

# On new server: Extract and start
tar xzf sub2api-complete.tar.gz
cd deployment/
docker compose -f docker-compose.local.yml up -d
```

Your entire deployment (configuration + data) is migrated!

---

## Gemini OAuth Configuration

Sub2API supports three methods to connect to Gemini:

### Method 1: Code Assist OAuth (Recommended for GCP Users)

**No configuration needed** - always uses the built-in Gemini CLI OAuth client (public).

1. Leave `GEMINI_OAUTH_CLIENT_ID` and `GEMINI_OAUTH_CLIENT_SECRET` empty
2. In the Admin UI, create a Gemini OAuth account and select **"Code Assist"** type
3. Complete the OAuth flow in your browser

> Note: Even if you configure `GEMINI_OAUTH_CLIENT_ID` / `GEMINI_OAUTH_CLIENT_SECRET` for AI Studio OAuth,
> Code Assist OAuth will still use the built-in Gemini CLI client.

**Requirements:**
- Google account with access to Google Cloud Platform
- A GCP project (auto-detected or manually specified)

**How to get Project ID (if auto-detection fails):**
1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Click the project dropdown at the top of the page
3. Copy the Project ID (not the project name) from the list
4. Common formats: `my-project-123456` or `cloud-ai-companion-xxxxx`

### Method 2: AI Studio OAuth (For Regular Google Accounts)

Requires your own OAuth client credentials.

**Step 1: Create OAuth Client in Google Cloud Console**

1. Go to [Google Cloud Console - Credentials](https://console.cloud.google.com/apis/credentials)
2. Create a new project or select an existing one
3. **Enable the Generative Language API:**
   - Go to "APIs & Services" → "Library"
   - Search for "Generative Language API"
   - Click "Enable"
4. **Configure OAuth Consent Screen** (if not done):
   - Go to "APIs & Services" → "OAuth consent screen"
   - Choose "External" user type
   - Fill in app name, user support email, developer contact
   - Add scopes: `https://www.googleapis.com/auth/generative-language.retriever` (and optionally `https://www.googleapis.com/auth/cloud-platform`)
   - Add test users (your Google account email)
5. **Create OAuth 2.0 credentials:**
   - Go to "APIs & Services" → "Credentials"
   - Click "Create Credentials" → "OAuth client ID"
   - Application type: **Web application** (or **Desktop app**)
   - Name: e.g., "Sub2API Gemini"
   - Authorized redirect URIs: Add `http://localhost:1455/auth/callback`
6. Copy the **Client ID** and **Client Secret**
7. **⚠️ Publish to Production (IMPORTANT):**
   - Go to "APIs & Services" → "OAuth consent screen"
   - Click "PUBLISH APP" to move from Testing to Production
   - **Testing mode limitations:**
     - Only manually added test users can authenticate (max 100 users)
     - Refresh tokens expire after 7 days
     - Users must be re-added periodically
   - **Production mode:** Any Google user can authenticate, tokens don't expire
   - Note: For sensitive scopes, Google may require verification (demo video, privacy policy)

**Step 2: Configure Environment Variables**

```bash
GEMINI_OAUTH_CLIENT_ID=your-client-id.apps.googleusercontent.com
GEMINI_OAUTH_CLIENT_SECRET=GOCSPX-your-client-secret

# 可选：如需使用 Gemini CLI 内置 OAuth Client（Code Assist / Google One）
# 安全说明：本仓库不会内置该 client_secret，请在运行环境通过环境变量注入。
# GEMINI_CLI_OAUTH_CLIENT_SECRET=GOCSPX-your-built-in-secret
```

**Step 3: Create Account in Admin UI**

1. Create a Gemini OAuth account and select **"AI Studio"** type
2. Complete the OAuth flow
   - After consent, your browser will be redirected to `http://localhost:1455/auth/callback?code=...&state=...`
   - Copy the full callback URL (recommended) or just the `code` and paste it back into the Admin UI

### Method 3: API Key (Simplest)

1. Go to [Google AI Studio](https://aistudio.google.com/app/apikey)
2. Click "Create API key"
3. In Admin UI, create a Gemini **API Key** account
4. Paste your API key (starts with `AIza...`)

### Comparison Table

| Feature | Code Assist OAuth | AI Studio OAuth | API Key |
|---------|-------------------|-----------------|---------|
| Setup Complexity | Easy (no config) | Medium (OAuth client) | Easy |
| GCP Project Required | Yes | No | No |
| Custom OAuth Client | No (built-in) | Yes (required) | N/A |
| Rate Limits | GCP quota | Standard | Standard |
| Best For | GCP developers | Regular users needing OAuth | Quick testing |

---

## Binary Installation

For production servers using systemd.

### One-Line Installation

```bash
curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/install.sh | sudo bash
```

### Manual Installation

1. Download the latest release from [GitHub Releases](https://github.com/Wei-Shaw/sub2api/releases)
2. Extract and copy the binary to `/opt/sub2api/`
3. Copy `sub2api.service` to `/etc/systemd/system/`
4. Run:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable sub2api
   sudo systemctl start sub2api
   ```
5. Open the Setup Wizard in your browser to complete configuration

### Commands

```bash
# Install
sudo ./install.sh

# Upgrade
sudo ./install.sh upgrade

# Uninstall
sudo ./install.sh uninstall
```

### Service Management

```bash
# Start the service
sudo systemctl start sub2api

# Stop the service
sudo systemctl stop sub2api

# Restart the service
sudo systemctl restart sub2api

# Check status
sudo systemctl status sub2api

# View logs
sudo journalctl -u sub2api -f

# Enable auto-start on boot
sudo systemctl enable sub2api
```

### Configuration

#### Server Address and Port

During installation, you will be prompted to configure the server listen address and port. These settings are stored in the systemd service file as environment variables.

To change after installation:

1. Edit the systemd service:
   ```bash
   sudo systemctl edit sub2api
   ```

2. Add or modify:
   ```ini
   [Service]
   Environment=SERVER_HOST=0.0.0.0
   Environment=SERVER_PORT=3000
   ```

3. Reload and restart:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl restart sub2api
   ```

#### Gemini OAuth Configuration

If you need to use AI Studio OAuth for Gemini accounts, add the OAuth client credentials to the systemd service file:

1. Edit the service file:
   ```bash
   sudo nano /etc/systemd/system/sub2api.service
   ```

2. Add your OAuth credentials in the `[Service]` section (after the existing `Environment=` lines):
   ```ini
   Environment=GEMINI_OAUTH_CLIENT_ID=your-client-id.apps.googleusercontent.com
   Environment=GEMINI_OAUTH_CLIENT_SECRET=GOCSPX-your-client-secret
   ```

   如需使用“内置 Gemini CLI OAuth Client”（Code Assist / Google One），还需要注入：
   ```ini
   Environment=GEMINI_CLI_OAUTH_CLIENT_SECRET=GOCSPX-your-built-in-secret
   ```

3. Reload and restart:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl restart sub2api
   ```

> **Note:** Code Assist OAuth does not require any configuration - it uses the built-in Gemini CLI client.
> See the [Gemini OAuth Configuration](#gemini-oauth-configuration) section above for detailed setup instructions.

#### Application Configuration

The main config file is at `/etc/sub2api/config.yaml` (created by Setup Wizard).

### Prerequisites

- Linux server (Ubuntu 20.04+, Debian 11+, CentOS 8+, etc.)
- PostgreSQL 14+
- Redis 6+ (single node or Sentinel; Redis Cluster is not supported — see "Redis High Availability")
- systemd

### Directory Structure

```
/opt/sub2api/
├── sub2api              # Main binary
├── sub2api.backup       # Backup (after upgrade)
└── data/                # Runtime data

/etc/sub2api/
└── config.yaml          # Configuration file
```

---

## Health and Readiness Probes

Sub2API exposes two probe endpoints. They answer different questions and fail independently, so wire them to different orchestrator probes:

| Endpoint | Question | Depends on | Response |
|----------|----------|------------|----------|
| `GET /health` | Is the process alive? (**liveness**) | Nothing | Always `200 {"status":"ok"}` while the process runs |
| `GET /readyz` | Can this instance serve traffic? (**readiness**) | PostgreSQL and Redis | `200 {"status":"ready","checks":{"postgres":"ok","redis":"ok"}}` or `503 {"status":"not_ready","checks":{"postgres":"ok","redis":"error: ..."}}` |

`/readyz` probes every dependency concurrently, each with its own timeout (`server.readiness_timeout_seconds`, default `2`, env `SERVER_READINESS_TIMEOUT_SECONDS`). Error texts are redacted (no DSNs or passwords), the response carries `Cache-Control: no-store`, and a dependency that turns unhealthy or recovers is logged once per transition. Neither endpoint requires authentication, and neither is written to the access log.

Why two endpoints: a dependency outage must fail readiness (stop routing traffic to the instance) but must **not** fail liveness. If `/health` depended on the database, Kubernetes would restart every replica during a database blip and turn a recoverable outage into a crash loop.

Kubernetes example:

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  periodSeconds: 10
  failureThreshold: 3
readinessProbe:
  httpGet:
    path: /readyz
    port: 8080
  periodSeconds: 10
  timeoutSeconds: 3       # must exceed server.readiness_timeout_seconds
  failureThreshold: 3     # tolerate a single slow probe under load instead of evicting the pod
```

The Docker Compose files in this directory keep using `/health` for the container healthcheck (process-level liveness). Point an external load balancer or ingress health check at `/readyz` instead, so an instance that lost its database or Redis is taken out of rotation rather than kept in it.

### In-app update with multiple instances

The admin panel's in-app update and online rollback replace the running binary of the **one process that served the request**. With several replicas that produces version skew (one replica on the new version, the rest on the old one, possibly with migrations already applied), and in a container the change lives in the writable layer and is undone by the next restart. The server therefore refuses the operation with HTTP 409 and error code `IN_APP_UPDATE_DISABLED`, and the UI disables the buttons up front, when any of these holds:

- **More than one live instance.** Every process heartbeats into the Redis sorted set `instances:heartbeat` every 15s and counts as live for 45s after its last heartbeat; a gracefully stopped process deregisters immediately, a crashed one drops out after the window.
- **The instance count is unknown** (Redis unreachable or an instance record the server cannot decode). This fails closed: an unknown topology is treated as multi-instance.
- **The executable's directory is not writable**, for example a read-only root filesystem. The check creates and removes a temp file next to the binary, which is exactly what the update would have to do.

`GET /api/v1/admin/system/check-updates` reports the decision as `in_app_update_allowed`, `in_app_update_blocked_code`, `in_app_update_blocked_reason` and `live_instances` (`-1` when unknown); refusals are also logged with the reason. Upgrade such deployments by changing the image tag (`docker compose pull && docker compose up -d`, or the image in your Kubernetes manifest) or by running `install.sh upgrade` on every host.

---

## Troubleshooting

### Docker

For **local directory version**:

```bash
# Check container status
docker compose -f docker-compose.local.yml ps

# View detailed logs
docker compose -f docker-compose.local.yml logs --tail=100 sub2api

# Check database connection
docker compose -f docker-compose.local.yml exec postgres pg_isready

# Check Redis connection
docker compose -f docker-compose.local.yml exec redis redis-cli ping

# Restart all services
docker compose -f docker-compose.local.yml restart

# Check data directories
ls -la data/ postgres_data/ redis_data/
```

For **named volumes version**:

```bash
# Check container status
docker compose ps

# View detailed logs
docker compose logs --tail=100 sub2api

# Check database connection
docker compose exec postgres pg_isready

# Check Redis connection
docker compose exec redis redis-cli ping

# Restart all services
docker compose restart
```

### Binary Install

```bash
# Check service status
sudo systemctl status sub2api

# View recent logs
sudo journalctl -u sub2api -n 50

# Check config file
sudo cat /etc/sub2api/config.yaml

# Check PostgreSQL
sudo systemctl status postgresql

# Check Redis
sudo systemctl status redis
```

### Common Issues

1. **Port already in use**: Change `SERVER_PORT` in `.env` or systemd config
2. **Database connection failed**: Check PostgreSQL is running and credentials are correct
3. **Redis connection failed**: Check Redis is running and password is correct. The startup log line `redis client configured redis.mode=...` shows the address or Sentinel master actually in use; `redis connection configured from redis.url` means the discrete `REDIS_*` connection variables were ignored in favour of `REDIS_URL`
4. **Permission denied**: Ensure proper file ownership for binary install
5. **Update button disabled / `409 IN_APP_UPDATE_DISABLED`**: More than one instance is running, the instance count is unknown (check Redis), or the program directory is read-only. Upgrade by changing the image tag instead; see [In-app update with multiple instances](#in-app-update-with-multiple-instances)

---

## TLS Fingerprint Configuration

Sub2API supports TLS fingerprint simulation to make requests appear as if they come from the official Claude CLI (Node.js client).

> **💡 Tip:** Visit **[tls.sub2api.org](https://tls.sub2api.org/)** to get TLS fingerprint information for different devices and browsers.

### Default Behavior

- Built-in `claude_cli_v2` profile simulates Node.js 20.x + OpenSSL 3.x
- JA3 Hash: `1a28e69016765d92e3b381168d68922c`
- JA4: `t13d5911h1_a33745022dd6_1f22a2ca17c4`
- Profile selection: `accountID % profileCount`

### Configuration

```yaml
gateway:
  tls_fingerprint:
    enabled: true  # Global switch
    profiles:
      # Simple profile (uses default cipher suites)
      profile_1:
        name: "Profile 1"

      # Profile with custom cipher suites (use compact array format)
      profile_2:
        name: "Profile 2"
        cipher_suites: [4866, 4867, 4865, 49199, 49195, 49200, 49196]
        curves: [29, 23, 24]
        point_formats: 0

      # Another custom profile
      profile_3:
        name: "Profile 3"
        cipher_suites: [4865, 4866, 4867, 49199, 49200]
        curves: [29, 23, 24, 25]
```

### Profile Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Display name (required) |
| `cipher_suites` | []uint16 | Cipher suites in decimal. Empty = default |
| `curves` | []uint16 | Elliptic curves in decimal. Empty = default |
| `point_formats` | []uint8 | EC point formats. Empty = default |

### Common Values Reference

**Cipher Suites (TLS 1.3):** `4865` (AES_128_GCM), `4866` (AES_256_GCM), `4867` (CHACHA20)

**Cipher Suites (TLS 1.2):** `49195`, `49196`, `49199`, `49200` (ECDHE variants)

**Curves:** `29` (X25519), `23` (P-256), `24` (P-384), `25` (P-521)
