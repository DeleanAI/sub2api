package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/channelmonitor"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/ent/setting"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// EncryptedStore 描述一处用落库密文密钥持久化密文的位置。
//
// 规则：凡是把 SecretEncryptor.Encrypt 的输出写进数据库的地方，都必须在
// EncryptedStores 里有一条注册；轮换命令遍历这张表，守护测试
// （encrypted_stores_guard_test.go）把源码里每个 Encrypt 调用点对回这张表。
// 两者消费同一个声明，新增一处密文存储而忘了登记会直接让测试失败。
type EncryptedStore struct {
	// Name 是报告里的显示名：table.column[.json_path]。
	Name string
	// SourceFiles 是把密文写到这里的源文件，相对 backend/。带行号时形如
	// "path/file.go:123"，用于同一个文件里既有落库密文、又有不落库密文的情况
	// （如插件管理器：配置要轮换，UI 会话令牌不落库）。
	SourceFiles []string

	table    string
	idColumn string
	column   string
	idKind   idKind
	// where 限定候选行（每次调用返回新的谓词，ent 的谓词对象不可复用）。
	where func() *entsql.Predicate
	// jsonPath 非空表示列值是 JSON 文档，密文是该路径下的字符串；段 "[]" 表示
	// 数组的每个元素；路径上缺失的键视为"这一行没有密文"。
	jsonPath []string
	// rewrite 覆盖默认的"对每个密文执行 ring.Rotate"逻辑。只有支付配置用它：
	// 那里的旧密文格式属于 payment 包，且目标形态是明文 JSON 而不是新密文。
	rewrite func(value string, ring *secretcipher.Ring) (string, bool, error)
}

type idKind int

const (
	idInt idKind = iota
	idString
)

// Table/IDColumn/Column/IDIsString 暴露声明里的物理位置。
//
// 大多数 store 的表由 ent schema 建出来，但 plugins 这类只存在于 SQL 迁移里的表
// 没有 ent schema。基于 ent schema 搭起来的测试库因此缺表，而缺表只会在轮换真的
// 跑到那一条 store 时炸开。测试用这些访问器遍历声明补齐缺的表——新增一处裸表
// store 当天就被覆盖，不需要再往测试里手写一次表名。
func (s EncryptedStore) Table() string    { return s.table }
func (s EncryptedStore) IDColumn() string { return s.idColumn }
func (s EncryptedStore) Column() string   { return s.column }
func (s EncryptedStore) IDIsString() bool { return s.idKind == idString }

// EncryptedStores 是全部落库密文位置的唯一声明。
var EncryptedStores = []EncryptedStore{
	{
		Name:        "users.totp_secret_encrypted",
		SourceFiles: []string{"internal/service/totp_service.go"},
		table:       user.Table, idColumn: user.FieldID, column: user.FieldTotpSecretEncrypted, idKind: idInt,
		where: func() *entsql.Predicate {
			return entsql.And(entsql.NotNull(user.FieldTotpSecretEncrypted), entsql.NEQ(user.FieldTotpSecretEncrypted, ""))
		},
	},
	{
		Name:        "channel_monitors.api_key_encrypted",
		SourceFiles: []string{"internal/service/channel_monitor_service.go"},
		table:       channelmonitor.Table, idColumn: channelmonitor.FieldID, column: channelmonitor.FieldAPIKeyEncrypted, idKind: idInt,
		where: func() *entsql.Predicate { return entsql.NEQ(channelmonitor.FieldAPIKeyEncrypted, "") },
	},
	settingStore(service.SettingKeyBackupS3Config, "internal/service/backup_service.go",
		jsonKey(service.BackupS3Config{}, "SecretAccessKey")),
	settingStore(service.SettingKeyImageStorageConfig, "internal/service/image_storage_settings.go",
		jsonKey(service.ImageStorageSettings{}, "SecretAccessKey")),
	settingStore(securityaudit.SettingKeyPromptAuditConfig, "internal/securityaudit/prompt_config_store.go",
		jsonKey(securityaudit.DefaultStorageConfig(), "Endpoints"), "[]", jsonKey(securityaudit.StorageEndpoint{}, "TokenCiphertext")),
	{
		Name:        "accounts.extra." + service.OllamaCloudUsageSessionExtraKey,
		SourceFiles: []string{"internal/service/ollama_cloud_usage.go"},
		table:       account.Table, idColumn: account.FieldID, column: account.FieldExtra, idKind: idInt,
		where: func() *entsql.Predicate {
			return sqljson.HasKey(account.FieldExtra, sqljson.Path(service.OllamaCloudUsageSessionExtraKey))
		},
		jsonPath: []string{service.OllamaCloudUsageSessionExtraKey},
	},
	{
		// 上游 0.2.x 的插件配置：明文 JSON 用同一把密钥加密后写进 plugins.config_encrypted
		// （internal/repository/plugin_repo.go:211）。plugins 表没有 ent schema，这里直接用表名。
		// 漏登记的后果与 issue #4 完全一致：轮换后插件配置解不开，而插件加载时才会发现。
		Name:        "plugins.config_encrypted",
		SourceFiles: []string{"internal/service/plugin_manager.go:743"},
		table:       "plugins", idColumn: "id", column: "config_encrypted", idKind: idInt,
		where: func() *entsql.Predicate { return entsql.NEQ("config_encrypted", "") },
	},
	{
		// 支付渠道配置已改为明文 JSON 落库，应用里没有写入密文的调用点；这一项
		// 只负责把升级前遗留的 iv:tag:ct 密文（用当时的密钥加密）解出来改成明文，
		// 让旧密钥退役后它们仍然可读。
		Name:  "payment_provider_instances.config (legacy ciphertext -> plaintext JSON)",
		table: paymentproviderinstance.Table, idColumn: paymentproviderinstance.FieldID, column: paymentproviderinstance.FieldConfig, idKind: idInt,
		where:   func() *entsql.Predicate { return entsql.NEQ(paymentproviderinstance.FieldConfig, "") },
		rewrite: rewriteLegacyPaymentConfig,
	},
}

// NonRotatableEncryptSites 列出调用了 Encrypt 但其输出不落库、或不在这把密钥环上
// 的源文件，以及原因。守护测试要求每个 Encrypt 调用点要么属于某个 EncryptedStore，
// 要么在这里给出理由——没有第三种"默默不管"的状态。
var NonRotatableEncryptSites = map[string]string{
	"internal/securityaudit/prompt_service.go":    "删除确认 token：5 分钟有效，只在请求之间往返，从不落库",
	"internal/service/plugin_manager.go:836":      "插件 UI 会话令牌：带用途前缀、TTL 内有效、只在请求之间往返，不落库",
	"internal/service/openai_live_attestation.go": "Live attestation 用 JWT secret 派生的独立密钥加密，不在落库密文密钥环上",
}

func settingStore(key, sourceFile string, jsonPath ...string) EncryptedStore {
	return EncryptedStore{
		Name:        setting.Table + "[" + key + "]." + strings.Join(jsonPath, "."),
		SourceFiles: []string{sourceFile},
		table:       setting.Table, idColumn: setting.FieldKey, column: setting.FieldValue, idKind: idString,
		where:    func() *entsql.Predicate { return entsql.EQ(setting.FieldKey, key) },
		jsonPath: jsonPath,
	}
}

// jsonKey 从结构体的 json tag 取字段名，让注册表跟着写入方的结构体走，
// 而不是在这里再抄一遍字段名。字段不存在是编码错误，直接 panic。
func jsonKey(v any, field string) string {
	t := reflect.TypeOf(v)
	f, ok := t.FieldByName(field)
	if !ok {
		panic(fmt.Sprintf("encrypted store registry: %s has no field %s", t, field))
	}
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		panic(fmt.Sprintf("encrypted store registry: %s.%s has no json name", t, field))
	}
	return name
}

func rewriteLegacyPaymentConfig(stored string, ring *secretcipher.Ring) (string, bool, error) {
	cfg, legacy, err := payment.DecodeProviderConfig(stored, ring.Keys())
	if err != nil {
		return "", false, err
	}
	if !legacy {
		return stored, false, nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", false, fmt.Errorf("marshal provider config: %w", err)
	}
	return string(data), true, nil
}

// rewriteValue 对一行的列值执行改写，返回新值、是否有变化、是否见到了密文。
func (s *EncryptedStore) rewriteValue(value string, ring *secretcipher.Ring) (out string, changed bool, sawCiphertext bool, err error) {
	if value == "" {
		return value, false, false, nil
	}
	if s.rewrite != nil {
		out, changed, err = s.rewrite(value, ring)
		return out, changed, true, err
	}
	if len(s.jsonPath) == 0 {
		out, changed, err = ring.Rotate(value)
		return out, changed, true, err
	}
	raw, changed, sawCiphertext, err := rewriteJSONStrings([]byte(value), s.jsonPath, ring.Rotate)
	if err != nil {
		return "", false, sawCiphertext, err
	}
	if !changed {
		return value, false, sawCiphertext, nil
	}
	return string(raw), true, sawCiphertext, nil
}

// rewriteJSONStrings 只改写 path 指向的字符串值；其余节点以 json.RawMessage 原样
// 保留，整数精度与未知字段都不会在往返中丢失。空字符串视为"没有密文"。
func rewriteJSONStrings(raw []byte, path []string, fn func(string) (string, bool, error)) (out []byte, changed bool, sawCiphertext bool, err error) {
	if len(path) == 0 {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, false, fmt.Errorf("expected a JSON string, got %s", truncateJSON(raw))
		}
		if s == "" {
			return raw, false, false, nil
		}
		rotated, changed, err := fn(s)
		if err != nil {
			return nil, false, true, err
		}
		if !changed {
			return raw, false, true, nil
		}
		encoded, err := json.Marshal(rotated)
		if err != nil {
			return nil, false, true, err
		}
		return encoded, true, true, nil
	}
	if isJSONNull(raw) {
		return raw, false, false, nil
	}
	segment, rest := path[0], path[1:]
	if segment == "[]" {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false, false, fmt.Errorf("expected a JSON array, got %s", truncateJSON(raw))
		}
		for i := range items {
			item, itemChanged, itemSaw, err := rewriteJSONStrings(items[i], rest, fn)
			if err != nil {
				return nil, false, sawCiphertext || itemSaw, fmt.Errorf("[%d]: %w", i, err)
			}
			sawCiphertext = sawCiphertext || itemSaw
			if itemChanged {
				items[i] = item
				changed = true
			}
		}
		if !changed {
			return raw, false, sawCiphertext, nil
		}
		out, err = json.Marshal(items)
		return out, true, sawCiphertext, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false, false, fmt.Errorf("expected a JSON object with key %q, got %s", segment, truncateJSON(raw))
	}
	child, ok := object[segment]
	if !ok || isJSONNull(child) {
		return raw, false, false, nil
	}
	child, changed, sawCiphertext, err = rewriteJSONStrings(child, rest, fn)
	if err != nil {
		return nil, false, sawCiphertext, fmt.Errorf("%s: %w", segment, err)
	}
	if !changed {
		return raw, false, sawCiphertext, nil
	}
	object[segment] = child
	out, err = json.Marshal(object)
	return out, true, sawCiphertext, err
}

func isJSONNull(raw []byte) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func truncateJSON(raw []byte) string {
	const limit = 40
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// RotateOptions 控制一次轮换。
type RotateOptions struct {
	// DryRun 只统计、不写库、不更新指纹。
	DryRun bool
}

// RowFailure 记录一行无法改写的原因。
type RowFailure struct {
	Row string
	Err string
}

// StoreReport 是一处密文存储的轮换结果。
type StoreReport struct {
	Store string
	// Rotated 已重写（dry-run 时表示将会重写）；Current 已是主密钥新格式；
	// Empty 候选行里没有密文；Failed 解不开或写不回。
	Rotated, Current, Empty, Failed int
	Failures                        []RowFailure
}

// RotationReport 是整次轮换的结果，供命令行打印与测试断言。
type RotationReport struct {
	DryRun         bool
	PrimaryKeyID   string
	PreviousKeyIDs []string
	// StoredFingerprintKeyID 是库里记录的主密钥指纹对应的 key-id；没有记录为空。
	StoredFingerprintKeyID string
	// StoredFingerprintKnown 表示记录的指纹属于当前密钥环（主密钥或历史密钥）。
	StoredFingerprintKnown bool
	Dialect                string
	RowLocks               bool
	Stores                 []StoreReport
	FingerprintUpdated     bool
}

// Totals 汇总各存储的计数。
func (r *RotationReport) Totals() (rotated, current, empty, failed int) {
	for _, s := range r.Stores {
		rotated += s.Rotated
		current += s.Current
		empty += s.Empty
		failed += s.Failed
	}
	return rotated, current, empty, failed
}

// Failed 报告是否有任何一行失败。
func (r *RotationReport) Failed() bool {
	_, _, _, failed := r.Totals()
	return failed > 0
}

// Write 以固定格式打印报告。
func (r *RotationReport) Write(w io.Writer) {
	mode := "execute"
	if r.DryRun {
		mode = "dry-run"
	}
	stored := "none"
	if r.StoredFingerprintKeyID != "" {
		stored = r.StoredFingerprintKeyID
		if !r.StoredFingerprintKnown {
			stored += " (not in key ring!)"
		}
	}
	fmt.Fprintf(w, "encryption-key rotate: mode=%s primary=%s previous=[%s] stored_fingerprint=%s dialect=%s row_locks=%t\n",
		mode, r.PrimaryKeyID, strings.Join(r.PreviousKeyIDs, ","), stored, r.Dialect, r.RowLocks)
	fmt.Fprintf(w, "%-72s %8s %8s %8s %8s\n", "store", "rotated", "current", "empty", "failed")
	for _, s := range r.Stores {
		fmt.Fprintf(w, "%-72s %8d %8d %8d %8d\n", s.Store, s.Rotated, s.Current, s.Empty, s.Failed)
	}
	rotated, current, empty, failed := r.Totals()
	fmt.Fprintf(w, "%-72s %8d %8d %8d %8d\n", "total", rotated, current, empty, failed)
	for _, s := range r.Stores {
		for _, f := range s.Failures {
			fmt.Fprintf(w, "failed: %s: %s\n", f.Row, f.Err)
		}
	}
	switch {
	case r.DryRun:
		fmt.Fprintln(w, "dry-run: nothing was written; the stored fingerprint is unchanged")
	case r.FingerprintUpdated:
		fmt.Fprintf(w, "stored fingerprint updated to primary key %s; TOTP_ENCRYPTION_KEY_PREVIOUS can now be removed\n", r.PrimaryKeyID)
	default:
		fmt.Fprintln(w, "stored fingerprint NOT updated: fix or reset the failed rows, then re-run (already-rotated rows are skipped)")
	}
}

// RotateEncryptionKey 把每一处落库密文重写到密钥环主密钥的新格式下。
//
// 每一行一个事务：PostgreSQL 上先 SELECT ... FOR UPDATE 锁行，再读、改、写，
// 运行中的服务对同一行的写入会排在事务之后，不会被旧值覆盖。已经是主密钥
// 新格式的行按 key-id 跳过，因此可以反复执行。解不开的行记为失败并继续，
// 只有全部行成功（且不是 dry-run）才把库里的密钥指纹更新为主密钥。
func RotateEncryptionKey(ctx context.Context, client *dbent.Client, ring *secretcipher.Ring, opts RotateOptions) (*RotationReport, error) {
	if client == nil {
		return nil, errors.New("nil ent client")
	}
	if ring == nil {
		return nil, errors.New("nil key ring")
	}
	d, err := detectDialect(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("detect database dialect: %w", err)
	}
	report := &RotationReport{
		DryRun:         opts.DryRun,
		PrimaryKeyID:   ring.PrimaryKeyID(),
		PreviousKeyIDs: ring.PreviousKeyIDs(),
		Dialect:        d,
		RowLocks:       d == dialect.Postgres,
	}
	if !report.RowLocks {
		slog.Warn("encryption key rotation: SELECT FOR UPDATE is unavailable on this dialect; concurrent writes to a row being rotated are not serialized",
			"dialect", d)
	}
	if stored, found, err := ReadEncryptionKeyFingerprint(ctx, client); err != nil {
		return nil, fmt.Errorf("read stored fingerprint: %w", err)
	} else if found {
		report.StoredFingerprintKeyID = secretcipher.KeyIDOfFingerprint(stored)
		_, _, report.StoredFingerprintKnown = ring.KeyIDByFingerprint(stored)
	}

	for i := range EncryptedStores {
		store := &EncryptedStores[i]
		result, err := store.rotateAll(ctx, client, d, report.RowLocks, ring, opts.DryRun)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", store.Name, err)
		}
		report.Stores = append(report.Stores, result)
	}

	rotated, current, empty, failed := report.Totals()
	logAttrs := []any{"dry_run", opts.DryRun, "rotated", rotated, "current", current, "empty", empty, "failed", failed, "primary_key_id", ring.PrimaryKeyID()}
	switch {
	case opts.DryRun:
		slog.Info("encryption key rotation dry-run finished", logAttrs...)
	case failed > 0:
		slog.Error("encryption key rotation finished with failures; stored fingerprint left unchanged so startup keeps requiring the old key", logAttrs...)
	default:
		if err := StoreEncryptionKeyFingerprint(ctx, client, ring.PrimaryFingerprint()); err != nil {
			return report, fmt.Errorf("update stored fingerprint: %w", err)
		}
		report.FingerprintUpdated = true
		slog.Info("encryption key rotation finished; stored fingerprint now points at the primary key", logAttrs...)
	}
	return report, nil
}

// detectDialect 通过一次空查询读出驱动方言：ent 客户端不暴露驱动，而
// FOR UPDATE 与 JSON 谓词的写法都依赖方言。
func detectDialect(ctx context.Context, client *dbent.Client) (string, error) {
	var d string
	_, err := client.SecuritySecret.Query().Where(func(s *entsql.Selector) { d = s.Dialect() }).Limit(1).Exist(ctx)
	if err != nil {
		return "", err
	}
	if d == "" {
		return "", errors.New("dialect not reported by query builder")
	}
	return d, nil
}

func (s *EncryptedStore) rotateAll(ctx context.Context, client *dbent.Client, d string, lock bool, ring *secretcipher.Ring, dryRun bool) (StoreReport, error) {
	result := StoreReport{Store: s.Name}
	ids, err := s.listIDs(ctx, client, d)
	if err != nil {
		return result, fmt.Errorf("list rows: %w", err)
	}
	for _, id := range ids {
		row := fmt.Sprintf("%s[%v]", s.table, id)
		outcome, err := s.rotateRow(ctx, client, d, lock, ring, dryRun, id)
		if err != nil {
			result.Failed++
			result.Failures = append(result.Failures, RowFailure{Row: row, Err: err.Error()})
			slog.Error("encryption key rotation: row failed", "store", s.Name, "row", row, "error", err)
			continue
		}
		switch outcome {
		case outcomeRotated:
			result.Rotated++
		case outcomeCurrent:
			result.Current++
		case outcomeEmpty:
			result.Empty++
		}
	}
	return result, nil
}

type rowOutcome int

const (
	outcomeRotated rowOutcome = iota
	outcomeCurrent
	outcomeEmpty
)

func (s *EncryptedStore) listIDs(ctx context.Context, client *dbent.Client, d string) ([]any, error) {
	builder := entsql.Dialect(d)
	sel := builder.Select(s.idColumn).From(builder.Table(s.table)).OrderBy(s.idColumn)
	if s.where != nil {
		sel.Where(s.where())
	}
	query, args := sel.Query()
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []any
	for rows.Next() {
		switch s.idKind {
		case idString:
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		default:
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (s *EncryptedStore) rotateRow(ctx context.Context, client *dbent.Client, d string, lock bool, ring *secretcipher.Ring, dryRun bool, id any) (rowOutcome, error) {
	tx, err := client.Tx(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	txClient := tx.Client()
	builder := entsql.Dialect(d)

	sel := builder.Select(s.column).From(builder.Table(s.table)).Where(entsql.EQ(s.idColumn, id))
	if lock {
		sel.ForUpdate()
	}
	query, args := sel.Query()
	value, found, err := queryNullString(ctx, txClient, query, args...)
	if err != nil {
		return 0, fmt.Errorf("read row: %w", err)
	}
	if !found {
		// 列表之后、锁定之前被删掉了：这一行已不存在，没有密文可轮换。
		slog.Warn("encryption key rotation: row disappeared before it could be locked", "store", s.Name, "id", id)
		return outcomeEmpty, nil
	}

	out, changed, sawCiphertext, err := s.rewriteValue(value, ring)
	if err != nil {
		return 0, err
	}
	if !changed {
		if !sawCiphertext {
			return outcomeEmpty, nil
		}
		return outcomeCurrent, nil
	}
	if dryRun {
		return outcomeRotated, nil
	}
	upd := builder.Update(s.table).Set(s.column, out).Where(entsql.EQ(s.idColumn, id))
	query, args = upd.Query()
	res, err := txClient.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("write row: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected != 1 {
		return 0, fmt.Errorf("write row: expected 1 row affected, got %d", affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return outcomeRotated, nil
}

// queryNullString 读单行单列文本；jsonb 列由驱动以字节返回，NullString 能接住。
func queryNullString(ctx context.Context, client *dbent.Client, query string, args ...any) (string, bool, error) {
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var value sql.NullString
	if err := rows.Scan(&value); err != nil {
		return "", false, err
	}
	return value.String, true, rows.Err()
}
