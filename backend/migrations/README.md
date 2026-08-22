# Database Migrations

## Overview

This directory contains SQL migration files for database schema changes. The migration system uses SHA256 checksums to ensure migration immutability and consistency across environments.

## Migration File Naming

Format: `NNN_description.sql`
- `NNN`: Sequential number (e.g., 001, 002, 003)
- `description`: Brief description in snake_case

Example: `017_add_gemini_tier_id.sql`

Fork-local migrations (added in this fork, not upstream) carry a `maycluster_` infix so their origin is obvious when merging upstream: `229_maycluster_repair_goose_down_side_effects.sql`.

### `_notx.sql` 命名与执行语义（并发索引专用）

当迁移包含 `CREATE INDEX CONCURRENTLY` 或 `DROP INDEX CONCURRENTLY` 时，必须使用 `_notx.sql` 后缀，例如：

- `062_add_accounts_priority_indexes_notx.sql`
- `063_drop_legacy_indexes_notx.sql`

运行规则：

1. `*.sql`（不带 `_notx`）按事务执行。
2. `*_notx.sql` 按非事务执行，不会包裹在 `BEGIN/COMMIT` 中。
3. `*_notx.sql` 仅允许并发索引语句，不允许混入事务控制语句或其他 DDL/DML。

幂等要求（必须）：

- 创建索引：`CREATE INDEX CONCURRENTLY IF NOT EXISTS ...`
- 删除索引：`DROP INDEX CONCURRENTLY IF EXISTS ...`

这样可以保证灾备重放、重复执行时不会因对象已存在/不存在而失败。

## Migration File Structure

This project uses a custom migration runner (`internal/repository/migrations_runner.go`). It is **forward-only**: there is no "down" path, a rollback is a new forward migration.

- Regular migrations (`*.sql`): executed in a transaction.
- Non-transactional migrations (`*_notx.sql`): split by statement and executed without a transaction (only for `CREATE/DROP INDEX CONCURRENTLY`).
- Files **without** goose markers are executed in full, exactly as written.
- Files **with** `-- +goose Up` / `-- +goose Down` markers: **only the Up section is executed; the Down section is never run.** The rule lives in one place, `migrations.ExecutableSQL` (`migrations/goose.go`); the runner goes through it and nothing else decides what runs. The checksum recorded in `schema_migrations` still covers the whole file, so databases migrated before this rule keep matching.
- Prefer files with no Down section at all. If one is present it must come after the Up section, the Up section must contain executable SQL, and the only recognised directives are `Up`, `Down`, `StatementBegin`, `StatementEnd`. Any other directive (`NO TRANSACTION`, `ENVSUB`, …) makes the runner refuse the file instead of silently treating it as a comment — use the `_notx.sql` suffix for non-transactional migrations.

```sql
-- Forward-only migration (recommended)
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS example_column VARCHAR(100);
```

History: until this fork's fix the runner handed the whole file to PostgreSQL, so a Down section ran right after its Up section inside the same transaction. `037_ops_alert_silences.sql` therefore created and immediately dropped its table, `019`/`024` undid their own data migration. `229_maycluster_repair_goose_down_side_effects.sql` repairs databases that were migrated by that runner; `go test -tags=unit ./migrations/` walks every embedded file and fails if a goose-marked file has no executable Up section or a Down statement leaks into the executable SQL.

## Important Rules

### ⚠️ Immutability Principle

**Once a migration is applied to ANY environment (dev, staging, production), it MUST NOT be modified.**

Why?
- Each migration has a SHA256 checksum stored in the `schema_migrations` table
- Modifying an applied migration causes checksum mismatch errors
- Different environments would have inconsistent database states
- Breaks audit trail and reproducibility

### ✅ Correct Workflow

1. **Create new migration**
   ```bash
   # Create new file with next sequential number
   touch migrations/018_your_change.sql
   ```

2. **Write forward-only migration SQL**
   - Put only the intended schema change in the file
   - If rollback is needed, create a new migration file to revert

3. **Test locally**
   ```bash
   cd backend
   # Structure guards: goose sections
   go test -tags=unit ./migrations/
   # Real database (Docker): apply every migration, then assert every table the
   # schema declares or the code references exists
   go test -tags=integration ./internal/repository/ -run 'TestMigrations|TestRepairMigration' -count=1
   ```

4. **Commit and deploy**
   ```bash
   git add migrations/018_your_change.sql
   git commit -m "feat(db): add your change"
   ```

### ❌ What NOT to Do

- ❌ Modify an already-applied migration file
- ❌ Delete migration files
- ❌ Change migration file names
- ❌ Reorder migration numbers

### 🔧 If You Accidentally Modified an Applied Migration

**Error message:**
```
migration 017_add_gemini_tier_id.sql checksum mismatch (db=abc123... file=def456...)
```

**Solution:**
```bash
# 1. Find the original version
git log --oneline -- migrations/017_add_gemini_tier_id.sql

# 2. Revert to the commit when it was first applied
git checkout <commit-hash> -- migrations/017_add_gemini_tier_id.sql

# 3. Create a NEW migration for your changes
touch migrations/018_your_new_change.sql
```

## Migration System Details

- **Checksum Algorithm**: SHA256 of the trimmed **whole** file (including any Down section)
- **Executed SQL**: the whole file, or only the `-- +goose Up` section when goose markers are present (`migrations.ExecutableSQL`)
- **Tracking Table**: `schema_migrations` (filename, checksum, applied_at)
- **Runner**: `internal/repository/migrations_runner.go`
- **Auto-run**: Migrations run automatically on every service startup

## Best Practices

1. **Keep migrations small and focused**
   - One logical change per migration
   - Easier to review and rollback

2. **Keep migrations forward-only**
   - A rollback is a new forward migration; Down sections are never executed

3. **Use transactions**
   - Wrap DDL statements in transactions when possible
   - Ensures atomicity

4. **Add comments**
   - Explain WHY the change is needed
   - Document any special considerations

5. **Test in development first**
   - Apply migration locally
   - Verify data integrity
   - Re-apply the whole set: every migration must be idempotent

## Example Migration

```sql
-- Add tier_id field to Gemini OAuth accounts for quota tracking
UPDATE accounts
SET credentials = jsonb_set(
    credentials,
    '{tier_id}',
    '"LEGACY"',
    true
)
WHERE platform = 'gemini'
  AND type = 'oauth'
  AND credentials->>'tier_id' IS NULL;
```

## Troubleshooting

### Checksum Mismatch
See "If You Accidentally Modified an Applied Migration" above.

### Migration Failed
```bash
# Check migration status
psql -d sub2api -c "SELECT * FROM schema_migrations ORDER BY applied_at DESC;"

# Manually rollback if needed (use with caution)
# Better to fix the migration and create a new one
```

### Need to Skip a Migration (Emergency Only)
```sql
-- DANGEROUS: Only use in development or with extreme caution
INSERT INTO schema_migrations (filename, checksum, applied_at)
VALUES ('NNN_migration.sql', 'calculated_checksum', NOW());
```

## References

- Migration runner: `internal/repository/migrations_runner.go`
- PostgreSQL docs: https://www.postgresql.org/docs/
