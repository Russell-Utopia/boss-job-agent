package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
	"github.com/Russell-Utopia/boss-job-agent/internal/webui"
)

type assembled struct {
	database *sql.DB
	handler  http.Handler
}

func assemble(ctx context.Context, config Config) (*assembled, error) {
	database, err := storage.Open(ctx, config.DatabasePath)
	if err != nil {
		return nil, err
	}

	pool := jobpool.New()
	settings := automationsettings.New(database, pool)
	assessmentService := assessment.New(database)
	now := config.Now()
	if err := assessmentService.EnsureDefaultPolicy(ctx, now); err != nil {
		return nil, closeDatabaseAfterError(database, err)
	}
	if err := settings.EnsureSafeDefaults(ctx, now); err != nil {
		return nil, closeDatabaseAfterError(database, err)
	}

	resumeVersions := onlineresume.New(database)
	discoveryService := discovery.New(resumeVersions)
	_ = outreach.New()

	return &assembled{
		database: database,
		handler: webui.New(webui.Dependencies{
			Resume:     resumeVersions,
			Discovery:  discoveryService,
			Assessment: assessmentService,
			Settings:   settings,
		}),
	}, nil
}

func closeDatabaseAfterError(database *sql.DB, cause error) error {
	if err := database.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close application database: %w", err))
	}
	return cause
}

func (a *assembled) close() error {
	if err := a.database.Close(); err != nil {
		return fmt.Errorf("close application database: %w", err)
	}
	return nil
}
