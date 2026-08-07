package profilemodel

import "math"

// CheckedNegate returns -v unless v is the one two's-complement value that
// cannot be represented after negation.
func CheckedNegate(v int64) (int64, bool) {
	if v == math.MinInt64 {
		return 0, false
	}
	return -v, true
}

// CheckedAbs returns the magnitude of v without allowing MinInt64 to wrap.
func CheckedAbs(v int64) (int64, bool) {
	if v >= 0 {
		return v, true
	}
	return CheckedNegate(v)
}

// CheckedAdd adds two signed values and reports whether the result fits in an
// int64.
func CheckedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// CheckedAddUint64 adds two unsigned values and reports overflow.
func CheckedAddUint64(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, false
	}
	return a + b, true
}

// CheckedMulUint64 multiplies two unsigned values and reports overflow.
func CheckedMulUint64(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxUint64/b {
		return 0, false
	}
	return a * b, true
}

// AbsoluteBudget returns the sum of the magnitudes in values. It is the
// precondition used before any profile merge or aggregation: if the entire
// absolute budget fits, every subset sum fits as well.
func AbsoluteBudget(values []int64) (int64, bool) {
	var total int64
	for _, value := range values {
		magnitude, ok := CheckedAbs(value)
		if !ok {
			return 0, false
		}
		total, ok = CheckedAdd(total, magnitude)
		if !ok {
			return 0, false
		}
	}
	return total, true
}
