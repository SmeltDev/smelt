package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// FactoryCacheConfig accepts the historical boolean or a cache policy object.
type FactoryCacheConfig struct {
	Enabled *bool    `json:"enabled,omitempty"`
	MaxGB   *float64 `json:"maxGB,omitempty"`
}

// UnmarshalJSON preserves boolean cache configuration compatibility.
func (c *FactoryCacheConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return err
		}
		*c = FactoryCacheConfig{Enabled: &enabled}
		return nil
	}
	type policy FactoryCacheConfig
	return json.Unmarshal(data, (*policy)(c))
}

// DefaultStorageGB is the decimal GB default for cache caps and free disk space.
const DefaultStorageGB = 10

// CacheMaxBytes returns the independent limit for each cache volume.
func (f *FactoryConfig) CacheMaxBytes() int64 {
	if f != nil && f.Cache != nil && f.Cache.MaxGB != nil {
		return int64(*f.Cache.MaxGB * 1e9)
	}
	return DefaultStorageGB * 1e9
}

// MinFreeBytes returns the minimum available space on each storage filesystem.
func (f *FactoryConfig) MinFreeBytes() int64 {
	if f != nil && f.MinFreeGB != nil {
		return int64(*f.MinFreeGB * 1e9)
	}
	return DefaultStorageGB * 1e9
}

func (f *FactoryConfig) validateStorage() error {
	limits := map[string]*float64{"factory.minFreeGB": f.MinFreeGB}
	if f.Cache != nil {
		limits["factory.cache.maxGB"] = f.Cache.MaxGB
	}
	for name, value := range limits {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 1e-9 || *value >= float64(math.MaxInt64)/1e9) {
			return fmt.Errorf("%s must be positive and fit in bytes", name)
		}
	}
	return nil
}
