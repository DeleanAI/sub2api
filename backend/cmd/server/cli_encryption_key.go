package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
)

func init() {
	registerSubcommand(subcommand{
		name:    "encryption-key",
		summary: "落库密文密钥运维：`encryption-key rotate` 把存量密文重写到当前主密钥",
		run:     runEncryptionKeyCommand,
	})
}

// encryptionKeyDeps 把配置加载与数据库打开做成可注入的依赖：测试用内存库跑完整命令路径。
type encryptionKeyDeps struct {
	loadConfig func() (*config.Config, error)
	openDB     func(cfg *config.Config) (*ent.Client, error)
}

func defaultEncryptionKeyDeps() encryptionKeyDeps {
	return encryptionKeyDeps{
		loadConfig: config.LoadForBootstrap,
		openDB: func(cfg *config.Config) (*ent.Client, error) {
			client, _, err := repository.OpenEnt(cfg)
			return client, err
		},
	}
}

func runEncryptionKeyCommand(args []string, stdout, stderr io.Writer) error {
	return runEncryptionKeyCommandWith(args, stdout, stderr, defaultEncryptionKeyDeps())
}

func runEncryptionKeyCommandWith(args []string, stdout, stderr io.Writer, deps encryptionKeyDeps) error {
	if len(args) == 0 {
		printEncryptionKeyUsage(stderr)
		return errors.New("missing subcommand: expected `rotate`")
	}
	switch args[0] {
	case "rotate":
		return runEncryptionKeyRotate(args[1:], stdout, stderr, deps)
	case "-h", "--help", "help":
		printEncryptionKeyUsage(stdout)
		return nil
	default:
		printEncryptionKeyUsage(stderr)
		return fmt.Errorf("unknown encryption-key subcommand %q", args[0])
	}
}

func printEncryptionKeyUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: sub2api encryption-key rotate [--dry-run] [--old-key HEX] [--new-key HEX]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Re-encrypts every stored ciphertext (TOTP secrets, channel monitor API keys, backup/image")
	fmt.Fprintln(w, "storage S3 secrets, Ollama web sessions, prompt-audit endpoint tokens) under the primary")
	fmt.Fprintln(w, "key and converts legacy payment provider ciphertext to the current plaintext format.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Procedure:")
	fmt.Fprintln(w, "  1. Set the NEW key as TOTP_ENCRYPTION_KEY and the OLD key in TOTP_ENCRYPTION_KEY_PREVIOUS on every instance, restart them.")
	fmt.Fprintln(w, "  2. Run `sub2api encryption-key rotate --dry-run`, then without --dry-run. Already-rotated rows are skipped, so it can be re-run.")
	fmt.Fprintln(w, "  3. When it reports the stored fingerprint updated, remove TOTP_ENCRYPTION_KEY_PREVIOUS and restart.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "--old-key/--new-key override TOTP_ENCRYPTION_KEY_PREVIOUS/TOTP_ENCRYPTION_KEY for this run only.")
}

func runEncryptionKeyRotate(args []string, stdout, stderr io.Writer, deps encryptionKeyDeps) error {
	fs := flag.NewFlagSet("encryption-key rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing anything")
	oldKey := fs.String("old-key", "", "key being retired (64 hex chars); overrides TOTP_ENCRYPTION_KEY_PREVIOUS")
	newKey := fs.String("new-key", "", "new primary key (64 hex chars); overrides TOTP_ENCRYPTION_KEY")
	fs.Usage = func() {
		printEncryptionKeyUsage(stderr)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	old, fresh := strings.TrimSpace(*oldKey), strings.TrimSpace(*newKey)
	if old != "" && fresh != "" && strings.EqualFold(old, fresh) {
		return errors.New("refusing to rotate: --old-key and --new-key are the same key")
	}
	// 覆盖值通过环境变量交给配置加载：密钥格式、release 模式必填、新旧重复
	// 这些规则只在配置加载里定义一次，命令行不另起一套校验。
	if fresh != "" {
		if err := os.Setenv("TOTP_ENCRYPTION_KEY", fresh); err != nil {
			return err
		}
	}
	if old != "" {
		if err := os.Setenv("TOTP_ENCRYPTION_KEY_PREVIOUS", old); err != nil {
			return err
		}
	}

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Totp.EncryptionKeyConfigured {
		return errors.New("refusing to rotate onto an auto-generated key: set TOTP_ENCRYPTION_KEY (or --new-key) to the new primary key")
	}
	ring, err := cfg.Totp.KeyRing()
	if err != nil {
		return err
	}
	if len(ring.PreviousKeyIDs()) == 0 {
		return errors.New("no key to rotate from: put the key being retired in TOTP_ENCRYPTION_KEY_PREVIOUS (or pass --old-key)")
	}

	client, err := deps.openDB(cfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = client.Close() }()

	report, err := repository.RotateEncryptionKey(context.Background(), client, ring, repository.RotateOptions{DryRun: *dryRun})
	if report != nil {
		report.Write(stdout)
	}
	if err != nil {
		return err
	}
	if _, _, _, failed := report.Totals(); failed > 0 {
		return fmt.Errorf("%d row(s) could not be rotated; fix or reset them and re-run", failed)
	}
	return nil
}
