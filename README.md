# HomesByMe

A local housing-market research tool: Go ingestion/API, PostgreSQL + PostGIS history, and a Next.js/TypeScript dashboard. **INACTIVE means only inactive, never pending, contingent, or sold.** No production demo data is inserted.

## Run

Install Docker Desktop and start its Linux container engine.

```powershell
Copy-Item .env.example .env
# Set RENTCAST_API_KEY in .env before ingestion.
docker compose up --build -d
```

Open http://localhost:3001. API health: http://localhost:18080/api/health. PostgreSQL uses port 55432. Override `WEB_PORT`, `API_PORT`, and `POSTGRES_PORT` in `.env` if needed. The dashboard works without a provider key and initially shows an empty state. Services bind to localhost. The migration service runs before the API; the database persists in `market_data`. `docker compose down` preserves history. This trusted local application has no authentication; do not expose it publicly.

For an existing checkout, keep your existing `.env` and database volume. Build/start with `docker compose up --build -d`; do not recreate the volume or use `down -v` to upgrade.

## Versioned database upgrades

`market migrate` applies numbered SQL files from `migrations/` and records their filenames, SHA-256 checksums, and application timestamps in `schema_migrations`. A PostgreSQL advisory transaction lock serializes concurrent migration attempts. All pending migrations and ledger entries commit together or roll back together. Changed applied files fail checksum validation; add a new numbered migration instead of editing an applied one. Checksums normalize CRLF/LF so Windows and containers agree.

The original unversioned database is upgraded in place: its idempotent `001_initial.sql` is adopted once, then `002_resumable_ingestion.sql` adds ingestion tables and nullable observation lineage. Existing properties, listings, observations, events, and raw responses are retained. History UPDATE/DELETE protection remains enabled.

```powershell
docker compose run --rm migrate
# Native equivalent:
go run ./cmd/market migrate
```

Legacy invalid search-area rows are preserved for correction. The replacement constraint is initially `NOT VALID`, so all new/updated rows must be valid immediately; it is fully validated automatically if no invalid legacy rows exist. Ingestion rejects enabled invalid legacy rows before creating a run or making requests. Correct them using `UPDATE search_areas`, then run `ALTER TABLE search_areas VALIDATE CONSTRAINT search_area_valid;`. Absent optional fields should be NULL, not empty strings. Do not disable the constraint.

## Search areas and manual ingestion

Search areas live in PostgreSQL. Start with one ZIP to conserve requests:

Go and PostgreSQL reject empty areas, malformed ZIPs, blank cities, and incomplete city/state pairs—even if another location field is valid. A ZIP must have exactly five digits; city/state requires a nonblank city and two uppercase state letters. If both a valid ZIP and a valid city/state pair are supplied, the provider searches by ZIP. An empty enabled-area set is rejected. Validation is syntactic; it does not verify that an area exists geographically.

```powershell
docker compose exec db psql -U market -d market -c "INSERT INTO search_areas(zip_code) VALUES ('75001');"
# Alternative: a city with its state
docker compose exec db psql -U market -d market -c "INSERT INTO search_areas(city,state) VALUES ('Richardson','TX');"
docker compose exec db psql -U market -d market -c "SELECT * FROM search_areas;"
docker compose exec db psql -U market -d market -c "UPDATE search_areas SET enabled=false WHERE id=2;"
docker compose run --rm api ingest
```

Each area is explicitly searched for Active and Inactive records, in pages of 500. A shorter page ends that search; a full page requires another request. Missing results never imply inactivity or removal. Overlapping results are deduplicated within the run, but overlapping searches still spend requests. Single-listing lookup is implemented for future targeted refreshes.

`RENTCAST_MONTHLY_BUDGET` defaults to 50, `MAX_REQUESTS_PER_RUN` to 4. The latter is now a **per-command-invocation request allowance**, not a lifetime limit on a durable run. Requests are reserved in PostgreSQL before dispatch under a transaction lock; failures/interrupted requests count conservatively toward the UTC monthly limit. Run/search request counters and request lineage are committed with that reservation. There are no automatic HTTP retries or scheduled requests. Dashboard reads do not call RentCast. Usage made by other applications with the same API key is not tracked here.

## Durable ingestion and resume

`market ingest` resumes the unfinished run if one exists; otherwise it starts a new run. `market resume` resumes only: it errors without spending requests if no unfinished run exists. The CLI session lock rejects concurrent ingestion attempts, and a unique database index permits only one unfinished RentCast run.

```powershell
docker compose run --rm api ingest
# Continue a request-capped, quota-limited, failed, or interrupted run:
docker compose run --rm api resume
# Native equivalents:
./market.exe ingest
./market.exe resume
```

Each run snapshots all enabled area IDs/configurations and creates separate Active/Inactive search checkpoints. Editing live search areas does not change a pending run; new settings apply to the next new run. Offset, page size, timestamps, request/record/observation counts, failures, and search completion persist in PostgreSQL.

Processing proceeds as follows:

1. Fetch the next unfinished search page, or load its already-stored payload.
2. Commit the raw response bytes and fetch timestamp **before** decoding/processing records.
3. Normalize each record. Persist valid candidates, rejected-record diagnostics, duplicate evidence, and conflicting evidence.
4. Commit page processing, counts, and the next offset/search-completed marker in one transaction. A processing failure rolls all of these back, retaining the fetched payload for replay.
5. After every search finishes, apply only nonconflicting candidates to listing history. An observation, its events/current state, run/search observation counters, and the candidate's applied marker commit together. A unique run/listing observation index is an additional idempotency guard.
6. Mark the run `completed`, or `degraded` if it has quarantined records/conflicts.

**Inventory changes are deferred until all searches have been staged**, so later overlapping responses cannot turn a previously accepted record into an invented status transition. During the application phase, listings commit individually; coverage remains incomplete until every accepted candidate is applied. Partial application resumes without duplicating history. Observation timestamps use the original page fetch time, not replay time. Repeated observations in different completed runs remain legitimate new history; replaying the same run is idempotent.

Identical normalized listing keys are deduplicated across the whole run. Differing business fields—including status, price, DOM, listed date, or property attributes—mark the key conflicted. All raw versions are retained; **none of that key's versions are applied**. Transport metadata such as `lastSeenDate` alone does not create a conflict. A conflicted existing listing retains its previous state and freshness timestamp. Per-search observation counts include distinct applied listings that appeared in that search, so overlapping searches' totals can exceed the run's deduplicated count.

Malformed individual records are quarantined while valid neighbors continue. Diagnostics and original raw records are available through the issues endpoint. An invalid whole-page envelope cannot safely establish pagination completion: it is retained with a failure reason, its checkpoint stays put, and resume reprocesses it without refetching. Such failures need investigation/a normalization fix; there is no automatic discard/refetch mechanism.

Run states: `collecting`, `paused` (request cap/monthly budget), `failed` (provider/storage/processing errors), `applying`, `completed`, and `degraded` (terminal but imperfect coverage). A hard process kill can leave `collecting`/`applying` visible; the next invocation reacquires the session lock and resumes persisted work. Failures never imply complete coverage. JSON logs include run/search identifiers and offsets without credentials.

**Unavoidable external-response crash window:** a process can die after RentCast receives the request or returns its response but before the payload INSERT commits. No local transaction can atomically include that external response. The reserved request stays counted, but resume must fetch that page again if no payload was durably stored. Once the payload is stored, processing/application replay requires no repeat request for that page.

## Coverage and freshness

The dashboard and comparison page show latest run state/failure, run totals, per-area Active/Inactive completion and next offset, rejected/conflict counts, and the last completed/last clean collection for the **current exact enabled configuration**. A changed configuration is incomplete until collected. Terminal degraded runs have a completion timestamp but remain visibly degraded. Coverage is global across configured areas, not scoped to dashboard property filters.

Set `STALE_AFTER_HOURS` (default 72; positive whole hours, maximum 876000) in `.env`; recreate the API container to change it. Invalid values fall back to 72. Listing rows expose `last_seen_at` and a computed `stale` flag. The dashboard labels inventory **last-observed active inventory**, including stale listings, and exposes sortable last-observed timestamps.

Warnings distinguish incomplete, degraded, and stale/unverified coverage. Collection freshness uses the oldest page fetch in the last completed matching run, so replaying an old run today does not make its data fresh. Any stored listing older than the threshold also triggers the global stale warning. A recent empty search does not refresh previously stored listings. Missing, withheld, and stale listings are never automatically marked inactive. `Refresh coverage` rereads the local database; it does not call RentCast. Reapply dashboard filters/reload to refresh listing metrics after ingestion.

## Architecture and files

- `cmd/market/main.go`: `serve`, `migrate`, `ingest`, `resume`, and explicit `fixture` commands.
- `internal/market/model.go`: provider-neutral records, address identity, and event derivation.
- `internal/market/provider.go`: provider interface, RentCast REST client, normalization, request accounting.
- `internal/market/store.go`: transactional history persistence and request reservations.
- `internal/market/ingestion.go`: durable checkpoints, page staging, quarantine/conflicts, and idempotent application.
- `internal/market/areas.go`, `migrations.go`, `coverage.go`: validation, serialized upgrades, coverage/freshness queries.
- `internal/market/analytics.go`: centralized parameterized filters and SQL analytics.
- `internal/market/http.go`: read-only REST API.
- `migrations/001_initial.sql`, `002_resumable_ingestion.sql`: original schema and additive resumability upgrade.
- `web/app`: dashboard, comparison page, property timeline, server-side API proxy, and styles.
- Compose, Dockerfiles, `.env.example`, dependency lockfiles, unit and integration tests.

The provider interface supports search, lookup, and normalization. Provider names and event/status values are text, allowing later providers and MLS statuses without changing table enums. Future MLS adapters need their own mappings and transition rules. Aggregate research datasets should use separate metric/source tables referencing geography instead of being forced into listing observations. No future providers are implemented.

## Database and duplicate handling

| Table | Purpose |
| --- | --- |
| properties | Normalized address, physical attributes, neighborhood, coordinates, indexed PostGIS geography |
| listings | Property link, provider identity, MLS number, original observed date/price, current status/price, first/last seen |
| listing_observations | Append-only state, price, DOM, source timestamp, listed date, property snapshot, raw provider JSON |
| listing_events | Append-only events linked to the triggering observation, old/new prices |
| search_areas | Enabled ZIP or city/state searches |
| provider_api_usage | Request time, provider, endpoint, success |
| schema_migrations | Applied version/checksum/time ledger |
| ingestion_runs | Durable state, timestamps, failures, request/observation/rejection/conflict totals |
| ingestion_searches | Snapshotted area/status, pagination checkpoint, counts, completion |
| ingestion_pages | Raw fetched bytes, fetch/process timestamps, page failure |
| ingestion_records | Every raw result, source page/position, identity, outcome/diagnostic |
| ingestion_candidates | Unique run/listing candidate, normalized fingerprint, conflict/apply markers |

Listing identity prefers stable provider ID, then provider-scoped MLS when ID is absent, then normalized address. A unique `(provider, provider_listing_id)` prevents repeated imports from creating new listings. Properties reuse an existing listing's property or match the unique normalized address. Normalization folds case, periods, commas, and whitespace while preserving unit identifiers. Different provider IDs sharing an MLS are conservatively separate listings; no fuzzy episode merge is attempted.

RentCast's identifier is property-oriented, so multiple listing episodes may share a stream. Listed-date changes are retained and can generate RELISTED. `original_list_price` is the earliest price observed by this app, not an inferred historical original asking price. Each property's update, observation, events, and current listing state commit atomically. CLI ingestion has a session lock; writes are serialized. Re-observing identical data adds an observation but no change events. Database triggers reject UPDATE/DELETE on observations and events.

## Status transitions

- First observation emits LISTED, meaning first recorded listing even if initially inactive.
- ACTIVE → INACTIVE emits BECAME_INACTIVE.
- INACTIVE → ACTIVE emits BECAME_ACTIVE.
- An active listing with a later known listed date emits RELISTED, possibly alongside reactivation.
- Two known unequal prices emit PRICE_CHANGE.
- A changed property snapshot emits PROPERTY_DETAILS_CHANGED.

Unknown-to-known price is not a price change. Missing listings do not emit REMOVED. No pending/contingent/sold events are invented. Event timestamps are local observation times. RentCast `lastSeenDate` is retained as a source timestamp, not claimed to be an actual MLS modification time. Timelines read only stored history.

## Metrics and REST endpoints

All endpoints are GET:

- `/api/analytics`: metrics and inventory grouped by city/ZIP.
- `/api/listings`: all statuses with `last_seen_at`/`stale`, 100 records/page; zero-based `page`; `sort=address|price|sqft|ppsf|beds|baths|dom|status|changed|observed`, `direction=asc|desc`.
- `/api/properties/{id}`: property, listing streams, chronological events/observations.
- `/api/usage`: current UTC month request count, success count, configured budget.
- `/api/areas`: search configuration.
- `/api/health`: database connectivity.
- `/api/coverage`: latest run, per-search coverage, current-area completion times, threshold and incomplete/degraded/stale flags.
- `/api/runs`: latest 50 runs.
- `/api/runs/{id}`: run and search checkpoints.
- `/api/runs/{id}/issues?page=0`: 100 rejected/conflicting records per page, including every version of conflicted keys. Raw evidence is returned as JSON text; full response pages remain in the database.

Listings and analytics accept `city`, `zip`, `type`, `min_price`, `max_price`, `min_sqft`, `max_sqft`, `beds`, `baths`, `min_year`, `max_year`, `min_lot`, `max_lot`. Numeric bounds are inclusive; beds/baths are minimums. City/type comparisons are case-insensitive exact matches. Filters apply to current attributes. PostGIS geography and its GiST index are ready for future radius/polygon filtering; those inputs are not implemented yet.

City/ZIP comparison uses the same shared price, sqft, beds/baths, year, property-type, and lot controls as the dashboard. Each area's query is cloned from one common filter set; only location differs. The submitted filter set remains displayed with the results. Up to ten areas can be compared at once.

Inventory counts last-observed active listing streams, including stale ones. Price, price/sqft, DOM medians and average DOM use that inventory; null values are excluded and zero sqft produces no ratio. Reduction percentage is active listings with at least one observed reduction divided by active inventory. Median reduction is the dollar median over downward price events in the matching cohort. Relisting percentage is matching listings with a RELISTED event divided by all matching listings. Inactive/reactivation counts are distinct matching listings ever making the transition. Time to inactive is the median fractional days from first observation to first inactive transition. Empty medians/percentages are null, displayed as a dash. Event metrics use all collected history; no date windows yet. These measure listings, not contracts or sales.

## Native development and tests

Go 1.24+, Node 24, and PostGIS are required. Compose reads `.env`; native Go does not, so set environment variables in your shell.

```powershell
docker compose up -d db
$env:DATABASE_URL='postgres://market:market@localhost:55432/market?sslmode=disable'
go run ./cmd/market migrate
go run ./cmd/market serve
# Another terminal with RENTCAST_API_KEY set:
go build -o market.exe ./cmd/market
./market.exe ingest
# Frontend in another terminal:
cd web
npm ci
npm run dev -- --port 3001
```

```powershell
go test ./...
go vet ./...
cd web
npm run build
node --test tests/comparison.test.mjs
```

Integration tests require a disposable PostGIS database with schema/extension creation permissions:

```powershell
docker compose exec db psql -U market -d postgres -c "CREATE DATABASE market_test;"
$env:TEST_DATABASE_URL='postgres://market:market@localhost:55432/market_test?sslmode=disable'
go test ./... -v -count=1 -timeout 120s
```

Database tests create/drop isolated schemas. Without `TEST_DATABASE_URL` they explicitly skip. Tests cover original normalization/events/analytics and invalid search areas, per-invocation cap/resume, monthly budget exhaustion, pagination, page/apply crash replay, transaction rollback, valid/malformed mixtures, overlapping conflicts, concurrent ingestion, concurrent migrations, legacy upgrades preserving history, HTTP failures, retained invalid pages, and coverage/freshness (including old fetch times after replay). All provider requests go to local `httptest` servers with test-only keys; tests do not call RentCast. Frontend tests verify identical shared filters for city and ZIP comparisons.

Verification commands are above. Live RentCast credentials and browser interaction/visual checks are deliberately separate from the automated database/HTTP and frontend-build checks. No real RentCast requests are needed for validation.

Explicit fixture mode: set `ALLOW_FIXTURE_IMPORT=true`, point `DATABASE_URL` at a disposable database, migrate, then run `go run ./cmd/market fixture path.json`. Input is a JSON array of RentCast-shaped records. Imported records use provider `fixture`; no production fixture is automatically loaded.

## Limitations and recommended next task

No scheduling or additional providers are included. There is no authentication, fuzzy address standardization, cross-provider reconciliation, or actual contract/sale data. Provider offset pagination is not a point-in-time snapshot: records can move between pages during long or resumed runs. Completion means all configured searches reached their end, not guaranteed provider completeness. Conflicts are withheld conservatively, even if they represent a genuine change during collection. Raw page storage has no automatic retention policy; it grows over time. There is no run cancellation or manual conflict-resolution UI. An irreparably malformed stored page requires investigation rather than automatic refetch. Property details currently return full history in one response.

Next: add a read-only diagnostics page for inspecting quarantined/conflicting records and deciding explicit reconciliation rules. Scheduling remains outside this change.

Provider documentation: [Sale listings](https://developers.rentcast.io/reference/sale-listings), [listing schema](https://developers.rentcast.io/reference/property-listings-schema).
