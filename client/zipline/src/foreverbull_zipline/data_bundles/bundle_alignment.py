import sqlite3

from collections.abc import Callable
from collections.abc import Mapping
from pathlib import Path
from typing import Any
from typing import cast

import pandas as pd

from zipline.data import bundles
from zipline.data.bundles.core import from_bundle_ingest_dirname
from zipline.utils.calendar_utils import get_calendar
from zipline.utils.paths import data_path


# Bundles ingested before the session-alignment fix carry provider stamps
# (Yahoo's 14:30 UTC session open) as asset start/end dates and bar days.
# Zipline keys assets on the midnight session label, so such a bundle is
# refused or loses its first session on a run. Version 1 bundles are keyed
# on session labels; the marker is written into the ingest directory so it
# travels with the bundle tarball a run downloads.
SESSION_ALIGNED_BUNDLE_VERSION = 1
BUNDLE_VERSION_MARKER = ".foreverbull-bundle-version"

_DATE_COLUMNS = ("start_date", "end_date", "auto_close_date")


def latest_ingest_dir(bundle_name: str, environ: Mapping[str, Any] | None = None) -> Path | None:
    """Newest ingest directory for a bundle, or None when nothing was ingested."""
    bundle_dir = Path(data_path([bundle_name], environ=environ))
    if not bundle_dir.is_dir():
        return None
    candidates: list[tuple[pd.Timestamp, Path]] = []
    for entry in bundle_dir.iterdir():
        if entry.name.startswith(".") or not entry.is_dir():
            continue
        try:
            ingested_at = cast(pd.Timestamp, from_bundle_ingest_dirname(entry.name))
        except (TypeError, ValueError):
            continue
        candidates.append((ingested_at, entry))
    if not candidates:
        return None
    return max(candidates, key=lambda candidate: candidate[0])[1]


def bundle_version(bundle_name: str, environ: Mapping[str, Any] | None = None) -> int:
    """Alignment version stamped on the newest ingest; 0 when unstamped."""
    ingest_dir = latest_ingest_dir(bundle_name, environ=environ)
    if ingest_dir is None:
        return 0
    try:
        return int((ingest_dir / BUNDLE_VERSION_MARKER).read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return 0


def stamp_bundle_version(
    bundle_name: str,
    environ: Mapping[str, Any] | None = None,
    version: int = SESSION_ALIGNED_BUNDLE_VERSION,
) -> None:
    ingest_dir = latest_ingest_dir(bundle_name, environ=environ)
    if ingest_dir is not None:
        (ingest_dir / BUNDLE_VERSION_MARKER).write_text(f"{version}\n", encoding="utf-8")


def _asset_db_path(ingest_dir: Path) -> Path | None:
    matches = sorted(ingest_dir.glob("assets-*.sqlite"))
    return matches[0] if matches else None


def _read_bundle_dates(db_path: Path) -> list[int]:
    stamps: list[int] = []
    with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as connection:
        for table in ("equities", "equity_symbol_mappings"):
            columns = [row[1] for row in connection.execute(f"PRAGMA table_info({table})")]
            wanted = [column for column in ("start_date", "end_date", "auto_close_date") if column in columns]
            if not wanted:
                continue
            for row in connection.execute(f"SELECT {', '.join(wanted)} FROM {table}"):
                stamps.extend(int(value) for value in row if value is not None)
    return stamps


def bundle_dates_misaligned(bundle_name: str, environ: Mapping[str, Any] | None = None) -> bool:
    """True when an asset date in the newest ingest is not a session label.

    Session labels are midnight, so a provider stamp inside the session
    (14:30 UTC) is the stale shape. The smallest sqlite timestamp is
    Zipline's no-date sentinel and is not a real date.
    """
    ingest_dir = latest_ingest_dir(bundle_name, environ=environ)
    if ingest_dir is None:
        return False
    db_path = _asset_db_path(ingest_dir)
    if db_path is None:
        return False
    for stamp in _read_bundle_dates(db_path):
        moment = cast(pd.Timestamp, pd.Timestamp(stamp, unit="ns"))
        if moment == pd.Timestamp.min:
            continue
        if moment != moment.normalize():
            return True
    return False


def _read_equity_metadata(db_path: Path) -> pd.DataFrame:
    query = """
        SELECT equities.sid AS sid, mappings.symbol AS symbol,
               mappings.start_date AS start_date, mappings.end_date AS end_date,
               equities.auto_close_date AS auto_close_date, equities.exchange AS exchange
        FROM equities
        JOIN equity_symbol_mappings AS mappings ON mappings.sid = equities.sid
        ORDER BY equities.sid, mappings.start_date
    """
    with sqlite3.connect(f"file:{db_path}?mode=ro", uri=True) as connection:
        metadata = pd.read_sql_query(query, connection)
    for column in _DATE_COLUMNS:
        metadata[column] = pd.to_datetime(metadata[column], unit="ns").dt.normalize()
    return metadata


def rebuild_session_aligned_bundle(bundle_name: str, environ: Mapping[str, Any] | None = None) -> None:
    """Re-ingest a bundle from its own bars, keyed on session labels.

    The stored bars are already in session order; only their day stamps
    and the asset dates built from them are misaligned. Reading the bars
    back through the daily reader and writing them on session labels
    therefore keeps every price, while no price database is needed on
    the run path, where only the bundle name is known.
    """
    ingest_dir = latest_ingest_dir(bundle_name, environ=environ)
    if ingest_dir is None:
        raise ValueError(f"no data for bundle {bundle_name!r}: nothing to rebuild")
    db_path = _asset_db_path(ingest_dir)
    if db_path is None:
        raise ValueError(f"bundle {bundle_name!r} has no asset database to rebuild from")
    metadata = _read_equity_metadata(db_path)
    if metadata.empty:
        raise ValueError(f"bundle {bundle_name!r} has no assets to rebuild")

    calendar = get_calendar("XNYS")
    old_bundle = bundles.load(bundle_name, environ, None)
    frames: dict[int, pd.DataFrame] = {}
    try:
        reader = old_bundle.equity_daily_bar_reader
        for sid in sorted(metadata["sid"].unique()):
            rows = metadata.loc[metadata["sid"] == sid]
            start = rows["start_date"].min()
            end = rows["end_date"].max()
            sessions = calendar.sessions_in_range(start, end)
            asset = old_bundle.asset_finder.retrieve_asset(int(sid))
            opens, highs, lows, closes, volumes = reader.load_raw_arrays(
                ["open", "high", "low", "close", "volume"], start, end, [asset]
            )
            frames[int(sid)] = pd.DataFrame(
                {
                    "open": opens[:, 0],
                    "high": highs[:, 0],
                    "low": lows[:, 0],
                    "close": closes[:, 0],
                    "volume": volumes[:, 0],
                },
                index=pd.DatetimeIndex(sessions),
            )
    finally:
        old_bundle.close()

    def reingest(
        _environ,
        asset_db_writer,
        _minute_bar_writer,
        daily_bar_writer,
        adjustment_writer,
        _calendar,
        _start_session,
        _end_session,
        _cache,
        _show_progress,
        _output_dir,
    ):
        daily_bar_writer.write(sorted(frames.items()))
        asset_db_writer.write(equities=metadata)
        adjustment_writer.write()

    bundles.register(bundle_name, reingest, calendar_name="XNYS")
    bundles.ingest(bundle_name, environ, pd.Timestamp.utcnow(), [], True)


def ensure_session_aligned_bundle(
    bundle_name: str,
    environ: Mapping[str, Any] | None = None,
    *,
    rebuild: Callable[[str, Mapping[str, Any] | None], None] | None = None,
) -> bool:
    """Rebuild a pre-alignment bundle once, on its first run after the fix.

    Returns True when a rebuild happened. A bundle stamped with the
    current version and session-aligned dates is left alone, so the
    check on every later run is one marker read and one small sqlite
    scan, never a re-ingest. An aligned bundle without a marker (fresh
    creates since the alignment fix, before this stamp) is only stamped.
    """
    if latest_ingest_dir(bundle_name, environ=environ) is None:
        raise ValueError(f"no data for bundle {bundle_name!r}: create the backtest before running it")
    misaligned = bundle_dates_misaligned(bundle_name, environ=environ)
    if not misaligned:
        if bundle_version(bundle_name, environ=environ) < SESSION_ALIGNED_BUNDLE_VERSION:
            stamp_bundle_version(bundle_name, environ=environ)
        return False
    rebuild_bundle = rebuild if rebuild is not None else rebuild_session_aligned_bundle
    try:
        rebuild_bundle(bundle_name, environ)
    except Exception as error:
        raise RuntimeError(f"rebuilding the stale bundle {bundle_name!r} for this run failed: {error}") from error
    stamp_bundle_version(bundle_name, environ=environ)
    return True
