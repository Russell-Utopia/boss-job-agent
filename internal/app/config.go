package app

import (
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

const defaultAddress = "127.0.0.1:8080"
const defaultRunlogRecheckInterval = time.Minute

type Config struct {
	Address               string
	DatabasePath          string
	LogPath               string
	RunlogRecheckInterval time.Duration
	Now                   func() time.Time
}

func DefaultConfig() (Config, error) {
	databasePath, err := storage.DefaultPath()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Address:               defaultAddress,
		DatabasePath:          databasePath,
		RunlogRecheckInterval: defaultRunlogRecheckInterval,
		Now:                   time.Now,
	}, nil
}

func (c Config) withDefaults() (Config, error) {
	if c.Address == "" {
		c.Address = defaultAddress
	}
	if c.DatabasePath == "" {
		defaults, err := DefaultConfig()
		if err != nil {
			return Config{}, err
		}
		c.DatabasePath = defaults.DatabasePath
	}
	if c.RunlogRecheckInterval <= 0 {
		c.RunlogRecheckInterval = defaultRunlogRecheckInterval
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c, nil
}
