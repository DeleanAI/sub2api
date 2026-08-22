package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// subcommand 是挂在服务二进制上的运维子命令。
//
// 为什么挂在服务二进制而不是 cmd/<tool> 独立程序：容器镜像只发布 /app/sub2api，
// 线上能执行的只有它。独立工具在镜像里不存在，等于线上用不了。
//
// 注册方式：各 cli_*.go 在 init() 里调用 registerSubcommand。名字冲突直接 panic，
// 这是编码期错误，不该等到运行期才发现。
type subcommand struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) error
}

var subcommands = map[string]subcommand{}

// exitCodeError 让子命令把判定结果编码成进程退出码，供脚本与 CI 门禁直接消费。
// message 非空时由 main 打印到 stderr；为空则静默退出（例如 --quiet）。
type exitCodeError struct {
	code    int
	message string
}

func (e *exitCodeError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.message
}

func registerSubcommand(cmd subcommand) {
	name := strings.TrimSpace(cmd.name)
	if name == "" || cmd.run == nil {
		panic("registerSubcommand: name and run are required")
	}
	if strings.HasPrefix(name, "-") {
		panic("registerSubcommand: subcommand names must not look like flags: " + name)
	}
	if _, dup := subcommands[name]; dup {
		panic("registerSubcommand: duplicate subcommand " + name)
	}
	subcommands[name] = cmd
}

func subcommandNames() []string {
	names := make([]string, 0, len(subcommands))
	for name := range subcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// runSubcommand 在 flag 解析之前分发子命令。
//
// 返回 handled=false 表示 args[0] 不是已注册的子命令（或以 '-' 开头，是旧式 flag），
// 调用方应继续走原有的 flag 路径，保持 `sub2api -setup` / `sub2api -version` 兼容。
func runSubcommand(args []string, stdout, stderr io.Writer) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	name := args[0]
	if strings.HasPrefix(name, "-") {
		return false, nil
	}
	if name == "help" {
		printSubcommandHelp(stdout)
		return true, nil
	}
	cmd, ok := subcommands[name]
	if !ok {
		return false, nil
	}
	return true, cmd.run(args[1:], stdout, stderr)
}

func printSubcommandHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage: sub2api [flags] | sub2api <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags (legacy):")
	fmt.Fprintln(w, "  -setup     Run setup wizard in CLI mode")
	fmt.Fprintln(w, "  -version   Show version information")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, name := range subcommandNames() {
		fmt.Fprintf(w, "  %-28s %s\n", name, subcommands[name].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'sub2api <command> -h' for command-specific flags.")
}
