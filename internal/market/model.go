package market

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type Property struct {
	Address      string   `json:"address_line_1"`
	City         string   `json:"city"`
	State        string   `json:"state"`
	ZIP          string   `json:"zip_code"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
	Beds         *float64 `json:"bedrooms"`
	Baths        *float64 `json:"bathrooms"`
	Sqft         *float64 `json:"square_feet"`
	Lot          *float64 `json:"lot_square_feet"`
	Year         *int     `json:"year_built"`
	Type         string   `json:"property_type"`
	Neighborhood string   `json:"neighborhood"`
}
type Record struct {
	Provider string
	ID       string
	MLS      string
	Property Property
	Status   string
	Price    *float64
	DOM      *int
	Listed   *time.Time
	Modified *time.Time
	Raw      json.RawMessage
}
type State struct {
	Status   string
	Price    *float64
	Listed   *time.Time
	Property Property
}

func AddressKey(p Property) string {
	clean := func(s string) string {
		return strings.Join(strings.Fields(strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", " "))), " ")
	}
	return clean(p.Address) + "|" + clean(p.City) + "|" + clean(p.State) + "|" + clean(p.ZIP)
}
func ListingKey(r Record) string {
	if r.ID != "" {
		return r.ID
	}
	if r.MLS != "" {
		return "mls:" + r.MLS
	}
	return "address:" + AddressKey(r.Property)
}
func Validate(r Record) error {
	if strings.TrimSpace(r.Property.Address) == "" || r.Property.ZIP == "" && (r.Property.City == "" || r.Property.State == "") {
		return fmt.Errorf("listing lacks a usable address")
	}
	if r.Status != "ACTIVE" && r.Status != "INACTIVE" {
		return fmt.Errorf("unsupported RentCast status %q", r.Status)
	}
	return nil
}
func Events(prev *State, r Record) []string {
	out := []string{}
	if prev == nil {
		return []string{"LISTED"}
	}
	if prev.Status != r.Status {
		if r.Status == "INACTIVE" {
			out = append(out, "BECAME_INACTIVE")
		}
		if r.Status == "ACTIVE" {
			out = append(out, "BECAME_ACTIVE")
		}
	}
	if r.Status == "ACTIVE" && prev.Listed != nil && r.Listed != nil && r.Listed.After(*prev.Listed) {
		out = append(out, "RELISTED")
	}
	if prev.Price != nil && r.Price != nil && *prev.Price != *r.Price {
		out = append(out, "PRICE_CHANGE")
	}
	if !reflect.DeepEqual(prev.Property, r.Property) {
		out = append(out, "PROPERTY_DETAILS_CHANGED")
	}
	return out
}
