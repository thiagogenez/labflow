"""Tests for sales report aggregation."""

from labflow.reporting import summarize_sales
from labflow.synthetic import Sale


def test_summarize_known_sales():
    """Calculate the expected summary for three known sales."""
    sales = (
        Sale(row_id=1, product_id=2, quantity=1, unit_price_cents=4606),
        Sale(row_id=2, product_id=4, quantity=2, unit_price_cents=2386),
        Sale(row_id=3, product_id=2, quantity=5, unit_price_cents=1524),
    )

    assert summarize_sales(sales) == {
        "sales_count": 3,
        "units_sold": 8,
        "revenue_cents": 16998,
    }


def test_summarize_empty_sales():
    """Return zero totals when no sales are provided."""
    assert summarize_sales(()) == {
        "sales_count": 0,
        "units_sold": 0,
        "revenue_cents": 0,
    }
