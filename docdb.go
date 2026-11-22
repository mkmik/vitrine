package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type DBError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *DBError) Error() string { return e.Code + ": " + e.Message }

func dbErr(code, f string, a ...any) *DBError { return &DBError{Code: code, Message: fmt.Sprintf(f, a...)} }

type Doc struct {
	Path      string         `json:"path"`
	ID        string         `json:"id"`
	Exists    bool           `json:"exists"`
	Data      map[string]any `json:"data,omitempty"`
	Version   int            `json:"version,omitempty"`
	UpdatedAt int64          `json:"updated_at,omitempty"`
}

// Viewer is who performs a db call: their user id (may be "") and level.
type Viewer struct {
	UID   string
	Level int
}

func collOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return ""
	}
	return path[:i]
}

func idOf(path string) string { return path[strings.LastIndex(path, "/")+1:] }

func (s *Store) canRead(c *Caps, v Viewer, path string) bool {
	return v.Level >= dbLevels(c, path, v.UID).read
}

func (s *Store) canWrite(c *Caps, v Viewer, path string) bool {
	return v.Level >= dbLevels(c, path, v.UID).write
}

func (s *Store) docGet(aid string, c *Caps, v Viewer, path string) (*Doc, *DBError) {
	if err := checkPath(path, true); err != nil {
		return nil, dbErr("invalid_argument", "%v", err)
	}
	d := &Doc{Path: path, ID: idOf(path)}
	if !s.canRead(c, v, path) {
		return d, nil
	}
	var body string
	err := s.db.QueryRow(`SELECT body, version, updated_at FROM docs WHERE artifact_id=? AND path=?`, aid, path).Scan(&body, &d.Version, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return d, nil
	}
	if err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	d.Exists = true
	json.Unmarshal([]byte(body), &d.Data)
	return d, nil
}

type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type Query struct {
	Coll    string   `json:"coll"`
	Where   []Filter `json:"where,omitempty"`
	OrderBy string   `json:"orderBy,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

const maxScan = 5000

func (s *Store) docQuery(aid string, c *Caps, v Viewer, q Query) ([]*Doc, *DBError) {
	if err := checkPath(q.Coll, false); err != nil {
		return nil, dbErr("invalid_argument", "%v", err)
	}
	if len(q.Where) > 10 {
		return nil, dbErr("invalid_argument", "at most 10 filters")
	}
	for _, f := range q.Where {
		switch f.Op {
		case "==", "!=", "<", "<=", ">", ">=", "array-contains":
		case "in", "not-in":
			arr, ok := f.Value.([]any)
			if !ok || len(arr) > 30 {
				return nil, dbErr("invalid_argument", "%s needs an array of at most 30 values", f.Op)
			}
		default:
			return nil, dbErr("invalid_argument", "unknown operator %q", f.Op)
		}
	}
	if q.Limit < 0 || q.Limit > 1000 {
		return nil, dbErr("invalid_argument", "limit must be 1-1000")
	}
	rows, err := s.db.Query(`SELECT path, body, version, updated_at FROM docs WHERE artifact_id=? AND coll=? ORDER BY path LIMIT ?`, aid, q.Coll, maxScan+1)
	if err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	defer rows.Close()
	out := []*Doc{}
	n := 0
	for rows.Next() {
		n++
		if n > maxScan {
			return nil, dbErr("resource_exhausted", "query scans more than %d documents", maxScan)
		}
		d := &Doc{Exists: true}
		var body string
		rows.Scan(&d.Path, &body, &d.Version, &d.UpdatedAt)
		d.ID = idOf(d.Path)
		if !s.canRead(c, v, d.Path) {
			continue
		}
		json.Unmarshal([]byte(body), &d.Data)
		if matchFilters(d.Data, q.Where) {
			out = append(out, d)
		}
	}
	if q.OrderBy != "" {
		desc := q.Dir == "desc"
		sort.SliceStable(out, func(i, j int) bool {
			a, aok := out[i].Data[q.OrderBy]
			b, bok := out[j].Data[q.OrderBy]
			if !aok || !bok {
				return aok && !bok // missing sorts last
			}
			c := compareJSON(a, b)
			if desc {
				return c > 0
			}
			return c < 0
		})
	} else {
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func matchFilters(d map[string]any, fs []Filter) bool {
	for _, f := range fs {
		v, ok := d[f.Field]
		switch f.Op {
		case "==":
			if !ok || compareJSON(v, f.Value) != 0 {
				return false
			}
		case "!=":
			if !ok || compareJSON(v, f.Value) == 0 {
				return false
			}
		case "<", "<=", ">", ">=":
			if !ok || typeRank(v) != typeRank(f.Value) {
				return false
			}
			c := compareJSON(v, f.Value)
			if (f.Op == "<" && c >= 0) || (f.Op == "<=" && c > 0) || (f.Op == ">" && c <= 0) || (f.Op == ">=" && c < 0) {
				return false
			}
		case "in", "not-in":
			found := false
			for _, x := range f.Value.([]any) {
				if ok && compareJSON(v, x) == 0 {
					found = true
					break
				}
			}
			if !ok || (f.Op == "in") != found {
				return false
			}
		case "array-contains":
			arr, isArr := v.([]any)
			if !isArr {
				return false
			}
			found := false
			for _, x := range arr {
				if compareJSON(x, f.Value) == 0 {
					found = true
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func typeRank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case float64, int, int64:
		return 2
	case string:
		return 3
	case []any:
		return 4
	default:
		return 5
	}
}

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return math.NaN()
}

func compareJSON(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return ra - rb
	}
	switch ra {
	case 0:
		return 0
	case 1:
		ab, bb := a.(bool), b.(bool)
		if ab == bb {
			return 0
		}
		if !ab {
			return -1
		}
		return 1
	case 2:
		fa, fb := toF(a), toF(b)
		if fa < fb {
			return -1
		}
		if fa > fb {
			return 1
		}
		return 0
	case 3:
		return strings.Compare(a.(string), b.(string))
	default:
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		return strings.Compare(string(ja), string(jb))
	}
}

func jsonDepth(v any) int {
	m := 0
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			m = max(m, jsonDepth(e))
		}
		return m + 1
	case []any:
		for _, e := range x {
			m = max(m, jsonDepth(e))
		}
		return m + 1
	}
	return 0
}

func deepMerge(dst, src map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

type WriteOp struct {
	Op        string         `json:"op"` // set | update | delete
	Path      string         `json:"path"`
	Data      map[string]any `json:"data,omitempty"`
	IfVersion *int           `json:"ifVersion,omitempty"`
}

type WriteResult struct {
	Version int `json:"version"`
}

// docWrite applies one write and returns the new version (0 for delete).
func (s *Store) docWrite(aid string, c *Caps, v Viewer, w WriteOp, maxBytes, maxDocs int) (*WriteResult, *DBError) {
	if err := checkPath(w.Path, true); err != nil {
		return nil, dbErr("invalid_argument", "%v", err)
	}
	if !s.canWrite(c, v, w.Path) {
		return nil, dbErr("invalid_argument", "this viewer may not write %s", w.Path)
	}
	if w.Op != "delete" {
		if w.Data == nil {
			return nil, dbErr("invalid_argument", "document body must be an object")
		}
		if jsonDepth(w.Data) > 32 {
			return nil, dbErr("invalid_argument", "document over 32 levels deep")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	defer tx.Rollback()
	var cur string
	var ver int
	err = tx.QueryRow(`SELECT body, version FROM docs WHERE artifact_id=? AND path=?`, aid, w.Path).Scan(&cur, &ver)
	exists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return nil, dbErr("unavailable", "%v", err)
	}
	if w.IfVersion != nil && *w.IfVersion != ver {
		return nil, dbErr("failed_precondition", "document is at version %d, not %d", ver, *w.IfVersion)
	}
	switch w.Op {
	case "delete":
		if exists {
			if _, err := tx.Exec(`DELETE FROM docs WHERE artifact_id=? AND path=?`, aid, w.Path); err != nil {
				return nil, dbErr("unavailable", "%v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, dbErr("unavailable", "%v", err)
		}
		return &WriteResult{}, nil
	case "update":
		if !exists {
			return nil, dbErr("invalid_argument", "update needs an existing document: %s", w.Path)
		}
		var old map[string]any
		json.Unmarshal([]byte(cur), &old)
		w.Data = deepMerge(old, w.Data)
	case "set":
	default:
		return nil, dbErr("invalid_argument", "unknown op %q", w.Op)
	}
	body, _ := json.Marshal(w.Data)
	if len(body) > maxBytes {
		return nil, dbErr("invalid_argument", "document over %d bytes", maxBytes)
	}
	if !exists {
		var n int
		tx.QueryRow(`SELECT COUNT(*) FROM docs WHERE artifact_id=?`, aid).Scan(&n)
		if n >= maxDocs {
			return nil, dbErr("quota_exceeded", "this artifact's database holds the maximum of %d documents", maxDocs)
		}
	}
	ver++
	if _, err := tx.Exec(`INSERT INTO docs(artifact_id, path, coll, body, version, updated_by, updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(artifact_id, path) DO UPDATE SET body=excluded.body, version=excluded.version, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		aid, w.Path, collOf(w.Path), string(body), ver, v.UID, now()); err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	return &WriteResult{Version: ver}, nil
}

type AcquireResult struct {
	Acquired  bool   `json:"acquired"`
	Version   int    `json:"version,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Holder    string `json:"holder,omitempty"`
}

func (s *Store) docAcquire(aid string, c *Caps, v Viewer, path, holder string, ttlMs int, data map[string]any, maxBytes int) (*AcquireResult, *DBError) {
	if err := checkPath(path, true); err != nil {
		return nil, dbErr("invalid_argument", "%v", err)
	}
	if holder == "" {
		return nil, dbErr("invalid_argument", "holder is required")
	}
	if !s.canWrite(c, v, path) {
		return nil, dbErr("invalid_argument", "this viewer may not write %s", path)
	}
	if ttlMs <= 0 {
		ttlMs = 30000
	}
	ttlMs = max(1000, min(600000, ttlMs))
	tx, err := s.db.Begin()
	if err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	defer tx.Rollback()
	var body, lh string
	var ver int
	var lu int64
	err = tx.QueryRow(`SELECT body, version, lease_holder, lease_until FROM docs WHERE artifact_id=? AND path=?`, aid, path).Scan(&body, &ver, &lh, &lu)
	exists := err == nil
	nowMs := time.Now().UnixMilli()
	if exists && lh != "" && lh != holder && lu > nowMs {
		return &AcquireResult{Acquired: false, ExpiresAt: time.UnixMilli(lu).UTC().Format(time.RFC3339Nano)}, nil
	}
	cur := map[string]any{}
	if exists {
		json.Unmarshal([]byte(body), &cur)
	}
	if data != nil {
		cur = deepMerge(cur, data)
	}
	b, _ := json.Marshal(cur)
	if len(b) > maxBytes {
		return nil, dbErr("invalid_argument", "document over %d bytes", maxBytes)
	}
	until := nowMs + int64(ttlMs)
	ver++
	if _, err := tx.Exec(`INSERT INTO docs(artifact_id, path, coll, body, version, updated_by, updated_at, lease_holder, lease_until) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(artifact_id, path) DO UPDATE SET body=excluded.body, version=excluded.version, updated_by=excluded.updated_by, updated_at=excluded.updated_at, lease_holder=excluded.lease_holder, lease_until=excluded.lease_until`,
		aid, path, collOf(path), string(b), ver, v.UID, now(), holder, until); err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, dbErr("unavailable", "%v", err)
	}
	return &AcquireResult{Acquired: true, Version: ver, ExpiresAt: time.UnixMilli(until).UTC().Format(time.RFC3339Nano), Holder: holder}, nil
}
