package requestsql

import (
	"context"
	"testing"
	"time"
)

func BenchmarkCompletedShapeOff(b *testing.B) {
	ctx, counter := WithCounter(context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CompletedShape(ctx, "SELECT ?", time.Microsecond, false)
	}
	b.StopTimer()
	if got := counter.Close(); got != int64(b.N) {
		b.Fatalf("count = %d", got)
	}
}

func BenchmarkCompletedShapeOn(b *testing.B) {
	ctx, counter := WithShapeCounter(context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CompletedShape(ctx, "SELECT ?", time.Microsecond, false)
	}
	b.StopTimer()
	if got := counter.Close(); got != int64(b.N) {
		b.Fatalf("count = %d", got)
	}
}
