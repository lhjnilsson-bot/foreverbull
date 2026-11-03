package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	commonPB "github.com/lhjnilsson/foreverbull/pkg/pb"
	backtest "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

// ListVisible returns the backtests the caller may see. It uses backtest_id.
func (db *Backtest) ListVisible(ctx context.Context, caller auth.Account) ([]*backtest.Backtest, error) {
	if caller.ID == "" {
		return nil, errors.New("account id is empty")
	}
	where, args := visibleWhere("b", caller, auth.DomainBacktest)
	query := `
		SELECT b.name, b.benchmark, b.provider, b.profile,
		       COALESCE(p.username, g.github_login, '')
		FROM backtest AS b
		LEFT JOIN account_password AS p ON p.account_id = b.owner_id
		LEFT JOIN account_github AS g ON g.account_id = b.owner_id
		WHERE ` + where + `
		ORDER BY b.name`
	rows, err := db.Conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list visible backtests: %w", err)
	}
	defer rows.Close()
	out := []*backtest.Backtest{}
	for rows.Next() {
		item := &backtest.Backtest{}
		if err := rows.Scan(&item.Name, &item.Benchmark, &item.Provider, &item.Profile, &item.OwnerLogin); err != nil {
			return nil, fmt.Errorf("list visible backtests: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list visible backtests: %w", err)
	}
	return out, nil
}

// GetByID loads one backtest after the caller has already been authorized.
func (db *Backtest) GetByID(ctx context.Context, id string) (*backtest.Backtest, error) {
	rows, err := db.Conn.Query(ctx, `
		SELECT b.name, b.benchmark, b.provider, b.profile,
		       COALESCE(p.username, g.github_login, ''), b.ticker_resolutions,
		       bp.start_date, bp.end_date, bpa.symbol, bpa.enabled
		FROM backtest AS b
		LEFT JOIN account_password AS p ON p.account_id = b.owner_id
		LEFT JOIN account_github AS g ON g.account_id = b.owner_id
		LEFT JOIN backtest_period AS bp ON bp.backtest_id = b.id
		LEFT JOIN backtest_period_asset AS bpa
		  ON bpa.backtest_id = bp.backtest_id
		 AND bpa.start_date = bp.start_date
		 AND bpa.end_date = bp.end_date
		WHERE b.id = $1
		ORDER BY bp.start_date, bpa.symbol DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("get backtest: %w", err)
	}
	defer rows.Close()
	return scanOwnedBacktest(rows)
}

// IDByOwnerAndName returns the backtest id for one owner. The pair is unique.
func (db *Backtest) IDByOwnerAndName(ctx context.Context, ownerID, name string) (string, error) {
	if ownerID == "" {
		return "", errors.New("account id is empty")
	}
	var id string
	err := db.Conn.QueryRow(ctx, `SELECT id FROM backtest WHERE owner_id = $1 AND name = $2`, ownerID, name).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// DeleteByID removes one backtest. It does not delete by name.
func (db *Backtest) DeleteByID(ctx context.Context, id string) error {
	if id == "" {
		return pgx.ErrNoRows
	}
	tag, err := db.Conn.Exec(ctx, `DELETE FROM backtest WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete backtest: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CreateOwned inserts a backtest for ownerID. Periods use backtest_id.
func (db *Backtest) CreateOwned(ctx context.Context, ownerID string, item *backtest.Backtest) (*backtest.Backtest, error) {
	if ownerID == "" {
		return nil, errors.New("account id is empty")
	}
	provider, err := normalizeBacktestProvider(item.GetProvider())
	if err != nil {
		return nil, err
	}
	var id string
	err = db.Conn.QueryRow(ctx, `
		INSERT INTO backtest (name, benchmark, provider, profile, owner_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`, item.GetName(), item.GetBenchmark(), provider, item.GetProfile(), ownerID).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create backtest: %w", err)
	}
	for _, period := range item.GetPeriods() {
		_, err = db.Conn.Exec(ctx, `
			INSERT INTO backtest_period (backtest_id, start_date, end_date)
			VALUES ($1, $2, $3)`, id, commonPB.DateToDateString(period.GetStartDate()), commonPB.DateToDateString(period.GetEndDate()))
		if err != nil {
			return nil, fmt.Errorf("create backtest period: %w", err)
		}
		for _, asset := range period.GetAssets() {
			_, err = db.Conn.Exec(ctx, `
				INSERT INTO backtest_period_asset (backtest_id, start_date, end_date, symbol, enabled)
				VALUES ($1, $2, $3, $4, $5)`,
				id, commonPB.DateToDateString(period.GetStartDate()), commonPB.DateToDateString(period.GetEndDate()), asset.GetSymbol(), asset.GetEnabled())
			if err != nil {
				return nil, fmt.Errorf("create backtest period asset: %w", err)
			}
		}
	}
	return db.GetByID(ctx, id)
}

func scanOwnedBacktest(rows pgx.Rows) (*backtest.Backtest, error) {
	item := &backtest.Backtest{}
	for rows.Next() {
		var (
			resolved []byte
			start    *time.Time
			end      *time.Time
			symbol   *string
			enabled  *bool
		)
		first := item.Name == ""
		err := rows.Scan(&item.Name, &item.Benchmark, &item.Provider, &item.Profile, &item.OwnerLogin, &resolved, &start, &end, &symbol, &enabled)
		if err != nil {
			return nil, fmt.Errorf("scan backtest: %w", err)
		}
		if first {
			item.TickerResolutions, err = decodeTickerResolutions(resolved)
			if err != nil {
				return nil, fmt.Errorf("backtest %s: %w", item.Name, err)
			}
		}
		if start == nil || end == nil {
			continue
		}
		startDate := commonPB.GoTimeToDate(*start)
		endDate := commonPB.GoTimeToDate(*end)
		var period *backtest.Backtest_Period
		for _, existing := range item.Periods {
			if sameDate(existing.GetStartDate(), startDate) && sameDate(existing.GetEndDate(), endDate) {
				period = existing
				break
			}
		}
		if period == nil {
			period = &backtest.Backtest_Period{StartDate: startDate, EndDate: endDate}
			item.Periods = append(item.Periods, period)
		}
		if symbol == nil {
			continue
		}
		on := false
		if enabled != nil {
			on = *enabled
		}
		period.Assets = append(period.Assets, &backtest.Backtest_Period_Asset{Symbol: *symbol, Enabled: on})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if item.Name == "" {
		return nil, pgx.ErrNoRows
	}
	return item, nil
}

func sameDate(a, b *commonPB.Date) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Year == b.Year && a.Month == b.Month && a.Day == b.Day
}

func visibleWhere(alias string, caller auth.Account, kind string) (string, []any) {
	switch caller.Role {
	case auth.RoleAdmin:
		return "TRUE", nil
	case auth.RoleEditor:
		return `(` + alias + `.owner_id = $1 OR EXISTS (
			SELECT 1 FROM domain_grant AS gr
			WHERE gr.domain_kind = $2 AND gr.domain_id = ` + alias + `.id AND gr.grantee_id = $1))`,
			[]any{caller.ID, kind}
	default:
		return `EXISTS (
			SELECT 1 FROM domain_grant AS gr
			WHERE gr.domain_kind = $2 AND gr.domain_id = ` + alias + `.id AND gr.grantee_id = $1)`,
			[]any{caller.ID, kind}
	}
}

// CreateForBacktest inserts a session for a backtest id.
func (db *Session) CreateForBacktest(ctx context.Context, backtestID string) (*backtest.Session, error) {
	if backtestID == "" {
		return nil, errors.New("backtest id is empty")
	}
	var id string
	err := db.Conn.QueryRow(ctx, `INSERT INTO session (backtest_id) VALUES ($1) RETURNING id`, backtestID).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return db.Get(ctx, id)
}

// BacktestID returns the backtest id stored on the session.
func (db *Session) BacktestID(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := db.Conn.QueryRow(ctx, `SELECT backtest_id FROM session WHERE id = $1`, sessionID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (db *Session) getJoined(ctx context.Context, sessionID string) (*backtest.Session, error) {
	session := backtest.Session{}
	err := db.Conn.QueryRow(ctx, `
		SELECT s.id, b.name, s.port
		FROM session AS s
		JOIN backtest AS b ON b.id = s.backtest_id
		WHERE s.id = $1`, sessionID).Scan(&session.Id, &session.Backtest, &session.Port)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	return &session, nil
}

func authOn() bool {
	return environment.GetAuthEnabled()
}
