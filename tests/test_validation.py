"""UTC normalization across offsets and daylight-saving clock transitions."""

from datetime import UTC, datetime, timedelta
from zoneinfo import ZoneInfo

import pytest

from labflow.validation import normalize_utc_datetime


def test_repeated_local_hour_with_different_offsets_has_distinct_instants():
    """The two occurrences of 02:30 during fall-back remain an hour apart."""
    first = normalize_utc_datetime(
        datetime.fromisoformat("2020-10-25T02:30:00+02:00"), "timestamp"
    )
    second = normalize_utc_datetime(
        datetime.fromisoformat("2020-10-25T02:30:00+01:00"), "timestamp"
    )
    assert first == datetime(2020, 10, 25, 0, 30, tzinfo=UTC)
    assert second == datetime(2020, 10, 25, 1, 30, tzinfo=UTC)
    assert second - first == timedelta(hours=1)


@pytest.mark.parametrize(
    "timestamp",
    [
        "2020-10-25T00:30:00+00:00",
        "2020-10-25T02:30:00+02:00",
        "2020-10-24T20:30:00-04:00",
    ],
)
def test_different_offsets_can_represent_the_same_instant(timestamp):
    """Normalization preserves the instant, including when the date changes."""
    original = datetime.fromisoformat(timestamp)
    normalized = normalize_utc_datetime(original, "timestamp")
    assert normalized == datetime(2020, 10, 25, 0, 30, tzinfo=UTC)
    assert normalized.tzinfo is UTC
    assert original.isoformat() == timestamp


def test_explicit_fold_disambiguates_iana_repeated_hour():
    """A caller must select the occurrence before normalizing local zone input."""
    berlin = ZoneInfo("Europe/Berlin")
    first = datetime(2020, 10, 25, 2, 30, tzinfo=berlin, fold=0)
    second = first.replace(fold=1)
    normalized_first = normalize_utc_datetime(first, "timestamp")
    normalized_second = normalize_utc_datetime(second, "timestamp")
    assert normalized_second - normalized_first == timedelta(hours=1)
    assert (first.fold, second.fold) == (0, 1)


def test_elapsed_duration_is_calculated_in_utc_across_spring_forward():
    """Ten elapsed minutes remain ten even when the display clock skips an hour."""
    start = normalize_utc_datetime(
        datetime.fromisoformat("2020-03-29T01:55:00+01:00"), "timestamp"
    )
    end = start + timedelta(minutes=10)
    local_end = end.astimezone(ZoneInfo("Europe/Berlin"))
    assert end - start == timedelta(minutes=10)
    assert local_end.isoformat() == "2020-03-29T03:05:00+02:00"


@pytest.mark.parametrize("value", [None, True, 123, "2020-10-25T00:30:00Z"])
def test_timestamp_requires_datetime_without_coercion(value):
    """Parsing external data remains the responsibility of the boundary."""
    with pytest.raises(TypeError, match="timestamp must be a datetime"):
        normalize_utc_datetime(value, "timestamp")


def test_local_timestamp_without_offset_is_rejected():
    """The normalizer never guesses a timezone or repeated-hour occurrence."""
    with pytest.raises(ValueError, match="timestamp must be offset-aware"):
        normalize_utc_datetime(datetime(2020, 10, 25, 2, 30), "timestamp")
