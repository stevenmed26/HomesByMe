package market

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRequestCap = errors.New("invocation request cap reached; run ingest or resume to continue")
var ErrIngestionBusy = errors.New("another ingestion is running")
var ErrNoUnfinishedRun = errors.New("no unfinished ingestion run to resume")

// Ingest resumes the one unfinished run, or snapshots enabled areas for a new run.
// No request is made for a durably fetched page, even after process restart.
func (s *Store) Ingest(ctx context.Context, p ListingProvider, maxRequests int) error {
	return s.ingest(ctx, p, maxRequests, nil)
}

func (s *Store) Resume(ctx context.Context, p ListingProvider, maxRequests int) error {
	return s.ingest(ctx, p, maxRequests, nil, true)
}

// The optional fault hook is used only by crash/replay integration tests.
func (s *Store) ingest(ctx context.Context, p ListingProvider, maxRequests int, hook func(string) error, resumeOnly ...bool) (err error) {
	if maxRequests < 1 {
		return fmt.Errorf("request cap must be positive")
	}
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(743203)").Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrIngestionBusy
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := conn.Exec(cleanup, "SELECT pg_advisory_unlock(743203)"); e != nil {
			conn.Conn().Close(context.Background())
		}
	}()
	runID, err := s.startOrResume(ctx, resumeOnly...)
	if err != nil {
		return err
	}
	slog.Info("ingestion_started_or_resumed", "run_id", runID)
	defer func() {
		if err != nil {
			state := "failed"
			if errors.Is(err, ErrRequestCap) || errors.Is(err, ErrMonthlyBudget) {
				state = "paused"
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, e := s.DB.Exec(cleanup, "UPDATE ingestion_runs SET state=$2,failure_reason=$3,updated_at=now() WHERE id=$1 AND completed_at IS NULL", runID, state, err.Error())
			slog.Error("ingestion_interrupted", "run_id", runID, "error", err, "checkpoint_error", e)
		}
	}()
	calls := 0
	for {
		var searchID int64
		var areaJSON []byte
		var status string
		var offset int
		err = s.DB.QueryRow(ctx, `SELECT id,area,status,next_offset FROM ingestion_searches WHERE run_id=$1 AND completed_at IS NULL ORDER BY id LIMIT 1`, runID).Scan(&searchID, &areaJSON, &status, &offset)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
			break
		}
		if err != nil {
			return err
		}
		var area Area
		if err = json.Unmarshal(areaJSON, &area); err != nil {
			return err
		}
		if err = area.Validate(); err != nil {
			return err
		}
		var pageID int64
		err = s.DB.QueryRow(ctx, "SELECT id FROM ingestion_pages WHERE search_id=$1 AND page_offset=$2", searchID, offset).Scan(&pageID)
		if errors.Is(err, pgx.ErrNoRows) {
			if calls >= maxRequests {
				return ErrRequestCap
			}
			calls++
			slog.Info("provider_request", "run_id", runID, "search_id", searchID, "status", status, "offset", offset)
			scoped := context.WithValue(ctx, requestScopeKey{}, requestScope{runID, searchID})
			var payload []byte
			payload, err = p.SearchPage(scoped, area, status, offset)
			if err != nil {
				return err
			}
			// Unavoidable window: the provider response can be lost before this INSERT
			// commits. Its reserved request remains counted; resume must fetch again.
			err = s.DB.QueryRow(ctx, `INSERT INTO ingestion_pages(search_id,page_offset,payload) VALUES($1,$2,$3) RETURNING id`, searchID, offset, payload).Scan(&pageID)
			if err != nil {
				return err
			}
			if hook != nil {
				if err = hook("page_stored"); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}
		if err = s.processPage(ctx, p, pageID); err != nil {
			_, _ = s.DB.Exec(ctx, "UPDATE ingestion_pages SET failure_reason=$2 WHERE id=$1", pageID, err.Error())
			return err
		}
		if hook != nil {
			if err = hook("page_processed"); err != nil {
				return err
			}
		}
	}
	if _, err = s.DB.Exec(ctx, "UPDATE ingestion_runs SET state='applying',updated_at=now() WHERE id=$1", runID); err != nil {
		return err
	}
	// Only after every search is staged can overlaps be known. Conflicting keys
	// never reach observations/events/current state. Each accepted apply is atomic.
	for {
		var key string
		err = s.DB.QueryRow(ctx, "SELECT listing_key FROM ingestion_candidates WHERE run_id=$1 AND NOT conflicted AND applied_at IS NULL ORDER BY listing_key LIMIT 1", runID).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
			break
		}
		if err != nil {
			return err
		}
		if err = s.applyCandidate(ctx, runID, key); err != nil {
			return err
		}
		if hook != nil {
			if err = hook("candidate_applied"); err != nil {
				return err
			}
		}
	}
	_, err = s.DB.Exec(ctx, `UPDATE ingestion_runs SET state=CASE WHEN rejected_count>0 OR conflict_count>0 THEN 'degraded' ELSE 'completed' END,completed_at=now(),updated_at=now(),failure_reason=NULL WHERE id=$1`, runID)
	if err == nil {
		slog.Info("ingestion_finished", "run_id", runID)
	}
	return err
}

func (s *Store) startOrResume(ctx context.Context, resumeOnly ...bool) (int64, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	var runID int64
	e = tx.QueryRow(ctx, "SELECT id FROM ingestion_runs WHERE provider='rentcast' AND completed_at IS NULL ORDER BY id LIMIT 1").Scan(&runID)
	if e == nil {
		_, e = tx.Exec(ctx, "UPDATE ingestion_runs SET state='collecting',failure_reason=NULL,updated_at=now() WHERE id=$1", runID)
		if e != nil {
			return 0, e
		}
		return runID, tx.Commit(ctx)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return 0, e
	}
	if len(resumeOnly) > 0 && resumeOnly[0] {
		return 0, ErrNoUnfinishedRun
	}
	rows, e := tx.Query(ctx, "SELECT id,city,state,zip_code FROM search_areas WHERE enabled ORDER BY id")
	if e != nil {
		return 0, e
	}
	type configured struct {
		ID   int64
		Area Area
	}
	var areas []configured
	for rows.Next() {
		var id int64
		var city, state, zip *string
		if e = rows.Scan(&id, &city, &state, &zip); e != nil {
			rows.Close()
			return 0, e
		}
		a := Area{}
		if city != nil {
			a.City = *city
		}
		if state != nil {
			a.State = *state
		}
		if zip != nil {
			a.ZIP = *zip
		}
		// Empty strings are invalid present values, not absent optional fields.
		if city != nil && *city == "" || state != nil && *state == "" || zip != nil && *zip == "" {
			rows.Close()
			return 0, fmt.Errorf("search area %d: empty fields must be NULL", id)
		}
		if e = a.Validate(); e != nil {
			rows.Close()
			return 0, fmt.Errorf("search area %d: %w", id, e)
		}
		areas = append(areas, configured{id, a})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, e
	}
	if len(areas) == 0 {
		return 0, fmt.Errorf("configure at least one valid enabled search area")
	}
	if e = tx.QueryRow(ctx, "INSERT INTO ingestion_runs DEFAULT VALUES RETURNING id").Scan(&runID); e != nil {
		return 0, e
	}
	for _, a := range areas {
		raw, _ := json.Marshal(a.Area)
		for _, status := range []string{"Active", "Inactive"} {
			if _, e = tx.Exec(ctx, "INSERT INTO ingestion_searches(run_id,area_id,area,status) VALUES($1,$2,$3,$4)", runID, a.ID, raw, status); e != nil {
				return 0, e
			}
		}
	}
	return runID, tx.Commit(ctx)
}

// fingerprint excludes transport metadata while comparing every normalized
// business field (including known-vs-missing values). Raw evidence is kept separately.
func fingerprint(r Record) string {
	r.Raw = nil
	r.Modified = nil
	b, _ := json.Marshal(r)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (s *Store) processPage(ctx context.Context, p ListingProvider, pageID int64) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var payload []byte
	var at time.Time
	var processed *time.Time
	var searchID, runID int64
	var offset, size int
	e = tx.QueryRow(ctx, `SELECT p.payload,p.fetched_at,p.processed_at,p.search_id,p.page_offset,s.run_id,s.page_size FROM ingestion_pages p JOIN ingestion_searches s ON s.id=p.search_id WHERE p.id=$1 FOR UPDATE OF p`, pageID).Scan(&payload, &at, &processed, &searchID, &offset, &runID, &size)
	if e != nil {
		return e
	}
	if processed != nil {
		return nil
	}
	var records []json.RawMessage
	if strings.TrimSpace(string(payload)) == "null" {
		return fmt.Errorf("page %d: expected JSON array, got null", pageID)
	}
	if e = json.Unmarshal(payload, &records); e != nil {
		return fmt.Errorf("page %d: invalid listing array: %w", pageID, e)
	}
	if len(records) > size {
		return fmt.Errorf("page %d exceeds requested page size", pageID)
	}
	rejected := 0
	conflicts := 0
	for i, raw := range records {
		r, normalErr := p.NormalizeListing(raw)
		outcome := "accepted"
		diagnostic := ""
		key := ""
		if normalErr == nil {
			normalErr = Validate(r)
		}
		if normalErr != nil {
			outcome = "rejected"
			diagnostic = normalErr.Error()
			rejected++
		} else {
			key = r.Provider + ":" + ListingKey(r)
			fp := fingerprint(r)
			encoded, err := json.Marshal(r)
			if err != nil {
				return err
			}
			var prior string
			var conflicted bool
			err = tx.QueryRow(ctx, "SELECT fingerprint,conflicted FROM ingestion_candidates WHERE run_id=$1 AND listing_key=$2 FOR UPDATE", runID, key).Scan(&prior, &conflicted)
			if errors.Is(err, pgx.ErrNoRows) {
				_, e = tx.Exec(ctx, "INSERT INTO ingestion_candidates(run_id,listing_key,record,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5)", runID, key, encoded, fp, at)
			} else if err != nil {
				return err
			} else if prior != fp {
				outcome = "conflict"
				diagnostic = "Normalized listing differs from another result in this run; all versions withheld from history"
				if !conflicted {
					conflicts++
				}
				_, e = tx.Exec(ctx, "UPDATE ingestion_candidates SET conflicted=true WHERE run_id=$1 AND listing_key=$2", runID, key)
			} else {
				outcome = "duplicate"
			}
			if e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, `INSERT INTO ingestion_records(page_id,position,raw_record,listing_key,outcome,diagnostic) VALUES($1,$2,$3,NULLIF($4,''),$5,NULLIF($6,''))`, pageID, i, []byte(raw), key, outcome, diagnostic); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE ingestion_pages SET processed_at=now(),failure_reason=NULL WHERE id=$1", pageID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE ingestion_searches SET next_offset=$2,completed_at=CASE WHEN $3 THEN now() ELSE NULL END,updated_at=now(),record_count=record_count+$4,rejected_count=rejected_count+$5 WHERE id=$1`, searchID, offset+size, len(records) < size, len(records), rejected); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE ingestion_runs SET rejected_count=rejected_count+$2,conflict_count=conflict_count+$3,updated_at=now() WHERE id=$1`, runID, rejected, conflicts); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Store) applyCandidate(ctx context.Context, runID int64, key string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var at time.Time
	var applied *time.Time
	var conflicted bool
	e = tx.QueryRow(ctx, "SELECT record,observed_at,applied_at,conflicted FROM ingestion_candidates WHERE run_id=$1 AND listing_key=$2 FOR UPDATE", runID, key).Scan(&raw, &at, &applied, &conflicted)
	if e != nil {
		return e
	}
	if applied != nil || conflicted {
		return nil
	}
	var r Record
	if e = json.Unmarshal(raw, &r); e != nil {
		return e
	}
	if e = s.saveTx(ctx, tx, r, at, &runID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE ingestion_candidates SET applied_at=now() WHERE run_id=$1 AND listing_key=$2", runID, key); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE ingestion_runs SET observation_count=observation_count+1,updated_at=now() WHERE id=$1", runID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE ingestion_searches s SET observation_count=observation_count+1,updated_at=now() WHERE s.run_id=$1 AND EXISTS(SELECT 1 FROM ingestion_records r JOIN ingestion_pages p ON p.id=r.page_id WHERE p.search_id=s.id AND r.listing_key=$2)`, runID, key); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
