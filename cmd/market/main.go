package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"homesbyme/internal/market"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func number(k string, d int) int {
	n, e := strconv.Atoi(env(k, fmt.Sprint(d)))
	if e != nil || n < 1 {
		panic("invalid " + k)
	}
	return n
}
func run() error {
	ctx := context.Background()
	db, e := pgxpool.New(ctx, env("DATABASE_URL", "postgres://market:market@localhost:55432/market?sslmode=disable"))
	if e != nil {
		return e
	}
	defer db.Close()
	s := &market.Store{DB: db}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "migrate":
		return s.Migrate(ctx, "migrations")
	case "ingest", "resume":
		provider := &market.RentCast{Store: s, Key: os.Getenv("RENTCAST_API_KEY"), Budget: number("RENTCAST_MONTHLY_BUDGET", 50)}
		if command == "resume" {
			return s.Resume(ctx, provider, number("MAX_REQUESTS_PER_RUN", 4))
		}
		return s.Ingest(ctx, provider, number("MAX_REQUESTS_PER_RUN", 4))
	case "fixture":
		if os.Getenv("ALLOW_FIXTURE_IMPORT") != "true" {
			return fmt.Errorf("fixture import requires ALLOW_FIXTURE_IMPORT=true; use a separate test database")
		}
		if len(os.Args) != 3 {
			return fmt.Errorf("usage: market fixture path.json")
		}
		b, e := os.ReadFile(os.Args[2])
		if e != nil {
			return e
		}
		var raws []json.RawMessage
		if e = json.Unmarshal(b, &raws); e != nil {
			return e
		}
		p := &market.RentCast{}
		for _, raw := range raws {
			r, e := p.NormalizeListing(raw)
			if e != nil {
				return e
			}
			r.Provider = "fixture"
			if e = s.Save(ctx, r, time.Now().UTC()); e != nil {
				return e
			}
		}
		return nil
	case "serve":
		return (&http.Server{Addr: env("LISTEN_ADDR", ":18080"), Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}).ListenAndServe()
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("command_failed", "error", e)
		os.Exit(1)
	}
}
