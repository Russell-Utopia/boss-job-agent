package app

import (
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

const defaultAddress = "127.0.0.1:8080"

type Config struct {
	Address      string
	DatabasePath string
	Now          func() time.Time
}

func DefaultConfig() (Config, error) {
	databasePath, err := storage.DefaultPath()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Address:      defaultAddress,
		DatabasePath: databasePath,
		Now:          time.Now,
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
	if c.Now == nil {
		c.Now = time.Now
	}
	return c, nil
}
