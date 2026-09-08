package main

import (
	"bufio"
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
	fmt.Fprintln(w, "Usage: sub2api encryption-key rotate [--dry-run] [--old-key-file PATH] [--new-key-file PATH]")
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
	fmt.Fprintln(w, "--old-key-file/--new-key-file read a key from a file (\"-\" reads one line from stdin) and override")
	fmt.Fprintln(w, "TOTP_ENCRYPTION_KEY_PREVIOUS/TOTP_ENCRYPTION_KEY for this run only. Keys are deliberately not")
	fmt.Fprintln(w, "accepted as flag values: argv is world-readable via /proc/<pid>/cmdline and lands in shell history.")
}

func runEncryptionKeyRotate(args []string, stdout, stderr io.Writer, deps encryptionKeyDeps) error {
	fs := flag.NewFlagSet("encryption-key rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing anything")
	// 密钥只从文件/stdin 读，不接受 flag 值：argv 在 /proc/<pid>/cmdline 里对同机
	// 任何用户可见，也会原样进入 shell 历史。这是密钥轮换命令，泄露的正是它要保护的东西。
	oldKeyFile := fs.String("old-key-file", "", `file holding the key being retired ("-" = stdin); overrides TOTP_ENCRYPTION_KEY_PREVIOUS`)
	newKeyFile := fs.String("new-key-file", "", `file holding the new primary key ("-" = stdin); overrides TOTP_ENCRYPTION_KEY`)
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
	old, err := readKeyFile(*oldKeyFile, stdout)
	if err != nil {
		return fmt.Errorf("--old-key-file: %w", err)
	}
	fresh, err := readKeyFile(*newKeyFile, stdout)
	if err != nil {
		return fmt.Errorf("--new-key-file: %w", err)
	}
	if old != "" && fresh != "" && strings.EqualFold(old, fresh) {
		return errors.New("refusing to rotate: --old-key-file and --new-key-file hold the same key")
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
		return errors.New("refusing to rotate onto an auto-generated key: set TOTP_ENCRYPTION_KEY (or --new-key-file) to the new primary key")
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

// readKeyFile 从文件（"-" 表示 stdin 的第一行）读出一把密钥。
//
// 只读第一行并去掉空白：密钥文件常见的写法是 `openssl rand -hex 32 > key`，
// 结尾会带换行。空路径表示没提供，交回空串由配置加载去决定。
func readKeyFile(path string, prompt io.Writer) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	var raw []byte
	var err error
	if path == "-" {
		fmt.Fprintln(prompt, "reading key from stdin...")
		reader := bufio.NewReader(os.Stdin)
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		raw = []byte(line)
	} else if raw, err = os.ReadFile(path); err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(raw))
	if idx := strings.IndexAny(key, "\r\n"); idx >= 0 {
		key = key[:idx]
	}
	if key == "" {
		return "", errors.New("file is empty")
	}
	return key, nil
}
