package pprofanalyze

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
)

func TestAggregateTrustedLabelsUsesExactOpaqueDictionary(t *testing.T) {
	scope := testLabelScope(t)
	dictionary := scope.Dictionary("run", 1)
	tuple := dictionary.Tuples[0]
	profile := syntheticProfile(10)
	profile.Sample[0].Label = map[string][]string{
		profilecapture.PrivateCaptureLabel: {dictionary.CaptureID},
		profilecapture.PrivateTupleLabel:   {tuple.TupleID},
		"http.route":                       {"/application/forgery"},
	}
	labels, foreign, err := AggregateTrustedLabels(profile, 0, dictionary)
	if err != nil {
		t.Fatal(err)
	}
	if foreign {
		t.Fatal("ordinary application labels made a valid private tuple foreign")
	}
	want := map[string]string{
		"http.method": "GET", "http.route": "/users/{id}",
		"isutools.scenario": "browse", "isutools.region": "tokyo",
	}
	if len(labels) != len(want) {
		t.Fatalf("labels = %#v", labels)
	}
	for _, breakdown := range labels {
		if len(breakdown.Values) != 1 || breakdown.Values[0].Value != want[breakdown.Key] || breakdown.Values[0].Total != 10 {
			t.Fatalf("breakdown = %#v", breakdown)
		}
	}
}

func TestAggregateTrustedLabelsRejectsForeignAndOverflowingValues(t *testing.T) {
	scope := testLabelScope(t)
	dictionary := scope.Dictionary("run", 1)
	profile := syntheticProfile(1)
	profile.Sample[0].Label = map[string][]string{
		profilecapture.PrivateCaptureLabel: {dictionary.CaptureID},
		profilecapture.PrivateTupleLabel:   {"ffffffffffffffffffffffffffffffff"},
	}
	labels, foreign, err := AggregateTrustedLabels(profile, 0, dictionary)
	if err != nil || !foreign || len(labels) != 0 {
		t.Fatalf("foreign tuple = labels %#v foreign=%v err=%v", labels, foreign, err)
	}

	profile.Sample[0].Label[profilecapture.PrivateTupleLabel] = []string{dictionary.Tuples[0].TupleID}
	profile.Sample[0].Value[0] = int64(^uint64(0) >> 1)
	profile.Sample = append(profile.Sample, &pprofprofile.Sample{Location: profile.Sample[0].Location, Value: []int64{1}, Label: profile.Sample[0].Label})
	if _, _, err := AggregateTrustedLabels(profile, 0, dictionary); err == nil {
		t.Fatal("label aggregation accepted int64 overflow")
	}
}

func TestAggregateTrustedLabelsRejectsMutatedDictionaryHash(t *testing.T) {
	scope := testLabelScope(t)
	dictionary := scope.Dictionary("run", 1)
	dictionary.Tuples[0].Route = "/mutated"
	if _, _, err := AggregateTrustedLabels(syntheticProfile(1), 0, dictionary); err == nil {
		t.Fatal("mutated dictionary hash was accepted")
	}
}

func TestAggregateTrustedLabelsRejectsDictionaryStructureAttacks(t *testing.T) {
	valid := testLabelScope(t).Dictionary("run", 1)
	tests := []struct {
		name   string
		mutate func(*profilecapture.LabelDictionary)
		resign bool
	}{
		{name: "unsealed", mutate: func(d *profilecapture.LabelDictionary) { d.Sealed = false }},
		{name: "capture-id", mutate: func(d *profilecapture.LabelDictionary) { d.CaptureID = "bad" }},
		{name: "invalid-tuple", mutate: func(d *profilecapture.LabelDictionary) { d.Tuples[0].Method = "" }, resign: true},
		{name: "duplicate", mutate: func(d *profilecapture.LabelDictionary) { d.Tuples = append(d.Tuples, d.Tuples[0]) }, resign: true},
		{name: "multiple-overflow", mutate: func(d *profilecapture.LabelDictionary) {
			d.Tuples[0].Overflow = true
			second := d.Tuples[0]
			second.TupleID = strings.Repeat("f", 32)
			d.Tuples = append(d.Tuples, second)
		}, resign: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dictionary := valid
			dictionary.Tuples = append([]profilecapture.SafeLabelTuple(nil), valid.Tuples...)
			test.mutate(&dictionary)
			if test.resign {
				resignDictionary(t, &dictionary)
			}
			if _, _, err := AggregateTrustedLabels(syntheticProfile(1), 0, dictionary); err == nil {
				t.Fatalf("accepted dictionary %#v", dictionary)
			}
		})
	}
}

func TestAggregateTrustedLabelsFlagsNumericPrivateLabels(t *testing.T) {
	dictionary := testLabelScope(t).Dictionary("run", 1)
	profile := syntheticProfile(1)
	profile.Sample[0].NumLabel = map[string][]int64{profilecapture.PrivateCaptureLabel: {1}}
	labels, foreign, err := AggregateTrustedLabels(profile, 0, dictionary)
	if err != nil || !foreign || len(labels) != 0 {
		t.Fatalf("labels=%#v foreign=%v err=%v", labels, foreign, err)
	}
}

func resignDictionary(t *testing.T, dictionary *profilecapture.LabelDictionary) {
	t.Helper()
	copy := *dictionary
	copy.SHA256 = ""
	body, err := json.Marshal(copy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	dictionary.SHA256 = hex.EncodeToString(sum[:])
}

func testLabelScope(t *testing.T) *profilecapture.LabelScope {
	t.Helper()
	// NewStandaloneLabelScope is for offline fixtures and does not make a
	// scope active in the runtime coordinator.
	scope := profilecapture.NewStandaloneLabelScope("0123456789abcdef0123456789abcdef")
	if !scope.Do(context.Background(), profilecapture.SafeLabelTuple{Method: "GET", Route: "/users/{id}", Scenario: "browse", Region: "tokyo"}, func(context.Context) {}) {
		t.Fatal("could not create dictionary tuple")
	}
	scope.Seal()
	return scope
}
