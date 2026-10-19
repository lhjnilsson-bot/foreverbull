import os
import sqlite3

from pathlib import Path

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.bundle_alignment import SESSION_ALIGNED_BUNDLE_VERSION
from foreverbull_zipline.data_bundles.bundle_alignment import bundle_dates_misaligned
from foreverbull_zipline.data_bundles.bundle_alignment import bundle_version
from foreverbull_zipline.data_bundles.bundle_alignment import ensure_session_aligned_bundle
from zipline.data import bundles
from zipline.data.bundles.core import to_bundle_ingest_dirname
from zipline.utils.calendar_utils import get_calendar


def _bundle_environ(tmp_path: Path, bundle_name: str, start_stamp: str) -> dict[str, str]:
    ingest_dir = tmp_path / "data" / bundle_name / to_bundle_ingest_dirname(pd.Timestamp("2023-02-01"))
    ingest_dir.mkdir(parents=True)
    db_path = ingest_dir / "assets-7.sqlite"
    start = pd.Timestamp(start_stamp)
    end = pd.Timestamp("2023-01-31 14:30") if start.hour else pd.Timestamp("2023-01-31")
    with sqlite3.connect(db_path) as connection:
        connection.execute(
            "CREATE TABLE equities (sid BIGINT, start_date BIGINT, end_date BIGINT, auto_close_date BIGINT, exchange TEXT)"
        )
        connection.execute(
            "CREATE TABLE equity_symbol_mappings (id BIGINT, sid BIGINT, symbol TEXT, start_date BIGINT, end_date BIGINT)"
        )
        connection.execute(
            "INSERT INTO equities VALUES (0, ?, ?, ?, 'NASDAQ')",
            (start.value, end.value, (end + pd.Timedelta(days=1)).value),
        )
        connection.execute(
            "INSERT INTO equity_symbol_mappings VALUES (0, 0, 'SPY', ?, ?)",
            (start.value, end.value),
        )
    return {"ZIPLINE_ROOT": str(tmp_path)}


def _marker_path(tmp_path: Path, bundle_name: str) -> Path:
    ingest_dir = tmp_path / "data" / bundle_name / to_bundle_ingest_dirname(pd.Timestamp("2023-02-01"))
    return ingest_dir / ".foreverbull-bundle-version"


def test_old_bundle_without_marker_triggers_rebuild(tmp_path):
    # A pre-alignment bundle: 14:30 asset dates and no version marker.
    # Its first run after the fix must rebuild it exactly once.
    environ = _bundle_environ(tmp_path, "old_bundle", "2023-01-03 14:30")
    rebuilds: list[str] = []

    rebuilt = ensure_session_aligned_bundle("old_bundle", environ, rebuild=lambda name, env: rebuilds.append(name))

    assert rebuilt is True
    assert rebuilds == ["old_bundle"]
    assert bundle_version("old_bundle", environ) == SESSION_ALIGNED_BUNDLE_VERSION


def test_current_bundle_does_not_trigger_rebuild(tmp_path):
    environ = _bundle_environ(tmp_path, "current_bundle", "2023-01-03")
    _marker_path(tmp_path, "current_bundle").write_text(f"{SESSION_ALIGNED_BUNDLE_VERSION}\n", encoding="utf-8")
    rebuilds: list[str] = []

    rebuilt = ensure_session_aligned_bundle("current_bundle", environ, rebuild=lambda name, env: rebuilds.append(name))

    assert rebuilt is False
    assert rebuilds == []


def test_aligned_bundle_without_marker_is_only_stamped(tmp_path):
    # Fresh creates since the alignment fix have midnight dates but were
    # ingested before this marker existed; stamping them must not pay a
    # rebuild on their next run.
    environ = _bundle_environ(tmp_path, "aligned_bundle", "2023-01-03")
    rebuilds: list[str] = []

    rebuilt = ensure_session_aligned_bundle("aligned_bundle", environ, rebuild=lambda name, env: rebuilds.append(name))

    assert rebuilt is False
    assert rebuilds == []
    assert bundle_version("aligned_bundle", environ) == SESSION_ALIGNED_BUNDLE_VERSION


def test_misaligned_bundle_triggers_rebuild_even_with_a_marker(tmp_path):
    # The dates are the ground truth: a stamped bundle whose asset dates
    # are still at 14:30 is stale, whatever its marker says.
    environ = _bundle_environ(tmp_path, "stamped_stale_bundle", "2023-01-03 14:30")
    _marker_path(tmp_path, "stamped_stale_bundle").write_text(f"{SESSION_ALIGNED_BUNDLE_VERSION}\n", encoding="utf-8")
    rebuilds: list[str] = []

    rebuilt = ensure_session_aligned_bundle(
        "stamped_stale_bundle", environ, rebuild=lambda name, env: rebuilds.append(name)
    )

    assert rebuilt is True
    assert rebuilds == ["stamped_stale_bundle"]


def test_missing_bundle_fails_before_a_run(tmp_path):
    with pytest.raises(ValueError, match="create the backtest before running it"):
        ensure_session_aligned_bundle("missing_bundle", {"ZIPLINE_ROOT": str(tmp_path)})


def _write_synthetic_bundle(bundle_name: str, *, stale: bool) -> tuple[pd.DatetimeIndex, list[float]]:
    calendar = get_calendar("XNYS")
    sessions = calendar.sessions_in_range(pd.Timestamp("2023-01-03"), pd.Timestamp("2023-01-31"))  # type: ignore[arg-type]
    closes = [100.0 + index for index in range(len(sessions))]
    index = sessions + pd.Timedelta(hours=14, minutes=30) if stale else sessions
    frame = pd.DataFrame(
        {
            "open": [close - 1.0 for close in closes],
            "high": [close + 1.0 for close in closes],
            "low": [close - 2.0 for close in closes],
            "close": closes,
            "volume": [1000] * len(closes),
        },
        index=index,
    )
    metadata = pd.DataFrame(
        {
            "sid": [0],
            "start_date": [index[0]],
            "end_date": [index[-1]],
            "auto_close_date": [index[-1] + pd.Timedelta(days=1)],
            "symbol": ["SPY"],
            "exchange": ["NASDAQ"],
        }
    )

    def ingest_bundle(
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
        daily_bar_writer.write([(0, frame)])
        asset_db_writer.write(equities=metadata)
        adjustment_writer.write()

    bundles.register(bundle_name, ingest_bundle, calendar_name="XNYS")
    bundles.ingest(bundle_name, os.environ, pd.Timestamp("2023-02-01", tz="UTC"), [], False)
    return sessions, closes


def test_rebuild_realigns_a_stale_bundle_and_preserves_its_prices(tmp_path, monkeypatch):
    # The real rebuild, not an injected one: a bundle written the
    # pre-alignment way (14:30 stamps, no marker) is re-ingested from
    # its own bars on the first run check, and never again after that.
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions, closes = _write_synthetic_bundle("stale_rebuild_bundle", stale=True)
    try:
        assert bundle_dates_misaligned("stale_rebuild_bundle", os.environ)

        assert ensure_session_aligned_bundle("stale_rebuild_bundle", os.environ) is True

        assert not bundle_dates_misaligned("stale_rebuild_bundle", os.environ)
        assert bundle_version("stale_rebuild_bundle", os.environ) == SESSION_ALIGNED_BUNDLE_VERSION
        bundle = bundles.load("stale_rebuild_bundle", os.environ, None)
        try:
            asset = bundle.asset_finder.lookup_symbol("SPY", as_of_date=None)
            assert asset.start_date == sessions[0]
            assert asset.end_date == sessions[-1]
            loaded = bundle.equity_daily_bar_reader.load_raw_arrays(["close"], sessions[0], sessions[-1], [asset])[0][
                :, 0
            ]
            assert list(loaded) == pytest.approx(closes)
        finally:
            bundle.close()

        assert ensure_session_aligned_bundle("stale_rebuild_bundle", os.environ) is False
    finally:
        bundles.unregister("stale_rebuild_bundle")
