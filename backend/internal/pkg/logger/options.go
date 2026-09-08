package logger

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultContainerLogPath 为容器内默认日志文件路径。
	DefaultContainerLogPath = "/app/data/logs/sub2api.log"
	defaultLogFilename      = "sub2api.log"
)

type InitOptions struct {
	Level           string
	Format          string
	ServiceName     string
	Environment     string
	Caller          bool
	StacktraceLevel string
	Output          OutputOptions
	Rotation        RotationOptions
	Sampling        SamplingOptions
}

type OutputOptions struct {
	ToStdout bool
	ToFile   bool
	FilePath string
}

type RotationOptions struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
	LocalTime  bool
}

type SamplingOptions struct {
	Enabled    bool
	Initial    int
	Thereafter int
}

func (o InitOptions) normalized() InitOptions {
	out := o
	out.Level = strings.ToLower(strings.TrimSpace(out.Level))
	if out.Level == "" {
		out.Level = "info"
	}
	out.Format = strings.ToLower(strings.TrimSpace(out.Format))
	if out.Format == "" {
		out.Format = "console"
	}
	out.ServiceName = strings.TrimSpace(out.ServiceName)
	if out.ServiceName == "" {
		out.ServiceName = "sub2api"
	}
	out.Environment = strings.TrimSpace(out.Environment)
	if out.Environment == "" {
		out.Environment = "production"
	}
	out.StacktraceLevel = strings.ToLower(strings.TrimSpace(out.StacktraceLevel))
	if out.StacktraceLevel == "" {
		out.StacktraceLevel = "error"
	}
	if !out.Output.ToStdout && !out.Output.ToFile {
		out.Output.ToStdout = true
	}
	out.Output.FilePath = resolveLogFilePath(out.Output.FilePath)
	if out.Rotation.MaxSizeMB <= 0 {
		out.Rotation.MaxSizeMB = 100
	}
	if out.Rotation.MaxBackups < 0 {
		out.Rotation.MaxBackups = 10
	}
	if out.Rotation.MaxAgeDays < 0 {
		out.Rotation.MaxAgeDays = 7
	}
	if out.Sampling.Enabled {
		if out.Sampling.Initial <= 0 {
			out.Sampling.Initial = 100
		}
		if out.Sampling.Thereafter <= 0 {
			out.Sampling.Thereafter = 100
		}
	}
	return out
}

func resolveLogFilePath(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	// 走 config.ResolveDataDir，不自己读 DATA_DIR：数据目录的裁决规则只有一条
	// （见 internal/config/data_dir.go）。这里曾经自己读一遍、并且用的是不同的兜底，
	// 于是"DATA_DIR 没设但 /app/data 可写"时日志目录与其它运行期数据分道扬镳。
	if dataDir := strings.TrimSpace(config.ResolveDataDir("")); dataDir != "" {
		return filepath.Join(dataDir, "logs", defaultLogFilename)
	}
	return DefaultContainerLogPath
}

func bootstrapOptions() InitOptions {
	return InitOptions{
		Level:       "info",
		Format:      "console",
		ServiceName: "sub2api",
		Environment: "bootstrap",
		Output: OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Rotation: RotationOptions{
			MaxSizeMB:  100,
			MaxBackups: 10,
			MaxAgeDays: 7,
			Compress:   true,
			LocalTime:  true,
		},
		Sampling: SamplingOptions{
			Enabled:    false,
			Initial:    100,
			Thereafter: 100,
		},
	}
}

func parseLevel(level string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn":
		return LevelWarn, true
	case "error":
		return LevelError, true
	default:
		return LevelInfo, false
	}
}

func parseStacktraceLevel(level string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return LevelFatal + 1, true
	case "error":
		return LevelError, true
	case "fatal":
		return LevelFatal, true
	default:
		return LevelError, false
	}
}

func samplingTick() time.Duration {
	return time.Second
}
