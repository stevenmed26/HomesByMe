CREATE EXTENSION IF NOT EXISTS postgis;
CREATE TABLE IF NOT EXISTS properties (
 id bigserial PRIMARY KEY, normalized_address text NOT NULL UNIQUE,
 address_line_1 text, city text, state text, zip_code text,
 latitude double precision, longitude double precision,
 location geography(Point,4326), bedrooms numeric, bathrooms numeric,
 square_feet numeric, lot_square_feet numeric, year_built integer,
 property_type text, neighborhood text,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS properties_location_idx ON properties USING gist(location);
CREATE TABLE IF NOT EXISTS listings (
 id bigserial PRIMARY KEY, property_id bigint NOT NULL REFERENCES properties(id),
 provider text NOT NULL, provider_listing_id text NOT NULL, mls_number text,
 original_list_date timestamptz, original_list_price numeric,
 first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL,
 current_status text NOT NULL, current_price numeric,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(provider,provider_listing_id)
);
CREATE INDEX IF NOT EXISTS listings_property_idx ON listings(property_id);
CREATE TABLE IF NOT EXISTS listing_observations (
 id bigserial PRIMARY KEY, listing_id bigint NOT NULL REFERENCES listings(id),
 observed_at timestamptz NOT NULL, status text NOT NULL, price numeric,
 days_on_market integer, provider_modified_at timestamptz,
 listed_date timestamptz, property_snapshot jsonb NOT NULL, raw_response jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS observations_latest_idx ON listing_observations(listing_id,observed_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS listing_events (
 id bigserial PRIMARY KEY, listing_id bigint NOT NULL REFERENCES listings(id),
 observation_id bigint NOT NULL REFERENCES listing_observations(id),
 occurred_at timestamptz NOT NULL, event_type text NOT NULL,
 old_price numeric, price numeric, details jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_listing_idx ON listing_events(listing_id,occurred_at);
CREATE TABLE IF NOT EXISTS search_areas (
 id bigserial PRIMARY KEY, city text, state text, zip_code text,
 enabled boolean NOT NULL DEFAULT true,
 CHECK ((zip_code ~ '^[0-9]{5}$') OR (length(trim(city)) > 0 AND state ~ '^[A-Z]{2}$'))
);
CREATE TABLE IF NOT EXISTS provider_api_usage (
 id bigserial PRIMARY KEY, provider text NOT NULL, request_time timestamptz NOT NULL DEFAULT now(),
 endpoint text NOT NULL, successful boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS usage_month_idx ON provider_api_usage(provider,request_time);
CREATE OR REPLACE FUNCTION deny_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'History is append-only'; END $$;
DROP TRIGGER IF EXISTS immutable_observations ON listing_observations;
CREATE TRIGGER immutable_observations BEFORE UPDATE OR DELETE ON listing_observations FOR EACH ROW EXECUTE FUNCTION deny_history_mutation();
DROP TRIGGER IF EXISTS immutable_events ON listing_events;
CREATE TRIGGER immutable_events BEFORE UPDATE OR DELETE ON listing_events FOR EACH ROW EXECUTE FUNCTION deny_history_mutation();
