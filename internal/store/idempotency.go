package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IdempotencyRecord is a stored request and, once done, its response.
type IdempotencyRecord struct {
	RequestHash  []byte
	Status       string // in_progress or completed
	ResponseCode int
	ResponseBody []byte
}

// IdempotencyBegin claims a key. created is true when this call inserted
// the row and now owns the request. When the key exists it returns the
// stored record instead. The primary key (merchant_id, key) makes the
// claim atomic even under concurrent duplicates.
func (s *Store) IdempotencyBegin(ctx context.Context, merchantID, key string, hash []byte) (IdempotencyRecord, bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO idempotency_keys (merchant_id, key, request_hash, status)
		VALUES ($1, $2, $3, 'in_progress') ON CONFLICT (merchant_id, key) DO NOTHING`, merchantID, key, hash)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return IdempotencyRecord{RequestHash: hash, Status: "in_progress"}, true, nil
	}
	var r IdempotencyRecord
	var code *int
	err = s.Pool.QueryRow(ctx, `SELECT request_hash, status, response_code, response_body FROM idempotency_keys
		WHERE merchant_id = $1 AND key = $2`, merchantID, key).Scan(&r.RequestHash, &r.Status, &code, &r.ResponseBody)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted between the two statements (the owner got a 5xx). Retry once.
		return s.IdempotencyBegin(ctx, merchantID, key, hash)
	}
	if code != nil {
		r.ResponseCode = *code
	}
	return r, false, err
}

// IdempotencyComplete stores the final response.
func (s *Store) IdempotencyComplete(ctx context.Context, merchantID, key string, code int, body []byte) error {
	_, err := s.Pool.Exec(ctx, `UPDATE idempotency_keys SET status = 'completed', response_code = $1, response_body = $2
		WHERE merchant_id = $3 AND key = $4`, code, body, merchantID, key)
	return err
}

// IdempotencyRelease deletes an unfinished claim so the client can retry
// (used when the request failed with a server error).
func (s *Store) IdempotencyRelease(ctx context.Context, merchantID, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE merchant_id = $1 AND key = $2 AND status = 'in_progress'`,
		merchantID, key)
	return err
}

// PurgeIdempotency removes completed keys older than completedBefore and
// abandoned in-progress claims older than staleBefore.
func (s *Store) PurgeIdempotency(ctx context.Context, completedBefore, staleBefore time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM idempotency_keys
		WHERE (status = 'completed' AND created_at < $1) OR (status = 'in_progress' AND created_at < $2)`,
		completedBefore, staleBefore)
	return tag.RowsAffected(), err
}
