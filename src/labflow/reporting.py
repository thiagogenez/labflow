"""Summaries of synthetic sales."""

from labflow.synthetic import Sale


def summarize_sales(sales: tuple[Sale, ...]) -> dict[str, int]:
    """Summarize sale records, units sold and revenue in cents."""
    return {
        "sales_count": len(sales),
        "units_sold": sum(sale.quantity for sale in sales),
        "revenue_cents": sum(sale.quantity * sale.unit_price_cents for sale in sales),
    }
