package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Area struct {
	City  string `json:"city"`
	State string `json:"state"`
	ZIP   string `json:"zip"`
}
type ListingProvider interface {
	SearchPage(context.Context, Area, string, int) ([]byte, error)
	GetListing(context.Context, string) (Record, error)
	NormalizeListing(json.RawMessage) (Record, error)
}
type RentCast struct {
	Key     string
	BaseURL string
	Client  *http.Client
	Store   *Store
	Budget  int
}

func (p *RentCast) request(ctx context.Context, path string) ([]byte, error) {
	if p.Key == "" {
		return nil, fmt.Errorf("RENTCAST_API_KEY is required")
	}
	id, err := p.Store.ReserveRequest(ctx, path, p.Budget)
	if err != nil {
		return nil, err
	}
	base := p.BaseURL
	if base == "" {
		base = "https://api.rentcast.io/v1"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", p.Key)
	req.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RentCast transport failure: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("RentCast HTTP %d (request %d)", resp.StatusCode, id)
	}
	_, err = p.Store.DB.Exec(ctx, "UPDATE provider_api_usage SET successful=true WHERE id=$1", id)
	return b, err
}
func (p *RentCast) SearchPage(ctx context.Context, a Area, status string, offset int) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{"status": {status}, "limit": {"500"}, "offset": {fmt.Sprint(offset)}}
	if a.ZIP != "" {
		q.Set("zipCode", a.ZIP)
	} else {
		q.Set("city", a.City)
		q.Set("state", a.State)
	}
	return p.request(ctx, "/listings/sale?"+q.Encode())
}

// Convenience method for callers that do not need quarantine. Durable ingestion
// always uses SearchPage and stores the bytes before decoding any record.
func (p *RentCast) SearchListings(ctx context.Context, a Area, status string, offset int) ([]Record, error) {
	b, err := p.SearchPage(ctx, a, status, offset)
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err = json.Unmarshal(b, &raws); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(raws))
	for _, raw := range raws {
		r, e := p.NormalizeListing(raw)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, nil
}
func (p *RentCast) GetListing(ctx context.Context, id string) (Record, error) {
	b, e := p.request(ctx, "/listings/sale/"+url.PathEscape(id))
	if e != nil {
		return Record{}, e
	}
	return p.NormalizeListing(b)
}
func (p *RentCast) NormalizeListing(b json.RawMessage) (Record, error) {
	if strings.Contains(string(b), `\u0000`) {
		return Record{}, fmt.Errorf("record contains unsupported NUL escape")
	}
	var x struct {
		ID           string     `json:"id"`
		Address      string     `json:"addressLine1"`
		Address2     string     `json:"addressLine2"`
		City         string     `json:"city"`
		State        string     `json:"state"`
		ZIP          string     `json:"zipCode"`
		Latitude     *float64   `json:"latitude"`
		Longitude    *float64   `json:"longitude"`
		Beds         *float64   `json:"bedrooms"`
		Baths        *float64   `json:"bathrooms"`
		Sqft         *float64   `json:"squareFootage"`
		Lot          *float64   `json:"lotSize"`
		Year         *int       `json:"yearBuilt"`
		Type         string     `json:"propertyType"`
		Neighborhood string     `json:"subdivision"`
		Status       string     `json:"status"`
		Price        *float64   `json:"price"`
		DOM          *int       `json:"daysOnMarket"`
		Listed       *time.Time `json:"listedDate"`
		Modified     *time.Time `json:"lastSeenDate"`
		MLS          string     `json:"mlsNumber"`
	}
	if e := json.Unmarshal(b, &x); e != nil {
		return Record{}, e
	}
	r := Record{Provider: "rentcast", ID: x.ID, MLS: x.MLS, Property: Property{strings.TrimSpace(x.Address + " " + x.Address2), x.City, x.State, x.ZIP, x.Latitude, x.Longitude, x.Beds, x.Baths, x.Sqft, x.Lot, x.Year, x.Type, x.Neighborhood}, Status: strings.ToUpper(x.Status), Price: x.Price, DOM: x.DOM, Listed: x.Listed, Modified: x.Modified, Raw: b}
	return r, Validate(r)
}
