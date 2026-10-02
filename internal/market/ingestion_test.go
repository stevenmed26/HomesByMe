package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAreaValidation(t *testing.T) {
	for _, a := range []Area{{}, {ZIP: ""}, {ZIP: "7500"}, {ZIP: "7500x"}, {ZIP: "75001-1234"}, {City: "Dallas"}, {State: "TX"}, {City: " ", State: "TX"}, {City: "Dallas", State: "tx"}, {ZIP: "75001", City: "Dallas"}, {ZIP: "bad", City: "Dallas", State: "TX"}} {
		if a.Validate() == nil {
			t.Errorf("accepted invalid area %+v", a)
		}
	}
	for _, a := range []Area{{ZIP: "75001"}, {City: "Dallas", State: "TX"}, {ZIP: "75001", City: "Dallas", State: "TX"}} {
		if e := a.Validate(); e != nil {
			t.Errorf("valid area %+v: %v", a, e)
		}
	}
}

func testStore(t *testing.T, migrate bool) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PostGIS integration requires TEST_DATABASE_URL")
	}
	ctx := context.Background()
	db, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("ingestion_test_%d", time.Now().UnixNano())
	if _, e = db.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		db.Close()
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close(); db.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); db.Close() })
	s := &Store{DB: pool}
	if migrate {
		if e = s.Migrate(ctx, filepath.Join("..", "..", "migrations")); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func execTest(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, e := s.DB.Exec(context.Background(), sql, args...); e != nil {
		t.Fatal(e)
	}
}
func countTest(t *testing.T, s *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if e := s.DB.QueryRow(context.Background(), sql, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func localProvider(t *testing.T, s *Store, handler http.HandlerFunc) (*RentCast, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler(w, r) }))
	t.Cleanup(server.Close)
	return &RentCast{Store: s, Key: "test-key-never-real", BaseURL: server.URL, Client: server.Client(), Budget: 50}, calls
}

const goodRecord = `{"id":"one","addressLine1":"1 Test St","zipCode":"75001","status":"Active","price":400000}`

func activeOnly(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("status") == "Active" {
		fmt.Fprint(w, "["+goodRecord+"]")
	} else {
		fmt.Fprint(w, "[]")
	}
}

func TestSearchAreaConstraints(t *testing.T) {
	s := testStore(t, true)
	ctx := context.Background()
	for _, values := range [][]any{{nil, nil, nil}, {nil, nil, ""}, {nil, nil, "1234"}, {"Dallas", nil, nil}, {nil, "TX", nil}, {" ", "TX", nil}, {"\t\n", "TX", nil}, {"Dallas", "tx", nil}, {"Dallas", nil, "75001"}, {"Dallas", "TX", "bad"}, {"", "", "75001"}} {
		if _, e := s.DB.Exec(ctx, "INSERT INTO search_areas(city,state,zip_code) VALUES($1,$2,$3)", values...); e == nil {
			t.Errorf("database accepted %v", values)
		}
	}
	if _, e := s.startOrResume(ctx); e == nil {
		t.Fatal("empty enabled configuration accepted")
	}
	for _, values := range [][]any{{nil, nil, "75001"}, {"Dallas", "TX", nil}, {"Dallas", "TX", "75001"}} {
		execTest(t, s, "INSERT INTO search_areas(city,state,zip_code) VALUES($1,$2,$3)", values...)
	}
}

func TestRequestCapResumeAndSnapshot(t *testing.T) {
	s := testStore(t, true)
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("zipCode") != "75001" {
			t.Error("resume used changed config")
		}
		activeOnly(w, r)
	})
	if e := s.Ingest(context.Background(), p, 1); !errors.Is(e, ErrRequestCap) {
		t.Fatalf("expected cap: %v", e)
	}
	if countTest(t, s, "SELECT count(*) FROM ingestion_searches WHERE completed_at IS NOT NULL") != 1 {
		t.Fatal("completed search missing")
	}
	if countTest(t, s, "SELECT count(*) FROM listing_observations") != 0 {
		t.Fatal("history applied before overlap checking")
	}
	execTest(t, s, "UPDATE search_areas SET zip_code='75002'")
	if e := s.Ingest(context.Background(), p, 1); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 2 || countTest(t, s, "SELECT count(*) FROM ingestion_runs") != 1 || countTest(t, s, "SELECT observation_count FROM ingestion_runs") != 1 {
		t.Fatal("resume repeated completed work")
	}
	if countTest(t, s, "SELECT observation_count FROM ingestion_searches WHERE status='Active'") != 1 || countTest(t, s, "SELECT observation_count FROM ingestion_searches WHERE status='Inactive'") != 0 {
		t.Fatal("per-search observation counts")
	}
	c, e := s.Coverage(context.Background(), time.Now(), 72)
	if e != nil {
		t.Fatal(e)
	}
	if c["incomplete"] != true || c["last_completed_collection_at"] != nil {
		t.Fatal("changed config treated as covered", c)
	}
}

func TestPaginationCheckpoint(t *testing.T) {
	s := testStore(t, true)
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == "Active" && r.URL.Query().Get("offset") == "0" {
			fmt.Fprint(w, "["+strings.TrimSuffix(strings.Repeat(goodRecord+",", 500), ",")+"]")
		} else {
			fmt.Fprint(w, "[]")
		}
	})
	if e := s.Ingest(context.Background(), p, 1); !errors.Is(e, ErrRequestCap) {
		t.Fatal(e)
	}
	if countTest(t, s, "SELECT next_offset FROM ingestion_searches WHERE status='Active'") != 500 {
		t.Fatal("offset not persisted")
	}
	if e := s.Ingest(context.Background(), p, 2); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 3 || countTest(t, s, "SELECT count(*) FROM listing_observations") != 1 {
		t.Fatal("pagination replay duplicated work")
	}
}

func TestMonthlyBudgetResume(t *testing.T) {
	s := testStore(t, true)
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	p, calls := localProvider(t, s, activeOnly)
	p.Budget = 1
	if e := s.Ingest(context.Background(), p, 4); !errors.Is(e, ErrMonthlyBudget) {
		t.Fatalf("budget: %v", e)
	}
	if calls.Load() != 1 || countTest(t, s, "SELECT request_count FROM ingestion_runs") != 1 {
		t.Fatal("exhausted budget dispatched or miscounted")
	}
	p.Budget = 2
	if e := s.Ingest(context.Background(), p, 1); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 2 || countTest(t, s, "SELECT count(*) FROM ingestion_runs WHERE state='completed'") != 1 {
		t.Fatal("budget resume failed")
	}
}

func TestCrashReplay(t *testing.T) {
	for _, stage := range []string{"page_stored", "page_processed", "candidate_applied"} {
		t.Run(stage, func(t *testing.T) {
			s := testStore(t, true)
			execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
			p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("status") == "Active" {
					second := strings.ReplaceAll(strings.ReplaceAll(goodRecord, `"one"`, `"two"`), "1 Test St", "2 Test St")
					fmt.Fprint(w, "["+goodRecord+","+second+"]")
				} else {
					fmt.Fprint(w, "[]")
				}
			})
			crash := errors.New("simulated process interruption")
			if e := s.ingest(context.Background(), p, 5, func(at string) error {
				if at == stage {
					return crash
				}
				return nil
			}); !errors.Is(e, crash) {
				t.Fatal(e)
			}
			if stage == "page_stored" && countTest(t, s, "SELECT next_offset FROM ingestion_searches WHERE status='Active'") != 0 {
				t.Fatal("checkpoint advanced before processing")
			}
			if e := s.Ingest(context.Background(), p, 1); e != nil {
				t.Fatal(e)
			}
			if calls.Load() != 2 || countTest(t, s, "SELECT count(*) FROM listing_observations") != 2 || countTest(t, s, "SELECT count(*) FROM listing_events") != 2 {
				t.Fatal("replay duplicated requests/history")
			}
			var pageID int64
			s.DB.QueryRow(context.Background(), "SELECT min(id) FROM ingestion_pages").Scan(&pageID)
			if e := s.processPage(context.Background(), p, pageID); e != nil {
				t.Fatal(e)
			}
			if countTest(t, s, "SELECT observation_count FROM ingestion_runs") != 2 || countTest(t, s, "SELECT count(*) FROM ingestion_records") != 2 {
				t.Fatal("idempotent checkpoint failed")
			}
		})
	}
}

func TestMalformedRecordsAndConflicts(t *testing.T) {
	t.Run("quarantine", func(t *testing.T) {
		s := testStore(t, true)
		execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
		p, _ := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("status") == "Active" {
				fmt.Fprint(w, "["+goodRecord+`,{"id":"bad","status":"Sold"},{"id":"bad-number","price":"oops"},null,`+strings.Replace(goodRecord, "400000", "-1", 1)+"]")
			} else {
				fmt.Fprint(w, "[]")
			}
		})
		if e := s.Ingest(context.Background(), p, 2); e != nil {
			t.Fatal(e)
		}
		if countTest(t, s, "SELECT rejected_count FROM ingestion_runs") != 4 || countTest(t, s, "SELECT count(*) FROM listing_observations") != 1 || countTest(t, s, "SELECT count(*) FROM ingestion_runs WHERE state='degraded'") != 1 {
			t.Fatal("quarantine counts")
		}
		c, e := s.Coverage(context.Background(), time.Now(), 72)
		if e != nil {
			t.Fatal(e)
		}
		if c["degraded"] != true || c["incomplete"] != false {
			t.Fatalf("coverage %v", c)
		}
		if e = s.Ingest(context.Background(), p, 1); !errors.Is(e, ErrRequestCap) {
			t.Fatal(e)
		}
		c, e = s.Coverage(context.Background(), time.Now(), 72)
		if e != nil {
			t.Fatal(e)
		}
		if c["degraded"] != true || c["incomplete"] != true || c["last_completed_collection_at"] == nil {
			t.Fatal("unfinished run hid prior coverage", c)
		}
	})
	t.Run("overlap", func(t *testing.T) {
		s := testStore(t, true)
		ctx := context.Background()
		execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001'),('75002')")
		prior, e := (&RentCast{}).NormalizeListing(json.RawMessage(goodRecord))
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Save(ctx, prior, time.Now().Add(-time.Hour)); e != nil {
			t.Fatal(e)
		}
		p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("status") == "Active" {
				raw := goodRecord
				if r.URL.Query().Get("zipCode") == "75002" {
					raw = strings.Replace(raw, "Active", "Inactive", 1)
				}
				fmt.Fprint(w, "["+raw+"]")
			} else {
				fmt.Fprint(w, "[]")
			}
		})
		if e = s.Ingest(ctx, p, 4); e != nil {
			t.Fatal(e)
		}
		if calls.Load() != 4 || countTest(t, s, "SELECT conflict_count FROM ingestion_runs") != 1 || countTest(t, s, "SELECT count(*) FROM listing_observations") != 1 || countTest(t, s, "SELECT count(*) FROM listing_events") != 1 {
			t.Fatal("conflict invented history")
		}
		var status string
		s.DB.QueryRow(ctx, "SELECT current_status FROM listings").Scan(&status)
		if status != "ACTIVE" {
			t.Fatal("conflict overwrote current state")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/runs/1/issues", nil))
		var issues []map[string]any
		if e = json.Unmarshal(rec.Body.Bytes(), &issues); e != nil || len(issues) != 2 {
			t.Fatalf("conflicting evidence: %s %v", rec.Body.String(), e)
		}
		c, e := s.Coverage(ctx, time.Now(), 72)
		if e != nil {
			t.Fatal(e)
		}
		if c["degraded"] != true {
			t.Fatal(c)
		}
	})
}

func TestConcurrentIngestion(t *testing.T) {
	s := testStore(t, true)
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	entered := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		fmt.Fprint(w, "[]")
	})
	done := make(chan error, 1)
	go func() { done <- s.Ingest(context.Background(), p, 2) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("first ingestion never fetched")
	}
	e := s.Ingest(context.Background(), p, 2)
	close(release)
	if !errors.Is(e, ErrIngestionBusy) {
		t.Errorf("concurrent run allowed: %v", e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 2 || countTest(t, s, "SELECT count(*) FROM ingestion_runs") != 1 {
		t.Fatal("concurrent work duplicated")
	}
}

func TestMigrationUpgradePreservesHistory(t *testing.T) {
	s := testStore(t, false)
	ctx := context.Background()
	body, e := os.ReadFile(filepath.Join("..", "..", "migrations", "001_initial.sql"))
	if e != nil {
		t.Fatal(e)
	}
	execTest(t, s, string(body))
	execTest(t, s, `INSERT INTO properties(normalized_address) VALUES('legacy'); INSERT INTO listings(property_id,provider,provider_listing_id,first_seen_at,last_seen_at,current_status,current_price) VALUES(1,'rentcast','old',now(),now(),'INACTIVE',123); INSERT INTO listing_observations(listing_id,observed_at,status,price,property_snapshot,raw_response) VALUES(1,now(),'INACTIVE',123,'{}','{"legacy":true}'); INSERT INTO listing_events(listing_id,observation_id,occurred_at,event_type,price) VALUES(1,1,now(),'LISTED',123); INSERT INTO search_areas DEFAULT VALUES;`)
	var before string
	if e = s.DB.QueryRow(ctx, "SELECT row_to_json(o)::text FROM listing_observations o").Scan(&before); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- s.Migrate(ctx, filepath.Join("..", "..", "migrations")) }()
	}
	for i := 0; i < 2; i++ {
		if e = <-done; e != nil {
			t.Fatal(e)
		}
	}
	var after string
	if e = s.DB.QueryRow(ctx, "SELECT (to_jsonb(o)-'ingestion_run_id')::text FROM listing_observations o").Scan(&after); e != nil {
		t.Fatal(e)
	}
	var a, b any
	json.Unmarshal([]byte(before), &a)
	json.Unmarshal([]byte(after), &b)
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(aa) != string(bb) {
		t.Fatal("legacy observation changed")
	}
	if countTest(t, s, "SELECT count(*) FROM listing_events") != 1 || countTest(t, s, "SELECT count(*) FROM schema_migrations") != 2 || countTest(t, s, "SELECT count(*) FROM search_areas") != 1 {
		t.Fatal("upgrade reset data")
	}
	if _, e = s.startOrResume(ctx); e == nil {
		t.Fatal("legacy invalid config accepted")
	}
	if _, e = s.DB.Exec(ctx, "INSERT INTO search_areas DEFAULT VALUES"); e == nil {
		t.Fatal("new invalid area accepted")
	}
	execTest(t, s, "UPDATE search_areas SET zip_code='75001'")
	execTest(t, s, "ALTER TABLE search_areas VALIDATE CONSTRAINT search_area_valid")
	if _, e = s.DB.Exec(ctx, "DELETE FROM listing_observations"); e == nil {
		t.Fatal("history protection lost")
	}
}

func TestCoverageFreshness(t *testing.T) {
	s := testStore(t, true)
	ctx := context.Background()
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	now := time.Now().UTC()
	c, e := s.Coverage(ctx, now, 72)
	if e != nil {
		t.Fatal(e)
	}
	if c["incomplete"] != true || c["stale"] != true {
		t.Fatal("empty coverage", c)
	}
	p, _ := localProvider(t, s, activeOnly)
	if e = s.Ingest(ctx, p, 2); e != nil {
		t.Fatal(e)
	}
	c, e = s.Coverage(ctx, time.Now(), 72)
	if e != nil {
		t.Fatal(e)
	}
	if c["incomplete"] != false || c["degraded"] != false || c["stale"] != false || c["last_completed_collection_at"] == nil {
		t.Fatal("fresh coverage", c)
	}
	c, e = s.Coverage(ctx, time.Now().Add(73*time.Hour), 72)
	if e != nil {
		t.Fatal(e)
	}
	if c["stale"] != true || c["stale_listing_count"] != float64(1) {
		t.Fatal("stale coverage", c)
	}
	if countTest(t, s, "SELECT count(*) FROM listings WHERE current_status='ACTIVE'") != 1 {
		t.Fatal("stale listing made inactive")
	}
	for _, path := range []string{"/api/coverage", "/api/runs", "/api/runs/1", "/api/runs/1/issues"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	t.Setenv("STALE_AFTER_HOURS", "12")
	if StaleHours() != 12 {
		t.Fatal("threshold ignored")
	}
}

func TestProcessingTransactionsRollback(t *testing.T) {
	for _, stage := range []string{"page", "history"} {
		t.Run(stage, func(t *testing.T) {
			s := testStore(t, true)
			ctx := context.Background()
			execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
			p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("status") == "Active" {
					other := strings.ReplaceAll(strings.ReplaceAll(goodRecord, `"one"`, `"two"`), "1 Test St", "2 Test St")
					fmt.Fprint(w, "["+goodRecord+","+other+"]")
				} else {
					fmt.Fprint(w, "[]")
				}
			})
			table := "ingestion_records"
			condition := "NEW.position=1"
			if stage == "history" {
				table = "listing_events"
				condition = "true"
			}
			execTest(t, s, fmt.Sprintf(`CREATE FUNCTION simulate_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'simulated mid-transaction failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER test_failure BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION simulate_failure();`, condition, table))
			if e := s.Ingest(ctx, p, 2); e == nil {
				t.Fatal("expected injected failure")
			}
			if countTest(t, s, "SELECT count(*) FROM listing_observations") != 0 || countTest(t, s, "SELECT count(*) FROM listing_events") != 0 || countTest(t, s, "SELECT count(*) FROM properties") != 0 {
				t.Fatal("failed transaction leaked history/current state")
			}
			if stage == "page" && (countTest(t, s, "SELECT count(*) FROM ingestion_candidates") != 0 || countTest(t, s, "SELECT count(*) FROM ingestion_records") != 0 || countTest(t, s, "SELECT next_offset FROM ingestion_searches WHERE status='Active'") != 0) {
				t.Fatal("page checkpoint/candidates committed early")
			}
			execTest(t, s, "DROP TRIGGER test_failure ON "+table)
			if e := s.Resume(ctx, p, 1); e != nil {
				t.Fatal(e)
			}
			if calls.Load() != 2 || countTest(t, s, "SELECT count(*) FROM listing_observations") != 2 || countTest(t, s, "SELECT count(*) FROM listing_events") != 2 {
				t.Fatal("rollback replay duplicated work")
			}
			if e := s.Resume(ctx, p, 1); !errors.Is(e, ErrNoUnfinishedRun) {
				t.Fatal("resume started an unwanted new collection", e)
			}
			if calls.Load() != 2 {
				t.Fatal("completed resume spent a request")
			}
		})
	}
}

func TestReplayRetainsFetchFreshness(t *testing.T) {
	s := testStore(t, true)
	ctx := context.Background()
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	p, _ := localProvider(t, s, activeOnly)
	if e := s.ingest(ctx, p, 2, func(stage string) error {
		if stage == "page_stored" {
			return errors.New("crash")
		}
		return nil
	}); e == nil {
		t.Fatal("expected crash")
	}
	old := time.Now().Add(-96 * time.Hour)
	execTest(t, s, "UPDATE ingestion_pages SET fetched_at=$1", old)
	if e := s.Resume(ctx, p, 1); e != nil {
		t.Fatal(e)
	}
	c, e := s.Coverage(ctx, time.Now(), 72)
	if e != nil {
		t.Fatal(e)
	}
	if c["stale"] != true || c["stale_listing_count"] != float64(1) || c["incomplete"] != false {
		t.Fatal("replay made old data fresh", c)
	}
	var observed time.Time
	if e = s.DB.QueryRow(ctx, "SELECT last_seen_at FROM listings").Scan(&observed); e != nil {
		t.Fatal(e)
	}
	if observed.Sub(old).Abs() > time.Millisecond {
		t.Fatal("replay reset observation time")
	}
}

func TestHTTPFailureCountedAndResumable(t *testing.T) {
	s := testStore(t, true)
	ctx := context.Background()
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	var fail atomic.Bool
	fail.Store(true)
	p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) {
		if fail.Swap(false) {
			http.Error(w, "rate limited", 429)
			return
		}
		activeOnly(w, r)
	})
	if e := s.Ingest(ctx, p, 2); e == nil {
		t.Fatal("HTTP failure swallowed")
	}
	if countTest(t, s, "SELECT request_count FROM ingestion_runs") != 1 || countTest(t, s, "SELECT count(*) FROM ingestion_pages") != 0 {
		t.Fatal("HTTP failure usage/page counts")
	}
	if e := s.Resume(ctx, p, 2); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 3 || countTest(t, s, "SELECT request_count FROM ingestion_runs") != 3 || countTest(t, s, "SELECT count(*) FROM provider_api_usage WHERE NOT successful") != 1 {
		t.Fatal("failed requests uncounted")
	}
}

func TestMalformedPageRetainedWithoutRefetch(t *testing.T) {
	s := testStore(t, true)
	ctx := context.Background()
	execTest(t, s, "INSERT INTO search_areas(zip_code) VALUES('75001')")
	p, calls := localProvider(t, s, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"unexpected":"not an array"}`) })
	for i := 0; i < 2; i++ {
		if e := s.Ingest(ctx, p, 1); e == nil {
			t.Fatal("invalid page accepted")
		}
	}
	if calls.Load() != 1 || countTest(t, s, "SELECT count(*) FROM ingestion_pages WHERE failure_reason IS NOT NULL") != 1 || countTest(t, s, "SELECT next_offset FROM ingestion_searches WHERE status='Active'") != 0 {
		t.Fatal("bad page lost/refetched or advanced")
	}
}
