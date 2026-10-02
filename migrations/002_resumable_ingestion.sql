-- NOT VALID preserves legacy invalid configurations for explicit correction while
-- enforcing the constraint on every new/updated row. Go rejects enabled invalid rows.
ALTER TABLE search_areas DROP CONSTRAINT IF EXISTS search_areas_check;
ALTER TABLE search_areas ADD CONSTRAINT search_area_valid CHECK (
 (zip_code IS NULL OR zip_code ~ '^[0-9]{5}$') AND
 ((city IS NULL AND state IS NULL) OR
  (city IS NOT NULL AND city ~ '[^[:space:]]' AND state IS NOT NULL AND state ~ '^[A-Z]{2}$')) AND
 (zip_code IS NOT NULL OR (city IS NOT NULL AND state IS NOT NULL))
) NOT VALID;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM search_areas WHERE NOT (
  (zip_code IS NULL OR zip_code ~ '^[0-9]{5}$') AND
  ((city IS NULL AND state IS NULL) OR (city IS NOT NULL AND city ~ '[^[:space:]]' AND state IS NOT NULL AND state ~ '^[A-Z]{2}$')) AND
  (zip_code IS NOT NULL OR (city IS NOT NULL AND state IS NOT NULL))
 )) THEN ALTER TABLE search_areas VALIDATE CONSTRAINT search_area_valid; END IF;
END $$;

CREATE TABLE ingestion_runs (
 id bigserial PRIMARY KEY, provider text NOT NULL DEFAULT 'rentcast',
 state text NOT NULL DEFAULT 'collecting' CHECK(state IN ('collecting','paused','failed','applying','completed','degraded')),
 started_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 completed_at timestamptz, failure_reason text,
 request_count integer NOT NULL DEFAULT 0, observation_count integer NOT NULL DEFAULT 0,
 rejected_count integer NOT NULL DEFAULT 0, conflict_count integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX one_unfinished_ingestion ON ingestion_runs(provider) WHERE completed_at IS NULL;
CREATE TABLE ingestion_searches (
 id bigserial PRIMARY KEY, run_id bigint NOT NULL REFERENCES ingestion_runs(id),
 area_id bigint NOT NULL, area jsonb NOT NULL,
 status text NOT NULL CHECK(status IN ('Active','Inactive')),
 next_offset integer NOT NULL DEFAULT 0, page_size integer NOT NULL DEFAULT 500,
 completed_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
 request_count integer NOT NULL DEFAULT 0, record_count integer NOT NULL DEFAULT 0,
 observation_count integer NOT NULL DEFAULT 0,
 rejected_count integer NOT NULL DEFAULT 0,
 UNIQUE(run_id,area_id,status)
);
CREATE TABLE ingestion_pages (
 id bigserial PRIMARY KEY, search_id bigint NOT NULL REFERENCES ingestion_searches(id),
 page_offset integer NOT NULL, fetched_at timestamptz NOT NULL DEFAULT now(),
 payload bytea NOT NULL, processed_at timestamptz, failure_reason text,
 UNIQUE(search_id,page_offset)
);
CREATE TABLE ingestion_records (
 id bigserial PRIMARY KEY, page_id bigint NOT NULL REFERENCES ingestion_pages(id),
 position integer NOT NULL, raw_record bytea NOT NULL, listing_key text,
 outcome text NOT NULL, diagnostic text, UNIQUE(page_id,position)
);
CREATE TABLE ingestion_candidates (
 run_id bigint NOT NULL REFERENCES ingestion_runs(id), listing_key text NOT NULL,
 record jsonb NOT NULL, fingerprint text NOT NULL, observed_at timestamptz NOT NULL,
 conflicted boolean NOT NULL DEFAULT false, applied_at timestamptz,
 PRIMARY KEY(run_id,listing_key)
);
ALTER TABLE provider_api_usage ADD COLUMN run_id bigint REFERENCES ingestion_runs(id);
ALTER TABLE provider_api_usage ADD COLUMN search_id bigint REFERENCES ingestion_searches(id);
ALTER TABLE listing_observations ADD COLUMN ingestion_run_id bigint REFERENCES ingestion_runs(id);
CREATE UNIQUE INDEX observations_run_listing ON listing_observations(ingestion_run_id,listing_id) WHERE ingestion_run_id IS NOT NULL;
CREATE INDEX ingestion_searches_run ON ingestion_searches(run_id,id);
CREATE INDEX ingestion_records_key ON ingestion_records(listing_key);
