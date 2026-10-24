// Package store keeps the local copy of the cluster in SQLite: the latest
// version of every object, optional revisions, the history of condition
// changes, and findings (unhealthy states) with the time they were open.
package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/guettli/check-conditions/pkg/checkconditions"
	"k8s.io/apimachinery/pkg/runtime/schema"
	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

const schemaVersion = 1

const schemaSQL = `
CREATE TABLE objects (
	uid              TEXT PRIMARY KEY,
	api_group        TEXT NOT NULL,
	api_version      TEXT NOT NULL,
	resource         TEXT NOT NULL,
	kind             TEXT NOT NULL,
	namespace        TEXT NOT NULL,
	name             TEXT NOT NULL,
	resource_version TEXT NOT NULL,
	hash             TEXT NOT NULL,
	object           TEXT NOT NULL,
	first_seen       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	deleted_at       INTEGER
);
CREATE INDEX objects_by_name ON objects(resource, namespace, name);
CREATE INDEX objects_by_group_resource ON objects(api_group, resource);

CREATE TABLE revisions (
	uid              TEXT NOT NULL,
	resource_version TEXT NOT NULL,
	observed_at      INTEGER NOT NULL,
	object_gz        BLOB NOT NULL
);
CREATE INDEX revisions_by_uid ON revisions(uid, observed_at);
CREATE INDEX revisions_by_time ON revisions(observed_at);

-- The current conditions of each object, used to detect changes.
CREATE TABLE conditions (
	uid                  TEXT NOT NULL,
	type                 TEXT NOT NULL,
	status               TEXT NOT NULL,
	reason               TEXT NOT NULL,
	message              TEXT NOT NULL,
	last_transition_time TEXT NOT NULL,
	PRIMARY KEY (uid, type)
);

-- One row per observed change of a condition.
CREATE TABLE condition_history (
	id                   INTEGER PRIMARY KEY,
	uid                  TEXT NOT NULL,
	observed_at          INTEGER NOT NULL,
	change               TEXT NOT NULL, -- 'set' or 'removed'
	type                 TEXT NOT NULL,
	status               TEXT NOT NULL,
	reason               TEXT NOT NULL,
	message              TEXT NOT NULL,
	last_transition_time TEXT NOT NULL
);
CREATE INDEX condition_history_by_uid ON condition_history(uid, observed_at);
CREATE INDEX condition_history_by_time ON condition_history(observed_at);

-- One row per period in which a finding was open. A finding that comes back
-- after it was resolved gets a new row.
CREATE TABLE findings (
	id          INTEGER PRIMARY KEY,
	key         TEXT NOT NULL,
	uid         TEXT NOT NULL,
	kind        TEXT NOT NULL,
	finding     TEXT NOT NULL, -- checkconditions.Finding as JSON, latest observation
	first_seen  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	resolved_at INTEGER
);
CREATE UNIQUE INDEX findings_open_key ON findings(key) WHERE resolved_at IS NULL;
CREATE INDEX findings_by_uid ON findings(uid, first_seen);
CREATE INDEX findings_by_time ON findings(first_seen, resolved_at);
`

// Store is the SQLite database. It is safe for concurrent use, but konmon
// has exactly one writer.
type Store struct {
	db *sql.DB
}

// Open opens (and creates or migrates) the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("database schema version %d is newer than this konmon (%d)", version, schemaVersion)
	}
	return s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		_, err := tx.tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	})
}

// Tx is a write transaction.
type Tx struct {
	ctx context.Context
	tx  *sql.Tx
}

// Update runs fn in one write transaction.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(&Tx{ctx: ctx, tx: sqlTx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMS(v.Int64)
}

// ObjectRef identifies a stored object.
type ObjectRef struct {
	GVR       schema.GroupVersionResource
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// UpsertObject stores the latest version of an object. hash identifies the
// content which matters: if it equals the stored hash nothing is written and
// changed is false. If keepRevision is true, a compressed copy is added to the
// revisions table.
func (tx *Tx) UpsertObject(ref ObjectRef, resourceVersion, hash string, object []byte, keepRevision bool, now time.Time) (changed bool, err error) {
	var oldHash string
	var deletedAt sql.NullInt64
	err = tx.tx.QueryRowContext(tx.ctx, "SELECT hash, deleted_at FROM objects WHERE uid = ?", ref.UID).Scan(&oldHash, &deletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("read object %s: %w", ref.UID, err)
	case oldHash == hash && !deletedAt.Valid:
		return false, nil
	}
	_, err = tx.tx.ExecContext(tx.ctx, `
		INSERT INTO objects (uid, api_group, api_version, resource, kind, namespace, name, resource_version, hash, object, first_seen, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (uid) DO UPDATE SET
			api_version = excluded.api_version, kind = excluded.kind,
			resource_version = excluded.resource_version, hash = excluded.hash,
			object = excluded.object, updated_at = excluded.updated_at, deleted_at = NULL`,
		ref.UID, ref.GVR.Group, ref.GVR.Version, ref.GVR.Resource, ref.Kind, ref.Namespace, ref.Name,
		resourceVersion, hash, string(object), ms(now), ms(now))
	if err != nil {
		return false, fmt.Errorf("write object %s: %w", ref.UID, err)
	}
	if keepRevision {
		gz, err := gzipBytes(object)
		if err != nil {
			return false, err
		}
		if _, err := tx.tx.ExecContext(tx.ctx,
			"INSERT INTO revisions (uid, resource_version, observed_at, object_gz) VALUES (?, ?, ?, ?)",
			ref.UID, resourceVersion, ms(now), gz); err != nil {
			return false, fmt.Errorf("write revision %s: %w", ref.UID, err)
		}
	}
	return true, nil
}

// MarkDeleted records that an object is gone. Its current conditions are
// dropped; the history is kept.
func (tx *Tx) MarkDeleted(uid string, now time.Time) error {
	if _, err := tx.tx.ExecContext(tx.ctx,
		"UPDATE objects SET deleted_at = ? WHERE uid = ? AND deleted_at IS NULL", ms(now), uid); err != nil {
		return fmt.Errorf("mark %s deleted: %w", uid, err)
	}
	if _, err := tx.tx.ExecContext(tx.ctx, "DELETE FROM conditions WHERE uid = ?", uid); err != nil {
		return fmt.Errorf("drop conditions of %s: %w", uid, err)
	}
	return nil
}

// MarkMissingDeleted marks all stored objects of a resource type as deleted
// which are not in live. Used after the initial list of a watch, to catch
// deletions which happened while konmon was not running.
func (tx *Tx) MarkMissingDeleted(gr schema.GroupResource, live map[string]struct{}, now time.Time) (int, error) {
	stored, err := tx.existingUIDs(gr)
	if err != nil {
		return 0, err
	}
	var missing []string
	for _, uid := range stored {
		if _, ok := live[uid]; !ok {
			missing = append(missing, uid)
		}
	}
	for _, uid := range missing {
		if err := tx.MarkDeleted(uid, now); err != nil {
			return 0, err
		}
	}
	return len(missing), nil
}

func (tx *Tx) existingUIDs(gr schema.GroupResource) ([]string, error) {
	rows, err := tx.tx.QueryContext(tx.ctx,
		"SELECT uid FROM objects WHERE api_group = ? AND resource = ? AND deleted_at IS NULL", gr.Group, gr.Resource)
	if err != nil {
		return nil, fmt.Errorf("list objects of %s: %w", gr, err)
	}
	defer func() { _ = rows.Close() }()
	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		uids = append(uids, uid)
	}
	return uids, rows.Err()
}

// Condition is one entry of status.conditions.
type Condition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime string
}

// UpdateConditions replaces the current conditions of an object and adds a
// history row for every condition which is new, changed or removed. A change
// of only lastTransitionTime is stored but not recorded as history.
func (tx *Tx) UpdateConditions(uid string, conditions []Condition, now time.Time) (historyRows int, err error) {
	old, err := tx.currentConditions(uid)
	if err != nil {
		return 0, err
	}
	seen := map[string]struct{}{}
	for _, c := range conditions {
		if _, dup := seen[c.Type]; dup {
			continue
		}
		seen[c.Type] = struct{}{}
		prev, existed := old[c.Type]
		if existed && prev == c {
			continue
		}
		if !existed || prev.Status != c.Status || prev.Reason != c.Reason || prev.Message != c.Message {
			if err := tx.addHistory(uid, "set", c, now); err != nil {
				return 0, err
			}
			historyRows++
		}
		if _, err := tx.tx.ExecContext(tx.ctx, `
			INSERT INTO conditions (uid, type, status, reason, message, last_transition_time) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (uid, type) DO UPDATE SET status = excluded.status, reason = excluded.reason,
				message = excluded.message, last_transition_time = excluded.last_transition_time`,
			uid, c.Type, c.Status, c.Reason, c.Message, c.LastTransitionTime); err != nil {
			return 0, fmt.Errorf("write condition %s of %s: %w", c.Type, uid, err)
		}
	}
	for t, c := range old {
		if _, ok := seen[t]; ok {
			continue
		}
		if err := tx.addHistory(uid, "removed", c, now); err != nil {
			return 0, err
		}
		historyRows++
		if _, err := tx.tx.ExecContext(tx.ctx, "DELETE FROM conditions WHERE uid = ? AND type = ?", uid, t); err != nil {
			return 0, fmt.Errorf("delete condition %s of %s: %w", t, uid, err)
		}
	}
	return historyRows, nil
}

func (tx *Tx) currentConditions(uid string) (map[string]Condition, error) {
	rows, err := tx.tx.QueryContext(tx.ctx,
		"SELECT type, status, reason, message, last_transition_time FROM conditions WHERE uid = ?", uid)
	if err != nil {
		return nil, fmt.Errorf("read conditions of %s: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]Condition{}
	for rows.Next() {
		var c Condition
		if err := rows.Scan(&c.Type, &c.Status, &c.Reason, &c.Message, &c.LastTransitionTime); err != nil {
			return nil, err
		}
		result[c.Type] = c
	}
	return result, rows.Err()
}

func (tx *Tx) addHistory(uid, change string, c Condition, now time.Time) error {
	_, err := tx.tx.ExecContext(tx.ctx, `
		INSERT INTO condition_history (uid, observed_at, change, type, status, reason, message, last_transition_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, ms(now), change, c.Type, c.Status, c.Reason, c.Message, c.LastTransitionTime)
	if err != nil {
		return fmt.Errorf("write condition history of %s: %w", uid, err)
	}
	return nil
}

// FindingRow is one period in which a finding was open.
type FindingRow struct {
	ID         int64
	Key        string
	Finding    checkconditions.Finding
	FirstSeen  time.Time
	LastSeen   time.Time
	ResolvedAt time.Time // zero while open
}

// SyncFindings makes the open findings equal to current: new findings are
// opened, known ones are updated, and open findings which are not in current
// are resolved. It returns the findings which are open afterwards and those
// resolved by this call.
func (tx *Tx) SyncFindings(current []checkconditions.Finding, now time.Time) (open, resolved []FindingRow, err error) {
	active, err := tx.openFindings()
	if err != nil {
		return nil, nil, err
	}
	byKey := make(map[string]FindingRow, len(active))
	for _, r := range active {
		byKey[r.Key] = r
	}
	seen := make(map[string]struct{}, len(current))
	for i := range current {
		f := current[i]
		key := f.Key()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		data, err := json.Marshal(f)
		if err != nil {
			return nil, nil, fmt.Errorf("encode finding %s: %w", key, err)
		}
		if r, ok := byKey[key]; ok {
			if _, err := tx.tx.ExecContext(tx.ctx,
				"UPDATE findings SET finding = ?, last_seen = ? WHERE id = ?", string(data), ms(now), r.ID); err != nil {
				return nil, nil, fmt.Errorf("update finding %s: %w", key, err)
			}
			r.Finding, r.LastSeen = f, now
			open = append(open, r)
			continue
		}
		res, err := tx.tx.ExecContext(tx.ctx,
			"INSERT INTO findings (key, uid, kind, finding, first_seen, last_seen) VALUES (?, ?, ?, ?, ?, ?)",
			key, string(f.UID), string(f.Kind), string(data), ms(now), ms(now))
		if err != nil {
			return nil, nil, fmt.Errorf("insert finding %s: %w", key, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, nil, err
		}
		open = append(open, FindingRow{ID: id, Key: key, Finding: f, FirstSeen: now, LastSeen: now})
	}
	for _, r := range active {
		if _, ok := seen[r.Key]; ok {
			continue
		}
		if _, err := tx.tx.ExecContext(tx.ctx,
			"UPDATE findings SET resolved_at = ? WHERE id = ?", ms(now), r.ID); err != nil {
			return nil, nil, fmt.Errorf("resolve finding %s: %w", r.Key, err)
		}
		r.ResolvedAt = now
		resolved = append(resolved, r)
	}
	return open, resolved, nil
}

func (tx *Tx) openFindings() ([]FindingRow, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, findingSelect+" WHERE resolved_at IS NULL")
	if err != nil {
		return nil, fmt.Errorf("read open findings: %w", err)
	}
	return scanFindings(rows)
}

const findingSelect = "SELECT id, key, finding, first_seen, last_seen, resolved_at FROM findings"

func scanFindings(rows *sql.Rows) ([]FindingRow, error) {
	defer func() { _ = rows.Close() }()
	var result []FindingRow
	for rows.Next() {
		var r FindingRow
		var data string
		var first, last int64
		var resolved sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Key, &data, &first, &last, &resolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &r.Finding); err != nil {
			return nil, fmt.Errorf("decode finding %d: %w", r.ID, err)
		}
		r.FirstSeen, r.LastSeen, r.ResolvedAt = fromMS(first), fromMS(last), fromNullMS(resolved)
		result = append(result, r)
	}
	return result, rows.Err()
}

// Prune deletes history older than before: revisions, condition history,
// resolved findings and objects deleted before that time.
func (tx *Tx) Prune(before time.Time) (int64, error) {
	var total int64
	for _, q := range []string{
		"DELETE FROM revisions WHERE observed_at < ?",
		"DELETE FROM condition_history WHERE observed_at < ?",
		"DELETE FROM findings WHERE resolved_at IS NOT NULL AND resolved_at < ?",
		"DELETE FROM revisions WHERE uid IN (SELECT uid FROM objects WHERE deleted_at < ?)",
		"DELETE FROM objects WHERE deleted_at < ?",
	} {
		res, err := tx.tx.ExecContext(tx.ctx, q, ms(before))
		if err != nil {
			return total, fmt.Errorf("prune: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// FindingsAt returns the findings which were open at time t, ordered by
// first_seen.
func (s *Store) FindingsAt(ctx context.Context, t time.Time) ([]FindingRow, error) {
	rows, err := s.db.QueryContext(ctx, findingSelect+
		" WHERE first_seen <= ? AND (resolved_at IS NULL OR resolved_at > ?) ORDER BY first_seen, id", ms(t), ms(t))
	if err != nil {
		return nil, fmt.Errorf("read findings: %w", err)
	}
	return scanFindings(rows)
}

// ResolvedSince returns findings resolved after t.
func (s *Store) ResolvedSince(ctx context.Context, t time.Time) ([]FindingRow, error) {
	rows, err := s.db.QueryContext(ctx, findingSelect+" WHERE resolved_at > ? ORDER BY resolved_at, id", ms(t))
	if err != nil {
		return nil, fmt.Errorf("read resolved findings: %w", err)
	}
	return scanFindings(rows)
}

// FindingsOfObject returns all findings of one object which were open after
// since.
func (s *Store) FindingsOfObject(ctx context.Context, uid string, since time.Time) ([]FindingRow, error) {
	rows, err := s.db.QueryContext(ctx, findingSelect+
		" WHERE uid = ? AND (resolved_at IS NULL OR resolved_at >= ?) ORDER BY first_seen, id", uid, ms(since))
	if err != nil {
		return nil, fmt.Errorf("read findings of %s: %w", uid, err)
	}
	return scanFindings(rows)
}

// ObjectRow is a stored object without its content.
type ObjectRow struct {
	ObjectRef
	FirstSeen time.Time
	UpdatedAt time.Time
	DeletedAt time.Time // zero if the object exists
}

// FindObjects returns the objects with this name. resource matches the plural
// resource name or the kind (case-insensitive); group is optional. An empty
// namespace matches all namespaces.
func (s *Store) FindObjects(ctx context.Context, resource, group, namespace, name string) ([]ObjectRow, error) {
	q := `SELECT uid, api_group, api_version, resource, kind, namespace, name, first_seen, updated_at, deleted_at
		FROM objects WHERE (resource = ? OR lower(kind) = ?) AND name = ?`
	args := []any{strings.ToLower(resource), strings.ToLower(resource), name}
	if group != "" {
		q += " AND api_group = ?"
		args = append(args, group)
	}
	if namespace != "" {
		q += " AND namespace = ?"
		args = append(args, namespace)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY first_seen", args...)
	if err != nil {
		return nil, fmt.Errorf("find objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []ObjectRow
	for rows.Next() {
		var r ObjectRow
		var first, updated int64
		var deleted sql.NullInt64
		if err := rows.Scan(&r.UID, &r.GVR.Group, &r.GVR.Version, &r.GVR.Resource, &r.Kind, &r.Namespace, &r.Name,
			&first, &updated, &deleted); err != nil {
			return nil, err
		}
		r.FirstSeen, r.UpdatedAt, r.DeletedAt = fromMS(first), fromMS(updated), fromNullMS(deleted)
		result = append(result, r)
	}
	return result, rows.Err()
}

// HistoryRow is one recorded condition change.
type HistoryRow struct {
	ObservedAt time.Time
	Change     string
	Condition
}

// ConditionHistory returns the condition changes of one object after since.
func (s *Store) ConditionHistory(ctx context.Context, uid string, since time.Time) ([]HistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT observed_at, change, type, status, reason, message, last_transition_time
		FROM condition_history WHERE uid = ? AND observed_at >= ? ORDER BY observed_at, id`, uid, ms(since))
	if err != nil {
		return nil, fmt.Errorf("read condition history of %s: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()
	var result []HistoryRow
	for rows.Next() {
		var r HistoryRow
		var observed int64
		if err := rows.Scan(&observed, &r.Change, &r.Type, &r.Status, &r.Reason, &r.Message, &r.LastTransitionTime); err != nil {
			return nil, err
		}
		r.ObservedAt = fromMS(observed)
		result = append(result, r)
	}
	return result, rows.Err()
}

// Revision is one stored version of an object.
type Revision struct {
	ResourceVersion string
	ObservedAt      time.Time
	Object          []byte
}

// Revisions returns the stored revisions of one object after since.
func (s *Store) Revisions(ctx context.Context, uid string, since time.Time) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT resource_version, observed_at, object_gz FROM revisions
		WHERE uid = ? AND observed_at >= ? ORDER BY observed_at`, uid, ms(since))
	if err != nil {
		return nil, fmt.Errorf("read revisions of %s: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()
	var result []Revision
	for rows.Next() {
		var r Revision
		var observed int64
		var gz []byte
		if err := rows.Scan(&r.ResourceVersion, &observed, &gz); err != nil {
			return nil, err
		}
		r.ObservedAt = fromMS(observed)
		if r.Object, err = gunzipBytes(gz); err != nil {
			return nil, fmt.Errorf("decompress revision %s of %s: %w", r.ResourceVersion, uid, err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
