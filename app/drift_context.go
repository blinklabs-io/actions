package main

import (
	"context"
	"fmt"
)

type driftContextKey struct{}

func reportFromContext(ctx context.Context) *driftReport {
	report, _ := ctx.Value(driftContextKey{}).(*driftReport)
	return report
}

func syncLogf(ctx context.Context, format string, args ...any) {
	if reportFromContext(ctx) == nil {
		fmt.Printf(format, args...)
	}
}
