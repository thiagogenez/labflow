"""Unit tests for reproducible synthetic sales and input validation."""

import random

import pytest

from labflow.synthetic import (
    GENERATOR_VERSION,
    MAX_ROWS,
    MAX_SEED,
    Sale,
    generate_sales,
)


def test_sales_v1_known_dataset():
    """A changed data contract requires an explicit generator version decision."""
    assert GENERATOR_VERSION == "sales-v1"
    assert generate_sales(seed=42, rows=3) == (
        Sale(row_id=1, product_id=2, quantity=1, unit_price_cents=4606),
        Sale(row_id=2, product_id=4, quantity=2, unit_price_cents=2386),
        Sale(row_id=3, product_id=2, quantity=5, unit_price_cents=1524),
    )


def test_same_seed_reproduces_sales_after_another_job():
    """Reproduce the dataset independently of intervening generation calls."""
    first = generate_sales(seed=42, rows=100)
    generate_sales(seed=7, rows=100)

    assert generate_sales(seed=42, rows=100) == first


def test_different_seed_changes_the_dataset():
    """Produce different datasets for the two selected seeds."""
    assert generate_sales(seed=42, rows=100) != generate_sales(seed=43, rows=100)


def test_generation_does_not_change_global_random_state():
    """Leave the shared random generator state unchanged."""
    before = random.getstate()

    generate_sales(seed=42, rows=100)

    assert random.getstate() == before


def test_more_rows_preserve_the_existing_prefix():
    """Preserve the dataset prefix when requesting additional rows."""
    assert generate_sales(seed=42, rows=20)[:10] == generate_sales(seed=42, rows=10)


def test_maximum_dataset_has_valid_sales_and_distinct_row_ids():
    """Generate the maximum dataset with valid fields and unique row IDs."""
    sales = generate_sales(seed=42, rows=MAX_ROWS)

    assert len(sales) == MAX_ROWS
    assert {sale.row_id for sale in sales} == set(range(1, MAX_ROWS + 1))
    assert all(1 <= sale.product_id <= 10 for sale in sales)
    assert all(1 <= sale.quantity <= 5 for sale in sales)
    assert all(
        isinstance(sale.unit_price_cents, int)
        and 100 <= sale.unit_price_cents <= 10_000
        for sale in sales
    )


@pytest.mark.parametrize("seed", (0, MAX_SEED), ids=("minimum", "maximum"))
def test_accepts_seed_boundaries(seed):
    """Accept both endpoints of the supported seed range."""
    assert len(generate_sales(seed=seed, rows=1)) == 1


@pytest.mark.parametrize(
    "rows", (0, -1, MAX_ROWS + 1), ids=("zero", "negative", "over-limit")
)
def test_rejects_row_counts_outside_the_budget(rows):
    """Reject row counts outside the permitted allocation budget."""
    with pytest.raises(ValueError, match="rows must be between"):
        generate_sales(seed=42, rows=rows)


@pytest.mark.parametrize(
    "rows",
    (True, False, 1.5, "10", None),
    ids=("true", "false", "float", "string", "none"),
)
def test_rejects_non_integer_row_counts(rows):
    """Reject booleans and other non-integer row counts."""
    with pytest.raises(TypeError, match="rows must be an integer"):
        generate_sales(seed=42, rows=rows)


@pytest.mark.parametrize("seed", (-1, MAX_SEED + 1), ids=("negative", "over-limit"))
def test_rejects_seeds_outside_the_supported_range(seed):
    """Reject seeds below or above the supported range."""
    with pytest.raises(ValueError, match="seed must be between"):
        generate_sales(seed=seed, rows=1)


@pytest.mark.parametrize(
    "seed",
    (True, False, 1.5, "42", None),
    ids=("true", "false", "float", "string", "none"),
)
def test_rejects_non_integer_seeds(seed):
    """Reject booleans and other non-integer seeds."""
    with pytest.raises(TypeError, match="seed must be an integer"):
        generate_sales(seed=seed, rows=1)


def test_sale_total_cents():
    """Calculate the total price from quantity and unit price in cents."""
    sale = Sale(row_id=1, product_id=2, quantity=3, unit_price_cents=1999)
    assert sale.total_cents == 5997
