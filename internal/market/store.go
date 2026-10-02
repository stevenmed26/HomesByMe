package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"time"
)

type Store struct{ DB *pgxpool.Pool }

var ErrMonthlyBudget = errors.New("monthly request budget exhausted")

type requestScope struct{ RunID, SearchID int64 }
type requestScopeKey struct{}

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
		return 0, fmt.Errorf("%w (%d/%d)", ErrMonthlyBudget, count, budget)
	}
	var id int64
	var runID, searchID *int64
	if scope, ok := ctx.Value(requestScopeKey{}).(requestScope); ok {
		runID = &scope.RunID
		searchID = &scope.SearchID
	}
	e = tx.QueryRow(ctx, "INSERT INTO provider_api_usage(provider,endpoint,run_id,search_id) VALUES('rentcast',$1,$2,$3) RETURNING id", endpoint, runID, searchID).Scan(&id)
	if e != nil {
		return 0, e
	}
	if runID != nil {
		if _, e = tx.Exec(ctx, "UPDATE ingestion_runs SET request_count=request_count+1,updated_at=now() WHERE id=$1", *runID); e != nil {
			return 0, e
		}
		if _, e = tx.Exec(ctx, "UPDATE ingestion_searches SET request_count=request_count+1,updated_at=now() WHERE id=$1", *searchID); e != nil {
			return 0, e
		}
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
	if e = s.saveTx(ctx, tx, r, at, nil); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// saveTx is shared by direct fixture imports and the atomic run apply checkpoint.
func (s *Store) saveTx(ctx context.Context, tx pgx.Tx, r Record, at time.Time, runID *int64) error {
	var e error
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
	e = tx.QueryRow(ctx, `INSERT INTO listing_observations(listing_id,observed_at,status,price,days_on_market,provider_modified_at,listed_date,property_snapshot,raw_response,ingestion_run_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, lid, at, r.Status, r.Price, r.DOM, r.Modified, r.Listed, snapshot, []byte(r.Raw), runID).Scan(&oid)
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
	return nil
}
