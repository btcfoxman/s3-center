package config

import (
	"fmt"
	"strings"
	"time"
)

// DirectUploadConfig controls browser-to-backend uploads. Tokens are read from
// environment-expanded YAML and are never returned by the public API.
type DirectUploadConfig struct {
	Enabled        bool                `yaml:"enabled"`
	PresignTTL     time.Duration       `yaml:"presign_ttl"`
	SessionTTL     time.Duration       `yaml:"session_ttl"`
	ReaperInterval time.Duration       `yaml:"reaper_interval"`
	MaxObjectSize  int64               `yaml:"max_object_size"`
	ConsoleHost    string              `yaml:"console_host"`
	Tokens         []DirectUploadToken `yaml:"tokens"`
}

type DirectUploadToken struct {
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Scopes []string `yaml:"scopes"`
}

func (c *DirectUploadConfig) setDefaultsAndValidate() []error {
	if !c.Enabled {
		return nil
	}
	if c.PresignTTL <= 0 {
		c.PresignTTL = 10 * time.Minute
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = 10 * time.Minute
	}
	if c.ReaperInterval <= 0 {
		c.ReaperInterval = time.Minute
	}
	if c.MaxObjectSize <= 0 {
		c.MaxObjectSize = 256 * 1024 * 1024
	}
	var errs []error
	if c.PresignTTL > c.SessionTTL {
		errs = append(errs, fmt.Errorf("direct_upload.presign_ttl must not exceed session_ttl"))
	}
	if len(c.Tokens) == 0 {
		errs = append(errs, fmt.Errorf("direct_upload.tokens requires at least one token"))
	}
	seen := make(map[string]struct{}, len(c.Tokens))
	for i, tok := range c.Tokens {
		if strings.TrimSpace(tok.Name) == "" || strings.TrimSpace(tok.Token) == "" {
			errs = append(errs, fmt.Errorf("direct_upload.tokens[%d] requires name and token", i))
		}
		if _, ok := seen[tok.Name]; ok {
			errs = append(errs, fmt.Errorf("direct_upload token name %q is duplicated", tok.Name))
		}
		seen[tok.Name] = struct{}{}
		if len(tok.Scopes) == 0 {
			c.Tokens[i].Scopes = []string{"uploads:write", "uploads:read"}
		}
	}
	return errs
}
