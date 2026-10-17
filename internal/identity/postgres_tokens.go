package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// splitScopes reads the delimited scope set a query produced.
//
// A write passes a plain `[]string`, which the driver encodes as a `TEXT[]` correctly; only
// the read side needed a representation, and the query supplies a delimited string so no
// array literal is parsed here.
func splitScopes(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, scopeSeparator)
}

// The PostgreSQL implementation of personal access tokens.
//
// Two rules from the documents shape every statement here, and they are the reason the
// queries look the way they do:
//
//   - **Only the digest is compared, and a revoked token is not a candidate.** The
//     lookup filters `revoked_at IS NULL`, so a revoked token and one that never existed
//     both find nothing and the caller cannot tell which it presented.
//   - **Revocation is a timestamp, never a delete.** The audit record has to name a token
//     that still exists, and a listing has to show what was revoked and when.

// scopeSeparator delimits the scope set in a read.
//
// A scope is a short lowercase identifier from a fixed set, so it cannot contain this
// character; the choice is therefore safe rather than merely convenient, and it is stated
// because a delimiter that a value could contain would split one scope into two.
const scopeSeparator = ","

// tokenColumns is the projection every read shares, so a scan can never drift from the
// query that feeds it.
//
// **The scope array is read as a delimited string, and the database does the
// serialisation.** The alternative was to scan a `TEXT[]` in Go, and that path is a dead
// end with this driver: the pgx stdlib returns an array column as a `string` and fills
// neither `*[]string` nor `*pgtype.Array[string]`, so Go would have to parse a PostgreSQL
// array literal — a small parser to get wrong about quoting, escaping, and NULLs, for a
// column whose contents this package controls. `array_to_string` produces a value the
// scan can read directly, and the delimiter is a character no scope contains.
const tokenColumns = `id, user_id, name, token_prefix,
	array_to_string(scopes, '` + scopeSeparator + `') AS scopes,
	created_at, last_used_at, revoked_at`

// CreateToken stores a new token.
//
// The prefix and the digest are written in one statement, so a row can never exist with
// one and not the other.
func (s *PostgresStore) CreateToken(ctx context.Context, token NewTokenRecord) (Token, error) {
	var created Token
	var scopes string
	var lastUsed, revoked sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO personal_access_tokens (id, user_id, name, token_prefix, token_hash, scopes)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tokenColumns,
		token.ID, token.UserID, token.Name, token.TokenPrefix, token.TokenHash, token.Scopes,
	).Scan(&created.ID, &created.UserID, &created.Name, &created.Prefix, &scopes,
		&created.CreatedAt, &lastUsed, &revoked)
	if err != nil {
		return Token{}, fmt.Errorf("create the token: %w", err)
	}
	created.Scopes = splitScopes(scopes)
	created.LastUsedAt = timeOrNil(lastUsed)
	created.RevokedAt = timeOrNil(revoked)
	return created, nil
}

// FindTokenByHash resolves a presented token to its account.
//
// One statement joins the token to its account, so a token whose account has been
// deleted finds nothing rather than resolving to a missing row — the foreign key makes
// that impossible today, and the join keeps it impossible if the delete rule changes.
//
// A revoked token is excluded by the predicate rather than by a check afterwards, which
// is what makes revocation take effect on the next request
// (`docs/architecture/secrets.md`: "a revoked session, token, or key fails on the next
// request, and any cache that could answer otherwise is invalidated on revocation").
// Nothing caches this, so the predicate is the whole of it.
func (s *PostgresStore) FindTokenByHash(ctx context.Context, tokenHash []byte) (User, Token, error) {
	var user User
	var token Token
	var scopes string
	var lastUsed, revoked sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.email, u.status, u.is_admin, u.created_at,
		       t.id, t.user_id, t.name, t.token_prefix, t.scopes, t.created_at, t.last_used_at, t.revoked_at
		FROM personal_access_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.revoked_at IS NULL`, tokenHash,
	).Scan(&user.ID, &user.Username, &user.Email, &user.Status, &user.IsAdmin, &user.CreatedAt,
		&token.ID, &token.UserID, &token.Name, &token.Prefix, &scopes,
		&token.CreatedAt, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Token{}, ErrInvalidToken
	}
	if err != nil {
		return User{}, Token{}, fmt.Errorf("resolve the token: %w", err)
	}

	// A disabled account's token stops working, which is the rule administration.md
	// states for every credential: "Disable is preferred over delete", and an enable that
	// "does not restore revoked sessions, tokens, or keys" is only true if a disabled
	// account is refused here rather than at the session layer.
	if user.Status != StatusActive {
		return User{}, Token{}, ErrAccountDisabled
	}

	token.Scopes = splitScopes(scopes)
	token.LastUsedAt = timeOrNil(lastUsed)
	token.RevokedAt = timeOrNil(revoked)
	return user, token, nil
}

// ListTokens returns an account's tokens, newest first.
//
// Revoked tokens are included, because the listing is where an owner sees what was
// revoked and when — and because omitting them would make a token appear to have
// vanished rather than to have been revoked.
func (s *PostgresStore) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+tokenColumns+`
		FROM personal_access_tokens
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list the tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tokens := []Token{}
	for rows.Next() {
		var token Token
		var scopes string
		var lastUsed, revoked sql.NullTime
		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &token.Prefix, &scopes,
			&token.CreatedAt, &lastUsed, &revoked); err != nil {
			return nil, fmt.Errorf("read a token: %w", err)
		}
		token.Scopes = splitScopes(scopes)
		token.LastUsedAt = timeOrNil(lastUsed)
		token.RevokedAt = timeOrNil(revoked)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the tokens: %w", err)
	}
	return tokens, nil
}

// RevokeToken stops one token working.
//
// The update is scoped to the owning account, so a caller cannot revoke a token it does
// not own even if it learns the identifier — the authorization is in the statement rather
// than in a check before it, which is what makes the two impossible to separate.
//
// It reports whether a live token was revoked. Revoking an already-revoked token is not
// an error and reports false, so a retry is safe and a caller can tell the two apart.
func (s *PostgresStore) RevokeToken(ctx context.Context, userID, tokenID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE personal_access_tokens SET revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return false, fmt.Errorf("revoke the token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke the token: %w", err)
	}
	return affected > 0, nil
}

// RevokeTokenByPrefix revokes every live token whose display prefix matches.
//
// It returns the count rather than a boolean, and that is deliberate:
// `administration.md` gives the operator a command that names a token "by its display
// prefix", and a prefix is short enough that two tokens can share one. Reporting the
// count lets the caller refuse an ambiguous prefix instead of revoking a token the
// operator did not mean.
func (s *PostgresStore) RevokeTokenByPrefix(ctx context.Context, prefix string) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE personal_access_tokens SET revoked_at = NOW()
		WHERE token_prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return 0, fmt.Errorf("revoke the token by prefix: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke the token by prefix: %w", err)
	}
	return int(affected), nil
}

// timeOrNil converts a nullable timestamp to a pointer.
func timeOrNil(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
