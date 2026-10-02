package market

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TEST_DATABASE_URL must point at a disposable PostGIS database.
func TestDatabasePipeline(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostGIS integration test")
	}
	ctx := context.Background()
	db, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	schema := "test_" + time.Now().Format("20060102150405")
	if _, e = db.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	defer db.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	testDB, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer testDB.Close()
	s := &Store{DB: testDB}
	if e = s.Migrate(ctx, filepath.Join("..", "..", "migrations")); e != nil {
		t.Fatal(e)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Record{Provider: "fixture", ID: "one", Property: Property{Address: "1 Main St", City: "Dallas", State: "TX", ZIP: "75001", Sqft: ptr(2000.0)}, Status: "ACTIVE", Price: ptr(400000.0), DOM: ptr(10), Listed: &at, Raw: json.RawMessage(`{}`)}
	for i := 0; i < 2; i++ {
		if e = s.Save(ctx, r, at.Add(time.Duration(i)*time.Hour)); e != nil {
			t.Fatal(e)
		}
	}
	r.Price = ptr(380000.0)
	if e = s.Save(ctx, r, at.Add(24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	r.Status = "INACTIVE"
	if e = s.Save(ctx, r, at.Add(48*time.Hour)); e != nil {
		t.Fatal(e)
	}
	r.Status = "ACTIVE"
	r.Listed = ptr(at.Add(72 * time.Hour))
	if e = s.Save(ctx, r, at.Add(72*time.Hour)); e != nil {
		t.Fatal(e)
	}
	var properties, listings, observations, events int
	e = testDB.QueryRow(ctx, `SELECT (SELECT count(*) FROM properties),(SELECT count(*) FROM listings),(SELECT count(*) FROM listing_observations),(SELECT count(*) FROM listing_events)`).Scan(&properties, &listings, &observations, &events)
	if e != nil {
		t.Fatal(e)
	}
	if properties != 1 || listings != 1 || observations != 5 || events != 5 {
		t.Fatalf("counts %d %d %d %d", properties, listings, observations, events)
	}
	m, e := s.Analytics(ctx, url.Values{})
	if e != nil {
		t.Fatal(e)
	}
	for k, want := range map[string]float64{"active_inventory": 1, "median_price": 380000, "median_ppsf": 190, "median_dom": 10, "average_dom": 10, "price_reduction_pct": 100, "relisting_pct": 100, "became_inactive": 1, "became_active": 1, "median_reduction": 20000, "median_days_to_inactive": 2} {
		if m[k] != want {
			t.Errorf("%s: got %v want %v", k, m[k], want)
		}
	}
	if _, e = testDB.Exec(ctx, "UPDATE listing_observations SET price=0"); e == nil {
		t.Fatal("history mutable")
	}
	// Even-sized cohorts exercise percentile interpolation and null DOM handling.
	r.ID = "two"
	r.Property.Address = "2 Main St"
	r.Price = ptr(600000.0)
	r.DOM = nil
	if e = s.Save(ctx, r, at); e != nil {
		t.Fatal(e)
	}
	m, e = s.Analytics(ctx, url.Values{})
	if e != nil {
		t.Fatal(e)
	}
	for k, want := range map[string]float64{"active_inventory": 2, "median_price": 490000, "median_ppsf": 245, "median_dom": 10, "price_reduction_pct": 50, "relisting_pct": 50} {
		if m[k] != want {
			t.Errorf("cohort %s: %v != %v", k, m[k], want)
		}
	}
	m, e = s.Analytics(ctx, url.Values{"max_price": {"400000"}})
	if e != nil || m["active_inventory"] != float64(1) {
		t.Fatalf("filtered metrics: %v %v", m, e)
	}
	m, e = s.Analytics(ctx, url.Values{"city": {"Missing"}})
	if e != nil || m["median_price"] != nil || m["active_inventory"] != float64(0) {
		t.Fatalf("empty metrics: %v %v", m, e)
	}
	var pid int64
	if e = testDB.QueryRow(ctx, "SELECT min(id) FROM properties").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/analytics", "/api/listings?sort=changed", "/api/listings?sort=ppsf&direction=desc", fmt.Sprintf("/api/properties/%d", pid), "/api/usage", "/api/areas", "/api/health"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if _, e = s.ReserveRequest(ctx, "/test", 1); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ReserveRequest(ctx, "/test", 1); e == nil {
		t.Fatal("budget not enforced")
	}
	// Exercise the actual REST adapter and orchestration without paid provider calls.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Api-Key") != "test-only" || req.URL.Query().Get("limit") != "500" || req.URL.Query().Get("zipCode") != "75001" {
			t.Error("incorrect provider request")
		}
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Query().Get("status") == "Active" {
			fmt.Fprint(w, `[{"id":"provider-test","addressLine1":"3 Test St","zipCode":"75001","status":"Active","price":300000}]`)
		} else {
			fmt.Fprint(w, `[]`)
		}
	}))
	defer server.Close()
	if _, e = testDB.Exec(ctx, "INSERT INTO search_areas(zip_code) VALUES('75001')"); e != nil {
		t.Fatal(e)
	}
	provider := &RentCast{Store: s, Key: "test-only", BaseURL: server.URL, Client: server.Client(), Budget: 10}
	for i := 0; i < 2; i++ {
		if e = s.Ingest(ctx, provider, 2); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e = testDB.QueryRow(ctx, "SELECT count(*) FROM listings WHERE provider='rentcast'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("provider dedup: %d %v", count, e)
	}
	if e = testDB.QueryRow(ctx, "SELECT count(*) FROM provider_api_usage WHERE successful").Scan(&count); e != nil || count != 4 {
		t.Fatalf("usage: %d %v", count, e)
	}
}
