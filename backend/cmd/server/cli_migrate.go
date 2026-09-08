package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/migrations"

	_ "github.com/lib/pq"
)

// `sub2api migrate plan` 的退出码是对外契约：升级脚本只看退出码就能决定是滚动更新
// 还是先缩到零副本。迁移在每次启动时自动执行（internal/repository/ent.go），而且只进不退，
// 所以必须在新镜像启动之前用它先看一眼。
const (
	// exitMigratePlanUpToDate：没有待执行迁移。
	exitMigratePlanUpToDate = 0
	// exitMigratePlanRollingSafe：有待执行迁移，但全部是 additive / data-rewrite，旧副本不受影响。
	exitMigratePlanRollingSafe = 10
	// exitMigratePlanDestructive：至少一个待执行迁移会删除/重命名/改类型/清空旧副本仍在用的东西。
	exitMigratePlanDestructive = 20
)

const (
	verdictUpToDate    = "up-to-date"
	verdictRollingSafe = "rolling-safe"
	verdictDestructive = "destructive"
	verdictBlocked     = "blocked"
)

func init() {
	registerSubcommand(subcommand{
		name:    "migrate",
		summary: "数据库迁移工具；`migrate plan` 列出待执行迁移并判定滚动更新是否安全（退出码 0/10/20）",
		run:     runMigrate,
	})
}

// migrationPlanner 抽象出「连库并算计划」这一步，测试用假计划覆盖渲染与退出码。
type migrationPlanner func(ctx context.Context) (*repository.MigrationPlan, error)

func runMigrate(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		printMigrateUsage(stderr)
		return errors.New("migrate: missing subcommand")
	}
	switch args[0] {
	case "plan":
		return runMigratePlan(args[1:], stdout, stderr, planMigrationsFromConfig)
	default:
		printMigrateUsage(stderr)
		return fmt.Errorf("migrate: unknown subcommand %q", args[0])
	}
}

func printMigrateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: sub2api migrate <subcommand> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  plan    List migrations this binary would apply on start and classify them")
	fmt.Fprintln(w, "          for rolling-update safety. Exit codes: 0 nothing pending,")
	fmt.Fprintln(w, "          10 pending but rolling-safe, 20 destructive pending, 1 error/blocked.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'sub2api migrate plan -h' for flags.")
}

type migratePlanEntry struct {
	Filename string   `json:"filename"`
	Kind     string   `json:"kind"`
	Reasons  []string `json:"reasons"`
}

type migratePlanChecksumMismatch struct {
	Filename     string `json:"filename"`
	DBChecksum   string `json:"db_checksum"`
	FileChecksum string `json:"file_checksum"`
}

// migratePlanReport 是 --json 的输出形态，也是文本渲染的数据源，两者不会分叉。
type migratePlanReport struct {
	AppliedCount       int                           `json:"applied_count"`
	LastApplied        string                        `json:"last_applied"`
	Pending            []migratePlanEntry            `json:"pending"`
	UnknownApplied     []string                      `json:"unknown_applied"`
	ChecksumMismatches []migratePlanChecksumMismatch `json:"checksum_mismatches"`
	Verdict            string                        `json:"verdict"`
	ExitCode           int                           `json:"exit_code"`
}

func buildMigratePlanReport(plan *repository.MigrationPlan) migratePlanReport {
	report := migratePlanReport{
		AppliedCount:       plan.AppliedCount,
		LastApplied:        plan.LastApplied,
		Pending:            make([]migratePlanEntry, 0, len(plan.Pending)),
		UnknownApplied:     append([]string{}, plan.UnknownApplied...),
		ChecksumMismatches: make([]migratePlanChecksumMismatch, 0, len(plan.ChecksumMismatches)),
	}
	for _, pending := range plan.Pending {
		reasons := pending.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		report.Pending = append(report.Pending, migratePlanEntry{
			Filename: pending.Filename,
			Kind:     string(pending.Kind),
			Reasons:  reasons,
		})
	}
	for _, mismatch := range plan.ChecksumMismatches {
		report.ChecksumMismatches = append(report.ChecksumMismatches, migratePlanChecksumMismatch(mismatch))
	}

	switch {
	case len(plan.ChecksumMismatches) > 0:
		report.Verdict = verdictBlocked
		report.ExitCode = 1
	case len(plan.Pending) == 0:
		report.Verdict = verdictUpToDate
		report.ExitCode = exitMigratePlanUpToDate
	case plan.PendingKind() == migrations.KindDestructive:
		report.Verdict = verdictDestructive
		report.ExitCode = exitMigratePlanDestructive
	default:
		report.Verdict = verdictRollingSafe
		report.ExitCode = exitMigratePlanRollingSafe
	}
	return report
}

func runMigratePlan(args []string, stdout, stderr io.Writer, plan migrationPlanner) error {
	flags := flag.NewFlagSet("migrate plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print the plan as JSON instead of a table")
	quiet := flags.Bool("quiet", false, "print nothing; the exit code alone carries the verdict")
	timeout := flags.Duration("timeout", 30*time.Second, "database connect/query timeout")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: sub2api migrate plan [-json] [-quiet] [-timeout 30s]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Connects to the configured database (same config as the server), lists the")
		fmt.Fprintln(stderr, "embedded migrations not yet recorded in schema_migrations and classifies each")
		fmt.Fprintln(stderr, "one by its Up section: additive, data-rewrite or destructive.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Exit codes:")
		fmt.Fprintf(stderr, "  %2d  nothing pending\n", exitMigratePlanUpToDate)
		fmt.Fprintf(stderr, "  %2d  pending, all additive/data-rewrite: rolling update is safe\n", exitMigratePlanRollingSafe)
		fmt.Fprintf(stderr, "  %2d  destructive migration pending: scale to zero and back up first\n", exitMigratePlanDestructive)
		fmt.Fprintln(stderr, "   1  error, or an applied migration no longer matches its recorded checksum")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("migrate plan: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := plan(ctx)
	if err != nil {
		return err
	}
	report := buildMigratePlanReport(result)

	if !*quiet {
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return fmt.Errorf("encode plan: %w", err)
			}
		} else {
			renderMigratePlanText(stdout, report)
		}
	}

	if report.Verdict == verdictBlocked {
		return fmt.Errorf("migrate plan: %d applied migration(s) no longer match their recorded checksum; this image would refuse to start against this database", len(report.ChecksumMismatches))
	}
	if report.ExitCode == exitMigratePlanUpToDate {
		return nil
	}
	// 判定已经打印在报告里（或被 --quiet 压掉），退出码本身就是结论，不再重复输出。
	return &exitCodeError{code: report.ExitCode}
}

func renderMigratePlanText(w io.Writer, report migratePlanReport) {
	if report.LastApplied == "" {
		fmt.Fprintf(w, "applied: %d (schema_migrations is empty or missing: fresh database)\n", report.AppliedCount)
	} else {
		fmt.Fprintf(w, "applied: %d (last: %s)\n", report.AppliedCount, report.LastApplied)
	}
	if len(report.UnknownApplied) > 0 {
		fmt.Fprintf(w, "warning: %d applied migration(s) are not embedded in this binary (database is ahead of this image): %s\n",
			len(report.UnknownApplied), strings.Join(report.UnknownApplied, ", "))
	}
	if len(report.ChecksumMismatches) > 0 {
		fmt.Fprintf(w, "checksum mismatches: %d\n", len(report.ChecksumMismatches))
		for _, mismatch := range report.ChecksumMismatches {
			fmt.Fprintf(w, "  %s  db=%s file=%s\n", mismatch.Filename, mismatch.DBChecksum, mismatch.FileChecksum)
		}
	}
	fmt.Fprintf(w, "pending: %d\n", len(report.Pending))
	if len(report.Pending) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "FILE\tKIND\tREASONS")
		for _, pending := range report.Pending {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", pending.Filename, pending.Kind, strings.Join(pending.Reasons, "; "))
		}
		_ = tw.Flush()
	}
	fmt.Fprintf(w, "verdict: %s (exit code %d)\n", describeVerdict(report.Verdict), report.ExitCode)
}

func describeVerdict(verdict string) string {
	switch verdict {
	case verdictUpToDate:
		return "up-to-date, nothing to apply"
	case verdictRollingSafe:
		return "rolling-safe, every pending migration is additive or data-rewrite"
	case verdictDestructive:
		return "destructive, scale to zero replicas and take a backup before upgrading"
	case verdictBlocked:
		return "blocked, this image will refuse to start against this database"
	default:
		return verdict
	}
}

// planMigrationsFromConfig 用与服务完全相同的配置来源和驱动连库，只读，不执行迁移。
func planMigrationsFromConfig(ctx context.Context) (*repository.MigrationPlan, error) {
	// LoadForSchemaTooling 而不是 LoadForBootstrap：plan 只读 schema_migrations 与
	// 内嵌迁移的校验和，不碰任何密文。用启动配置会让还没配 TOTP_ENCRYPTION_KEY 的
	// 存量实例连闸门都跑不了（退出 1 = 阻止升级），正好是最需要这道闸门的那批。
	cfg, err := config.LoadForSchemaTooling()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	db, err := sql.Open("postgres", cfg.Database.DSNWithTimezone(cfg.Timezone))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect to %s:%d/%s: %w", cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName, err)
	}
	return repository.PlanMigrations(ctx, db, migrations.FS)
}
