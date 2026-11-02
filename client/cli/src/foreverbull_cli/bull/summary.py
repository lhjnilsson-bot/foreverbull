import re

from typing import Literal

from rich.markup import escape
from rich.padding import Padding
from rich.table import Table

from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull_cli.output import console
from foreverbull_cli.output import execution_link


# The broker stores one complete line per last-price date in
# Execution.notices, e.g. "3 symbols were delisted after their last price on
# 2026-03-31 and were left out: ABC, DEF, GHI.", ending "Positions held were
# closed at the last price on 2026-03-31." when the run held one of them. The
# web UI reads the same lines, so the CLI folds them only when it prints.
_LEFT_OUT_LINE = re.compile(
    r"^(?P<count>\d+) symbols? (?:was delisted after its|were delisted after their) last price on "
    r"(?P<date>\d{4}-\d{2}-\d{2}) and (?:was|were) left out: (?P<symbols>.+?)\."
    r"(?P<held> Positions held were closed at the last price on \d{4}-\d{2}-\d{2}\.)?$"
)
# One line names at most this many symbols, then "and N more".
LEFT_OUT_SYMBOLS_SHOWN = 5
# Up to this many last-price dates print one line each; more fold into one line.
LEFT_OUT_DATES_SHOWN = 2
NOTICE_PREFIX = "⚠ "


def _left_out_line(count: int, last_date: str, symbols: list[str], held: str) -> str:
    shown = ", ".join(symbols[:LEFT_OUT_SYMBOLS_SHOWN])
    hidden = len(symbols) - LEFT_OUT_SYMBOLS_SHOWN
    if hidden > 0:
        shown = f"{shown} and {hidden} more"
    if count == 1:
        return f"1 symbol was delisted after its last price on {last_date} and was left out: {shown}.{held}"
    return f"{count} symbols were delisted after their last price on {last_date} and were left out: {shown}.{held}"


def run_notice_lines(notices: list[str], *, verbose: bool) -> list[str]:
    """The notice lines `fbull backtest run` prints, without the ⚠ prefix.

    One or two last-price dates print one line each, naming at most five
    symbols. Three or more fold into one line with the total; --verbose
    adds every stored per-date line, with every symbol, under it. A line
    whose symbols were held keeps its "Positions held were closed" sentence, and
    the folded line says so when any line does. A notice that is not a
    left-out line prints as stored.
    """
    left_out: list[re.Match[str]] = []
    other: list[str] = []
    for line in notices:
        match = _LEFT_OUT_LINE.match(line)
        if match is None:
            other.append(line)
        else:
            left_out.append(match)
    if len(left_out) <= LEFT_OUT_DATES_SHOWN:
        shown = [
            _left_out_line(int(match["count"]), match["date"], match["symbols"].split(", "), match["held"] or "")
            for match in left_out
        ]
        return [*shown, *other]
    total = sum(int(match["count"]) for match in left_out)
    summary = f"{total} symbols were delisted before the run ended and were left out."
    if any(match["held"] for match in left_out):
        # A position Zipline auto-closed is a trade the strategy never placed.
        summary = f"{summary} Positions held were closed at their last price."
    if verbose:
        return [summary, *(match.string for match in left_out), *other]
    return [f"{summary} Run with --verbose for the list.", *other]


# Eight hex characters of a UUID are enough to tell a user's executions apart;
# `fbull backtest execution` refuses a prefix that matches more than one.
SHORT_EXECUTION_ID_LENGTH = 8


def short_execution_id(execution_id: str) -> str:
    return execution_id[:SHORT_EXECUTION_ID_LENGTH]


def distinguishing_execution_ids(execution_ids: list[str]) -> list[str]:
    """Segment-aligned prefixes long enough to tell the given executions apart.

    Extends past the eight-character short id until the prefix is unique
    among `execution_ids`, then out to the end of that hyphen segment, so
    `01234567-89ab` is shown instead of the indistinguishable `01234567`.
    """
    shown: list[str] = []
    for execution_id in execution_ids:
        length = SHORT_EXECUTION_ID_LENGTH
        while length < len(execution_id) and any(
            other != execution_id and other.startswith(execution_id[:length]) for other in execution_ids
        ):
            length += 1
        segment_end = execution_id.find("-", length)
        if segment_end != -1:
            length = segment_end
        shown.append(execution_id[:length])
    return shown


def format_percent(fraction: float) -> str:
    """Zipline reports returns and drawdown as fractions, so 0.042 prints as +4.2%."""
    return f"{fraction * 100:+.1f}%"


def format_drawdown(fraction: float) -> str:
    """Drawdown is at most zero, so 0 prints as 0.0% rather than +0.0%."""
    percent = fraction * 100
    if round(percent, 1) == 0:
        percent = 0.0
    return f"{percent:.1f}%"


def format_usd(amount: float) -> str:
    """Portfolio dollars: two decimals and thousands separators. A minus sits before the dollar sign."""
    if amount < 0:
        return f"-${abs(amount):,.2f}"
    return f"${amount:,.2f}"


def format_active_days(active: int, total: int) -> str:
    return f"{active} of {total}"


def starting_portfolio_value(period: execution_pb.Period) -> float:
    """Open account is cash plus positions value.

    Zipline stores those as starting_cash and starting_value. The first
    session's pair is the capital the run opened with. The last session's
    starting_cash is only that morning's cash.
    """
    return period.starting_cash + period.starting_value


def _ratio(period: execution_pb.Period, field: Literal["sharpe", "sortino"]) -> str:
    # A missing ratio stays a dash. A stored 0 is a real ratio and prints as 0.00.
    if not period.has_field(field):
        return "-"
    return f"{getattr(period, field):.2f}"


def _benchmark_cells(benchmark: str, period: execution_pb.Period) -> tuple[str, str]:
    if not benchmark:
        return "Benchmark", "not set"
    label = f"Benchmark ({escape(benchmark)})"
    if not period.has_field("benchmark_period_return"):
        return label, "not available"
    return label, format_percent(period.benchmark_period_return)


def position_held(period: execution_pb.Period) -> bool:
    """A session is active when the close still holds a long or a short.

    Zipline fills a daily order on the next bar, so a fill shows up as a
    held position at some period close. Counts are integers, so no float compare.
    """
    return period.longs_count != 0 or period.shorts_count != 0


def active_day_count(periods: list[execution_pb.Period]) -> int:
    return sum(1 for period in periods if position_held(period))


def held_no_position(periods: list[execution_pb.Period]) -> bool:
    return active_day_count(periods) == 0


def metrics_table(benchmark: str, periods: list[execution_pb.Period]) -> Table:
    """Last-period scalars, the first session's open value, and days in a position.

    Return, ratios, drawdown, and ending portfolio value come from the last
    stored period. Sharpe and Sortino stay dashes when that period left them
    unset. Start is the first session's open account, not the last morning.
    """
    last = periods[-1]
    table = Table.grid(padding=(0, 2))
    table.add_column(style="bold")
    table.add_column(justify="right")
    table.add_column(style="bold")
    table.add_column(justify="right")
    benchmark_label, benchmark_value = _benchmark_cells(benchmark, last)
    table.add_row(
        "Start",
        format_usd(starting_portfolio_value(periods[0])),
        "End",
        format_usd(last.portfolio_value),
    )
    table.add_row("Return", format_percent(last.algorithm_period_return), benchmark_label, benchmark_value)
    table.add_row("Sharpe", _ratio(last, "sharpe"), "Sortino", _ratio(last, "sortino"))
    table.add_row(
        "Max drawdown",
        format_drawdown(last.max_drawdown),
        "Active days",
        format_active_days(active_day_count(periods), len(periods)),
    )
    return table


def print_run_summary(
    *,
    backtest_name: str,
    benchmark: str,
    provider: str,
    start_iso: str,
    end_iso: str,
    execution_id: str,
    periods: list[execution_pb.Period],
    notices: list[str] | None = None,
    verbose: bool = False,
) -> None:
    if not periods:
        console.print("[green]✔[/green] Backtest finished, but no results were stored for this execution.")
        console.print()
    else:
        console.print(
            f"[green]✔[/green] Backtest finished: {escape(backtest_name)} · {start_iso} → {end_iso} · provider: {provider}"
        )
        console.print()
        console.print(Padding(metrics_table(benchmark, periods), (0, 0, 0, 2), expand=False))
        console.print()
        if held_no_position(periods):
            # Without this line a tradeless run reads as a broken one: zeros and dashes, no reason.
            console.print(
                f"  No trades: the strategy held no position on any of the {len(periods)} trading days.",
                highlight=False,
            )
            console.print(
                "  Return and drawdown stay at 0 and Sharpe and Sortino are not defined.",
                highlight=False,
            )
            console.print()
    if notices:
        # The broker left these members out of the run, e.g. after a delisting.
        # Never silent: the lines name the symbols and the date their prices
        # ended, folded like the create notices once there are three dates.
        for line in run_notice_lines(notices, verbose=verbose):
            console.print(f"  {NOTICE_PREFIX}{line}", markup=False, highlight=False, soft_wrap=True)
        console.print()
    # soft_wrap leaves wrapping to the terminal, so a pasted command or URL has no inserted newline.
    console.print(
        f"  Details: uv run fbull backtest execution {short_execution_id(execution_id)}",
        highlight=False,
        soft_wrap=True,
    )
    console.print(f"  Charts:  {execution_link(execution_id)}", highlight=False, soft_wrap=True)
