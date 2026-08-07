package profilemodel

import (
	"math"
	"testing"
)

func TestCheckedNegate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int64
		want int64
		ok   bool
	}{
		{name: "zero", in: 0, want: 0, ok: true},
		{name: "positive", in: 42, want: -42, ok: true},
		{name: "negative", in: -42, want: 42, ok: true},
		{name: "max", in: math.MaxInt64, want: -math.MaxInt64, ok: true},
		{name: "min overflows", in: math.MinInt64, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CheckedNegate(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("CheckedNegate(%d) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCheckedAbs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		in   int64
		want int64
		ok   bool
	}{
		{in: 0, want: 0, ok: true},
		{in: 7, want: 7, ok: true},
		{in: -7, want: 7, ok: true},
		{in: math.MinInt64, ok: false},
	} {
		got, ok := CheckedAbs(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("CheckedAbs(%d) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCheckedAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{name: "normal", a: 20, b: 22, want: 42, ok: true},
		{name: "max exact", a: math.MaxInt64 - 1, b: 1, want: math.MaxInt64, ok: true},
		{name: "positive overflow", a: math.MaxInt64, b: 1, ok: false},
		{name: "min exact", a: math.MinInt64 + 1, b: -1, want: math.MinInt64, ok: true},
		{name: "negative overflow", a: math.MinInt64, b: -1, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CheckedAdd(tt.a, tt.b)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("CheckedAdd(%d, %d) = (%d, %t), want (%d, %t)", tt.a, tt.b, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCheckedUint64Arithmetic(t *testing.T) {
	t.Parallel()

	if got, ok := CheckedAddUint64(20, 22); !ok || got != 42 {
		t.Fatalf("CheckedAddUint64(20, 22) = (%d, %t)", got, ok)
	}
	if _, ok := CheckedAddUint64(math.MaxUint64, 1); ok {
		t.Fatal("CheckedAddUint64 accepted overflow")
	}
	if got, ok := CheckedMulUint64(6, 7); !ok || got != 42 {
		t.Fatalf("CheckedMulUint64(6, 7) = (%d, %t)", got, ok)
	}
	if got, ok := CheckedMulUint64(0, math.MaxUint64); !ok || got != 0 {
		t.Fatalf("CheckedMulUint64(0, MaxUint64) = (%d, %t)", got, ok)
	}
	if _, ok := CheckedMulUint64(math.MaxUint64, 2); ok {
		t.Fatal("CheckedMulUint64 accepted overflow")
	}
}

func TestAbsoluteBudget(t *testing.T) {
	t.Parallel()

	if got, ok := AbsoluteBudget([]int64{-20, 0, 22}); !ok || got != 42 {
		t.Fatalf("AbsoluteBudget = (%d, %t), want (42, true)", got, ok)
	}
	if _, ok := AbsoluteBudget([]int64{math.MinInt64}); ok {
		t.Fatal("AbsoluteBudget accepted MinInt64")
	}
	if _, ok := AbsoluteBudget([]int64{math.MaxInt64, 1}); ok {
		t.Fatal("AbsoluteBudget accepted an overflowing sum")
	}
}

func FuzzCheckedArithmetic(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(math.MaxInt64), int64(1))
	f.Add(int64(math.MinInt64), int64(-1))

	f.Fuzz(func(t *testing.T, a, b int64) {
		if sum, ok := CheckedAdd(a, b); ok {
			if b > 0 && sum < a || b < 0 && sum > a {
				t.Fatalf("accepted wrapped sum: %d + %d = %d", a, b, sum)
			}
		}
		if a == math.MinInt64 {
			if _, ok := CheckedAbs(a); ok {
				t.Fatal("accepted abs(MinInt64)")
			}
		}
	})
}
