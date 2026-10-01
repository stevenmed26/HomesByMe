package market

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }
func TestNormalizationAndIdentity(t *testing.T) {
	a := Property{Address: " 123 Main St. Apt 2 ", City: "Dallas", State: "tx", ZIP: "75001"}
	b := a
	b.Address = "123 MAIN ST APT 2"
	b.State = "TX"
	if AddressKey(a) != AddressKey(b) {
		t.Fatal("equivalent addresses not matched")
	}
	b.Address = "123 Main St Apt 3"
	if AddressKey(a) == AddressKey(b) {
		t.Fatal("units collapsed")
	}
	r := Record{ID: "stable", MLS: "123", Property: a}
	if ListingKey(r) != "stable" {
		t.Fatal("stable ID precedence")
	}
	r.ID = ""
	if ListingKey(r) != "mls:123" {
		t.Fatal("MLS fallback")
	}
	r.MLS = ""
	if ListingKey(r) != "address:"+AddressKey(a) {
		t.Fatal("address fallback")
	}
}
func TestNormalizeRentCast(t *testing.T) {
	p := &RentCast{}
	r, e := p.NormalizeListing(json.RawMessage(`{"id":"abc","addressLine1":"123 Main St","addressLine2":"Unit 2","city":"Dallas","state":"TX","zipCode":"75001","status":"Inactive","price":500000,"lastSeenDate":"2026-01-01T00:00:00Z"}`))
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "INACTIVE" || r.Property.Address != "123 Main St Unit 2" || r.DOM != nil {
		t.Fatalf("bad normalization: %+v", r)
	}
	_, e = p.NormalizeListing(json.RawMessage(`{"addressLine1":"123 Main","zipCode":"75001","status":"Sold"}`))
	if e == nil {
		t.Fatal("invented status accepted")
	}
}
func TestEvents(t *testing.T) {
	oldDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newDate := oldDate.AddDate(0, 1, 0)
	for _, tc := range []struct {
		name string
		prev *State
		r    Record
		want []string
	}{
		{"first", nil, Record{Status: "ACTIVE"}, []string{"LISTED"}},
		{"inactive", &State{Status: "ACTIVE"}, Record{Status: "INACTIVE"}, []string{"BECAME_INACTIVE"}},
		{"reactivate", &State{Status: "INACTIVE"}, Record{Status: "ACTIVE"}, []string{"BECAME_ACTIVE"}},
		{"relist", &State{Status: "INACTIVE", Listed: &oldDate}, Record{Status: "ACTIVE", Listed: &newDate}, []string{"BECAME_ACTIVE", "RELISTED"}},
		{"price", &State{Status: "ACTIVE", Price: ptr(500000.0)}, Record{Status: "ACTIVE", Price: ptr(490000.0)}, []string{"PRICE_CHANGE"}},
		{"unchanged", &State{Status: "ACTIVE"}, Record{Status: "ACTIVE"}, []string{}},
		{"details", &State{Status: "ACTIVE"}, Record{Status: "ACTIVE", Property: Property{Beds: ptr(3.0)}}, []string{"PROPERTY_DETAILS_CHANGED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Events(tc.prev, tc.r); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
func TestFilters(t *testing.T) {
	w, a, e := Filter(url.Values{"city": {"Dallas' OR true--"}, "min_price": {"100"}})
	if e != nil || len(a) != 2 || w != "TRUE AND l.current_price >= $1 AND lower(p.city) = $2" {
		t.Fatalf("%s %v %v", w, a, e)
	}
	if _, _, e = Filter(url.Values{"beds": {"oops"}}); e == nil {
		t.Fatal("bad numeric input accepted")
	}
}
