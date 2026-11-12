package main

import (
	"context"
	"crypto/sha256"
	"fmt"
)

func fileDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("sha256: %x", sum)
}

func reportManagedFile(ctx context.Context, batch *commitBatch, path, before, after, status string) {
	if report := reportFromContext(ctx); report != nil {
		entry := report.change("managed file", path, before, after)
		entry.status = status
		if status == "pending" {
			batch.reportEntries = append(batch.reportEntries, entry)
		}
	}
}
