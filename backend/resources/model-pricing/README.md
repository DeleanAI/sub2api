# Model Pricing Data

This directory ships two pricing files:

- `official_prices.json` — **the official price catalog**: the only price source for the OpenAI,
  Anthropic and DeepSeek models it declares (see below).
- `model_prices_and_context_window.json` — a local copy of the mirrored LiteLLM catalog, used as a
  fallback when the remote catalog cannot be downloaded.

## Official price catalog (`official_prices.json`)

Loaded through `pricing.official_file` (default `./resources/model-pricing/official_prices.json`).
Prices are **USD per million tokens**, transcribed from each vendor's official pricing page so every
number can be checked against the page cell by cell (each provider records `source` and `checked_at`).

- Each model, its `aliases` and its dated / provider-prefixed snapshot keys
  (`claude-opus-4-1-20250805`, `openai/gpt-4o`, `vertex_ai/claude-opus-4-5@20251101`) replace the
  remote/fallback catalog entry wholesale. The operator override file does not apply to these models.
- Fields: `input`, `cached_input`, `cache_write` (Anthropic: 5-minute write), `cache_write_1h`, `output`,
  `image_input`, `image_cached_input`, `image_output`, `mode` (`chat` | `embedding` | `image_generation`).
  A missing `cached_input` or `cache_write` bills those tokens as uncached input (no discount / no
  separate write charge), which is how the vendors define it.
- `long_context`: `above_input_tokens` threshold (strictly greater) with `input_multiplier` (applies to
  input, cached input and cache writes) and `output_multiplier`.
- `tiers`: `fast` / `flex` / `ultrafast`, each either a multiplier (`"flex": 0.5`) or a price object
  copied from the vendor's tier table (`{"input": 20, "cached_input": 2, "cache_write": 25, "output": 100}`);
  the loader turns a price object into a multiplier and rejects it unless every component has the same
  ratio. A tier a model does not declare is billed at standard rates and logged
  (`billing.service_tier_not_offered`).
- The loader is strict: unknown fields, missing prices, inconsistent tier ratios, duplicate or
  non-lowercase names all fail the load. When the file is configured but missing or invalid, the
  pricing service refuses to start (and a hot reload keeps the previous data).
- `internal/service/official_pricing_test.go` walks every model, alias, snapshot spelling, tier and
  token kind in this file and checks that billing produces exactly these numbers; it also checks the
  vendors' published rules (cache write/read ratios, long-context multipliers, Flex = Batch rates).

To update prices: edit the numbers, set `checked_at`, run `go test -tags=unit ./internal/service/`.
For an urgent change without a new image, mount an edited copy and point `pricing.official_file`
(env `PRICING_OFFICIAL_FILE`) at it; edits are picked up at the next hash check.

## LiteLLM fallback catalog (`model_prices_and_context_window.json`)

This directory contains a local copy of the mirrored model pricing data as a fallback mechanism.

## Source
The original file is maintained by the LiteLLM project and mirrored into the `price-mirror` branch of this repository via GitHub Actions:
- Mirror branch (configurable via `PRICE_MIRROR_REPO`): https://raw.githubusercontent.com/<your-repo>/price-mirror/model_prices_and_context_window.json
- Upstream source: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json

## Purpose
This local copy serves as a fallback when the remote file cannot be downloaded due to:
- Network restrictions
- Firewall rules
- DNS resolution issues
- GitHub being blocked in certain regions
- Docker container network limitations

## Update Process
The pricingService will:
1. First attempt to download the latest version from GitHub
2. If download fails, use this local copy as fallback
3. Log a warning when using the fallback file

## Manual Update
To manually update this file with the latest pricing data (if automation is unavailable):
```bash
curl -s https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json -o model_prices_and_context_window.json
```

## File Format
The file contains JSON data with model pricing information including:
- Model names and identifiers
- Input/output token costs
- Context window sizes
- Model capabilities

Last updated: 2025-08-10
