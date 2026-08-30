package onlineresume

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume/internal/sqlitedb"
)

// Versions owns the saved BOSS online resume versions.
type Versions struct {
	queries *sqlitedb.Queries
}

// Version is one immutable saved online resume version.
type Version struct {
	ID        int64     `json:"-"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
}

func New(db *sql.DB) *Versions {
	return &Versions{queries: sqlitedb.New(db)}
}

func (v *Versions) GetCurrent(ctx context.Context) (*Version, error) {
	row, err := v.queries.GetCurrentOnlineResume(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query current online resume: %w", err)
	}
	return &Version{
		ID:        row.ID,
		Version:   int(row.VersionNo),
		CreatedAt: time.UnixMilli(row.CreatedAt),
	}, nil
}
