-- 汇率存档与金额折算快照（fork）。
--
-- 站内只有一个记账币种（settings.balance_currency，默认 USD）。以其它币种支付或计价的金额在交易发生时按当天
-- 汇率折算成记账币种，所用汇率连同来源一起记在该笔记录上，事后可复现：
--   - exchange_rates：抓到的每一条汇率原样存档（人民币中间价来自中国外汇交易中心，USDT/USDC 来自 CoinGecko）。
--     usd_per_unit 是 1 单位该币值多少美元；quote 是来源原样的报价（中间价是「1 美元合多少人民币」）。
--   - redeem_codes.currency_conversion：管理员按非记账币种调整余额时的原币金额与汇率。
--   - payment_orders.currency_conversion：在线支付的支付币种与记账币种不同时，入账金额所用的汇率。
--   - usage_logs.currency_conversion：本次用量的价格不是记账币种（如方舟按人民币标价）时所用的汇率。
--     百万级行：nullable、无 DEFAULT、无回填，PostgreSQL 只改元数据，不重写表。
--   - batch_image_jobs.currency_conversion：批量图片提交时单价来自非记账币种价卡的折算依据，结算写用量日志时带上。
--   - channel_model_pricing / channel_account_stats_model_pricing 的 currency 与 groups.model_pricing 每个条目的
--     "currency"：该条价格的标价币种。存量条目当时就是按记账币种填的，回填成迁移时的 balance_currency
--     （未设置即 USD），含义不变。
CREATE TABLE IF NOT EXISTS exchange_rates (
    id           BIGSERIAL PRIMARY KEY,
    currency     VARCHAR(10)    NOT NULL,
    rate_date    DATE           NOT NULL,
    usd_per_unit NUMERIC(24,12) NOT NULL CHECK (usd_per_unit > 0),
    quote        NUMERIC(24,12) NOT NULL CHECK (quote > 0),
    quote_unit   VARCHAR(32)    NOT NULL,
    source       VARCHAR(64)    NOT NULL,
    source_url   TEXT           NOT NULL,
    published_at TIMESTAMPTZ    NOT NULL,
    fetched_at   TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    UNIQUE (currency, rate_date)
);

CREATE INDEX IF NOT EXISTS idx_exchange_rates_currency_published_at ON exchange_rates (currency, published_at DESC);

COMMENT ON TABLE exchange_rates IS
    'Archived exchange rates: CNY = CFETS USD/CNY central parity (published 09:15 Asia/Shanghai on business days), USDT/USDC = CoinGecko daily price at 00:00 UTC';

ALTER TABLE redeem_codes ADD COLUMN IF NOT EXISTS currency_conversion JSONB;
ALTER TABLE payment_orders ADD COLUMN IF NOT EXISTS currency_conversion JSONB;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS currency_conversion JSONB;
ALTER TABLE batch_image_jobs ADD COLUMN IF NOT EXISTS currency_conversion JSONB;

COMMENT ON COLUMN redeem_codes.currency_conversion IS
    'Original amount/currency and the exchange rate used when a balance adjustment was made in a non-accounting currency (NULL = accounting currency)';
COMMENT ON COLUMN payment_orders.currency_conversion IS
    'Exchange rate used to credit the balance when the payment currency differs from the accounting currency (NULL = same currency)';
COMMENT ON COLUMN usage_logs.currency_conversion IS
    'Exchange rate used when the price card is not in the accounting currency (NULL = priced in the accounting currency)';
COMMENT ON COLUMN batch_image_jobs.currency_conversion IS
    'Exchange rate used to snapshot the unit price when the price card is not in the accounting currency (NULL = same currency)';

ALTER TABLE channel_model_pricing ADD COLUMN IF NOT EXISTS currency VARCHAR(10);
ALTER TABLE channel_account_stats_model_pricing ADD COLUMN IF NOT EXISTS currency VARCHAR(10);

UPDATE channel_model_pricing
SET currency = COALESCE((SELECT NULLIF(UPPER(BTRIM(value)), '') FROM settings WHERE key = 'balance_currency'), 'USD')
WHERE currency IS NULL;

UPDATE channel_account_stats_model_pricing
SET currency = COALESCE((SELECT NULLIF(UPPER(BTRIM(value)), '') FROM settings WHERE key = 'balance_currency'), 'USD')
WHERE currency IS NULL;

ALTER TABLE channel_model_pricing ALTER COLUMN currency SET DEFAULT 'USD';
ALTER TABLE channel_model_pricing ALTER COLUMN currency SET NOT NULL;
ALTER TABLE channel_account_stats_model_pricing ALTER COLUMN currency SET DEFAULT 'USD';
ALTER TABLE channel_account_stats_model_pricing ALTER COLUMN currency SET NOT NULL;

UPDATE groups g
SET model_pricing = (
    SELECT jsonb_agg(
               CASE
                   WHEN jsonb_typeof(entry) = 'object' AND NOT entry ? 'currency'
                       THEN entry || jsonb_build_object('currency',
                            COALESCE((SELECT NULLIF(UPPER(BTRIM(value)), '') FROM settings WHERE key = 'balance_currency'), 'USD'))
                   ELSE entry
               END
               ORDER BY ord)
    FROM jsonb_array_elements(g.model_pricing) WITH ORDINALITY AS t(entry, ord)
)
WHERE jsonb_typeof(g.model_pricing) = 'array'
  AND jsonb_array_length(g.model_pricing) > 0
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(g.model_pricing) AS e(entry)
      WHERE jsonb_typeof(entry) = 'object' AND NOT entry ? 'currency'
  );
