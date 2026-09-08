package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// 数据目录只有一条裁决规则，写在 ResolveDataDir 里；其它地方只能调用它，不许再各自读 DATA_DIR。
// 这条规则由 TestNothingElseReadsDataDirEnv 遍历源码执行——它曾经只是一句注释，
// 而实际有三处（配置文件搜索路径、日志路径、插件根目录）各自读了一遍，兜底还互不相同。
//
// 为什么需要 fallback 参数而不是一个固定默认值：裸机安装（deploy/install.sh）把二进制、
// config.yaml/.installed 放在 /opt/sub2api（工作目录），把运行期数据放在 /opt/sub2api/data。
// 这两个目录在容器里合并成同一个 /app/data，但在裸机上确实是两个目录，所以"没有 DATA_DIR
// 也没有 /app/data 时退到哪里"由调用方按用途声明：安装标记用 "."，运行期数据用 "./data"。
const (
	containerDataDir          = "/app/data"
	runtimeDataDirFallback    = "./data"
	staticOverrideSubdir      = "public"
	customPagesImportSubdir   = "pages"
	dataDirWritabilityProbeFn = ".write_test"
)

// ResolveDataDir 返回数据目录：DATA_DIR 环境变量 > 可写的 /app/data（容器环境）> fallback。
func ResolveDataDir(fallback string) string {
	if dir := strings.TrimSpace(os.Getenv("DATA_DIR")); dir != "" {
		return dir
	}
	if info, err := os.Stat(containerDataDir); err == nil && info.IsDir() {
		probe := filepath.Join(containerDataDir, dataDirWritabilityProbeFn)
		if f, err := os.Create(probe); err == nil {
			_ = f.Close()
			_ = os.Remove(probe)
			return containerDataDir
		}
	}
	return fallback
}

// DefaultRuntimeDataDir 返回运行期数据目录（定价缓存、自定义页面导入源、前端静态覆盖）的默认值，
// 即 pricing.data_dir 未配置时的取值。返回绝对路径，后续任何 chdir 都不会改变它的指向。
func DefaultRuntimeDataDir() string {
	return normalizeRuntimeDataDir(ResolveDataDir(runtimeDataDirFallback))
}

// normalizeRuntimeDataDir 把配置/默认得到的目录转成绝对路径；空值退回默认值。
// 相对路径只在进程启动这一刻按工作目录解析一次，避免 issue #7 里
// "data/public 随进程 CWD 漂移" 这一类问题。
func normalizeRuntimeDataDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = ResolveDataDir(runtimeDataDirFallback)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		// Getwd 失败极罕见（工作目录被删除等）；保留原值并留下痕迹，而不是悄悄换成别的目录。
		slog.Warn("data dir could not be made absolute; keeping configured value",
			"dir", dir, "error", err)
		return filepath.Clean(dir)
	}
	return abs
}

// StaticOverrideDir 返回前端静态资源覆盖目录 <dataDir>/public。
// 该目录是每个实例各自的本地文件（品牌资源覆盖），多副本部署必须在每个副本挂载相同内容。
func StaticOverrideDir(dataDir string) string {
	return filepath.Join(dataDir, staticOverrideSubdir)
}

// CustomPagesImportDir 返回旧版自定义页面的磁盘目录 <dataDir>/pages。
// 自 custom_pages 表引入后它只作为一次性导入源，请求路径不再读它。
func CustomPagesImportDir(dataDir string) string {
	return filepath.Join(dataDir, customPagesImportSubdir)
}
