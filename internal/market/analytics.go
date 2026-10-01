package market

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

func Filter(q url.Values) (string, []any, error) {
	clauses := []string{"TRUE"}
	args := []any{}
	add := func(col, op string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf("%s %s $%d", col, op, len(args)))
	}
	for _, f := range []struct{ key, col, op string }{{"min_price", "l.current_price", ">="}, {"max_price", "l.current_price", "<="}, {"min_sqft", "p.square_feet", ">="}, {"max_sqft", "p.square_feet", "<="}, {"beds", "p.bedrooms", ">="}, {"baths", "p.bathrooms", ">="}, {"min_year", "p.year_built", ">="}, {"max_year", "p.year_built", "<="}, {"min_lot", "p.lot_square_feet", ">="}, {"max_lot", "p.lot_square_feet", "<="}} {
		if v := q.Get(f.key); v != "" {
			n, e := strconv.ParseFloat(v, 64)
			if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e12 {
				return "", nil, fmt.Errorf("invalid %s", f.key)
			}
			add(f.col, f.op, n)
		}
	}
	for _, f := range []struct{ key, col string }{{"city", "p.city"}, {"zip", "p.zip_code"}, {"type", "p.property_type"}} {
		if v := q.Get(f.key); v != "" {
			add("lower("+f.col+")", "=", strings.ToLower(v))
		}
	}
	return strings.Join(clauses, " AND "), args, nil
}

const cohortSQL = `WITH cohort AS (
 SELECT l.*,p.city,p.zip_code,p.square_feet,o.days_on_market,
 EXISTS(SELECT 1 FROM listing_events e WHERE e.listing_id=l.id AND e.event_type='PRICE_CHANGE' AND e.price<e.old_price) reduced,
 EXISTS(SELECT 1 FROM listing_events e WHERE e.listing_id=l.id AND e.event_type='RELISTED') relisted
 FROM listings l JOIN properties p ON p.id=l.property_id
 LEFT JOIN LATERAL (SELECT days_on_market FROM listing_observations WHERE listing_id=l.id ORDER BY observed_at DESC,id DESC LIMIT 1) o ON true WHERE %s
 ) `

func (s *Store) Analytics(ctx context.Context, q url.Values) (map[string]any, error) {
	where, args, e := Filter(q)
	if e != nil {
		return nil, e
	}
	sql := fmt.Sprintf(cohortSQL, where) + `SELECT jsonb_build_object(
 'active_inventory',count(*) FILTER(WHERE current_status='ACTIVE'),
 'median_price',percentile_cont(0.5) WITHIN GROUP(ORDER BY current_price) FILTER(WHERE current_status='ACTIVE'),
 'median_ppsf',percentile_cont(0.5) WITHIN GROUP(ORDER BY current_price/nullif(square_feet,0)) FILTER(WHERE current_status='ACTIVE'),
 'median_dom',percentile_cont(0.5) WITHIN GROUP(ORDER BY days_on_market) FILTER(WHERE current_status='ACTIVE'),
 'average_dom',avg(days_on_market) FILTER(WHERE current_status='ACTIVE'),
 'price_reduction_pct',100.0*count(*) FILTER(WHERE reduced AND current_status='ACTIVE')/nullif(count(*) FILTER(WHERE current_status='ACTIVE'),0),
 'relisting_pct',100.0*count(*) FILTER(WHERE relisted)/nullif(count(*),0),
 'became_inactive',(SELECT count(DISTINCT e.listing_id) FROM listing_events e JOIN cohort c ON c.id=e.listing_id WHERE event_type='BECAME_INACTIVE'),
 'became_active',(SELECT count(DISTINCT e.listing_id) FROM listing_events e JOIN cohort c ON c.id=e.listing_id WHERE event_type='BECAME_ACTIVE'),
 'median_reduction',(SELECT percentile_cont(0.5) WITHIN GROUP(ORDER BY e.old_price-e.price) FROM listing_events e JOIN cohort c ON c.id=e.listing_id WHERE event_type='PRICE_CHANGE' AND e.price<e.old_price),
 'median_days_to_inactive',(SELECT percentile_cont(0.5) WITHIN GROUP(ORDER BY days) FROM (SELECT extract(epoch FROM (min(e.occurred_at)-c.first_seen_at))/86400 days FROM cohort c JOIN listing_events e ON e.listing_id=c.id AND e.event_type='BECAME_INACTIVE' GROUP BY c.id,c.first_seen_at) durations),
 'inventory_by_city',(SELECT coalesce(jsonb_object_agg(city,n),'{}') FROM (SELECT coalesce(city,'Unknown') city,count(*) n FROM cohort WHERE current_status='ACTIVE' GROUP BY city) groups),
 'inventory_by_zip',(SELECT coalesce(jsonb_object_agg(zip_code,n),'{}') FROM (SELECT coalesce(zip_code,'Unknown') zip_code,count(*) n FROM cohort WHERE current_status='ACTIVE' GROUP BY zip_code) groups)
 ) FROM cohort`
	var out map[string]any
	e = s.DB.QueryRow(ctx, sql, args...).Scan(&out)
	return out, e
}
