"""A missing bar fails the ingest; nothing is filled with the previous close.

Yahoo answers null for a session it has no price for. The broker leaves
that bar out, so the stock's rows skip a session. Rows stored as 0 before
the broker refused non-positive prices are missing bars too. The broker
fails ``fbull backtest create`` on such a session before the ingest
(universe.md section 7.5); the ingester refuses one too, in case a bar
went missing in between.

The META and OEF files are Yahoo's daily chart response for 2021-10-01
to 2021-11-30 (see test_rename_yahoo.py). The golden file is the buy and
hold run below, recorded at 9b3cfb54 before this change: portfolio_value
on the complete bars, and filled_portfolio_value on the gapped bars that
were then filled with the previous close.
"""

import logging
import os

from datetime import date
from pathlib import Path
from typing import Any
from typing import cast

import numpy as np
import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.foreverbull import MissingSessionsError
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.data_bundles.foreverbull import drop_unpriced_bars
from foreverbull_zipline.data_bundles.foreverbull import missing_sessions
from zipline.data import bundles
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance import metrics
from zipline.finance.blotter import Blotter
from zipline.finance.trading import SimulationParameters
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from zipline import TradingAlgorithm


TESTDATA = Path(__file__).parent / "testdata"
GOLDEN = TESTDATA / "golden_meta_buy_and_hold_2021-10-01_2021-11-30.csv"
SHARES = 10
# Yahoo null on one session, and on two sessions in a row.
GAP_DAYS = [date(2021, 10, 20), date(2021, 11, 3), date(2021, 11, 4)]
# A row the broker stored as 0 before it refused non-positive prices.
ZERO_DAY = date(2021, 11, 16)


def _yahoo_bars(symbol: str) -> pd.DataFrame:
    frame = pd.read_csv(TESTDATA / f"yahoo_{symbol.lower()}_2021-10-01_2021-11-30.csv")
    frame["time"] = pd.to_datetime(frame["time"], utc=True)
    return frame


def _with_gaps(frame: pd.DataFrame) -> pd.DataFrame:
    days = pd.Series(frame["time"]).dt.date
    gapped = cast(pd.DataFrame, frame[~days.isin(GAP_DAYS)].copy())
    zero = pd.Series(gapped["time"]).dt.date == ZERO_DAY
    gapped.loc[zero, ["open", "high", "low", "close", "volume"]] = 0
    return gapped


class OHLCRows:
    def __init__(self, frames: dict[str, pd.DataFrame]):
        self.frames = frames

    def get_data(self, symbol, from_date, to_date):
        frame = self.frames[symbol]
        days = frame["time"].dt.date
        return frame[(days >= from_date) & (days <= to_date)].copy()


def _backtest(name: str) -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=name,
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2021, month=10, day=1),
                end_date=common_pb.Date(year=2021, month=11, day=30),
                assets=[backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True)],
            )
        ],
    )


def _ingest(name: str, frames: dict[str, pd.DataFrame]) -> tuple[Any, SQLIngester]:
    ingester = SQLIngester(
        _backtest(name),
        engine=OHLCRows(frames),
        aliases=pd.DataFrame(),
        events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
    )
    bundles.register(name, ingester, calendar_name="XNYS")
    try:
        bundles.ingest(name, os.environ, pd.Timestamp("2021-12-01", tz="UTC"), [], False)
        return bundles.load(name, os.environ, None), ingester
    finally:
        bundles.unregister(name)


def _sessions() -> pd.DatetimeIndex:
    return get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2021-10-01"),  # type: ignore[arg-type]
        pd.Timestamp("2021-11-30"),  # type: ignore[arg-type]
    )


def _buy_and_hold(bundle) -> pd.DataFrame:
    calendar = get_calendar("XNYS")
    sessions = _sessions()
    ordered: list[bool] = []

    def handle_data(context, data):
        if not ordered:
            context.order(context.symbol("META"), SHARES)
            ordered.append(True)

    data_portal = DataPortal(
        bundle.asset_finder,
        trading_calendar=calendar,
        first_trading_day=bundle.equity_minute_bar_reader.first_trading_day,
        equity_minute_reader=bundle.equity_minute_bar_reader,
        equity_daily_reader=bundle.equity_daily_bar_reader,
        adjustment_reader=bundle.adjustment_reader,
    )
    algorithm = TradingAlgorithm(
        namespace={},
        data_portal=data_portal,
        trading_calendar=calendar,
        sim_params=SimulationParameters(
            start_session=sessions[0],
            end_session=sessions[-1],
            trading_calendar=calendar,
            data_frequency="daily",
            capital_base=10_000,
        ),
        metrics_set=metrics.load("default"),
        blotter=load(Blotter, "default"),
        benchmark_returns=pd.Series(index=sessions, data=0.0),
        benchmark_sid=None,
        handle_data=handle_data,
    )
    logging.getLogger("zipline").setLevel(logging.ERROR)
    return algorithm.run()


@pytest.fixture
def zipline_root(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))


def test_missing_sessions_lists_sessions_inside_the_bars_only():
    sessions = pd.DatetimeIndex(["2026-04-30", "2026-05-01", "2026-05-04", "2026-05-05", "2026-05-06", "2026-05-07"])
    bars = pd.DataFrame(
        {"open": [9.0, 11.0], "high": [10.5, 11.5], "low": [8.5, 10.5], "close": [10.0, 11.0], "volume": [100, 200]},
        index=pd.DatetimeIndex(["2026-05-01", "2026-05-06"]),
    )
    # 2026-04-30 is before the first bar and 2026-05-07 after the last.
    assert missing_sessions(bars, sessions) == [pd.Timestamp("2026-05-04"), pd.Timestamp("2026-05-05")]


def test_missing_sessions_leaves_complete_bars_alone():
    sessions = pd.DatetimeIndex(["2026-05-01", "2026-05-04"])
    bars = pd.DataFrame(
        {"open": [1.0, 1.0], "high": [1.0, 1.0], "low": [1.0, 1.0], "close": [1.0, 1.0], "volume": [1, 1]},
        index=sessions,
    )
    assert missing_sessions(bars, sessions) == []


def test_missing_sessions_skips_good_friday():
    # 2024-03-29 is Good Friday: not an XNYS session, so no bar is not missing.
    sessions = get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2024-03-27"),  # type: ignore[arg-type]
        pd.Timestamp("2024-04-02"),  # type: ignore[arg-type]
    )
    assert pd.Timestamp("2024-03-29") not in sessions
    days = pd.DatetimeIndex(["2024-03-27", "2024-03-28", "2024-04-01", "2024-04-02"])
    bars = pd.DataFrame({"close": [1.0] * len(days)}, index=days)
    assert missing_sessions(bars, sessions) == []


def test_drop_unpriced_bars_drops_zero_and_negative_prices():
    bars = pd.DataFrame(
        {
            "open": [1.0, 0.0, 1.0, 1.0],
            "high": [1.0, 0.0, 1.0, 1.0],
            "low": [1.0, 0.0, -1.0, 1.0],
            "close": [1.0, 0.0, 1.0, np.nan],
            "volume": [1, 0, 1, 1],
        }
    )
    assert drop_unpriced_bars(bars).index.tolist() == [0]


def _golden() -> pd.DataFrame:
    return pd.read_csv(GOLDEN, parse_dates=["date"]).set_index("date")


@pytest.mark.usefixtures("zipline_root")
def test_gap_fails_the_ingest_and_refetched_bars_match_the_golden_run():
    meta, oef = _yahoo_bars("META"), _yahoo_bars("OEF")

    # The run that used to be filled now fails, naming the ticker and every
    # missing session; 0-priced rows count as missing.
    with pytest.raises(MissingSessionsError) as refused:
        _ingest("meta_gap", {"META": _with_gaps(meta), "OEF": oef})
    assert str(refused.value) == "missing prices on trading days: META 2021-10-20,2021-11-03,2021-11-04,2021-11-16"
    assert list(refused.value.gaps) == ["META"]

    # The same run with the gap refetched gives the golden equity curve.
    refetched, _ = _ingest("meta_refetched", {"META": meta, "OEF": oef})
    run = _buy_and_hold(refetched)
    golden = _golden()
    days = pd.DatetimeIndex([pd.Timestamp(day.date()) for day in run.index])
    assert list(days) == list(golden.index)
    assert run["portfolio_value"].to_numpy() == pytest.approx(golden["portfolio_value"].to_numpy(), abs=1e-6)

    # The filled run was off on exactly the four filled sessions.
    differs = golden.index[(golden["portfolio_value"] - golden["filled_portfolio_value"]).abs() > 1e-6]
    assert [day.date() for day in differs] == [*GAP_DAYS, ZERO_DAY]


@pytest.mark.usefixtures("zipline_root")
def test_bars_that_start_late_or_end_early_still_ingest():
    meta, oef = _yahoo_bars("META"), _yahoo_bars("OEF")
    days = pd.Series(meta["time"]).dt.date
    # A listing after the period start and a delisting before its end:
    # sessions outside the bars are not missing.
    window = cast(pd.DataFrame, meta[(days >= date(2021, 10, 11)) & (days <= date(2021, 11, 19))].copy())
    bundle, _ = _ingest("meta_window", {"META": window, "OEF": oef})
    asset = bundle.asset_finder.lookup_symbol("META", as_of_date=pd.Timestamp("2021-11-19"))
    assert asset.start_date == pd.Timestamp("2021-10-11")
    assert asset.end_date == pd.Timestamp("2021-11-19")


@pytest.mark.usefixtures("zipline_root")
def test_every_gapped_sid_is_named_in_one_error():
    meta, oef = _yahoo_bars("META"), _yahoo_bars("OEF")
    days = pd.Series(oef["time"]).dt.date
    gapped_oef = cast(pd.DataFrame, oef[days != date(2021, 10, 25)].copy())
    with pytest.raises(MissingSessionsError) as refused:
        _ingest("two_gaps", {"META": _with_gaps(meta), "OEF": gapped_oef})
    assert refused.value.gaps.keys() == {"META", "OEF"}
    assert [day.date() for day in refused.value.gaps["OEF"]] == [date(2021, 10, 25)]
