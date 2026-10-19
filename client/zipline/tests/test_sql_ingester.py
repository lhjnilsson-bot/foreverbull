from datetime import UTC
from datetime import date
from datetime import datetime

import pandas as pd

from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.data_bundles.foreverbull import bundle_provider
from foreverbull_zipline.data_bundles.foreverbull import group_symbols_by_instrument
from foreverbull_zipline.data_bundles.foreverbull import membership_auto_close_date
from foreverbull_zipline.data_bundles.foreverbull import session_labels

from foreverbull.models.finance import DEFAULT_PROVIDER
from foreverbull.models.finance import ohlc_provider
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb


META_ISIN = "US30303M1027"
AAPL_ISIN = "US0378331005"


def _bars(symbol: str, days: list[date], price: float) -> pd.DataFrame:
    return pd.DataFrame(
        {
            "open": [price] * len(days),
            "high": [price] * len(days),
            "low": [price] * len(days),
            "close": [price] * len(days),
            "volume": [100] * len(days),
            "time": [datetime(d.year, d.month, d.day) for d in days],
        }
    )


def _bars_at_open(days: list[date], price: float) -> pd.DataFrame:
    # Yahoo stamps a daily bar at the 09:30 ET session open, 14:30 UTC.
    return pd.DataFrame(
        {
            "open": [price] * len(days),
            "high": [price] * len(days),
            "low": [price] * len(days),
            "close": [price] * len(days),
            "volume": [100] * len(days),
            "time": [datetime(d.year, d.month, d.day, 14, 30, tzinfo=UTC) for d in days],
        }
    )


class FakeEngine:
    def __init__(self, frames: dict[str, pd.DataFrame]):
        self.frames = frames

    def get_data(self, symbol, from_date, to_date):
        return self.frames[symbol].copy()


def _backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name="sp100",
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2022, month=12, day=1),
                end_date=common_pb.Date(year=2023, month=2, day=28),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="FB", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="AAPL", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True),
                ],
            )
        ],
    )


def test_ohlc_provider_unset_defaults_to_yahoo(monkeypatch):
    monkeypatch.delenv("OHLC_PROVIDER", raising=False)
    assert ohlc_provider() == "yahoo"
    monkeypatch.setenv("OHLC_PROVIDER", "  ")
    assert ohlc_provider() == "yahoo"


def test_bundle_provider_uses_backtest_before_env(monkeypatch):
    # The env value differs from DEFAULT_PROVIDER so each fallback step is visible.
    monkeypatch.setenv("OHLC_PROVIDER", "stooq")
    assert bundle_provider(backtest_pb.Backtest(provider="alpaca")) == "alpaca"
    assert bundle_provider(backtest_pb.Backtest()) == "stooq"
    monkeypatch.delenv("OHLC_PROVIDER", raising=False)
    assert bundle_provider(backtest_pb.Backtest()) == DEFAULT_PROVIDER


class _ProviderEngine:
    def __init__(self):
        self.provider = ""

    def get_data(self, symbol, from_date, to_date):
        return None


def test_sql_ingester_threads_backtest_provider(monkeypatch):
    monkeypatch.setenv("OHLC_PROVIDER", "stooq")
    engine = _ProviderEngine()
    SQLIngester(backtest_pb.Backtest(name="sp100", provider="yahoo"), engine=engine)
    assert engine.provider == "yahoo"


def test_membership_auto_close_date_uses_drop_not_last_bar():
    end = pd.Timestamp("2023-12-29")
    assert membership_auto_close_date(end, None) == pd.Timestamp("2023-12-30")
    assert membership_auto_close_date(end, date(2023, 2, 28)) == pd.Timestamp("2023-02-28")


def test_group_symbols_by_instrument_keeps_rename_on_one_sid():
    aliases = pd.DataFrame(
        {
            "isin": [META_ISIN, META_ISIN, AAPL_ISIN],
            "symbol": ["FB", "META", "AAPL"],
            "from_date": [date(2022, 12, 31), date(2023, 1, 31), date(2022, 12, 31)],
            "to_date": [date(2023, 1, 31), None, date(2023, 2, 28)],
            "member_to": [None, None, date(2023, 2, 28)],
        }
    )
    groups = group_symbols_by_instrument(["AAPL", "FB", "META", "OEF"], aliases)
    assert len(groups) == 3
    meta = next(group for group in groups if group.aliases[0].symbol in {"FB", "META"})
    assert [alias.symbol for alias in meta.aliases] == ["FB", "META"]
    assert meta.member_to is None
    aapl = next(group for group in groups if group.aliases[0].symbol == "AAPL")
    assert aapl.member_to == date(2023, 2, 28)
    oef = next(group for group in groups if group.aliases[0].symbol == "OEF")
    assert len(oef.aliases) == 1
    assert oef.member_to is None


def test_sql_ingester_one_sid_for_ticker_change_and_autoclose_on_drop():
    engine = FakeEngine(
        {
            "FB": _bars("FB", [date(2022, 12, 30), date(2023, 1, 30)], 10.0),
            "META": _bars("META", [date(2023, 1, 31), date(2023, 2, 27)], 11.0),
            "AAPL": _bars("AAPL", [date(2022, 12, 30), date(2023, 2, 27)], 12.0),
            "OEF": _bars("OEF", [date(2022, 12, 30), date(2023, 2, 27)], 13.0),
        }
    )
    aliases = pd.DataFrame(
        {
            "isin": [META_ISIN, META_ISIN, AAPL_ISIN],
            "symbol": ["FB", "META", "AAPL"],
            "from_date": [date(2022, 12, 31), date(2023, 1, 31), date(2022, 12, 31)],
            "to_date": [date(2023, 1, 31), None, date(2023, 2, 28)],
            "member_to": [None, None, date(2023, 2, 28)],
        }
    )
    ingester = SQLIngester(_backtest(), engine=engine, aliases=aliases)
    ingester._df_metadata = ingester.create_metadata()
    ingested = list(ingester.writer(False))
    metadata = pd.DataFrame(ingester._df_metadata)

    sids = sorted(set(metadata["sid"].tolist()))
    assert len(sids) == 3
    meta_rows = metadata.loc[metadata["symbol"].isin(["FB", "META"])]
    assert sorted(meta_rows["symbol"].tolist()) == ["FB", "META"]
    assert int(meta_rows["sid"].nunique()) == 1
    assert int(meta_rows["auto_close_date"].nunique()) == 1
    assert pd.Timestamp(meta_rows["auto_close_date"].iloc[0]) == pd.Timestamp("2023-02-28")

    aapl_rows = metadata.loc[metadata["symbol"] == "AAPL"]
    assert len(aapl_rows) == 1
    assert pd.Timestamp(aapl_rows["auto_close_date"].iloc[0]) == pd.Timestamp("2023-02-28")

    meta_sid = int(meta_rows["sid"].iloc[0])
    combined = dict(ingested)[meta_sid]
    assert combined.index.min() == pd.Timestamp("2022-12-30")
    assert combined.index.max() == pd.Timestamp("2023-02-27")


def test_sql_ingester_sets_auto_close_when_no_membership_table():
    engine = FakeEngine(
        {
            "AAPL": _bars("AAPL", [date(2022, 12, 1), date(2023, 2, 28)], 12.0),
            "FB": _bars("FB", [date(2022, 12, 1), date(2023, 2, 28)], 10.0),
            "META": _bars("META", [date(2022, 12, 1), date(2023, 2, 28)], 11.0),
            "OEF": _bars("OEF", [date(2022, 12, 1), date(2023, 2, 28)], 13.0),
        }
    )
    ingester = SQLIngester(_backtest(), engine=engine, aliases=pd.DataFrame())
    ingester._df_metadata = ingester.create_metadata()
    list(ingester.writer(False))
    metadata = pd.DataFrame(ingester._df_metadata)
    assert "autoclose_date" not in metadata.columns
    assert bool(metadata["auto_close_date"].notna().all())
    aapl = metadata.loc[metadata["symbol"] == "AAPL"].iloc[0]
    assert pd.Timestamp(aapl["auto_close_date"]) == pd.Timestamp("2023-03-01")


def test_session_labels_drop_the_session_open_time():
    labels = session_labels(["2023-01-03T14:30:00Z", "2023-01-04T14:30:00Z"])
    assert list(labels) == [pd.Timestamp("2023-01-03"), pd.Timestamp("2023-01-04")]
    assert list(session_labels(["2023-01-03", "2023-01-04"])) == [
        pd.Timestamp("2023-01-03"),
        pd.Timestamp("2023-01-04"),
    ]


def test_sql_ingester_aligns_bars_to_the_first_session():
    # The benchmark (OEF) and every asset start on 2023-01-03, stamped at
    # 14:30 UTC. Their Zipline start must be that session's label, or
    # Zipline refuses the benchmark as not yet trading on 2023-01-03.
    days = [date(2023, 1, 3), date(2023, 1, 4)]
    engine = FakeEngine(
        {
            "FB": _bars_at_open(days, 10.0),
            "AAPL": _bars_at_open(days, 12.0),
            "META": _bars_at_open(days, 11.0),
            "OEF": _bars_at_open(days, 13.0),
        }
    )
    ingester = SQLIngester(_backtest(), engine=engine, aliases=pd.DataFrame())
    ingester._df_metadata = ingester.create_metadata()
    ingested = list(ingester.writer(False))
    metadata = pd.DataFrame(ingester._df_metadata)

    first_session = pd.Timestamp("2023-01-03")
    for _, frame in ingested:
        assert list(frame.index) == [first_session, pd.Timestamp("2023-01-04")]
    assert set(metadata["start_date"].tolist()) == {first_session}
    assert set(metadata["end_date"].tolist()) == {pd.Timestamp("2023-01-04")}
    benchmark_rows = metadata.loc[metadata["symbol"] == "OEF"]
    assert pd.Timestamp(benchmark_rows["start_date"].iloc[0]) == first_session
