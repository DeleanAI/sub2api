import { describe, expect, it } from "vitest";

import {
  MAX_GROUP_MODEL_RATE_MULTIPLIER,
  emptyModelRateMultiplierEntry,
  modelRateMultipliersFromAPI,
  modelRateMultipliersToAPI,
  normalizeModelPatternForDedupe,
  validateModelRateMultiplierEntries,
  type ModelRateMultiplierFormEntry,
} from "../groupsModelRateMultipliers";

const entry = (
  overrides: Partial<ModelRateMultiplierFormEntry> = {},
): ModelRateMultiplierFormEntry => ({
  model_pattern: "claude-opus-*",
  multiplier: 2,
  ...overrides,
});

describe("validateModelRateMultiplierEntries", () => {
  it("accepts an ordered list of valid rows and an empty list", () => {
    expect(validateModelRateMultiplierEntries([])).toBeNull();
    expect(
      validateModelRateMultiplierEntries([
        entry(),
        entry({ model_pattern: "claude-haiku-*", multiplier: "0.5" }),
        entry({ model_pattern: "gpt-5.4", multiplier: MAX_GROUP_MODEL_RATE_MULTIPLIER }),
      ]),
    ).toBeNull();
  });

  it("reports the first offending row for an empty pattern", () => {
    expect(
      validateModelRateMultiplierEntries([entry(), entry({ model_pattern: "   " })]),
    ).toEqual({ index: 1, errorKey: "patternRequired" });
  });

  it("rejects multipliers outside (0, 100] and non-numeric input", () => {
    for (const bad of [null, "", 0, -1, "abc", MAX_GROUP_MODEL_RATE_MULTIPLIER + 0.01, Number.NaN]) {
      expect(validateModelRateMultiplierEntries([entry({ multiplier: bad })])).toEqual({
        index: 0,
        errorKey: "multiplierRange",
      });
    }
  });

  it("rejects duplicates using the backend normalization rule", () => {
    expect(
      validateModelRateMultiplierEntries([
        entry({ model_pattern: "Claude-Opus-4.1" }),
        entry({ model_pattern: " claude-opus-4-1 ", multiplier: 3 }),
      ]),
    ).toEqual({ index: 1, errorKey: "duplicatePattern" });
    expect(
      validateModelRateMultiplierEntries([
        entry({ model_pattern: "GPT-5.4" }),
        entry({ model_pattern: "gpt-5.4" }),
      ]),
    ).toEqual({ index: 1, errorKey: "duplicatePattern" });
  });
});

describe("normalizeModelPatternForDedupe", () => {
  it("lowercases, trims and only rewrites dots for claude models", () => {
    expect(normalizeModelPatternForDedupe(" Claude-Opus-4.1 ")).toBe("claude-opus-4-1");
    expect(normalizeModelPatternForDedupe("GPT-5.4")).toBe("gpt-5.4");
  });
});

describe("API conversion", () => {
  it("round-trips entries and preserves order", () => {
    const api = [
      { model_pattern: "claude-opus-*", multiplier: 2 },
      { model_pattern: "claude-haiku-*", multiplier: 0.5 },
    ];
    const form = modelRateMultipliersFromAPI(api);
    expect(form).toEqual(api);
    expect(modelRateMultipliersToAPI(form)).toEqual(api);
    expect(modelRateMultipliersFromAPI(undefined)).toEqual([]);
    expect(modelRateMultipliersFromAPI(null)).toEqual([]);
  });

  it("trims patterns and coerces numeric strings when serializing", () => {
    expect(
      modelRateMultipliersToAPI([{ model_pattern: "  gpt-5* ", multiplier: "1.5" }]),
    ).toEqual([{ model_pattern: "gpt-5*", multiplier: 1.5 }]);
  });

  it("starts new rows empty", () => {
    expect(emptyModelRateMultiplierEntry()).toEqual({ model_pattern: "", multiplier: null });
  });
});
