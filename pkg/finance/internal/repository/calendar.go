package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
	commonpb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const CalendarTable = `CREATE TABLE IF NOT EXISTS trading_calendar_date (
	code TEXT NOT NULL,
	date date NOT NULL,
	open timestamp NOT NULL,
	close timestamp NOT NULL,
	PRIMARY KEY (code, date)
);`

var (
	ErrMissingTradingDate  = errors.New("missing trading date")
	ErrMissingCalendarDate = errors.New("missing calendar date")
)

type Calendar struct {
	Conn postgres.Query
}

func (db *Calendar) StoreCalendarDate(ctx context.Context, calendarCode string, date *pb.CalendarDate) error {
	_, err := db.Conn.Exec(ctx, `INSERT INTO trading_calendar_date (code, date, open, close) VALUES ($1, $2, $3, $4)`,
		calendarCode, commonpb.DateToDateString(date.Date), date.Open.AsTime().Format(time.RFC3339), date.Close.AsTime().Format(time.RFC3339))
	return err
}

func (db *Calendar) ListCalendarDates(ctx context.Context, calendarCode string, start, end *commonpb.Date) ([]*pb.CalendarDate, error) {
	startstr, endstr := commonpb.DateToDateString(start), commonpb.DateToDateString(end)
	rows, err := db.Conn.Query(ctx, `SELECT date, open, close FROM trading_calendar_date WHERE code = $1 AND date >= $2 AND date <= $3`, calendarCode, startstr, endstr)
	if err != nil {
		return nil, fmt.Errorf("failed to list calendar dates: %w", err)
	}

	dates := []*pb.CalendarDate{}
	defer rows.Close()
	for rows.Next() {
		cd := pb.CalendarDate{}
		var date, open, close time.Time
		err := rows.Scan(&date, &open, &close)
		if err != nil {
			return nil, fmt.Errorf("failed to scan: %w", err)
		}
		cd.Date = commonpb.GoTimeToDate(date)
		cd.Open = timestamppb.New(open)
		cd.Close = timestamppb.New(close)
		dates = append(dates, &cd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read calendar dates: %w", err)
	}
	return dates, nil
}

func (db *Calendar) CalendarHasTradingDates(ctx context.Context, calendarCode, provider string, symbols []string, fromDate, toDate *commonpb.Date) (bool, error) {
	var MissingTradingDates bool
	var MissingCalendarDates bool
	strFromDate, strToDate := commonpb.DateToDateString(fromDate), commonpb.DateToDateString(toDate)
	err := db.Conn.QueryRow(ctx, `
	with calendar_dates as (
		select date from trading_calendar_date 
		where code = $1 and date >= $3 and date <= $4
	),
	ohlc_dates as (
		select distinct time::date as ohlc_date from ohlc
		where symbol = any($2) and provider = $5 and time::date >= $3 and time::date <= $4
	),
	missing_trading_dates as (
		select count(*) > 0 as has_missing_trading
		from calendar_dates c
		left join ohlc_dates o on c.date = o.ohlc_date
		where o.ohlc_date is null
	),
	missing_calendar_dates as (
		select 
			case 
				when (select count(*) from calendar_dates) = 0 then true
				when $3::date < (select min(date) from calendar_dates) then true
				when $4::date > (select max(date) from calendar_dates) then true
				else false
			end as has_missing_calendar
	)
	select 
		m.has_missing_calendar as missing_cal_dates,
		t.has_missing_trading as missing_trading_dates
	from missing_trading_dates t
	cross join missing_calendar_dates m
	`, calendarCode, symbols, strFromDate, strToDate, priceProvider(provider)).Scan(&MissingCalendarDates, &MissingTradingDates)
	if err != nil {
		return false, err
	}
	if MissingCalendarDates {
		return false, ErrMissingCalendarDate
	}
	if MissingTradingDates {
		return false, ErrMissingTradingDate
	}
	return true, nil
}
