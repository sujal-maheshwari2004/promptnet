// Package store is the durable, ACID prompt store. SQLite (pure-Go driver) so
// the server stays a single binary with zero external dependencies.
// ponytail: SQLite only; add a Postgres impl behind this same API for Phase enterprise.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	_ "modernc.org/sqlite"
)

type Prompt struct {
	URI         string
	Template    string
	Slots       []string
	VersionHash string
}

var ErrNotFound = errors.New("prompt not found")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS prompts(
		uri          TEXT PRIMARY KEY,
		template     TEXT NOT NULL,
		slots        TEXT NOT NULL,
		version_hash TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Hash is the version identity: same content -> same hash -> same cache key.
func Hash(template string, slots []string) string {
	h := sha256.New()
	h.Write([]byte(template))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(slots, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) Put(ctx context.Context, p Prompt) error {
	slots, err := json.Marshal(p.Slots)
	if err != nil {
		return err
	}
	p.VersionHash = Hash(p.Template, p.Slots)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO prompts(uri, template, slots, version_hash) VALUES(?,?,?,?)
		 ON CONFLICT(uri) DO UPDATE SET
		   template=excluded.template, slots=excluded.slots, version_hash=excluded.version_hash`,
		p.URI, p.Template, string(slots), p.VersionHash)
	return err
}

func (s *Store) Get(ctx context.Context, uri string) (Prompt, error) {
	var p Prompt
	var slots string
	err := s.db.QueryRowContext(ctx,
		`SELECT uri, template, slots, version_hash FROM prompts WHERE uri=?`, uri).
		Scan(&p.URI, &p.Template, &slots, &p.VersionHash)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(slots), &p.Slots); err != nil {
		return p, err
	}
	return p, nil
}
