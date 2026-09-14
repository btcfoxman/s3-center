package directupload

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

const accessPresignTTLSettingKey = "direct_upload.access_presign_ttl_seconds"

// ErrAccessSettingsUnavailable indicates the durable runtime-setting store
// could not be read. Access signing fails closed because silently falling back
// could issue a URL for longer than the operator's current policy.
var ErrAccessSettingsUnavailable = errors.New("access URL expiration setting is unavailable")

// AccessPresignTTL returns the current GET/HEAD signing window and whether it
// came from a management-console override. With no override, the validated
// configuration value is used; older configs default to three hours.
func (m *Manager) AccessPresignTTL(ctx context.Context) (time.Duration, bool, error) {
	fallback := m.cfg.AccessPresignTTL
	if fallback <= 0 {
		fallback = config.DefaultAccessPresignTTL
	}
	if m.settings == nil {
		return fallback, false, nil
	}

	raw, found, err := m.settings.GetRuntimeSetting(ctx, accessPresignTTLSettingKey)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrAccessSettingsUnavailable, err)
	}
	if !found {
		return fallback, false, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: stored value is invalid", ErrAccessSettingsUnavailable)
	}
	minimumSeconds := int64(time.Hour / time.Second)
	maximumSeconds := int64(config.MaxAccessPresignTTL / time.Second)
	if seconds < minimumSeconds || seconds > maximumSeconds || seconds%minimumSeconds != 0 {
		return 0, false, fmt.Errorf("%w: stored value is invalid", ErrAccessSettingsUnavailable)
	}
	ttl := time.Duration(seconds) * time.Second
	return ttl, true, nil
}

// SetAccessPresignTTL persists an hour-granularity signing window. Every
// instance reads this value at request time, so changes take effect without a
// restart and remain consistent across replicas.
func (m *Manager) SetAccessPresignTTL(ctx context.Context, ttl time.Duration) error {
	if err := validateAccessPresignTTL(ttl); err != nil {
		return err
	}
	if m.settings == nil {
		return ErrAccessSettingsUnavailable
	}
	value := strconv.FormatInt(int64(ttl/time.Second), 10)
	if err := m.settings.SetRuntimeSetting(ctx, accessPresignTTLSettingKey, value); err != nil {
		return fmt.Errorf("%w: %v", ErrAccessSettingsUnavailable, err)
	}
	return nil
}

func validateAccessPresignTTL(ttl time.Duration) error {
	if ttl < time.Hour || ttl > config.MaxAccessPresignTTL || ttl%time.Hour != 0 {
		return fmt.Errorf("access URL expiration must be a whole number of hours between 1 and 168")
	}
	return nil
}
