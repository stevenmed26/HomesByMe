package market

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	send := func(w http.ResponseWriter, v any, e error) {
		w.Header().Set("Content-Type", "application/json")
		if e != nil {
			slog.Error("api_error", "error", e)
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Database request failed"})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		send(w, map[string]string{"status": "ok"}, s.DB.Ping(r.Context()))
	})
	mux.HandleFunc("GET /api/coverage", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Coverage(r.Context(), time.Now().UTC(), StaleHours())
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var v []map[string]any
		e := s.DB.QueryRow(r.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id DESC),'[]') FROM (SELECT * FROM ingestion_runs ORDER BY id DESC LIMIT 50)r`).Scan(&v)
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 {
			http.Error(w, "invalid run ID", 400)
			return
		}
		var v map[string]any
		e = s.DB.QueryRow(r.Context(), `SELECT to_jsonb(r) || jsonb_build_object('searches',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY id),'[]') FROM ingestion_searches s WHERE run_id=r.id)) FROM ingestion_runs r WHERE id=$1`, id).Scan(&v)
		if e != nil && e.Error() == "no rows in result set" {
			http.Error(w, "Run not found", 404)
			return
		}
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/runs/{id}/issues", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 {
			http.Error(w, "invalid run ID", 400)
			return
		}
		page, e := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("page") == "" {
			page = 0
			e = nil
		}
		if e != nil || page < 0 || page > 100000 {
			http.Error(w, "invalid page", 400)
			return
		}
		var v []map[string]any
		e = s.DB.QueryRow(r.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY id),'[]') FROM (SELECT ir.id,ir.page_id,ir.position,ir.listing_key,ir.outcome,ir.diagnostic,convert_from(ir.raw_record,'UTF8') raw_record,coalesce(c.conflicted,false) conflicted,s.area,s.status,p.page_offset FROM ingestion_records ir JOIN ingestion_pages p ON p.id=ir.page_id JOIN ingestion_searches s ON s.id=p.search_id LEFT JOIN ingestion_candidates c ON c.run_id=s.run_id AND c.listing_key=ir.listing_key WHERE s.run_id=$1 AND (ir.outcome='rejected' OR c.conflicted) ORDER BY ir.id LIMIT 100 OFFSET $2)t`, id, page*100).Scan(&v)
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/analytics", func(w http.ResponseWriter, r *http.Request) {
		if _, _, e := Filter(r.URL.Query()); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		v, e := s.Analytics(r.Context(), r.URL.Query())
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		e := s.DB.QueryRow(r.Context(), `SELECT jsonb_build_object('provider','rentcast','requests',count(*),'successful',count(*) FILTER(WHERE successful),'month',to_char(now() AT TIME ZONE 'UTC','YYYY-MM')) FROM provider_api_usage WHERE provider='rentcast' AND request_time >= date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&v)
		if e == nil {
			budget, err := strconv.Atoi(os.Getenv("RENTCAST_MONTHLY_BUDGET"))
			if err != nil || budget < 1 {
				budget = 50
			}
			v["budget"] = budget
		}
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/areas", func(w http.ResponseWriter, r *http.Request) {
		var v []map[string]any
		e := s.DB.QueryRow(r.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]') FROM search_areas a`).Scan(&v)
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/listings", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		where, args, e := Filter(q)
		if e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		sorts := map[string]string{"address": "p.address_line_1", "price": "l.current_price", "sqft": "p.square_feet", "ppsf": "l.current_price/nullif(p.square_feet,0)", "beds": "p.bedrooms", "baths": "p.bathrooms", "dom": "o.days_on_market", "status": "l.current_status", "changed": "changed", "observed": "l.last_seen_at"}
		sort := sorts[q.Get("sort")]
		if sort == "" {
			sort = "l.id"
		}
		dir := "ASC"
		if q.Get("direction") == "desc" {
			dir = "DESC"
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 0 || page > 100000 {
			http.Error(w, "invalid page", 400)
			return
		}
		sql := fmt.Sprintf(`SELECT coalesce(jsonb_agg(to_jsonb(t)),'[]') FROM (SELECT l.id,l.property_id,p.address_line_1,p.city,p.zip_code,l.current_price,p.square_feet,l.current_price/nullif(p.square_feet,0) ppsf,p.bedrooms,p.bathrooms,o.days_on_market,l.current_status,l.last_seen_at,l.last_seen_at < now()-make_interval(hours=>%d) stale,(SELECT max(occurred_at) FROM listing_events WHERE listing_id=l.id) changed FROM listings l JOIN properties p ON p.id=l.property_id LEFT JOIN LATERAL(SELECT days_on_market FROM listing_observations WHERE listing_id=l.id ORDER BY observed_at DESC,id DESC LIMIT 1)o ON true WHERE %s ORDER BY %s %s NULLS LAST,l.id LIMIT 100 OFFSET %d)t`, StaleHours(), where, sort, dir, page*100)
		var v []map[string]any
		e = s.DB.QueryRow(r.Context(), sql, args...).Scan(&v)
		send(w, v, e)
	})
	mux.HandleFunc("GET /api/properties/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 {
			http.Error(w, "invalid property ID", 400)
			return
		}
		var v map[string]any
		e = s.DB.QueryRow(r.Context(), `SELECT jsonb_build_object('property',to_jsonb(p)-'location','listings',(SELECT coalesce(jsonb_agg(to_jsonb(l)),'[]') FROM listings l WHERE property_id=p.id),'events',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY occurred_at,id),'[]') FROM listing_events e JOIN (SELECT id lid FROM listings WHERE property_id=p.id) l ON l.lid=e.listing_id),'observations',(SELECT coalesce(jsonb_agg(to_jsonb(o)-'raw_response' ORDER BY observed_at,id),'[]') FROM listing_observations o JOIN (SELECT id lid FROM listings WHERE property_id=p.id) l ON l.lid=o.listing_id)) FROM properties p WHERE id=$1`, id).Scan(&v)
		if e != nil && e.Error() == "no rows in result set" {
			http.Error(w, "Property not found", 404)
			return
		}
		send(w, v, e)
	})
	return mux
}
