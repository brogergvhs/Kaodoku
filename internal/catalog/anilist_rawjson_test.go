package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The strip-likely heuristic reads countryOfOrigin out of raw_json, which is
// a re-marshal of anilistMedia — the field must survive that round trip.
func TestRawJSONKeepsCountryOfOrigin(t *testing.T) {
	t.Parallel()
	var m anilistMedia
	if err := json.Unmarshal([]byte(`{"id":1,"countryOfOrigin":"KR"}`), &m); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"countryOfOrigin":"KR"`) {
		t.Fatalf("countryOfOrigin lost in re-marshal: %s", raw)
	}
}
