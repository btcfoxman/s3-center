package config

import (
	"cmp"
	"fmt"
)

// ManagedBackendsConfig controls database-backed backend definitions created
// from the admin UI. The master key encrypts the complete definition (including
// S3 credentials and proxy authentication) before it reaches the database.
type ManagedBackendsConfig struct {
	Enabled               bool   `yaml:"enabled"`
	MasterKey             string `yaml:"master_key"` //nolint:gosec // operator-provided encryption secret
	DefaultWarningPercent int    `yaml:"default_warning_percent"`
}

func (c *ManagedBackendsConfig) setDefaultsAndValidate() []error {
	c.DefaultWarningPercent = cmp.Or(c.DefaultWarningPercent, 80)
	var errs []error
	if c.Enabled && len(c.MasterKey) < 32 {
		errs = append(errs, fmt.Errorf("managed_backends.master_key must contain at least 32 bytes when managed backends are enabled"))
	}
	if c.DefaultWarningPercent < 50 || c.DefaultWarningPercent > 100 {
		errs = append(errs, fmt.Errorf("managed_backends.default_warning_percent must be between 50 and 100"))
	}
	return errs
}
