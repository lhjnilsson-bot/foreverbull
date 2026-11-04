"""Cash dividends checked and written into the bundle."""

import sqlite3

from datetime import date
from pathlib import Path
from typing import cast

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.dividends import DIVIDEND_COLUMNS
from foreverbull_zipline.data_bundles.dividends import STOOQ_CASH_DIVIDENDS
from foreverbull_zipline.data_bundles.dividends import DividendProblem
from foreverbull_zipline.data_bundles.dividends import SidBars
from foreverbull_zipline.data_bundles.dividends import books_cash_dividends
from foreverbull_zipline.data_bundles.dividends import build_bundle_dividends
from foreverbull_zipline.data_bundles.dividends import corporate_action_problems
from foreverbull_zipline.data_bundles.dividends import dividend_problems_line
from foreverbull_zipline.data_bundles.dividends import no_dividends
from foreverbull_zipline.data_bundles.dividends import read_foreign_dividends
from foreverbull_zipline.data_bundles.dividends import write_foreign_dividends
from foreverbull_zipline.data_bundles.foreverbull import MissingSessionsError
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.data_bundles.foreverbull import fetched_span_problems
from foreverbull_zipline.errors import PlainSentence
from zipline.data import bundles
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb


CALENDAR = get_calendar("XNYS")
# 2024-03-11 (Mon) to 2024-03-22 (Fri), every session priced at 60.
SESSIONS = CALENDAR.sessions_in_range(pd.Timestamp("2024-03-11"), pd.Timestamp("2024-03-22"))  # type: ignore[arg-type]


def _seconds(day: str) -> int:
    """A date as Zipline's adjustments database stores it."""
    return int(cast(pd.Timestamp, pd.Timestamp(day)).timestamp())


def _closes(sessions=SESSIONS, close: float = 60.0) -> pd.Series:
    return pd.Series(close, index=pd.DatetimeIndex(sessions))


def _dividend(ex_date, **fields) -> dict:
    row = {
        "symbol": "KO",
        "ex_date": pd.Timestamp(ex_date) if ex_date is not None else None,
        "pay_date": None,
        "record_date": None,
        "declared_date": None,
        "amount": 0.485,
        "currency": "USD",
        "is_foreign": False,
    }
    row.update(fields)
    return row


def _build(rows: list[dict], sids=None):
    frame = pd.DataFrame(rows, columns=pd.Index(DIVIDEND_COLUMNS))
    sids = sids or [SidBars(sid=0, symbols=("KO",), closes=_closes())]
    return build_bundle_dividends("yahoo", sids, frame, CALENDAR)


def test_yahoo_and_alpaca_book_cash_dividends_and_stooq_does_not():
    assert books_cash_dividends("yahoo")
    assert books_cash_dividends("alpaca")
    assert not books_cash_dividends("stooq")
    with pytest.raises(ValueError, match="unknown price provider 'polygon'"):
        books_cash_dividends("polygon")


# Cash lands on the ex-date: the bundle's pay date is always the ex-date.


def test_a_dividend_without_a_pay_date_is_paid_on_its_ex_date():
    built, problems = _build([_dividend("2024-03-14")])
    assert problems == []
    assert built.frame.to_dict("records") == [
        {
            "sid": 0,
            "ex_date": pd.Timestamp("2024-03-14"),
            "declared_date": pd.NaT,
            "record_date": pd.NaT,
            "pay_date": pd.Timestamp("2024-03-14"),
            "amount": 0.485,
        }
    ]


@pytest.mark.parametrize(
    "pay_date",
    [
        pd.Timestamp("2024-04-01"),  # the real one, after the bundle
        pd.Timestamp("2024-03-30"),  # a Saturday
        pd.Timestamp("2024-03-13"),  # before the ex-date
    ],
)
def test_the_providers_pay_date_is_not_used(pay_date):
    built, problems = _build([_dividend("2024-03-14", pay_date=pay_date, is_foreign=True)])
    assert problems == []
    assert list(built.frame["pay_date"]) == [pd.Timestamp("2024-03-14")]
    assert built.foreign.to_dict("records") == [{"sid": 0, "ex_date": pd.Timestamp("2024-03-14")}]


def test_dividends_outside_the_bars_are_left_out():
    built, problems = _build([_dividend("2024-03-08"), _dividend("2024-03-25")])
    assert problems == []
    assert built.frame.empty


def test_a_dividend_on_the_first_bar_is_left_out():
    # No position can be held at a close before the sid's first bar.
    built, problems = _build([_dividend("2024-03-11")])
    assert problems == []
    assert built.frame.empty


def test_every_unusable_dividend_is_named():
    closes = _closes().drop(pd.Timestamp("2024-03-18"))
    sids = [SidBars(sid=0, symbols=("KO",), closes=closes)]
    _, problems = _build(
        [
            _dividend(None),
            _dividend("2024-03-12", amount=0.0),
            _dividend("2024-03-13", currency="CAD"),
            _dividend("2024-03-16"),  # a Saturday
            _dividend("2024-03-19"),  # no bar on 03-18
            _dividend("2024-03-22", amount=61.0),
        ],
        sids,
    )
    assert [(p.ex_date, p.reason) for p in problems] == [
        (None, "missing ex-date"),
        (date(2024, 3, 12), "amount is 0, negative or not a number"),
        (date(2024, 3, 13), "currency CAD is not USD"),
        (date(2024, 3, 16), "the ex-date is not a trading day"),
        (date(2024, 3, 19), "there is no stored price on the trading day before the ex-date"),
        (date(2024, 3, 22), "the amount 61 is at or above the 60 close on the trading day before the ex-date"),
    ]


def test_an_empty_currency_is_usd():
    _, problems = _build([_dividend("2024-03-14", currency="")])
    assert problems == []


def test_two_tickers_of_one_instrument_book_a_dividend_once():
    sids = [SidBars(sid=0, symbols=("FB", "META"), closes=_closes())]
    rows = [_dividend("2024-03-14", symbol="FB"), _dividend("2024-03-14", symbol="META")]
    built, problems = _build(rows, sids)
    assert problems == []
    assert len(built.frame) == 1


def test_two_tickers_of_one_instrument_that_disagree_are_refused():
    sids = [SidBars(sid=0, symbols=("FB", "META"), closes=_closes())]
    rows = [_dividend("2024-03-14", symbol="FB"), _dividend("2024-03-14", symbol="META", amount=0.5)]
    _, problems = _build(rows, sids)
    assert [p.reason for p in problems] == [
        "FB and META are the same company but have different dividends on this ex-date"
    ]


def test_the_refusal_names_the_backtest_and_each_dividend():
    _, problems = _build([_dividend("2024-03-13", currency="CAD"), _dividend(None)])
    assert dividend_problems_line("sp100", problems) == (
        "Couldn't load data for backtest 'sp100': dividends for 1 symbol can't be booked. The backtest wasn't saved.\n"
        "  yahoo KO ex-date 2024-03-13: currency CAD is not USD.\n"
        "  yahoo KO: missing ex-date.\n"
        "Use another provider with --provider, or report it at "
        "https://github.com/lhjnilsson/foreverbull/issues with this message."
    )


def test_bars_stored_before_dividends_were_fetched_are_refused():
    problems = fetched_span_problems(
        "yahoo",
        {"KO": (date(2024, 3, 12), date(2024, 3, 22)), "PEP": (date(2024, 1, 2), date(2024, 12, 31))},
        {
            "KO": (date(2024, 3, 11), date(2024, 3, 22)),
            "PEP": (date(2024, 3, 11), date(2024, 3, 22)),
            "AAPL": (date(2024, 3, 11), date(2024, 3, 22)),
        },
    )
    assert [p.line() for p in problems] == [
        (
            "  yahoo KO: its dividends were loaded for 2024-03-12 to 2024-03-22, "
            "not for its prices from 2024-03-11 to 2024-03-22."
        ),
        "  yahoo AAPL: its dividends were never loaded.",
    ]


def test_foreign_dividends_round_trip_and_an_old_bundle_has_no_table():
    connection = sqlite3.connect(":memory:")
    assert read_foreign_dividends(connection) is None
    write_foreign_dividends(connection, pd.DataFrame({"sid": [3], "ex_date": [pd.Timestamp("2024-03-14")]}))
    assert read_foreign_dividends(connection) == {(3, pd.Timestamp("2024-03-14"))}


class _StoredRows:
    """Bars plus the broker's dividend tables, as DatabaseEngine reads them."""

    def __init__(
        self,
        bars: pd.DataFrame,
        dividends: pd.DataFrame,
        fetched: dict[str, tuple[date, date]],
        actions: pd.DataFrame | None = None,
    ):
        self.bars = bars
        self.dividends = dividends
        self.fetched = fetched
        self.actions = actions if actions is not None else pd.DataFrame(columns=pd.Index(["symbol", "kind", "ex_date"]))
        self.provider = "yahoo"

    def get_data(self, symbol, from_date, to_date):
        days = self.bars["time"].dt.date
        return self.bars[(days >= from_date) & (days <= to_date)].copy()

    def get_dividends(self, symbols, from_date, to_date):
        return self.dividends

    def get_dividend_fetches(self, symbols):
        return self.fetched

    def get_corporate_actions(self, symbols, from_date, to_date):
        return self.actions


def _ingest(tmp_path, monkeypatch, engine, provider="yahoo", benchmark="", **given) -> None:
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    backtest = backtest_pb.Backtest(
        name="ko_stored",
        provider=provider,
        benchmark=benchmark,
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2024, month=2, day=28),
                end_date=common_pb.Date(year=2024, month=7, day=31),
                assets=[backtest_pb.Backtest.Period.Asset(symbol="KO", enabled=True)],
            )
        ],
    )
    ingester = SQLIngester(
        backtest,
        engine=engine,
        aliases=pd.DataFrame(),
        events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
        **given,
    )
    bundles.register("ko_stored", ingester, calendar_name="XNYS")
    try:
        bundles.ingest("ko_stored", {"ZIPLINE_ROOT": str(tmp_path)}, pd.Timestamp("2024-08-01", tz="UTC"), [], False)
    finally:
        bundles.unregister("ko_stored")


def _ko_bars() -> pd.DataFrame:
    frame = pd.read_csv(Path(__file__).parent / "testdata" / "yahoo_ko_2024-02-28_2024-07-31.csv")
    frame["time"] = pd.to_datetime(frame["time"], utc=True)
    return frame


def test_ingest_reads_stored_dividends_and_refuses_unfetched_bars(tmp_path, monkeypatch):
    dividends = pd.DataFrame([_dividend("2024-03-14", pay_date=pd.Timestamp("2024-04-01"))])
    engine = _StoredRows(_ko_bars(), dividends, {"KO": (date(2024, 3, 1), date(2024, 7, 31))})
    with pytest.raises(PlainSentence) as refused:
        _ingest(tmp_path, monkeypatch, engine)
    assert str(refused.value) == (
        "Couldn't load data for backtest 'ko_stored': dividends for 1 symbol weren't loaded. The backtest wasn't saved.\n"
        "  yahoo KO: its dividends were loaded for 2024-03-01 to 2024-07-31, "
        "not for its prices from 2024-02-28 to 2024-07-31.\n"
        "Run `fbull backtest create` again to load the missing dividends, or use another provider with --provider."
    )
    # Nothing was written.
    assert not list((tmp_path / "data").glob("ko_stored/*/adjustments.sqlite"))

    engine.fetched = {"KO": (date(2024, 2, 28), date(2024, 7, 31))}
    _ingest(tmp_path, monkeypatch, engine)
    (adjustments,) = list((tmp_path / "data").glob("ko_stored/*/adjustments.sqlite"))
    connection = sqlite3.connect(adjustments)
    assert connection.execute("SELECT ex_date, pay_date, amount FROM dividend_payouts").fetchall() == [
        (_seconds("2024-03-14"), _seconds("2024-03-14"), 0.485)
    ]
    assert read_foreign_dividends(connection) == set()


KO_SPAN = (date(2024, 2, 28), date(2024, 7, 31))


def _nothing_written(tmp_path) -> bool:
    return not list((tmp_path / "data").glob("ko_stored/*/adjustments.sqlite"))


def _refusal(tmp_path, monkeypatch, engine, **ingest) -> str:
    with pytest.raises(PlainSentence) as refused:
        _ingest(tmp_path, monkeypatch, engine, **ingest)
    assert _nothing_written(tmp_path)
    return str(refused.value)


def test_ingest_refuses_a_benchmark_whose_dividends_were_never_loaded(tmp_path, monkeypatch):
    # Rule 6.1: the benchmark's dividend fetch failed, so the broker stored
    # none of its bars' dividends.
    engine = _StoredRows(_ko_bars(), pd.DataFrame([_dividend("2024-03-14")]), {"KO": KO_SPAN})
    assert _refusal(tmp_path, monkeypatch, engine, benchmark="SPY") == (
        "Couldn't load data for backtest 'ko_stored': dividends for 1 symbol weren't loaded. The backtest wasn't saved.\n"
        "  yahoo SPY: its dividends were never loaded.\n"
        "Run `fbull backtest create` again to load the missing dividends, or use another provider with --provider."
    )


def test_ingest_refuses_cash_dividends_on_stooq(tmp_path, monkeypatch):
    # Rule 6.2: asking a stooq backtest to book cash dividends fails with the
    # reason, not by dropping them.
    engine = _StoredRows(_ko_bars(), no_dividends(), {})
    refused = _refusal(
        tmp_path, monkeypatch, engine, provider="stooq", dividends=pd.DataFrame([_dividend("2024-03-14")])
    )
    assert refused == (
        "Couldn't load data for backtest 'ko_stored': foreverbull has cash dividends stored for stooq, "
        "whose prices already include them, so they'd be counted twice. The backtest wasn't saved. "
        "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
    )
    assert STOOQ_CASH_DIVIDENDS in refused


def test_a_stooq_backtest_without_dividends_books_none(tmp_path, monkeypatch):
    engine = _StoredRows(_ko_bars(), pd.DataFrame([_dividend("2024-03-14")]), {})
    _ingest(tmp_path, monkeypatch, engine, provider="stooq")
    (adjustments,) = list((tmp_path / "data").glob("ko_stored/*/adjustments.sqlite"))
    assert sqlite3.connect(adjustments).execute("SELECT count(*) FROM dividend_payouts").fetchone() == (0,)


@pytest.mark.parametrize(
    ("dividend", "line"),
    [
        # Rule 6.3: missing ex-date, amount <= 0, non-USD.
        (_dividend(None), "  yahoo KO: missing ex-date."),
        (_dividend("2024-03-14", amount=0.0), "  yahoo KO ex-date 2024-03-14: amount is 0, negative or not a number."),
        (_dividend("2024-03-14", currency="EUR"), "  yahoo KO ex-date 2024-03-14: currency EUR is not USD."),
        # Rule 6.4: an ex-date that is not a trading day.
        (_dividend("2024-03-16"), "  yahoo KO ex-date 2024-03-16: the ex-date is not a trading day."),
    ],
)
def test_ingest_refuses_a_dividend_that_cant_be_booked(tmp_path, monkeypatch, dividend, line):
    engine = _StoredRows(_ko_bars(), pd.DataFrame([dividend]), {"KO": KO_SPAN})
    assert _refusal(tmp_path, monkeypatch, engine) == (
        "Couldn't load data for backtest 'ko_stored': dividends for 1 symbol can't be booked. The backtest wasn't saved.\n"
        f"{line}\n"
        "Use another provider with --provider, or report it at "
        "https://github.com/lhjnilsson/foreverbull/issues with this message."
    )


def test_ingest_refuses_a_dividend_whose_day_before_has_no_bar_with_the_gap(tmp_path, monkeypatch):
    # The trading day before the 03-14 ex-date has no bar. The missing-session
    # check refuses the ingest before any dividend is checked, so the reason
    # "there is no stored price on the trading day before the ex-date" can't
    # be reached here (test_every_unusable_dividend_is_named pins it), and
    # nothing is written.
    bars = _ko_bars()
    bars = cast(pd.DataFrame, bars[bars["time"].dt.date != date(2024, 3, 13)])
    engine = _StoredRows(bars, pd.DataFrame([_dividend("2024-03-14")]), {"KO": KO_SPAN})
    with pytest.raises(MissingSessionsError) as refused:
        _ingest(tmp_path, monkeypatch, engine)
    assert str(refused.value) == "missing prices on trading days: KO 2024-03-13"
    assert _nothing_written(tmp_path)


def _actions(*rows: tuple[str, str]) -> pd.DataFrame:
    return pd.DataFrame(
        [{"symbol": "KO", "kind": kind, "ex_date": pd.Timestamp(day)} for kind, day in rows],
        columns=pd.Index(["symbol", "kind", "ex_date"]),
    )


def test_ingest_refuses_a_corporate_action_inside_the_window(tmp_path, monkeypatch):
    # Rule 6.5: a spin-off inside the backtest's window fails it.
    engine = _StoredRows(
        _ko_bars(),
        pd.DataFrame([_dividend("2024-03-14")]),
        {"KO": KO_SPAN},
        actions=_actions(("spin_off", "2024-05-06")),
    )
    assert _refusal(tmp_path, monkeypatch, engine, provider="alpaca") == (
        "Couldn't load data for backtest 'ko_stored': 1 corporate action can't be booked. The backtest wasn't saved.\n"
        "  alpaca KO ex-date 2024-05-06: a spin-off, which foreverbull can't book.\n"
        "Change the backtest dates to leave out the corporate action, then run `fbull backtest create` again."
    )


def test_ingest_books_a_window_that_ends_before_a_corporate_action(tmp_path, monkeypatch):
    # Rule 6.5: a merger after the window's end, or a stock dividend before
    # its start, changes nothing in the run.
    engine = _StoredRows(
        _ko_bars(),
        pd.DataFrame([_dividend("2024-03-14")]),
        {"KO": KO_SPAN},
        actions=_actions(("cash_merger", "2024-08-15"), ("stock_dividend", "2024-02-27")),
    )
    _ingest(tmp_path, monkeypatch, engine, provider="alpaca")
    (adjustments,) = list((tmp_path / "data").glob("ko_stored/*/adjustments.sqlite"))
    assert sqlite3.connect(adjustments).execute("SELECT ex_date, amount FROM dividend_payouts").fetchall() == [
        (_seconds("2024-03-14"), 0.485)
    ]


def test_each_refused_dividend_gets_the_step_that_can_fix_it():
    # Dividends that don't cover the prices are loaded again; a dividend the
    # provider sent wrong comes back the same, so its step is another provider.
    problems = [
        DividendProblem("yahoo", "SPY", None, "its dividends were never loaded", reload=True),
        DividendProblem("yahoo", "KO", date(2024, 3, 14), "currency EUR is not USD"),
    ]
    assert dividend_problems_line("sp100", problems) == (
        "Couldn't load data for backtest 'sp100': dividends for 2 symbols can't be booked. The backtest wasn't saved.\n"
        "  yahoo SPY: its dividends were never loaded.\n"
        "  yahoo KO ex-date 2024-03-14: currency EUR is not USD.\n"
        "Run `fbull backtest create` again to load the missing dividends. For the others, use another provider "
        "with --provider, or report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
    )


def test_dividends_that_werent_loaded_and_a_corporate_action_share_one_refusal():
    problems = [
        DividendProblem("alpaca", "SPY", None, "its dividends were never loaded", reload=True),
        DividendProblem("alpaca", "KO", None, "its dividends were never loaded", reload=True),
        *corporate_action_problems(
            "alpaca", ["KO"], _actions(("spin_off", "2024-05-06")), date(2024, 2, 28), date(2024, 7, 31)
        ),
    ]
    assert dividend_problems_line("sp100", problems) == (
        "Couldn't load data for backtest 'sp100': dividends for 2 symbols weren't loaded and 1 corporate action "
        "can't be booked. The backtest wasn't saved.\n"
        "  alpaca SPY: its dividends were never loaded.\n"
        "  alpaca KO: its dividends were never loaded.\n"
        "  alpaca KO ex-date 2024-05-06: a spin-off, which foreverbull can't book.\n"
        "Run `fbull backtest create` again to load the missing dividends, or use another provider with --provider. "
        "Change the backtest dates to leave out the corporate action, then run `fbull backtest create` again."
    )


@pytest.mark.parametrize(
    ("kind", "reason"),
    [
        ("stock_dividend", "a stock dividend, which foreverbull can't book"),
        ("spin_off", "a spin-off, which foreverbull can't book"),
        ("cash_merger", "a cash merger, which foreverbull can't book"),
        ("stock_merger", "a stock merger, which foreverbull can't book"),
        ("stock_and_cash_merger", "a stock and cash merger, which foreverbull can't book"),
    ],
)
def test_each_corporate_action_is_named(kind, reason):
    (problem,) = corporate_action_problems(
        "alpaca", ["KO"], _actions((kind, "2024-05-06")), date(2024, 2, 28), date(2024, 7, 31)
    )
    assert problem.reason == reason


def test_dividend_and_corporate_action_problems_share_one_refusal():
    problems = [
        DividendProblem("alpaca", "KO", date(2024, 3, 14), "currency EUR is not USD"),
        *corporate_action_problems(
            "alpaca",
            ["KO"],
            _actions(("spin_off", "2024-05-06"), ("cash_merger", "2024-09-02")),
            date(2024, 2, 28),
            date(2024, 7, 31),
        ),
    ]
    assert dividend_problems_line("sp100", problems) == (
        "Couldn't load data for backtest 'sp100': dividends for 1 symbol and 1 corporate action can't be booked. "
        "The backtest wasn't saved.\n"
        "  alpaca KO ex-date 2024-03-14: currency EUR is not USD.\n"
        "  alpaca KO ex-date 2024-05-06: a spin-off, which foreverbull can't book.\n"
        "Use another provider with --provider, or report it at "
        "https://github.com/lhjnilsson/foreverbull/issues with this message. "
        "Change the backtest dates to leave out the corporate action, then run `fbull backtest create` again."
    )
