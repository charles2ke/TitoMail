package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"strings"
)

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// escapeLike escapes LIKE wildcards; use with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func queryInt64s(ctx context.Context, q queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// chunk splits ids into slices of at most n elements to keep SQL parameter
// counts reasonable.
func chunk[T any](ids []T, n int) [][]T {
	var out [][]T
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

func dedupeInt64(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func txQueryer(tx *sql.Tx) queryer { return tx }
