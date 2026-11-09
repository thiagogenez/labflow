# Orders, Inventory and Merchant Summary Contract

Status: agreed business contract, 2026-11-09; implementation pending. Buyers place orders with finite-stock reservations at admission and asynchronous processing. Merchants query confirmed and pending order values by acceptance period. Payments and inventory-replenishment endpoints are deferred. Endpoint shapes and numerical bounds below remain implementation proposals.

## Actors and Scope

Buyers place orders. Merchants query their store's order values over a period. These values do not represent payments received. A load generator simulates both actors through the API; its seed reproduces a request sequence and belongs to experiment configuration, not either public business interface.

An order creates one asynchronous processing job. An order-summary query reads existing orders; it does not create sales or another job. Large exports or expensive analytical reports can receive a separate asynchronous contract when needed.

The existing `sales-v1` generator and aggregation tests remain intact. Their sale rows are not order entities: `sales_count` must not be relabelled as an order count. A future order-request generator will use a versioned catalog with consistent product prices and explicit initial stock per store/product. Initial stock is seeded once; restarting services must not replenish it.

## Buyer Interface

| Operation | Behavior |
| --- | --- |
| `POST /v1/orders` | Accept a bounded order with an `Idempotency-Key`; return `202`, its identifier, accepted price quote and `pending` status, only after the stock reservation commits |
| `GET /v1/orders/{order_id}` | Return the buyer's order, status and final totals when confirmed |

Example request body:

```json
{
  "store_id": "store-a",
  "items": [
    {"product_id": "sku-a", "quantity": 2},
    {"product_id": "sku-b", "quantity": 1}
  ]
}
```

The server resolves prices from its trusted catalog and freezes product identifiers, unit prices, quantities, currency, catalog version and `quoted_total_cents` when accepting the order. Buyers do not supply trusted prices, totals or acceptance timestamps. For example, unit prices of 1,990 and 3,500 cents produce a quoted total of 7,480 cents. Workers check the accepted snapshot and applicable business rules and return a proposed outcome. The core alone makes the authoritative decision. Workers neither invent purchases nor reprice accepted orders. A successful final total must match the frozen quote; inconsistent results must not finalize the order.

Proposed admission limits are 1–20 distinct product lines and integer quantities of 1–100 per line, with a maximum serialized event size of 64 KiB. These are initial policy bounds, not measured capacity. Reject invalid types, duplicate product lines, unknown products, currency mismatches and arithmetic overflow. Rate, pending-order and storage limits must also be set before load experiments.

Order state is `pending -> confirmed | rejected | processing_failed`. Pending orders have a durable reservation. A business rejection and an exhausted technical recovery have distinct safe reason codes. Retries leave the order pending; terminal states do not regress. Payment and fulfillment are not implied by confirmation.

## Finite Inventory and Reservations

For each store/product, track on-hand and reserved quantities; available stock is on-hand minus reserved. Require `0 <= reserved <= on_hand`. At admission, atomically check and reserve every item, save the accepted order/quote/job and its outbox event in one PostgreSQL transaction. Lock or conditionally update inventory rows in a consistent order. Any unavailable item rolls back all reservations; return a documented out-of-stock response, proposed `409`, rather than an accepted pending order. Such refused submissions are excluded from accepted-order summaries.

With ten available pieces and three concurrent submissions of five, exactly two can be accepted while the third is refused, assuming no intervening stock changes. Availability checks outside the reservation transaction are insufficient. No arrival-order fairness is promised. An HTTP replay with the same idempotency key must not reserve again.

On confirmation, decrement both on-hand and reserved by the reserved quantity. On rejection or definitive technical failure, decrement reserved only. These changes, reservation resolution, authoritative order/job state and duplicate suppression must commit together. Multi-item orders are resolved in full; partial confirmation is outside this slice.

A durable reservation deadline bounds abandoned work. A core recovery task must atomically expire overdue pending orders as `processing_failed` with a safe expiration reason and release their reservations, without requiring workers or Kafka to be available. Competing expiry and completion paths must serialize on the order/reservation: only one can finalize. Confirmation must require an active, unexpired reservation using core database time; late outcomes are recorded/suppressed without consuming released stock or reopening the order. An outage of the core can delay cleanup; recovery must process overdue reservations. Select the deadline and cleanup cadence during implementation/calibration and verify them before load experiments.

Replenishment through an authenticated, idempotent inventory-adjustment endpoint is a later extension. Workers, including Oracle workers, never update inventory or access PostgreSQL directly.

## Merchant Interface and Order Values

`GET /v1/stores/{store_id}/order-summary?from=<timestamp>&to=<timestamp>` returns a bounded order-value summary directly from PostgreSQL. The merchant identity must authorize that store. Buyer identities may read only their own orders; neither interface trusts a caller-supplied identity as authorization.

**Merchant summary:** include confirmed and pending orders, with separate amounts and counts. The combined amount represents confirmed orders plus the quoted value still pending; it must not be labelled as completed processing or confirmed payment. The agreed time filter uses server-assigned `accepted_at`. Payment states and events belong to a later extension.

| Amount field | Definition |
| --- | --- |
| `confirmed_amount_cents` | Sum of final `total_cents` for orders in `confirmed` state |
| `pending_amount_cents` | Sum of frozen `quoted_total_cents` for orders still in `pending` state, including retries |
| `total_amount_cents` | `confirmed_amount_cents + pending_amount_cents`; rejected and technically failed orders are excluded |

- Require offset-aware ISO 8601 timestamps and normalize to UTC. Use `[from, to)`: include the start and exclude the end. Initially cap the elapsed range at 31 days.
- Use integer cents and one configured two-decimal currency per authorized store; never sum different currencies together. Each order contributes to exactly one status bucket.
- Return the three amount fields above, `confirmed_orders`, `pending_orders`, `rejected_orders`, `processing_failed_orders`, currency, the requested range and `generated_at`. Empty periods return zero totals and counts.
- A successful transition moves the same quoted amount from pending to confirmed without increasing the combined total. A terminal failure removes it from pending and reduces the combined total; retain the appropriate rejection or technical-failure count. A duplicate result changes neither amounts nor counts.
- Calculate totals and status counts in one SQL statement, giving them the same committed-data view. A later request can change as pending orders finish; this endpoint is not a frozen historical report. `generated_at` is response metadata, not a replay watermark. [PostgreSQL statement snapshots](https://www.postgresql.org/docs/current/transaction-iso.html)
- Bound query duration and database concurrency. Reject an oversized query or report a timeout rather than silently returning a truncated total.

### Worked Example, Not Execution Evidence

For store A, `[2026-11-08T09:00:00Z, 2026-11-08T11:00:00Z)`:

| Order | Store | Accepted at (UTC) | Status | Amount, cents | Contribution |
| --- | --- | --- | --- | ---: | --- |
| A1 | A | 09:00 | confirmed | 5,000 | Confirmed amount |
| A2 | A | 10:30 | confirmed | 7,000 | Confirmed amount |
| A3 | A | 10:45 | pending | 9,000 quoted | Pending amount |
| A4 | A | 10:50 | processing_failed | 4,000 quoted | Technical-failure count only |
| A5 | A | 11:00 | confirmed | 2,000 | Excluded by end boundary |
| B1 | B | 10:00 | confirmed | 8,000 | Excluded by store |

Expected amounts are 12,000 cents confirmed, 9,000 pending and 21,000 combined, with two confirmed orders, one pending order and one technically failed order. The response's amount/count portion is:

```json
{
  "confirmed_amount_cents": 12000,
  "pending_amount_cents": 9000,
  "total_amount_cents": 21000,
  "confirmed_orders": 2,
  "pending_orders": 1,
  "rejected_orders": 0,
  "processing_failed_orders": 1
}
```

Redelivering A1's result leaves every value unchanged. If A3 becomes confirmed, the next query returns 21,000 confirmed, zero pending and the same 21,000 combined. Alternatively, if A3 reaches a definitive technical failure, the query returns 12,000 confirmed, zero pending and 12,000 combined, with two technically failed orders.

## Events and Persistence

There is **one Kafka cluster**, initially one broker/controller in KRaft mode, with two initial topics:

| Topic | Producer | Consumer | Contents |
| --- | --- | --- | --- |
| `labflow.orders.requests.v1` | Outbox publisher | Order workers | `order.received.v1`: immutable, bounded order snapshot |
| `labflow.orders.results.v1` | Order workers | Result consumer | `order.processing.result.v1`: compact outcome and final totals |

All local and later Oracle order workers share the order-worker consumer group. The result consumer uses a separate group. Three workers in an illustration represent concurrent orders, not three copies of one purchase. Partitions and available CPU constrain useful parallelism.

PostgreSQL stores official orders, inventory/reservations, processing jobs, attempts and results. The result consumer finalizes processing outcomes; the core recovery task can finalize expired pending orders through the same guarded transactional resolution rules. Merchant order values are derived from these deduplicated order rows, not incremented whenever an event is delivered.

| Boundary | Required behavior |
| --- | --- |
| API admission | Reserve every item and store the order, frozen price quote, processing job and outbox event in one transaction. An order must not commit without its publication intent. |
| HTTP retry | Scope the key by authenticated buyer, store and operation. For an accepted request, resolve the key before checking current prices or stock. Same key and canonical input return the existing order, including terminal or expired orders without a new reservation; changed input returns `409`. A catalog update must not reprice that replay. |
| Outbox publication | Mark sent after Kafka acknowledgement. A crash between acknowledgement and marking may publish again. |
| Worker completion | Publish the outcome durably before committing the input offset. Computation may repeat. Never advance past unfinished records in a partition. |
| Result persistence | Validate final totals against the accepted quote, then commit deduplication, authoritative order/job state, reservation consumption/release and final totals together; finally commit the result offset. A successful transition moves value between buckets; repeated delivery must not create another order or alter the summary. |
| Attempts and retries | Persist bounded logical attempts and scheduled retries with backoff and jitter. Redelivery of one message can re-execute the same logical attempt; it must not automatically create another attempt. Schedule retries through the outbox transaction; stale attempts cannot overwrite a terminal result. Quarantine malformed events and define explicit replay handling. |

Every event carries its schema version, event ID, order ID, job ID, attempt identifier and correlation ID. Order-request snapshots identify the reservation and its durable deadline. Persist the processing-contract version for reproducible retries; additional commercial policies require a justified requirement before adoption. Deduplication must also protect the final effect by order/job/attempt, since repeated computation can produce a new event ID. No system-wide exactly-once claim is made.

Workers receive enough accepted order data through Kafka to compute without database access. PostgreSQL stays in the core; Oracle workers use private Kafka connectivity only. A merchant's summary request reaches the core API, which performs the authorized database query.

## Load, Observability and Acceptance

Vary buyer arrival rate, basket size and merchant query concurrency independently. Record the catalog, request-generator version, seed, request trace, resources and Kafka configuration. Limits protect the Mac; the larger business scope is not a promise to finish within the original twenty-hour estimate.

Measure order acceptance-to-terminal time, pending age, processing throughput, retries, duplicates, consumer lag and worker resources. Measure summary-query latency/errors, database load, pending-order count/value and movement into confirmed, rejected or technically failed states. A steady combined amount can hide stalled workers and is not a completion SLI. Identifiers belong in structured logs, not metric labels.

The duplicate-delivery experiment must prove unchanged order counts, merchant order values and inventory after a result-consumer crash between database and offset commits. The CPU-HPA experiment remains a completion gate, but first measure whether the bounded order workload is CPU-limited. More workers cannot remedy a database/query bottleneck or create physical capacity. Do not add fake delays or busy loops to manufacture a result.

M2 acceptance requires concurrent tests for ten pieces/three five-piece submissions, multi-item reservation rollback, duplicate admission and completion, stock invariants, reservation release, restart-safe initial stock, expiry versus completion races, late-result suppression, core-outage recovery, and tests for price snapshots, mismatched final totals, HTTP idempotency (including price changes), duplicate results, stale attempts, retry exhaustion, period boundaries, status-bucket calculations and transitions, rejected/failed-amount exclusion, empty periods and cross-store access. Test outbox/offset crash windows against real PostgreSQL and Kafka.

The next implementation slice is deliberately small: typed order items, a versioned catalog/initial-stock fixture, integer total calculation and unit tests. It adds no HTTP server or infrastructure. Follow with transactional API/database reservation and summary reads, then the event pipeline and its failure tests, one reviewed step at a time.
