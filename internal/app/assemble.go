package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	bossadapter "github.com/Russell-Utopia/boss-job-agent/internal/adapters/boss"
	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
	"github.com/Russell-Utopia/boss-job-agent/internal/webui"
)

type assembled struct {
	database  *sql.DB
	handler   http.Handler
	logs      *runlog.Log
	discovery *discovery.Service
}

func assemble(ctx context.Context, config Config) (*assembled, error) {
	logs := openRunlog(config.LogPath)
	database, err := storage.Open(ctx, config.DatabasePath)
	if err != nil {
		return nil, closeRunlogAfterError(logs, err)
	}

	pool := jobpool.New(database)
	settings := automationsettings.New(database, pool)
	assessmentService := assessment.New(database)
	now := config.Now()
	if err := assessmentService.EnsureDefaultPolicy(ctx, now); err != nil {
		return nil, closeApplicationStorageAfterError(database, logs, err)
	}
	if err := settings.EnsureSafeDefaults(ctx, now); err != nil {
		return nil, closeApplicationStorageAfterError(database, logs, err)
	}

	resumeVersions := onlineresume.New(database, bossadapter.NewDefaultOnlineResume(), logs, config.Now)
	discoveryService := discovery.New(
		database,
		resumeVersions,
		pool,
		bossadapter.NewDefaultJobDiscovery(),
		logs,
		config.Now,
	)
	_ = outreach.New()

	return &assembled{
		database:  database,
		discovery: discoveryService,
		handler: webui.New(webui.Dependencies{
			Resume:     resumeVersions,
			Discovery:  discoveryService,
			Jobs:       pool,
			Assessment: assessmentService,
			Settings:   settings,
			Runlog:     logs,
		}),
		logs: logs,
	}, nil
}

func openRunlog(path string) *runlog.Log {
	if path == "" {
		return runlog.OpenDefault()
	}
	return runlog.Open(path)
}

func closeRunlogAfterError(logs *runlog.Log, cause error) error {
	return errors.Join(cause, logs.Close())
}

func closeApplicationStorageAfterError(database *sql.DB, logs *runlog.Log, cause error) error {
	return errors.Join(cause, database.Close(), logs.Close())
}

func (a *assembled) close() error {
	var logErr error
	if a.logs != nil {
		if err := a.logs.Close(); err != nil {
			logErr = fmt.Errorf("close application runlog: %w", err)
		}
	}
	var databaseErr error
	if a.database != nil {
		if err := a.database.Close(); err != nil {
			databaseErr = fmt.Errorf("close application database: %w", err)
		}
	}
	return errors.Join(logErr, databaseErr)
}

func (a *assembled) startBackground(ctx context.Context, recheckInterval time.Duration) func() {
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		a.logs.RunRechecks(ctx, recheckInterval)
	}()
	go func() {
		defer group.Done()
		a.discovery.Run(ctx)
	}()

	return group.Wait
}
