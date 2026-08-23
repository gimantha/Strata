package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestObjectKeyTreatsEqualValuesAsEqual(t *testing.T) {
	// Equality of values is what deduplication and conflict detection rest on, so
	// equivalent values written differently must produce the same key.
	cases := []struct{ a, b AssertionObject }{
		{ObjectOfDecimal("12.50"), ObjectOfDecimal("12.5")},
		{ObjectOfDecimal("12345678.90"), ObjectOfDecimal("12345678.9000")},
		{ObjectOfDecimal("-0.0"), ObjectOfDecimal("0")},
		{ObjectOfSymbol("enterprise"), ObjectOfSymbol("ENTERPRISE")},
		{ObjectOfJSON(json.RawMessage(`{"a":1,"b":2}`)), ObjectOfJSON(json.RawMessage(`{"b":2,"a":1}`))},
		{ObjectOfJSON(json.RawMessage(`{"a": 1}`)), ObjectOfJSON(json.RawMessage(`{"a":1}`))},
		{
			ObjectOfTimestamp(time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)),
			ObjectOfTimestamp(time.Date(2026, 3, 3, 14, 0, 0, 0, time.FixedZone("+02:00", 2*3600))),
		},
		{ObjectOfDate(time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)),
			ObjectOfDate(time.Date(2026, 3, 3, 23, 59, 0, 0, time.UTC))},
	}
	for _, tc := range cases {
		if tc.a.Key() != tc.b.Key() {
			t.Fatalf("equal values produced different keys: %q vs %q", tc.a.Key(), tc.b.Key())
		}
		if !tc.a.Equal(tc.b) {
			t.Fatalf("%q and %q should compare equal", tc.a.Display(), tc.b.Display())
		}
	}
}

func TestObjectKeyDistinguishesDifferentValues(t *testing.T) {
	cases := []struct{ a, b AssertionObject }{
		{ObjectOfString("Acme"), ObjectOfString("acme")}, // strings are case sensitive
		{ObjectOfString("42"), ObjectOfInteger(42)},      // a string is not a number
		{ObjectOfInteger(42), ObjectOfDecimal("42")},     // nor is an integer a decimal
		{ObjectOfDecimal("12.5"), ObjectOfDecimal("12.6")},
		{ObjectOfBool(true), ObjectOfBool(false)},
		{ObjectOfURI("https://a.example"), ObjectOfString("https://a.example")},
		{ObjectOfGeo(52.520008, 13.404954), ObjectOfGeo(52.520008, 13.404955)},
		{ObjectOfDuration(time.Hour), ObjectOfDuration(2 * time.Hour)},
	}
	for _, tc := range cases {
		if tc.a.Key() == tc.b.Key() {
			t.Fatalf("different values collided on key %q", tc.a.Key())
		}
		if tc.a.Equal(tc.b) {
			t.Fatal("values of different type or magnitude must not compare equal")
		}
	}
}

func TestObjectValidation(t *testing.T) {
	valid := []AssertionObject{
		ObjectOfEntity("e1"),
		ObjectOfString("x"),
		ObjectOfInteger(0),
		ObjectOfDecimal("0.001"),
		ObjectOfBool(false),
		ObjectOfTimestamp(time.Now()),
		ObjectOfDate(time.Now()),
		ObjectOfDuration(0),
		ObjectOfGeo(-90, 180),
		ObjectOfJSON(json.RawMessage(`{"a":1}`)),
		ObjectOfURI("https://example"),
		ObjectOfSymbol("A"),
	}
	for _, o := range valid {
		if err := o.Validate(); err != nil {
			t.Fatalf("%s object rejected: %v", o.Kind, err)
		}
	}

	invalid := map[string]AssertionObject{
		"unknown kind":      {Kind: "quaternion"},
		"entity without id": {Kind: ObjectEntity},
		"empty string":      {Kind: ObjectString},
		"bad decimal":       {Kind: ObjectDecimal, Decimal: "twelve"},
		"latitude too high": {Kind: ObjectGeo, Geo: GeoPoint{Latitude: 91}},
		"longitude too low": {Kind: ObjectGeo, Geo: GeoPoint{Longitude: -181}},
		"invalid json":      {Kind: ObjectJSON, JSON: json.RawMessage(`{`)},
		"missing timestamp": {Kind: ObjectTimestamp},
	}
	for name, o := range invalid {
		if err := o.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestObjectMarshalsWithItsType(t *testing.T) {
	// The wire form must carry the kind, or a consumer cannot tell 42 the integer from
	// "42" the string.
	encoded, err := json.Marshal(ObjectOfInteger(42))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["kind"] != string(ObjectInteger) {
		t.Fatalf("kind missing from wire form: %s", encoded)
	}
	if decoded["value"].(float64) != 42 {
		t.Fatalf("value missing from wire form: %s", encoded)
	}
}
