package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// insertSQL is the one statement that writes a record.
//
// `created_at` is absent on purpose: the database sets it, so two records from
// two processes are ordered by one clock.
const insertSQL = `
	INSERT INTO audit_events (
		id, action, actor_id, actor_kind, scope_type, scope_id,
		subject_type, subject_id, detail, source_address, user_agent
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// PostgresWriter records events in PostgreSQL.
type PostgresWriter struct {
	db *sql.DB
}

// NewPostgresWriter returns a writer over an open pool.
//
// It is for a caller that has no change to make alongside the record. A domain
// write uses RecordTx instead, so both land in one transaction.
func NewPostgresWriter(db *sql.DB) *PostgresWriter {
	return &PostgresWriter{db: db}
}

// Record writes one event on its own.
func (w *PostgresWriter) Record(ctx context.Context, event Event) error {
	return Record(ctx, w.db, event)
}

// Executor is the part of a database handle the ledger needs.
//
// *sql.DB and *sql.Tx both satisfy it, which is what lets a domain store write a
// record inside its own transaction while a caller with no transaction writes one
// directly.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Record writes one event through an executor.
//
// It is the single write path, so the identifier, the JSON encoding, and the
// null handling of an absent actor or scope are decided once.
func Record(ctx context.Context, executor Executor, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}

	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	detail, err := encodeDetail(event.Detail)
	if err != nil {
		return err
	}

	_, err = executor.ExecContext(ctx, insertSQL,
		id.String(),
		event.Action,
		nullable(event.ActorID),
		string(event.ActorKind),
		string(event.ScopeType),
		nullable(event.ScopeID),
		event.SubjectType,
		nullable(event.SubjectID),
		detail,
		nullable(event.SourceAddress),
		nullable(event.UserAgent),
	)
	if err != nil {
		return fmt.Errorf("record the audit event %s: %w", event.Action, err)
	}
	return nil
}

// encodeDetail renders a detail blob.
//
// A nil detail becomes `{}` rather than NULL, so a query never has to tell "no
// detail" from "a detail that was not recorded".
func encodeDetail(detail map[string]any) ([]byte, error) {
	if len(detail) == 0 {
		return []byte("{}"), nil
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("encode the audit detail: %w", err)
	}
	return encoded, nil
}

// nullable turns an empty identifier into SQL NULL.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Reader reads the ledger, for the tests that assert what was recorded and for
// the query surface phase 8 adds.
type Reader struct {
	db *sql.DB
}

// NewReader returns a reader over an open pool.
func NewReader(db *sql.DB) *Reader {
	return &Reader{db: db}
}

// Entry is one stored record.
type Entry struct {
	Action        string
	ActorID       string
	ActorKind     string
	ScopeType     string
	ScopeID       string
	SubjectType   string
	SubjectID     string
	Detail        map[string]any
	SourceAddress string
	UserAgent     string
	// CreatedAt is when the record was written, set by the database.
	//
	// It is read here because a reader asking for the history of something needs to know when
	// each change happened — W2.2's state-change history is unreadable without it — and because
	// the query already orders by this column, so exposing it costs no extra work. `Event`, the
	// write-side type, still has no timestamp: the rule that the application does not generate
	// "now" applies to writing, not to reading back what the database recorded.
	CreatedAt time.Time
}

// ForSubject returns the records about one subject, oldest first.
//
// It exists for the domain tests, which assert that an operation recorded what
// it claims to have recorded, and it is the shape the phase-8 console queries.
func (r *Reader) ForSubject(ctx context.Context, subjectType, subjectID string) ([]Entry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT action, COALESCE(actor_id::text, ''), actor_kind, scope_type,
		       COALESCE(scope_id::text, ''), subject_type, COALESCE(subject_id::text, ''),
		       detail, COALESCE(source_address, ''), COALESCE(user_agent, ''), created_at
		FROM audit_events
		WHERE subject_type = $1 AND subject_id = $2
		ORDER BY created_at, id`, subjectType, subjectID)
	if err != nil {
		return nil, fmt.Errorf("read the audit trail: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []Entry
	for rows.Next() {
		var (
			entry  Entry
			detail []byte
		)
		if err := rows.Scan(
			&entry.Action, &entry.ActorID, &entry.ActorKind, &entry.ScopeType,
			&entry.ScopeID, &entry.SubjectType, &entry.SubjectID, &detail,
			&entry.SourceAddress, &entry.UserAgent, &entry.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("read an audit record: %w", err)
		}
		if err := json.Unmarshal(detail, &entry.Detail); err != nil {
			return nil, fmt.Errorf("decode the audit detail: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Count returns how many records exist, which a test uses to prove that a
// refused write left no trace and that an accepted one left exactly one.
func (r *Reader) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events").Scan(&count); err != nil {
		return 0, fmt.Errorf("count the audit records: %w", err)
	}
	return count, nil
}

// UpdateIsRefused reports whether the database refuses to change a record.
//
// It exists so a test can assert the guarantee rather than restate it: the
// append-only rule is a trigger, and a comment claiming a trigger exists is not
// evidence that it does.
func (r *Reader) UpdateIsRefused(ctx context.Context, action string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE audit_events SET action = action WHERE action = $1", action)
	if err == nil {
		return errors.New("the database allowed an audit record to be updated")
	}
	return nil
}
