// Package feedback is the Postgres-backed store for bug reports and
// suggestions submitted through the frontend's floating feedback widget
// (see internal/api/feedback.go, the one caller). Kept as its own
// package, the same way internal/bcpcache is Postgres logic separate
// from internal/bcp: most submissions aren't even tied to a signed-in
// account, so this isn't really "user" data either.
package feedback

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status's two valid values (also enforced by migration 0013's CHECK
// constraint).
const (
	StatusOpen     = "open"
	StatusResolved = "resolved"
)

// Report is one submitted bug report or suggestion, as stored in
// Postgres.
type Report struct {
	ID           int64
	Kind         string // "bug" or "suggestion"
	Message      string
	Page         string
	ContactEmail string
	// SubmittedByUserID is 0 if the submitter wasn't signed in (or their
	// session cookie wasn't valid) at submission time — see
	// internal/api/feedback.go's Submit. SubmittedByName/Email are
	// captured then too, rather than joined against the users table
	// live, so a report still reads correctly even if that account is
	// later renamed or deleted.
	SubmittedByUserID int64
	SubmittedByName   string
	SubmittedByEmail  string
	Status            string // "open" or "resolved"
	CreatedAt         time.Time
}

// Store implements Postgres persistence for Report.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing connection pool (see internal/db) as a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts r and returns it with its assigned ID/Status/CreatedAt
// filled in.
func (s *Store) Create(ctx context.Context, r Report) (Report, error) {
	var submittedByUserID *int64
	if r.SubmittedByUserID != 0 {
		submittedByUserID = &r.SubmittedByUserID
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO feedback (kind, message, page, contact_email, submitted_by_user_id, submitted_by_name, submitted_by_email)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, status, created_at
	`, r.Kind, r.Message, r.Page, r.ContactEmail, submittedByUserID, r.SubmittedByName, r.SubmittedByEmail,
	).Scan(&r.ID, &r.Status, &r.CreatedAt)
	if err != nil {
		return Report{}, fmt.Errorf("creating feedback: %w", err)
	}
	return r, nil
}

// List returns every report for the admin panel (internal/api/admin.go)
// — open first (what an admin actually needs to act on), then resolved,
// newest first within each group. No pagination — same reasoning as
// user.Store.ListUsers.
func (s *Store) List(ctx context.Context) ([]Report, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, message, page, contact_email,
		       COALESCE(submitted_by_user_id, 0), submitted_by_name, submitted_by_email,
		       status, created_at
		FROM feedback
		ORDER BY (status = 'open') DESC, created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing feedback: %w", err)
	}
	defer rows.Close()

	reports := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(
			&r.ID, &r.Kind, &r.Message, &r.Page, &r.ContactEmail,
			&r.SubmittedByUserID, &r.SubmittedByName, &r.SubmittedByEmail,
			&r.Status, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning feedback: %w", err)
		}
		reports = append(reports, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing feedback: %w", err)
	}
	return reports, nil
}

// SetStatus marks a report open or resolved (see StatusOpen/StatusResolved).
func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE feedback SET status = $1 WHERE id = $2`, status, id)
	if err != nil {
		return fmt.Errorf("setting feedback status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountOpen returns how many reports are still open — the frontend's
// nav-drawer badge calls this once per app load (GET
// /api/admin/feedback/open-count) rather than fetching the full list
// just to show a number.
func (s *Store) CountOpen(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE status = $1`, StatusOpen).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting open feedback: %w", err)
	}
	return count, nil
}
