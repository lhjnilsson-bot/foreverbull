from datetime import UTC
from datetime import date
from datetime import datetime

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.foreverbull import MissingSessionsError
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.data_bundles.foreverbull import bundle_provider
from foreverbull_zipline.data_bundles.foreverbull import group_symbols_by_instrument
from foreverbull_zipline.data_bundles.foreverbull import membership_auto_close_date
from foreverbull_zipline.data_bundles.foreverbull import session_labels
from foreverbull_zipline.errors import PlainSentence
from sqlalchemy import create_engine
from zipline.utils.calendar_utils import get_calendar

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


def _session_bars(symbol: str, first: date, last: date, price: float) -> pd.DataFrame:
    # A bar on every XNYS session from first to last: the ingester refuses a
    # sid that skips one.
    sessions = get_calendar("XNYS").sessions_in_range(pd.Timestamp(first), pd.Timestamp(last))  # type: ignore[arg-type]
    return _bars(symbol, [day.date() for day in sessions], price)


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


def test_membership_auto_close_date_is_the_drop_or_the_day_after_the_last_bar():
    newest = pd.Timestamp("2023-12-29")
    bundle_end = date(2023, 12, 31)

    def close(end, dropped):
        return membership_auto_close_date(end, dropped, newest_member_end=newest, bundle_end=bundle_end)

    # The stored data ends here: no early close, only a drop closes it.
    assert close(newest, None) == pd.Timestamp("2024-01-01")
    assert close(newest, date(2023, 2, 28)) == pd.Timestamp("2023-02-28")
    # Delisted mid-month: the bars end before the newest member bar and
    # before the month-end drop, so the position closes the day after the
    # last bar, when the broker leaves the member out.
    assert close(pd.Timestamp("2023-02-10"), date(2023, 2, 28)) == pd.Timestamp("2023-02-11")
    assert close(pd.Timestamp("2023-02-10"), None) == pd.Timestamp("2023-02-11")
    assert close(pd.Timestamp("2023-02-10"), date(2023, 2, 5)) == pd.Timestamp("2023-02-05")


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


def test_group_symbols_by_instrument_follows_the_final_membership_interval():
    # One row per ticker and membership interval: AAPL dropped on 01-31 and
    # added back on 02-28. The rows come back in either interval order.
    def group(member_to: list[date | None]):
        aliases = pd.DataFrame(
            {
                "isin": [AAPL_ISIN] * len(member_to),
                "symbol": ["AAPL"] * len(member_to),
                "from_date": [date(2022, 12, 31)] * len(member_to),
                "to_date": [None] * len(member_to),
                "member_to": member_to,
            }
        )
        groups = group_symbols_by_instrument(["AAPL"], aliases)
        assert len(groups) == 1
        assert [alias.symbol for alias in groups[0].aliases] == ["AAPL"]
        return groups[0].member_to

    assert group([date(2023, 1, 31), None]) is None
    assert group([None, date(2023, 1, 31)]) is None
    assert group([date(2023, 1, 31), date(2023, 3, 15)]) == date(2023, 3, 15)
    assert group([date(2023, 3, 15), date(2023, 1, 31)]) == date(2023, 3, 15)


def test_sql_ingester_one_sid_for_ticker_change_and_autoclose_on_drop():
    engine = FakeEngine(
        {
            "FB": _session_bars("FB", date(2022, 12, 30), date(2023, 1, 30), 10.0),
            "META": _session_bars("META", date(2023, 1, 31), date(2023, 2, 27), 11.0),
            "AAPL": _session_bars("AAPL", date(2022, 12, 30), date(2023, 2, 27), 12.0),
            "OEF": _session_bars("OEF", date(2022, 12, 30), date(2023, 2, 27), 13.0),
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
    # Every member's bars end 2023-02-27, the newest member bar: the data
    # ends there, so META is not closed on the 02-28 session, only after
    # the bundle ends.
    assert pd.Timestamp(meta_rows["auto_close_date"].iloc[0]) == pd.Timestamp("2023-03-01")

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
            "AAPL": _session_bars("AAPL", date(2022, 12, 1), date(2023, 2, 28), 12.0),
            "FB": _session_bars("FB", date(2022, 12, 1), date(2023, 2, 28), 10.0),
            "META": _session_bars("META", date(2022, 12, 1), date(2023, 2, 28), 11.0),
            "OEF": _session_bars("OEF", date(2022, 12, 1), date(2023, 2, 28), 13.0),
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


def test_sql_ingester_names_every_symbol_without_bars_and_writes_nothing():
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "AAPL": _session_bars("AAPL", date(2022, 12, 1), date(2023, 2, 28), 12.0),
            "META": _bars("META", [], 11.0),
            "OEF": _session_bars("OEF", date(2022, 12, 1), date(2023, 2, 28), 13.0),
        }
    )
    ingester = SQLIngester(_backtest(), engine=engine, aliases=pd.DataFrame())
    ingester._df_metadata = ingester.create_metadata()
    written = []

    with pytest.raises(PlainSentence) as error:
        written.extend(ingester.writer(False))

    assert str(error.value) == (
        "Couldn't load data for backtest 'sp100': 2 symbols have no stored prices. The backtest wasn't saved.\n"
        "  FB: no prices between 2022-12-01 and 2023-02-28.\n"
        "  META: no prices between 2022-12-01 and 2023-02-28.\n"
        "Try another provider with --provider, then run `fbull backtest create` again."
    )
    assert written == []
    assert ingester._df_metadata.empty


def _quarter_backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name="oef-2024q1",
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2024, month=1, day=1),
                end_date=common_pb.Date(year=2024, month=3, day=31),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="FB", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True),
                ],
            )
        ],
    )


def _meta_rename() -> pd.DataFrame:
    return pd.DataFrame(
        {
            "isin": [META_ISIN, META_ISIN],
            "symbol": ["FB", "META"],
            "from_date": [date(2012, 5, 18), date(2022, 6, 9)],
            "to_date": [date(2022, 6, 9), None],
            "member_to": [None, None],
        }
    )


def test_sql_ingester_names_a_renamed_symbol_without_bars_by_its_latest_ticker():
    # The run spans the first and last 2024 Q1 sessions: 2024-01-01 is New
    # Year's Day and 2024-03-29 Good Friday.
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "META": _bars("META", [], 11.0),
            "OEF": _session_bars("OEF", date(2024, 1, 2), date(2024, 3, 28), 13.0),
        }
    )
    ingester = SQLIngester(_quarter_backtest(), engine=engine, aliases=_meta_rename())
    ingester._df_metadata = ingester.create_metadata()

    with pytest.raises(PlainSentence) as error:
        list(ingester.writer(False))

    assert str(error.value) == (
        "Couldn't load data for backtest 'oef-2024q1': there are no stored prices for META (earlier FB) "
        "between 2024-01-02 and 2024-03-28. The backtest wasn't saved. "
        "Try another provider with --provider, then run `fbull backtest create` again."
    )


def test_sql_ingester_names_a_symbol_without_an_earlier_ticker_alone():
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "META": _session_bars("META", date(2024, 1, 2), date(2024, 3, 28), 11.0),
            "OEF": _session_bars("OEF", date(2024, 1, 2), date(2024, 3, 28), 13.0),
        }
    )
    ingester = SQLIngester(_quarter_backtest(), engine=engine, aliases=pd.DataFrame())
    ingester._df_metadata = ingester.create_metadata()

    with pytest.raises(PlainSentence) as error:
        list(ingester.writer(False))

    assert str(error.value) == (
        "Couldn't load data for backtest 'oef-2024q1': there are no stored prices for FB "
        "between 2024-01-02 and 2024-03-28. The backtest wasn't saved. "
        "Try another provider with --provider, then run `fbull backtest create` again."
    )


def test_sql_ingester_lists_a_renamed_symbol_in_the_block():
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "META": _bars("META", [], 11.0),
            "OEF": _bars("OEF", [], 13.0),
        }
    )
    ingester = SQLIngester(_quarter_backtest(), engine=engine, aliases=_meta_rename())
    ingester._df_metadata = ingester.create_metadata()

    with pytest.raises(PlainSentence) as error:
        list(ingester.writer(False))

    assert str(error.value) == (
        "Couldn't load data for backtest 'oef-2024q1': 2 symbols have no stored prices. The backtest wasn't saved.\n"
        "  META (earlier FB): no prices between 2024-01-02 and 2024-03-28.\n"
        "  OEF: no prices between 2024-01-02 and 2024-03-28.\n"
        "Try another provider with --provider, then run `fbull backtest create` again."
    )


def test_sql_ingester_names_symbols_without_bars_and_gaps_in_one_error():
    # A symbol with no bars and another with a missing session are both
    # named, and nothing is written.
    sessions = get_calendar("XNYS").sessions_in_range(pd.Timestamp("2024-01-02"), pd.Timestamp("2024-03-28"))  # type: ignore[arg-type]
    gapped = [day.date() for day in sessions if day not in (pd.Timestamp("2024-02-14"), pd.Timestamp("2024-02-20"))]
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "META": _bars("META", [], 11.0),
            "OEF": _bars("OEF", gapped, 13.0),
        }
    )
    ingester = SQLIngester(_quarter_backtest(), engine=engine, aliases=_meta_rename())
    ingester._df_metadata = ingester.create_metadata()
    written = []

    with pytest.raises(PlainSentence) as error:
        written.extend(ingester.writer(False))

    assert str(error.value) == (
        "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices. The backtest wasn't saved.\n"
        "  META (earlier FB): no prices between 2024-01-02 and 2024-03-28.\n"
        "  OEF: no prices on 2024-02-14, 2024-02-20.\n"
        "Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again."
    )
    assert written == []
    assert ingester._df_metadata.empty


def test_sql_ingester_writes_nothing_when_a_later_sid_has_a_gap():
    sessions = get_calendar("XNYS").sessions_in_range(pd.Timestamp("2024-01-02"), pd.Timestamp("2024-03-28"))  # type: ignore[arg-type]
    gapped = [day.date() for day in sessions if day != pd.Timestamp("2024-02-14")]
    engine = FakeEngine(
        {
            "FB": _bars("FB", [], 10.0),
            "META": _session_bars("META", date(2024, 1, 2), date(2024, 3, 28), 11.0),
            "OEF": _bars("OEF", gapped, 13.0),
        }
    )
    ingester = SQLIngester(_quarter_backtest(), engine=engine, aliases=_meta_rename())
    ingester._df_metadata = ingester.create_metadata()
    written = []

    with pytest.raises(MissingSessionsError) as error:
        written.extend(ingester.writer(False))

    assert list(error.value.gaps) == ["OEF"]
    assert written == []
    assert ingester._df_metadata.empty


class _BrokenDatabase:
    def __init__(self):
        self._engine = create_engine("sqlite://")


def test_sql_ingester_fails_when_the_ticker_history_cant_be_read(caplog):
    ingester = SQLIngester(_quarter_backtest(), engine=_BrokenDatabase())

    with pytest.raises(PlainSentence) as error:
        ingester.load_aliases()

    assert str(error.value) == (
        "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't read its record of ticker changes, "
        "so renamed symbols can't be followed. The backtest wasn't saved. "
        "Run `fbenv status` to check that the environment is running, then run `fbull backtest create` again."
    )
    assert error.value.__cause__ is None
    assert "sqlite3.OperationalError" not in str(error.value)
    assert "Couldn't read the ticker history of backtest 'oef-2024q1'" in caplog.text
    assert "(sqlite3.OperationalError) no such table: universe_member" in caplog.text


def test_sql_ingester_logs_the_ticker_history_cause_redacted(caplog, monkeypatch):
    from sqlalchemy.exc import OperationalError

    def broken_query(*_args, **_kwargs):
        raise OperationalError("SELECT 1", {}, Exception("connect to postgresql://fb:hunter2@db.invalid:5432 failed"))

    monkeypatch.setattr("foreverbull_zipline.data_bundles.foreverbull.read_sql_query", broken_query)
    ingester = SQLIngester(_quarter_backtest(), engine=_BrokenDatabase())

    with pytest.raises(PlainSentence):
        ingester.load_aliases()

    assert "postgresql://fb:***@db.invalid:5432" in caplog.text
    assert "hunter2" not in caplog.text
