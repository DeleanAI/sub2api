package repository

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/migrations"
)

// PendingMigration 是一条尚未应用到数据库的迁移及其滚动更新判定。
type PendingMigration struct {
	Filename string
	Kind     migrations.Kind
	Reasons  []string
}

// MigrationChecksumMismatch 表示已应用迁移的文件内容与数据库记录不一致，且不在兼容白名单内。
// 新镜像会在启动时因此拒绝启动，所以它必须在滚动更新之前被看见。
type MigrationChecksumMismatch struct {
	Filename     string
	DBChecksum   string
	FileChecksum string
}

// MigrationPlan 是「当前二进制嵌入的迁移集」相对「数据库已应用集」的差异。
//
// 它只读，不取 advisory lock，不改数据库：用途是在滚动更新之前用新镜像回答
// 「启动时会跑哪些迁移，其中有没有会打断旧副本的」。
type MigrationPlan struct {
	// AppliedCount 是 schema_migrations 里的记录数（包含 UnknownApplied）。
	AppliedCount int
	// LastApplied 是已应用记录里文件名最大的那个；运行器按文件名顺序执行，所以它就是「进度」。
	LastApplied string
	// Pending 按执行顺序列出将被应用的迁移。
	Pending []PendingMigration
	// UnknownApplied 是数据库里有、当前二进制里没有的迁移：说明数据库比镜像新（正在回滚镜像）。
	UnknownApplied []string
	// ChecksumMismatches 非空时新镜像无法启动，滚动更新没有意义。
	ChecksumMismatches []MigrationChecksumMismatch
}

// PendingKind 是所有待执行迁移中最严重的等级；没有待执行迁移时返回空串。
func (p *MigrationPlan) PendingKind() migrations.Kind {
	var worst migrations.Kind
	for _, pending := range p.Pending {
		if worst == "" || kindSeverity(pending.Kind) > kindSeverity(worst) {
			worst = pending.Kind
		}
	}
	return worst
}

func kindSeverity(kind migrations.Kind) int {
	switch kind {
	case migrations.KindDestructive:
		return 2
	case migrations.KindDataRewrite:
		return 1
	default:
		return 0
	}
}

// PlanMigrations 计算 fsys 中的迁移相对数据库的差异，判定逻辑与运行器共用：
// 同一套文件遍历与空文件跳过规则、同一个 checksum 算法与兼容白名单、同一个 Up 段提取。
func PlanMigrations(ctx context.Context, db migrationQuerier, fsys fs.FS) (*MigrationPlan, error) {
	if db == nil {
		return nil, errors.New("nil sql db")
	}

	applied := map[string]string{}
	hasTable, err := tableExists(ctx, db, "schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("check schema_migrations: %w", err)
	}
	if hasTable {
		rows, err := db.QueryContext(ctx, "SELECT filename, checksum FROM schema_migrations")
		if err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var filename, checksum string
			if err := rows.Scan(&filename, &checksum); err != nil {
				return nil, fmt.Errorf("scan schema_migrations: %w", err)
			}
			applied[filename] = checksum
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate schema_migrations: %w", err)
		}
	}

	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(files)

	plan := &MigrationPlan{AppliedCount: len(applied)}
	embedded := make(map[string]bool, len(files))
	for _, name := range files {
		contentBytes, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		content := strings.TrimSpace(string(contentBytes))
		if content == "" {
			continue // 运行器同样跳过空文件
		}
		embedded[name] = true

		if dbChecksum, ok := applied[name]; ok {
			fileChecksum := migrationChecksum(content)
			if dbChecksum != fileChecksum && !isMigrationChecksumCompatible(name, dbChecksum, fileChecksum) {
				plan.ChecksumMismatches = append(plan.ChecksumMismatches, MigrationChecksumMismatch{
					Filename:     name,
					DBChecksum:   dbChecksum,
					FileChecksum: fileChecksum,
				})
			}
			continue
		}

		kind, reasons, err := migrations.Classify(content)
		if err != nil {
			// 运行器会因为同样的原因拒绝这个文件，计划阶段就把它暴露出来。
			return nil, fmt.Errorf("classify migration %s: %w", name, err)
		}
		plan.Pending = append(plan.Pending, PendingMigration{Filename: name, Kind: kind, Reasons: reasons})
	}

	for name := range applied {
		if name > plan.LastApplied {
			plan.LastApplied = name
		}
		if !embedded[name] {
			plan.UnknownApplied = append(plan.UnknownApplied, name)
		}
	}
	sort.Strings(plan.UnknownApplied)
	return plan, nil
}
