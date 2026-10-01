# HomesByMe

A local housing-market research tool: Go ingestion/API, PostgreSQL + PostGIS history, and a Next.js/TypeScript dashboard. **INACTIVE means only inactive, never pending or sold.** No production demo data is inserted.

## Run

Install Docker Desktop and start its Linux container engine.

```powershell
Copy-Item .env.example .env
# Set RENTCAST_API_KEY in .env before ingestion.
docker compose up --build -d
```

Open http://localhost:3001. API health: http://localhost:18080/api/health. PostgreSQL uses port 55432. Override `WEB_PORT`, `API_PORT`, and `POSTGRES_PORT` in `.env` if needed. The dashboard works without a provider key and initially shows an empty state. Services bind to localhost. The migration service runs before the API; the database persists in `market_data`. `docker compose down` preserves history. This trusted local application has no authentication; do not expose it publicly.

## Search areas and manual ingestion

Search areas live in PostgreSQL. Start with one ZIP to conserve requests:

```powershell
docker compose exec db psql -U market -d market -c "INSERT INTO search_areas(zip_code) VALUES ('75001');"
# Alternative: a city with its state
docker compose exec db psql -U market -d market -c "INSERT INTO search_areas(city,state) VALUES ('Richardson','TX');"
docker compose exec db psql -U market -d market -c "SELECT * FROM search_areas;"
docker compose exec db psql -U market -d market -c "UPDATE search_areas SET enabled=false WHERE id=2;"
docker compose run --rm api ingest
```

Each area is explicitly searched for Active and Inactive records, in pages of 500. A shorter page ends that search; a full page requires another request. Missing results never imply inactivity or removal. Overlapping results are deduplicated within the run, but overlapping searches still spend requests. Single-listing lookup is implemented for future targeted refreshes.

`RENTCAST_MONTHLY_BUDGET` defaults to 50, `MAX_REQUESTS_PER_RUN` to 4. Requests are reserved in PostgreSQL before dispatch under a transaction lock; failures/interrupted requests count conservatively toward the UTC monthly limit. There are no automatic retries or scheduled requests. Dashboard reads do not call RentCast. Usage made by other applications with the same API key is not tracked here.

The per-run cap returns an explicit error and retains already committed observations. It does not claim complete coverage. Narrow the search area or deliberately raise the cap. There is no resume cursor yet; reruns start at the first area/page. JSON logs report areas, statuses, offsets, totals, and failures without logging credentials.

## Architecture and files

- `cmd/market/main.go`: `serve`, `migrate`, `ingest`, and explicit `fixture` commands.
- `internal/market/model.go`: provider-neutral records, address identity, and event derivation.
- `internal/market/provider.go`: provider interface, RentCast REST client, normalization, request accounting.
- `internal/market/store.go`: transactional ingestion and history persistence.
- `internal/market/analytics.go`: centralized parameterized filters and SQL analytics.
- `internal/market/http.go`: read-only REST API.
- `migrations/001_initial.sql`: transactional/idempotent initial schema, PostGIS, immutable-history triggers.
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
- `/api/listings`: all statuses, 100 records/page; zero-based `page`; `sort=address|price|sqft|ppsf|beds|baths|dom|status|changed`, `direction=asc|desc`.
- `/api/properties/{id}`: property, listing streams, chronological events/observations.
- `/api/usage`: current UTC month request count, success count, configured budget.
- `/api/areas`: search configuration.
- `/api/health`: database connectivity.

Listings and analytics accept `city`, `zip`, `type`, `min_price`, `max_price`, `min_sqft`, `max_sqft`, `beds`, `baths`, `min_year`, `max_year`, `min_lot`, `max_lot`. Numeric bounds are inclusive; beds/baths are minimums. City/type comparisons are case-insensitive exact matches. Filters apply to current attributes. PostGIS geography and its GiST index are ready for future radius/polygon filtering; those inputs are not implemented yet.

Inventory counts active listing streams. Price, price/sqft, DOM medians and average DOM use active listings; null values are excluded and zero sqft produces no ratio. Reduction percentage is active listings with at least one observed reduction divided by active inventory. Median reduction is the dollar median over downward price events in the matching cohort. Relisting percentage is matching listings with a RELISTED event divided by all matching listings. Inactive/reactivation counts are distinct matching listings ever making the transition. Time to inactive is the median fractional days from first observation to first inactive transition. Empty medians/percentages are null, displayed as a dash. Event metrics use all collected history; no date windows yet. These measure listings, not contracts or sales.

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
```

Integration tests require a disposable PostGIS database with schema/extension creation permissions:

```powershell
docker compose exec db psql -U market -d postgres -c "CREATE DATABASE market_test;"
$env:TEST_DATABASE_URL='postgres://market:market@localhost:55432/market_test?sslmode=disable'
go test ./internal/market -run TestDatabasePipeline -v
```

The test creates/drops an isolated schema. Without `TEST_DATABASE_URL` it is explicitly skipped. Tests cover normalization, unit preservation, ID/MLS/address precedence, RentCast mapping, status transitions, relisting evidence, price/details changes, unchanged observations, safe filters, and (with PostgreSQL) deduplication, preserved history, SQL analytics, immutable history, and API budgets.

Verified during implementation: `go test ./...` including the PostGIS integration test passed; `go vet ./...` passed; Next.js production build/type checking passed; Docker Compose built and started successfully; live HTTP checks for dashboard, comparison, health, analytics, listings, and usage returned 200. Integration coverage also exercises the real HTTP adapter against a local test server, repeated ingestion, request accounting, REST property history, null/empty analytics, and even-sized median interpolation. Dependency installation reported zero npm vulnerabilities after updating Next.js. No real RentCast requests were used. Browser interaction/visual testing and live provider credentials remain unverified.

Explicit fixture mode: set `ALLOW_FIXTURE_IMPORT=true`, point `DATABASE_URL` at a disposable database, migrate, then run `go run ./cmd/market fixture path.json`. Input is a JSON array of RentCast-shaped records. Imported records use provider `fixture`; no production fixture is automatically loaded.

## Limitations and recommended next task

No scheduler, authentication, migration version ledger, ingestion-run/resume table, fuzzy address standardization, cross-provider reconciliation, or actual contract/sale data. Quota/failure interruptions can leave partial coverage. Absent listings retain their last observed state. Reactivation without a changed listed date does not count as relisting. Property details currently return full history in one response. Multiple future provider streams would count separately until reconciliation is added. Provider records can be stale; raw history and last-observed times preserve evidence.

Next: add durable ingestion-run checkpoints and coverage/staleness indicators before scheduling recurring collection, so quota-limited partial runs can resume and incomplete coverage is visible.

Provider documentation: [Sale listings](https://developers.rentcast.io/reference/sale-listings), [listing schema](https://developers.rentcast.io/reference/property-listings-schema).
