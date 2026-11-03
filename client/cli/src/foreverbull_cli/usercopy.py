"""Lines fbull prints from data the broker stored, so a wording change is a swap in one place.

This is the CLI's counterpart of the broker's internal/usercopy: the broker
words its own failures there, and the CLI words here what it builds from
stored fields.
"""

from collections.abc import Sequence
from dataclasses import dataclass

from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb


# One block names at most this many later-ticker resolutions, then one line for the rest.
LATER_TICKERS_SHOWN = 5


@dataclass(frozen=True)
class LaterTicker:
    """A symbol whose bars create read under a later ticker of the same ISIN.

    symbol is the ticker the backtest asked for and read_as the ticker the
    bars were read and stored under. No day is known: the provider doesn't
    say when the ticker changed.
    """

    symbol: str
    read_as: str


def later_tickers(entity: backtest_pb.Backtest) -> list[LaterTicker]:
    """The backtest's stored resolutions, in the broker's order (by symbol)."""
    return [
        LaterTicker(symbol=resolution.symbol, read_as=resolution.read_as) for resolution in entity.ticker_resolutions
    ]


def later_ticker_line(resolution: LaterTicker) -> str:
    """The line for one symbol read under a later ticker."""
    return f"Prices for {resolution.symbol} come from {resolution.read_as}, the company's later ticker."


def later_tickers_hidden_line(hidden: int) -> str:
    """The line that counts the resolutions past LATER_TICKERS_SHOWN."""
    if hidden == 1:
        return "Plus 1 more symbol whose prices come from a later ticker."
    return f"Plus {hidden} more symbols whose prices come from a later ticker."


def later_ticker_lines(resolutions: Sequence[LaterTicker]) -> list[str]:
    """The lines `fbull backtest create` and the run summary print for later tickers.

    One line per resolution up to LATER_TICKERS_SHOWN, then one line counting
    the rest. No resolution prints nothing. The exact strings are pinned in
    tests/test_usercopy.py.
    """
    lines = [later_ticker_line(resolution) for resolution in resolutions[:LATER_TICKERS_SHOWN]]
    hidden = len(resolutions) - LATER_TICKERS_SHOWN
    if hidden > 0:
        lines.append(later_tickers_hidden_line(hidden))
    return lines
