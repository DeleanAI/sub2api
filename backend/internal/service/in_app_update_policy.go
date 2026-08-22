package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ErrInAppUpdateDisabled 是所有「拒绝原地替换二进制」错误的哨兵：409 + IN_APP_UPDATE_DISABLED。
// 实际返回的错误带具体原因，用 errors.Is 与它比较（Code + Reason 相同即匹配）。
var ErrInAppUpdateDisabled = infraerrors.Conflict("IN_APP_UPDATE_DISABLED", "in-app update is disabled for this deployment")

// InAppUpdateBlockCode 是机器可读的阻断原因，前端据此本地化提示文案。
type InAppUpdateBlockCode string

const (
	// InAppUpdateBlockMultipleInstances：不止一个实例存活，原地更新只会替换本进程。
	InAppUpdateBlockMultipleInstances InAppUpdateBlockCode = "multiple_instances"
	// InAppUpdateBlockInstanceCountUnknown：实例数未知（注册表不可用），按多实例处理（fail closed）。
	InAppUpdateBlockInstanceCountUnknown InAppUpdateBlockCode = "instance_count_unknown"
	// InAppUpdateBlockExecutableNotWritable：可执行文件所在目录不可写（只读根文件系统）。
	InAppUpdateBlockExecutableNotWritable InAppUpdateBlockCode = "executable_not_writable"
)

// LiveInstancesUnknown 是 LiveInstances 字段表示「未知」的取值。
const LiveInstancesUnknown = -1

// upgradeByImageHint 是每条阻断原因共同的出路，指向镜像/二进制级别的升级。
const upgradeByImageHint = "Upgrade by changing the image tag (or the installed binary) on every instance instead."

// InAppUpdateDecision 是一次策略裁决的结果。
type InAppUpdateDecision struct {
	Allowed       bool
	BlockCode     InAppUpdateBlockCode
	BlockReason   string
	LiveInstances int // LiveInstancesUnknown 表示未知
}

// InAppUpdatePolicy 是「能否在进程内替换运行中的二进制」的唯一裁决点。
//
// 规则：只有当本进程确认自己是唯一存活实例、且可执行文件目录可写时才允许；
// 任何无法确认的情况（实例数未知）都按拒绝处理。
type InAppUpdatePolicy struct {
	instances     LiveInstanceCounter
	writableProbe func() error
}

// NewInAppUpdatePolicy 构造策略。writableProbe 返回 nil 表示可执行文件目录可写。
func NewInAppUpdatePolicy(instances LiveInstanceCounter, writableProbe func() error) *InAppUpdatePolicy {
	return &InAppUpdatePolicy{instances: instances, writableProbe: writableProbe}
}

// Evaluate 给出裁决；nil 策略视为「无法确认」，同样拒绝。
func (p *InAppUpdatePolicy) Evaluate(ctx context.Context) InAppUpdateDecision {
	if p == nil {
		slog.Warn("in_app_update: no policy configured; refusing to replace the running binary (fail closed)")
		return InAppUpdateDecision{
			BlockCode:     InAppUpdateBlockInstanceCountUnknown,
			BlockReason:   "in-app update policy is not configured, so the deployment topology cannot be verified. " + upgradeByImageHint,
			LiveInstances: LiveInstancesUnknown,
		}
	}

	instances, err := p.instances.ActiveInstances(ctx)
	if err != nil {
		slog.Warn("in_app_update: live instance count unknown; treating the deployment as multi-instance (fail closed)", "error", err)
		return InAppUpdateDecision{
			BlockCode:     InAppUpdateBlockInstanceCountUnknown,
			BlockReason:   fmt.Sprintf("the number of live instances is unknown (%v); refusing to replace the binary of a possibly multi-instance deployment. %s", err, upgradeByImageHint),
			LiveInstances: LiveInstancesUnknown,
		}
	}

	live := len(instances)
	if live > 1 {
		return InAppUpdateDecision{
			BlockCode:     InAppUpdateBlockMultipleInstances,
			BlockReason:   fmt.Sprintf("%d live instances share this deployment (%s); an in-app update would replace only this process's binary and leave the others on the old version. %s", live, describeInstances(instances), upgradeByImageHint),
			LiveInstances: live,
		}
	}

	if err := p.writableProbe(); err != nil {
		return InAppUpdateDecision{
			BlockCode:     InAppUpdateBlockExecutableNotWritable,
			BlockReason:   fmt.Sprintf("the executable directory is not writable (%v), so the running binary cannot be replaced in place; the filesystem is probably mounted read-only. %s", err, upgradeByImageHint),
			LiveInstances: live,
		}
	}

	return InAppUpdateDecision{Allowed: true, LiveInstances: live}
}

// Require 在不允许时返回 409 IN_APP_UPDATE_DISABLED，并把原因写进日志。
func (p *InAppUpdatePolicy) Require(ctx context.Context) error {
	decision := p.Evaluate(ctx)
	if decision.Allowed {
		return nil
	}
	slog.Warn("in_app_update: refused",
		"block_code", decision.BlockCode,
		"live_instances", decision.LiveInstances,
		"reason", decision.BlockReason)
	return infraerrors.Conflict(ErrInAppUpdateDisabled.Reason, decision.BlockReason).WithMetadata(map[string]string{
		"block_code":     string(decision.BlockCode),
		"live_instances": strconv.Itoa(decision.LiveInstances),
	})
}

// describeInstances 以 hostname 汇总实例列表，便于操作者对照。
func describeInstances(instances []InstanceInfo) string {
	hosts := make([]string, 0, len(instances))
	for _, inst := range instances {
		label := inst.Hostname
		if inst.Version != "" {
			label += "@" + inst.Version
		}
		hosts = append(hosts, label)
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ", ")
}

// probeExecutableDirWritable 是生产用的可写探测：在可执行文件所在目录建一个临时文件再删掉。
// 这正是原地更新要做的事（同目录临时文件 + rename），探测失败即更新必然失败。
func probeExecutableDirWritable() error {
	exePath, err := resolveExecutablePath()
	if err != nil {
		return err
	}
	return probeDirWritable(filepath.Dir(exePath))
}

func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".sub2api-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}
