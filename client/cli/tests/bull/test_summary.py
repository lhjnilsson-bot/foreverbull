from io import StringIO

from foreverbull_cli.bull.summary import active_day_count
from foreverbull_cli.bull.summary import format_active_days
from foreverbull_cli.bull.summary import format_usd
from foreverbull_cli.bull.summary import held_no_position
from foreverbull_cli.bull.summary import metrics_table
from foreverbull_cli.bull.summary import run_notice_lines
from foreverbull_cli.bull.summary import starting_portfolio_value
from rich.console import Console

from foreverbull.pb.foreverbull.backtest.v1 import execution_pb


def _render(renderable) -> str:
    buf = StringIO()
    Console(file=buf, width=120, force_terminal=False, color_system=None).print(renderable)
    return buf.getvalue()


def test_format_usd_uses_two_decimals_and_a_leading_minus():
    assert format_usd(104200) == "$104,200.00"
    assert format_usd(100000.126) == "$100,000.13"
    assert format_usd(0) == "$0.00"
    assert format_usd(-12.3) == "-$12.30"


def test_format_active_days_is_n_of_m():
    assert format_active_days(0, 250) == "0 of 250"
    assert format_active_days(2, 3) == "2 of 3"


def test_starting_portfolio_value_is_cash_plus_positions():
    # ending_value and portfolio_value are different stored numbers and are not the open.
    period = execution_pb.Period(
        starting_cash=100_000,
        starting_value=0,
        ending_value=90_000,
        portfolio_value=104_200,
    )
    assert starting_portfolio_value(period) == 100_000


def test_metrics_table_prints_last_period_scalars_and_first_open():
    """Stand-in for the golden period row Quant will post on internal #82.

    Replace ``first``, ``held_short``, and ``last`` and the expected lines
    below with that stored period before this pull request leaves draft.
    The printed row must stay those stored fields, not a recomputed return.
    """
    first = execution_pb.Period(
        starting_cash=100_000,
        starting_value=0,
        portfolio_value=101_000,
        ending_value=50_000,
        algorithm_period_return=0.01,
        max_drawdown=-0.01,
        sharpe=9,
        longs_count=0,
    )
    held_short = execution_pb.Period(longs_count=0, shorts_count=1)
    last = execution_pb.Period(
        starting_cash=40_000,
        starting_value=62_000,
        ending_cash=14_200,
        ending_value=90_000,
        portfolio_value=104_200,
        algorithm_period_return=0.042,
        max_drawdown=-0.068,
        sharpe=1.35,
        sortino=1.1,
        benchmark_period_return=0.031,
        longs_count=2,
    )
    rendered = _render(metrics_table("SPY", [first, held_short, last]))
    assert "$100,000.00" in rendered
    assert "$104,200.00" in rendered
    assert "$102,000.00" not in rendered
    assert "$90,000.00" not in rendered
    assert "+4.2%" in rendered
    assert "+1.0%" not in rendered
    assert "Benchmark (SPY)" in rendered
    assert "+3.1%" in rendered
    assert "1.35" in rendered
    assert "1.10" in rendered
    assert "-6.8%" in rendered
    assert "2 of 3" in rendered
    drawdown_line = next(line for line in rendered.splitlines() if "Max drawdown" in line)
    assert "Active days" in drawdown_line
    assert "2 of 3" in drawdown_line
    assert (
        rendered.index("Start")
        < rendered.index("Return")
        < rendered.index("Sharpe")
        < rendered.index("Max drawdown")
        < rendered.index("Active days")
    )


def test_stored_zero_ratio_prints_and_a_missing_ratio_stays_a_dash():
    zero = execution_pb.Period(starting_cash=1, portfolio_value=1, sharpe=0.0, sortino=0.0)
    zero_rendered = _render(metrics_table("SPY", [zero]))
    sharpe_line = next(line for line in zero_rendered.splitlines() if "Sharpe" in line)
    assert "0.00" in sharpe_line

    missing = execution_pb.Period(starting_cash=1, portfolio_value=1, algorithm_period_return=0.01)
    missing.clear_field("sharpe")
    missing.clear_field("sortino")
    missing_rendered = _render(metrics_table("SPY", [missing]))
    sharpe_line = next(line for line in missing_rendered.splitlines() if "Sharpe" in line)
    assert "-" in sharpe_line
    assert "0.00" not in sharpe_line


def test_tradeless_periods_keep_zero_return_and_leave_ratios_unset():
    day = execution_pb.Period(
        starting_cash=100_000,
        starting_value=0,
        portfolio_value=100_000,
        ending_value=0,
        algorithm_period_return=0.0,
        max_drawdown=0.0,
        benchmark_period_return=0.248,
        longs_count=0,
        shorts_count=0,
    )
    day.clear_field("sharpe")
    day.clear_field("sortino")
    periods = [day, day]
    assert held_no_position(periods)
    assert active_day_count(periods) == 0
    rendered = _render(metrics_table("SPY", periods))
    assert "$100,000.00" in rendered
    assert "+0.0%" in rendered
    assert "+24.8%" in rendered
    assert "0 of 2" in rendered
    drawdown_line = next(line for line in rendered.splitlines() if "Max drawdown" in line)
    assert "0.0%" in drawdown_line
    assert "+0.0%" not in drawdown_line
    assert "Active days" in drawdown_line
    assert "0 of 2" in drawdown_line
    sharpe_line = next(line for line in rendered.splitlines() if "Sharpe" in line)
    assert "-" in sharpe_line
    assert "0.00" not in sharpe_line


def test_run_notice_lines_keeps_one_or_two_dates_as_lines():
    one = "1 symbol was delisted after its last price on 2026-03-31 and was left out: ABC."
    two = "2 symbols were delisted after their last price on 2026-04-30 and were left out: DEF, GHI."
    assert run_notice_lines([one], verbose=False) == [one]
    assert run_notice_lines([one, two], verbose=False) == [one, two]


def test_run_notice_lines_folds_three_dates_and_verbose_lists_them():
    lines = [
        "1 symbol was delisted after its last price on 2026-03-31 and was left out: ABC.",
        "2 symbols were delisted after their last price on 2026-04-30 and were left out: DEF, GHI.",
        "9 symbols were delisted after their last price on 2026-05-29 and were left out: A, B, C, D, E, F, G, H, I.",
    ]
    summary = "12 symbols were delisted before the run ended and were left out."
    assert run_notice_lines(lines, verbose=False) == [f"{summary} Run with --verbose for the list."]
    assert run_notice_lines(lines, verbose=True) == [summary, *lines]


def test_run_notice_lines_names_five_symbols_then_the_rest_as_a_count():
    line = "7 symbols were delisted after their last price on 2026-03-31 and were left out: A, B, C, D, E, F, G."
    assert run_notice_lines([line], verbose=False) == [
        "7 symbols were delisted after their last price on 2026-03-31 and were left out: A, B, C, D, E and 2 more."
    ]


def test_run_notice_lines_shows_a_listing_next_to_an_auto_close():
    # A confirmed delisting uses the same left-out line as a symbol the run
    # left out. A confirmed listing is printed beside it, as stored.
    auto_close = "1 symbol was delisted after its last price on 2024-03-01 and was left out: OLD."
    listing = "XYZ was listed on 2024-03-15 and has no prices before then."
    assert run_notice_lines([auto_close, listing], verbose=False) == [auto_close, listing]


def test_run_notice_lines_prints_other_notices_as_stored():
    other = "Something else the broker said."
    assert run_notice_lines([other], verbose=False) == [other]


def test_run_notice_lines_keep_the_held_sentence_per_line_and_fold_it():
    held = (
        "1 symbol was delisted after its last price on 2023-01-13 and was left out: XYZ. "
        "Positions held were closed at the last price on 2023-01-13."
    )
    plain = "1 symbol was delisted after its last price on 2023-01-20 and was left out: ABC."
    assert run_notice_lines([held, plain], verbose=False) == [held, plain]
    third = "1 symbol was delisted after its last price on 2023-01-27 and was left out: DEF."
    summary = "3 symbols were delisted before the run ended and were left out. Positions held were closed at their last price."
    assert run_notice_lines([held, plain, third], verbose=False) == [f"{summary} Run with --verbose for the list."]
    assert run_notice_lines([held, plain, third], verbose=True) == [summary, held, plain, third]
