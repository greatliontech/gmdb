package typed

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "db.gmdb")
}

func collectIssues(seq func(func(gmdb.CheckIssue) bool)) []gmdb.CheckIssue {
	var out []gmdb.CheckIssue
	for iss := range seq {
		out = append(out, iss)
	}
	return out
}

func openWith(t *testing.T, ctx context.Context, path string, opts gmdb.Options) *gmdb.DB {
	t.Helper()
	db, err := gmdb.Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}
