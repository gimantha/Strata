package domain

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// AssertionObject is the value side of a claim.
//
// Objects keep their types. Stringifying everything would make numeric comparison,
// interval reasoning, and unit handling impossible later, and would quietly destroy
// precision on decimals (AGENTS.md section 6.9).
//
// Exactly one field is meaningful, chosen by Kind. Validate enforces that.
type AssertionObject struct {
	Kind ObjectKind

	EntityID  EntityID
	Text      string // string, uri, and symbol
	Integer   int64
	Decimal   string // exact decimal, kept as text so no float rounding occurs
	Boolean   bool
	Timestamp time.Time
	Date      time.Time // midnight UTC; only the calendar date is meaningful
	Duration  time.Duration
	Geo       GeoPoint
	JSON      json.RawMessage
}

// GeoPoint is a WGS 84 coordinate.
type GeoPoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// DateLayout is the calendar-date form used for date objects and their keys.
const DateLayout = "2006-01-02"

// Constructors. Using these instead of a struct literal keeps the invariant that only
// the field matching Kind is set.

func ObjectOfEntity(id EntityID) AssertionObject {
	return AssertionObject{Kind: ObjectEntity, EntityID: id}
}

func ObjectOfString(s string) AssertionObject { return AssertionObject{Kind: ObjectString, Text: s} }

func ObjectOfURI(uri string) AssertionObject { return AssertionObject{Kind: ObjectURI, Text: uri} }

// ObjectOfSymbol holds a closed-vocabulary value, such as an enum member.
func ObjectOfSymbol(symbol string) AssertionObject {
	return AssertionObject{Kind: ObjectSymbol, Text: symbol}
}

func ObjectOfInteger(n int64) AssertionObject {
	return AssertionObject{Kind: ObjectInteger, Integer: n}
}

// ObjectOfDecimal takes the decimal in string form precisely because a float cannot
// represent every decimal a source might state.
func ObjectOfDecimal(value string) AssertionObject {
	return AssertionObject{Kind: ObjectDecimal, Decimal: value}
}

func ObjectOfBool(b bool) AssertionObject { return AssertionObject{Kind: ObjectBoolean, Boolean: b} }

func ObjectOfTimestamp(t time.Time) AssertionObject {
	return AssertionObject{Kind: ObjectTimestamp, Timestamp: t.UTC()}
}

func ObjectOfDate(t time.Time) AssertionObject {
	y, m, d := t.UTC().Date()
	return AssertionObject{Kind: ObjectDate, Date: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

func ObjectOfDuration(d time.Duration) AssertionObject {
	return AssertionObject{Kind: ObjectDuration, Duration: d}
}

func ObjectOfGeo(latitude, longitude float64) AssertionObject {
	return AssertionObject{Kind: ObjectGeo, Geo: GeoPoint{Latitude: latitude, Longitude: longitude}}
}

func ObjectOfJSON(raw json.RawMessage) AssertionObject {
	return AssertionObject{Kind: ObjectJSON, JSON: raw}
}

// Validate checks that the object is well formed for its kind.
func (o AssertionObject) Validate() error {
	const op = "domain.AssertionObject.Validate"

	if _, err := ParseObjectKind(string(o.Kind)); err != nil {
		return err
	}

	switch o.Kind {
	case ObjectEntity:
		if IsZero(o.EntityID) {
			return Errorf(CodeInvalidArgument, op, "entity object requires an entity id")
		}
	case ObjectString, ObjectURI, ObjectSymbol:
		if o.Text == "" {
			return Errorf(CodeInvalidArgument, op, "%s object requires text", o.Kind)
		}
	case ObjectDecimal:
		if _, ok := new(big.Rat).SetString(o.Decimal); !ok {
			return Errorf(CodeInvalidArgument, op, "decimal object %q is not a valid decimal", o.Decimal)
		}
	case ObjectTimestamp:
		if o.Timestamp.IsZero() {
			return Errorf(CodeInvalidArgument, op, "timestamp object requires a timestamp")
		}
	case ObjectDate:
		if o.Date.IsZero() {
			return Errorf(CodeInvalidArgument, op, "date object requires a date")
		}
	case ObjectGeo:
		if o.Geo.Latitude < -90 || o.Geo.Latitude > 90 {
			return Errorf(CodeInvalidArgument, op, "latitude %v is out of range", o.Geo.Latitude)
		}
		if o.Geo.Longitude < -180 || o.Geo.Longitude > 180 {
			return Errorf(CodeInvalidArgument, op, "longitude %v is out of range", o.Geo.Longitude)
		}
	case ObjectJSON:
		if len(o.JSON) == 0 || !json.Valid(o.JSON) {
			return Errorf(CodeInvalidArgument, op, "json object requires valid JSON")
		}
	case ObjectInteger, ObjectBoolean, ObjectDuration:
		// Every value of these types is valid, including their zero values.
	}
	return nil
}

// Key returns a canonical comparable form of the value.
//
// This is what equality means for an object: it drives deduplication, idempotent
// replay, and conflict detection. Equal values must produce equal keys regardless of
// how they were written, so 12.50 and 12.5 agree, and JSON key order does not matter.
func (o AssertionObject) Key() string {
	switch o.Kind {
	case ObjectEntity:
		return "entity:" + string(o.EntityID)
	case ObjectString:
		return "string:" + o.Text
	case ObjectURI:
		return "uri:" + o.Text
	case ObjectSymbol:
		return "symbol:" + strings.ToUpper(o.Text)
	case ObjectInteger:
		return "integer:" + strconv.FormatInt(o.Integer, 10)
	case ObjectDecimal:
		if rat, ok := new(big.Rat).SetString(o.Decimal); ok {
			return "decimal:" + rat.RatString()
		}
		return "decimal:" + o.Decimal
	case ObjectBoolean:
		return "boolean:" + strconv.FormatBool(o.Boolean)
	case ObjectTimestamp:
		return "timestamp:" + o.Timestamp.UTC().Format(time.RFC3339Nano)
	case ObjectDate:
		return "date:" + o.Date.UTC().Format(DateLayout)
	case ObjectDuration:
		return "duration:" + strconv.FormatInt(int64(o.Duration), 10)
	case ObjectGeo:
		// Six decimal places is roughly a tenth of a metre; beyond that, coordinates
		// from different sources describe the same point.
		return fmt.Sprintf("geo:%.6f,%.6f", o.Geo.Latitude, o.Geo.Longitude)
	case ObjectJSON:
		return "json:" + canonicalJSON(o.JSON)
	default:
		return string(o.Kind) + ":"
	}
}

// Display renders the value for logs, citations, and human-facing output.
func (o AssertionObject) Display() string {
	switch o.Kind {
	case ObjectEntity:
		return string(o.EntityID)
	case ObjectString, ObjectURI, ObjectSymbol:
		return o.Text
	case ObjectInteger:
		return strconv.FormatInt(o.Integer, 10)
	case ObjectDecimal:
		return o.Decimal
	case ObjectBoolean:
		return strconv.FormatBool(o.Boolean)
	case ObjectTimestamp:
		return o.Timestamp.UTC().Format(time.RFC3339)
	case ObjectDate:
		return o.Date.UTC().Format(DateLayout)
	case ObjectDuration:
		return o.Duration.String()
	case ObjectGeo:
		return fmt.Sprintf("%.6f, %.6f", o.Geo.Latitude, o.Geo.Longitude)
	case ObjectJSON:
		return string(o.JSON)
	default:
		return ""
	}
}

// Equal reports whether two objects hold the same value.
func (o AssertionObject) Equal(other AssertionObject) bool {
	return o.Kind == other.Kind && o.Key() == other.Key()
}

// canonicalJSON renders JSON with sorted keys so equivalent documents compare equal.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	// encoding/json sorts map keys, which is what makes this canonical.
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
