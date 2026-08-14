package flowstats

import "testing"

func TestFlowAndStoryAggregation(t *testing.T) {
	r := NewRegistry()
	for _, session := range []string{"s1", "s2"} {
		r.Observe(session, "browse", "GET", "/")
		r.Observe(session, "browse", "GET", "/posts/{id}")
	}
	snapshot := r.Snapshot()
	if len(snapshot.Flows) != 1 || snapshot.Flows[0].Count != 2 {
		t.Fatalf("flows = %#v", snapshot.Flows)
	}
	if len(snapshot.Stories) != 1 || snapshot.Stories[0].Sessions != 2 {
		t.Fatalf("stories = %#v", snapshot.Stories)
	}
}
