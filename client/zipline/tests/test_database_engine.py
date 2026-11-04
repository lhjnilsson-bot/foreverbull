from datetime import date
from datetime import datetime

import pandas as pd

from foreverbull_zipline.data_bundles.foreverbull import DatabaseEngine
from sqlalchemy import text


def test_database_engine_reads_the_last_day_stamped_at_the_open(fb_database):
    # Yahoo stamps a daily bar at the 14:30 UTC session open. The bar on the
    # period's last day is read; the day after it is not.
    engine, _ = fb_database
    symbol = "LASTDAYBAR"
    days = [datetime(2021, 3, 30, 14, 30), datetime(2021, 3, 31, 14, 30), datetime(2021, 4, 1, 14, 30)]
    with engine.begin() as conn:
        conn.execute(text("DELETE FROM ohlc WHERE symbol = :symbol"), {"symbol": symbol})
        conn.execute(
            text("INSERT INTO asset (symbol, name) VALUES (:symbol, '') ON CONFLICT DO NOTHING"), {"symbol": symbol}
        )
        for day in days:
            conn.execute(
                text(
                    """INSERT INTO ohlc (provider, symbol, open, high, low, close, volume, time)
                    VALUES ('yahoo', :symbol, 1, 1, 1, 1, 1, :time)"""
                ),
                {"symbol": symbol, "time": day},
            )
    reader = DatabaseEngine.__new__(DatabaseEngine)
    reader._engine = engine
    reader.provider = "yahoo"
    try:
        bars = reader.get_data(symbol, date(2021, 3, 30), date(2021, 3, 31))
    finally:
        with engine.begin() as conn:
            conn.execute(text("DELETE FROM ohlc WHERE symbol = :symbol"), {"symbol": symbol})
    assert [pd.Timestamp(t).to_pydatetime() for t in bars["time"]] == days[:2]


def test_database_engine_reads_dividends_and_their_fetched_span(fb_database):
    engine, _ = fb_database
    with engine.begin() as conn:
        conn.execute(text("DELETE FROM dividend WHERE symbol IN ('DIVA', 'DIVB')"))
        conn.execute(text("DELETE FROM dividend_fetch WHERE symbol IN ('DIVA', 'DIVB')"))
        conn.execute(
            text(
                """INSERT INTO dividend (provider, symbol, ex_date, pay_date, amount, currency, is_foreign) VALUES
                ('yahoo', 'DIVA', '2024-03-14', NULL, 0.485, 'USD', false),
                ('yahoo', 'DIVA', '2024-06-14', '2024-07-01', 0.485, 'USD', true),
                ('alpaca', 'DIVA', '2024-03-14', '2024-04-01', 0.485, 'USD', false),
                ('yahoo', 'DIVB', '2025-01-02', NULL, 1.0, 'USD', false)"""
            )
        )
        conn.execute(
            text(
                """INSERT INTO dividend_fetch (provider, symbol, first_date, last_date) VALUES
                ('yahoo', 'DIVA', '2024-01-02', '2024-12-31'), ('alpaca', 'DIVB', '2024-01-02', '2024-12-31')"""
            )
        )
    reader = DatabaseEngine.__new__(DatabaseEngine)
    reader._engine = engine
    reader.provider = "yahoo"
    try:
        dividends = reader.get_dividends(["DIVA", "DIVB"], date(2024, 1, 2), date(2024, 12, 31))
        fetched = reader.get_dividend_fetches(["DIVA", "DIVB"])
    finally:
        with engine.begin() as conn:
            conn.execute(text("DELETE FROM dividend WHERE symbol IN ('DIVA', 'DIVB')"))
            conn.execute(text("DELETE FROM dividend_fetch WHERE symbol IN ('DIVA', 'DIVB')"))
    columns = ["symbol", "ex_date", "pay_date", "amount", "is_foreign"]
    assert list(dividends[columns].itertuples(index=False, name=None)) == [
        ("DIVA", date(2024, 3, 14), None, 0.485, False),
        ("DIVA", date(2024, 6, 14), date(2024, 7, 1), 0.485, True),
    ]
    assert fetched == {"DIVA": (date(2024, 1, 2), date(2024, 12, 31))}


def test_database_engine_reads_corporate_actions_in_the_window(fb_database):
    engine, _ = fb_database
    with engine.begin() as conn:
        conn.execute(text("DELETE FROM corporate_action WHERE symbol IN ('DIVA', 'DIVB')"))
        conn.execute(
            text(
                """INSERT INTO corporate_action (provider, symbol, kind, ex_date) VALUES
                ('alpaca', 'DIVA', 'spin_off', '2024-05-06'),
                ('alpaca', 'DIVA', 'stock_dividend', '2025-02-03'),
                ('yahoo', 'DIVA', 'cash_merger', '2024-05-06'),
                ('alpaca', 'DIVB', 'cash_merger', '2024-01-02')"""
            )
        )
    reader = DatabaseEngine.__new__(DatabaseEngine)
    reader._engine = engine
    reader.provider = "alpaca"
    try:
        actions = reader.get_corporate_actions(["DIVA", "DIVB"], date(2024, 1, 2), date(2024, 12, 31))
    finally:
        with engine.begin() as conn:
            conn.execute(text("DELETE FROM corporate_action WHERE symbol IN ('DIVA', 'DIVB')"))
    assert list(actions[["symbol", "kind", "ex_date"]].itertuples(index=False, name=None)) == [
        ("DIVB", "cash_merger", date(2024, 1, 2)),
        ("DIVA", "spin_off", date(2024, 5, 6)),
    ]
