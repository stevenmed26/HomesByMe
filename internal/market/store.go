package market

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"time"
)

type Store struct{ DB *pgxpool.Pool }

func (s *Store) ReserveRequest(ctx context.Context, endpoint string, budget int) (int64, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(743201)"); e != nil {
		return 0, e
	}
	var count int
	e = tx.QueryRow(ctx, "SELECT count(*) FROM provider_api_usage WHERE provider='rentcast' AND request_time >= date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'").Scan(&count)
	if e != nil {
		return 0, e
	}
	if count >= budget {
		return 0, fmt.Errorf("monthly request budget exhausted (%d/%d)", count, budget)
	}
	var id int64
	e = tx.QueryRow(ctx, "INSERT INTO provider_api_usage(provider,endpoint) VALUES('rentcast',$1) RETURNING id", endpoint).Scan(&id)
	if e != nil {
		return 0, e
	}
	return id, tx.Commit(ctx)
}
func (s *Store) Save(ctx context.Context, r Record, at time.Time) error {
	if e := Validate(r); e != nil {
		return e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(743202)"); e != nil {
		return e
	}
	var lid, pid int64
	e = tx.QueryRow(ctx, "SELECT id,property_id FROM listings WHERE provider=$1 AND provider_listing_id=$2", r.Provider, ListingKey(r)).Scan(&lid, &pid)
	if e != nil && e != pgx.ErrNoRows {
		return e
	}
	p := r.Property
	if pid == 0 {
		e = tx.QueryRow(ctx, `INSERT INTO properties(normalized_address) VALUES($1) ON CONFLICT(normalized_address) DO UPDATE SET normalized_address=excluded.normalized_address RETURNING id`, AddressKey(p)).Scan(&pid)
		if e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `UPDATE properties SET address_line_1=$2,city=$3,state=$4,zip_code=$5,latitude=$6,longitude=$7,bedrooms=$8,bathrooms=$9,square_feet=$10,lot_square_feet=$11,year_built=$12,property_type=$13,neighborhood=$14,location=CASE WHEN $6::float8 IS NOT NULL AND $7::float8 IS NOT NULL THEN ST_SetSRID(ST_MakePoint($7,$6),4326)::geography ELSE NULL END,updated_at=$15 WHERE id=$1`, pid, p.Address, p.City, p.State, p.ZIP, p.Latitude, p.Longitude, p.Beds, p.Baths, p.Sqft, p.Lot, p.Year, p.Type, p.Neighborhood, at)
	if e != nil {
		return e
	}
	var prev *State
	if lid != 0 {
		var st State
		var snapshot []byte
		e = tx.QueryRow(ctx, `SELECT status,price,listed_date,property_snapshot FROM listing_observations WHERE listing_id=$1 ORDER BY observed_at DESC,id DESC LIMIT 1`, lid).Scan(&st.Status, &st.Price, &st.Listed, &snapshot)
		if e != nil && e != pgx.ErrNoRows {
			return e
		}
		if e == nil {
			if e = json.Unmarshal(snapshot, &st.Property); e != nil {
				return e
			}
			prev = &st
		}
	} else {
		e = tx.QueryRow(ctx, `INSERT INTO listings(property_id,provider,provider_listing_id,mls_number,original_list_date,original_list_price,first_seen_at,last_seen_at,current_status,current_price) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$8,$6) RETURNING id`, pid, r.Provider, ListingKey(r), r.MLS, r.Listed, r.Price, at, r.Status).Scan(&lid)
		if e != nil {
			return e
		}
	}
	snapshot, e := json.Marshal(p)
	if e != nil {
		return e
	}
	var oid int64
	e = tx.QueryRow(ctx, `INSERT INTO listing_observations(listing_id,observed_at,status,price,days_on_market,provider_modified_at,listed_date,property_snapshot,raw_response) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, lid, at, r.Status, r.Price, r.DOM, r.Modified, r.Listed, snapshot, []byte(r.Raw)).Scan(&oid)
	if e != nil {
		return e
	}
	var old *float64
	if prev != nil {
		old = prev.Price
	}
	for _, kind := range Events(prev, r) {
		_, e = tx.Exec(ctx, `INSERT INTO listing_events(listing_id,observation_id,occurred_at,event_type,old_price,price) VALUES($1,$2,$3,$4,$5,$6)`, lid, oid, at, kind, old, r.Price)
		if e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `UPDATE listings SET last_seen_at=$2,current_status=$3,current_price=$4,updated_at=$2 WHERE id=$1`, lid, at, r.Status, r.Price)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Ingest(ctx context.Context, p ListingProvider, maxRequests int) error {
	// A session lock prevents interleaved runs and inconsistent observation ordering.
	conn, e := s.DB.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	var locked bool
	if e = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(743203)").Scan(&locked); e != nil {
		return e
	}
	if !locked {
		return fmt.Errorf("another ingestion is running")
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(743203)")
	rows, e := s.DB.Query(ctx, "SELECT coalesce(city,''),coalesce(state,''),coalesce(zip_code,'') FROM search_areas WHERE enabled ORDER BY id")
	if e != nil {
		return e
	}
	var areas []Area
	for rows.Next() {
		var a Area
		if e = rows.Scan(&a.City, &a.State, &a.ZIP); e != nil {
			rows.Close()
			return e
		}
		areas = append(areas, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(areas) == 0 {
		return fmt.Errorf("configure at least one enabled search area")
	}
	seen := map[string]bool{}
	calls := 0
	saved := 0
	for _, a := range areas {
		for _, status := range []string{"Active", "Inactive"} {
			for offset := 0; ; offset += 500 {
				if calls >= maxRequests {
					return fmt.Errorf("run request cap reached (%d); partial history retained; narrow search areas or increase MAX_REQUESTS_PER_RUN", calls)
				}
				calls++
				slog.Info("provider_request", "city", a.City, "zip", a.ZIP, "status", status, "offset", offset)
				records, err := p.SearchListings(ctx, a, status, offset)
				if err != nil {
					return err
				}
				for _, r := range records {
					key := r.Provider + ":" + ListingKey(r)
					if seen[key] {
						continue
					}
					if err = s.Save(ctx, r, time.Now().UTC()); err != nil {
						return err
					}
					seen[key] = true
					saved++
				}
				if len(records) < 500 {
					break
				}
			}
		}
	}
	slog.Info("ingestion_complete", "requests", calls, "observations", saved)
	return nil
}
