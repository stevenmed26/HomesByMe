package market

import (
	"context"
	"os"
	"strconv"
	"time"
)

func StaleHours() int {
	n, e := strconv.Atoi(os.Getenv("STALE_AFTER_HOURS"))
	if e != nil || n < 1 || n > 876000 {
		return 72
	}
	return n
}

// Coverage is global collection coverage, not a completeness guarantee from the
// provider. Freshness is measured from fetch time, never from a later replay.
func (s *Store) Coverage(ctx context.Context, now time.Time, hours int) (map[string]any, error) {
	var out map[string]any
	e := s.DB.QueryRow(ctx, `WITH latest AS (SELECT * FROM ingestion_runs ORDER BY id DESC LIMIT 1),
 configured AS (SELECT id,jsonb_build_object('city',coalesce(city,''),'state',coalesce(state,''),'zip',coalesce(zip_code,'')) area FROM search_areas WHERE enabled),
 matching_runs AS (
  SELECT r.* FROM ingestion_runs r WHERE completed_at IS NOT NULL
  AND NOT EXISTS(SELECT id,area FROM configured EXCEPT SELECT area_id,area FROM ingestion_searches WHERE run_id=r.id)
  AND NOT EXISTS(SELECT area_id,area FROM ingestion_searches WHERE run_id=r.id EXCEPT SELECT id,area FROM configured)
 ), last_done AS (SELECT * FROM matching_runs ORDER BY id DESC LIMIT 1),
 facts AS (SELECT
  NOT EXISTS(SELECT 1 FROM configured) OR NOT EXISTS(SELECT 1 FROM latest WHERE completed_at IS NOT NULL)
   OR EXISTS(SELECT id,area FROM configured EXCEPT SELECT area_id,area FROM ingestion_searches WHERE run_id=(SELECT id FROM latest))
   OR EXISTS(SELECT area_id,area FROM ingestion_searches WHERE run_id=(SELECT id FROM latest) EXCEPT SELECT id,area FROM configured) incomplete,
  (coalesce((SELECT rejected_count>0 OR conflict_count>0 FROM latest),false)
    OR coalesce((SELECT state='degraded' FROM last_done),false)) degraded,
  (SELECT min(p.fetched_at) FROM ingestion_pages p JOIN ingestion_searches s ON s.id=p.search_id WHERE s.run_id=(SELECT id FROM last_done)) oldest_fetch,
  (SELECT count(*) FROM listings WHERE last_seen_at < $1::timestamptz-make_interval(hours=>$2)) stale_listings
 ) SELECT jsonb_build_object(
 'latest_run',(SELECT to_jsonb(r) FROM latest r),
 'searches',(SELECT coalesce(jsonb_agg(to_jsonb(s) || jsonb_build_object(
   'oldest_fetch_at',(SELECT min(fetched_at) FROM ingestion_pages WHERE search_id=s.id),
   'conflict_count',(SELECT count(DISTINCT c.listing_key) FROM ingestion_candidates c JOIN ingestion_records ir ON ir.listing_key=c.listing_key JOIN ingestion_pages p ON p.id=ir.page_id WHERE c.run_id=s.run_id AND c.conflicted AND p.search_id=s.id),
   'stale',coalesce((SELECT min(fetched_at)<$1::timestamptz-make_interval(hours=>$2) FROM ingestion_pages WHERE search_id=s.id),true)
  ) ORDER BY s.id),'[]') FROM ingestion_searches s WHERE run_id=(SELECT id FROM latest)),
 'configured_areas',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]') FROM configured c),
 'last_completed_collection_at',(SELECT completed_at FROM last_done),
 'last_clean_collection_at',(SELECT max(completed_at) FROM matching_runs WHERE state='completed'),
 'oldest_fetch_at',oldest_fetch,
 'stale_after_hours',$2::integer,'as_of',$1::timestamptz,
 'stale_listing_count',stale_listings,'incomplete',incomplete,'degraded',degraded,
 'stale',oldest_fetch IS NULL OR oldest_fetch<$1::timestamptz-make_interval(hours=>$2) OR stale_listings>0
 ) FROM facts`, now, hours).Scan(&out)
	return out, e
}
