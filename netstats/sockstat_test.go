package netstats

import "testing"

// TestParseSockstat covers the field-order and field-membership drift that
// /proc/net/sockstat has shown across kernel versions: the parser must key on
// names, never on position.
func TestParseSockstat(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    TCPSummary
		wantErr bool
	}{
		{
			name: "documented order",
			data: "sockets: used 340\n" +
				"TCP: inuse 12 orphan 3 tw 57 alloc 20 mem 4\n" +
				"UDP: inuse 4 mem 2\n",
			want: TCPSummary{InUse: 12, TimeWait: 57, Orphan: 3},
		},
		{
			name: "shuffled order",
			data: "TCP: tw 57 mem 4 inuse 12 alloc 20 orphan 3\n",
			want: TCPSummary{InUse: 12, TimeWait: 57, Orphan: 3},
		},
		{
			name: "missing orphan and tw stay zero",
			data: "sockets: used 5\nTCP: inuse 9\n",
			want: TCPSummary{InUse: 9},
		},
		{
			name: "trailing key without value is ignored",
			data: "TCP: inuse 9 tw\n",
			want: TCPSummary{InUse: 9},
		},
		{
			name: "unknown keys are ignored",
			data: "TCP: inuse 9 newcounter 1 tw 2\n",
			want: TCPSummary{InUse: 9, TimeWait: 2},
		},
		{
			name: "TCP6 line does not satisfy the v4 parser",
			data: "TCP6: inuse 9\n",
			// A prefix match on "TCP" would wrongly accept this line, so the
			// parser compares the whole token.
			wantErr: true,
		},
		{
			name:    "no TCP line",
			data:    "sockets: used 5\nUDP: inuse 4 mem 2\n",
			wantErr: true,
		},
		{
			name:    "non numeric counter",
			data:    "TCP: inuse many\n",
			wantErr: true,
		},
		{
			name:    "empty file",
			data:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSockstat([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSockstat() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSockstat() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseSockstat() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseSockstat6 fixes the v6 file's much smaller shape.
func TestParseSockstat6(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    int64
		wantErr bool
	}{
		{
			name: "typical file",
			data: "TCP6: inuse 7\nUDP6: inuse 2\nRAW6: inuse 0\n",
			want: 7,
		},
		{
			name: "TCP6 last",
			data: "UDP6: inuse 2\nFRAG6: inuse 0 mem 0\nTCP6: inuse 7\n",
			want: 7,
		},
		{
			name: "TCP6 without inuse",
			data: "TCP6: mem 3\n",
			want: 0,
		},
		{
			name:    "no TCP6 line",
			data:    "UDP6: inuse 2\n",
			wantErr: true,
		},
		{
			name:    "non numeric",
			data:    "TCP6: inuse -\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSockstat6([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSockstat6() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSockstat6() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseSockstat6() = %d, want %d", got, tt.want)
			}
		})
	}
}
