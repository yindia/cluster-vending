package config

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/yindia/cluster-vending/pkg/database"
)

// Flag names and environment variables of the database settings.
const (
	FlagDatabaseDriver          = "database-driver"
	EnvDatabaseDriver           = "CV_DATABASE_DRIVER"
	FlagDatabaseDSN             = "database-dsn"
	EnvDatabaseDSN              = "CV_DATABASE_DSN"
	FlagDatabaseMaxOpenConns    = "database-max-open-conns"
	EnvDatabaseMaxOpenConns     = "CV_DATABASE_MAX_OPEN_CONNS"
	FlagDatabaseMaxIdleConns    = "database-max-idle-conns"
	EnvDatabaseMaxIdleConns     = "CV_DATABASE_MAX_IDLE_CONNS"
	FlagDatabaseConnMaxLifetime = "database-conn-max-lifetime"
	EnvDatabaseConnMaxLifetime  = "CV_DATABASE_CONN_MAX_LIFETIME"
)

var databaseSettings = []Setting{
	{Flag: FlagDatabaseDriver, Env: EnvDatabaseDriver, Default: database.DriverSQLite,
		Usage: "database driver: sqlite (local file) or postgres (production)"},
	{Flag: FlagDatabaseDSN, Env: EnvDatabaseDSN, Default: database.DefaultSQLiteFile,
		Usage: "SQLite file path, or PostgreSQL connection string (URL or key=value); keep passwords in the environment, not the config file"},
	{Flag: FlagDatabaseMaxOpenConns, Env: EnvDatabaseMaxOpenConns, Default: "0",
		Usage: "maximum open connections; 0 means unlimited"},
	{Flag: FlagDatabaseMaxIdleConns, Env: EnvDatabaseMaxIdleConns, Default: "0",
		Usage: "maximum idle connections; 0 means the database/sql default (2)"},
	{Flag: FlagDatabaseConnMaxLifetime, Env: EnvDatabaseConnMaxLifetime, Default: "0s",
		Usage: "maximum lifetime of a connection, e.g. 30m; 0s means no limit"},
}

var databaseDrivers = []string{database.DriverSQLite, database.DriverPostgres}

// loadDatabase parses the resolved database settings. Errors name the
// setting, like every other configuration error.
func loadDatabase(v map[string]string) (database.Config, []error) {
	var errs []error
	cfg := database.Config{
		Driver: v[FlagDatabaseDriver],
		DSN:    v[FlagDatabaseDSN],
	}
	if cfg.Driver != "" && !slices.Contains(databaseDrivers, cfg.Driver) {
		errs = append(errs, fmt.Errorf("%s must be one of %v, got %q", setting(FlagDatabaseDriver), databaseDrivers, cfg.Driver))
	}

	var err error
	if cfg.MaxOpenConns, err = nonNegativeInt(FlagDatabaseMaxOpenConns, v); err != nil {
		errs = append(errs, err)
	}
	if cfg.MaxIdleConns, err = nonNegativeInt(FlagDatabaseMaxIdleConns, v); err != nil {
		errs = append(errs, err)
	}
	if raw := v[FlagDatabaseConnMaxLifetime]; raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", setting(FlagDatabaseConnMaxLifetime), err))
		case d < 0:
			errs = append(errs, fmt.Errorf("%s must not be negative, got %s", setting(FlagDatabaseConnMaxLifetime), raw))
		default:
			cfg.ConnMaxLifetime = d
		}
	}
	return cfg, errs
}

func nonNegativeInt(flag string, v map[string]string) (int, error) {
	raw := v[flag]
	if raw == "" {
		return 0, nil // already reported as set but empty
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number, got %q", setting(flag), raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %d", setting(flag), n)
	}
	return n, nil
}
