// 分组逐模型倍率表单辅助：表单行 <-> API 列表换算与提交前校验。
// 后端 NormalizeGroupModelRateMultipliers 是校验的唯一权威（模式非空、倍率 ∈ (0,100]、
// 归一化后不重复）；这里按同一规则做提交前预检，只为把错误定位到具体行并就地提示，
// 不替代后端校验。

import type { GroupModelRateMultiplier } from "@/types";

export const MAX_GROUP_MODEL_RATE_MULTIPLIER = 100;

export type ModelRateMultiplierFormEntry = {
  model_pattern: string;
  multiplier: number | string | null;
};

export type ModelRateMultiplierValidationError = {
  index: number;
  errorKey: "patternRequired" | "multiplierRange" | "duplicatePattern";
};

export const emptyModelRateMultiplierEntry = (): ModelRateMultiplierFormEntry => ({
  model_pattern: "",
  multiplier: null,
});

export const modelRateMultipliersFromAPI = (
  entries: GroupModelRateMultiplier[] | null | undefined,
): ModelRateMultiplierFormEntry[] =>
  (entries ?? []).map((entry) => ({
    model_pattern: entry.model_pattern,
    multiplier: entry.multiplier,
  }));

// 与后端 normalizeChannelPricingModelName 同口径的归一化：小写 + 去空白；claude 系列 "." → "-"。
// 只用于前端重复检测，不改变提交给后端的原始模式字符串。
export const normalizeModelPatternForDedupe = (pattern: string): string => {
  const lowered = pattern.trim().toLowerCase();
  return lowered.startsWith("claude-") ? lowered.replace(/\./g, "-") : lowered;
};

// 提交前校验：返回 null 表示通过，否则返回第一条出错行的下标与 i18n key
//（相对 admin.groups.modelRateMultipliers 前缀）。
export const validateModelRateMultiplierEntries = (
  entries: ModelRateMultiplierFormEntry[],
): ModelRateMultiplierValidationError | null => {
  const seen = new Map<string, number>();
  for (let index = 0; index < entries.length; index++) {
    const entry = entries[index];
    const pattern = entry.model_pattern.trim();
    if (!pattern) {
      return { index, errorKey: "patternRequired" };
    }
    const multiplier = Number(entry.multiplier);
    if (
      entry.multiplier === null ||
      entry.multiplier === "" ||
      !Number.isFinite(multiplier) ||
      multiplier <= 0 ||
      multiplier > MAX_GROUP_MODEL_RATE_MULTIPLIER
    ) {
      return { index, errorKey: "multiplierRange" };
    }
    const key = normalizeModelPatternForDedupe(pattern);
    if (seen.has(key)) {
      return { index, errorKey: "duplicatePattern" };
    }
    seen.set(key, index);
  }
  return null;
};

// 表单行 -> API 列表。顺序即匹配优先级，原样保留；调用方应先通过 validate 再调用。
export const modelRateMultipliersToAPI = (
  entries: ModelRateMultiplierFormEntry[],
): GroupModelRateMultiplier[] =>
  entries.map((entry) => ({
    model_pattern: entry.model_pattern.trim(),
    multiplier: Number(entry.multiplier),
  }));
