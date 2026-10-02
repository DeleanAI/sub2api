package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type exchangeRateRepository struct {
	db dbExec
}

// NewExchangeRateRepository 创建汇率存档仓库（表 exchange_rates，迁移 241）。
func NewExchangeRateRepository(db *sql.DB) service.ExchangeRateRepository {
	return &exchangeRateRepository{db: db}
}

const exchangeRateColumns = "currency, rate_date, usd_per_unit, quote, quote_unit, source, source_url, published_at, fetched_at"

// UpsertExchangeRates 按 (currency, rate_date) 写入；来源修订过的同日报价以新抓到的为准。
func (r *exchangeRateRepository) UpsertExchangeRates(ctx context.Context, rates []service.ExchangeRate) error {
	if len(rates) == 0 {
		return nil
	}
	var (
		sb   strings.Builder
		args = make([]any, 0, len(rates)*9)
	)
	sb.WriteString("INSERT INTO exchange_rates (" + exchangeRateColumns + ") VALUES ")
	for i, rate := range rates {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 9
		fmt.Fprintf(&sb, "($%d, $%d::date, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9)
		fetchedAt := rate.FetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = time.Now()
		}
		args = append(args, rate.Currency, rate.RateDate, rate.USDPerUnit, rate.Quote, rate.QuoteUnit,
			rate.Source, rate.SourceURL, rate.PublishedAt, fetchedAt)
	}
	sb.WriteString(` ON CONFLICT (currency, rate_date) DO UPDATE SET
		usd_per_unit = EXCLUDED.usd_per_unit, quote = EXCLUDED.quote, quote_unit = EXCLUDED.quote_unit,
		source = EXCLUDED.source, source_url = EXCLUDED.source_url, published_at = EXCLUDED.published_at,
		fetched_at = EXCLUDED.fetched_at`)
	_, err := r.db.ExecContext(ctx, sb.String(), args...)
	return err
}

func (r *exchangeRateRepository) LatestExchangeRatePublishedBy(ctx context.Context, currency string, at time.Time) (*service.ExchangeRate, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+exchangeRateColumns+` FROM exchange_rates
		WHERE currency = $1 AND published_at <= $2 ORDER BY published_at DESC LIMIT 1`, currency, at)
	return scanExchangeRate(row)
}

func (r *exchangeRateRepository) GetExchangeRate(ctx context.Context, currency, rateDate string) (*service.ExchangeRate, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+exchangeRateColumns+` FROM exchange_rates
		WHERE currency = $1 AND rate_date = $2::date`, currency, rateDate)
	return scanExchangeRate(row)
}

func (r *exchangeRateRepository) ListExchangeRates(ctx context.Context, currency string, limit int) ([]service.ExchangeRate, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+exchangeRateColumns+` FROM exchange_rates
		WHERE currency = $1 ORDER BY rate_date DESC LIMIT $2`, currency, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.ExchangeRate
	for rows.Next() {
		rate, err := scanExchangeRate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rate)
	}
	return out, rows.Err()
}

type exchangeRateScanner interface {
	Scan(dest ...any) error
}

func scanExchangeRate(row exchangeRateScanner) (*service.ExchangeRate, error) {
	var (
		rate     service.ExchangeRate
		rateDate time.Time
	)
	err := row.Scan(&rate.Currency, &rateDate, &rate.USDPerUnit, &rate.Quote, &rate.QuoteUnit,
		&rate.Source, &rate.SourceURL, &rate.PublishedAt, &rate.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rate.RateDate = rateDate.Format(time.DateOnly)
	return &rate, nil
}
