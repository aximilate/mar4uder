package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// Config represents the server configuration
type Config struct {
	HTTPAddr             string `json:"http_addr"`
	UDPAddr              string `json:"udp_addr"`
	TCPAddr              string `json:"tcp_addr"`
	OperatorAddr         string `json:"operator_addr"`
	RFBAddr              string `json:"rfb_addr"`
	AuthToken            string `json:"auth_token"`
	AdminPassword        string `json:"admin_password"`
	NodeOnlineTimeoutSec int    `json:"node_online_timeout_sec"`
}

// CheckPassword checks if provided password matches AdminPassword or AuthToken
func (c *Config) CheckPassword(pass string) bool {
	if pass == "" {
		return false
	}
	if c.AdminPassword != "" && pass == c.AdminPassword {
		return true
	}
	if c.AuthToken != "" && pass == c.AuthToken {
		return true
	}
	return false
}

// GetAdminPassword returns AdminPassword or falls back to AuthToken
func (c *Config) GetAdminPassword() string {
	if c.AdminPassword != "" {
		return c.AdminPassword
	}
	if c.AuthToken != "" {
		return c.AuthToken
	}
	return "mar4uder"
}

// ServerStats tracks runtime telemetry
type ServerStats struct {
	StartTime  time.Time    `json:"start_time"`
	InPackets  atomic.Int64 `json:"in_packets"`
	OutPackets atomic.Int64 `json:"out_packets"`
	InBytes    atomic.Int64 `json:"in_bytes"`
	OutBytes   atomic.Int64 `json:"out_bytes"`
}

// DefaultConfig returns default configuration settings
func DefaultConfig() *Config {
	return &Config{
		HTTPAddr:             "0.0.0.0:8080",
		UDPAddr:              "0.0.0.0:443",
		TCPAddr:              "0.0.0.0:443",
		OperatorAddr:         "0.0.0.0:9000",
		RFBAddr:              "0.0.0.0:5900",
		AuthToken:            GenerateRandomToken(16),
		AdminPassword:        "mar4uder",
		NodeOnlineTimeoutSec: 15,
	}
}

// GenerateRandomToken generates a cryptographically secure hex string
func GenerateRandomToken(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "mar4uder-secret-token"
	}
	return hex.EncodeToString(b)
}

// LoadConfig loads configuration from a JSON file, creating it if missing
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			_ = SaveConfig(path, cfg)
			return cfg, nil
		}
		return nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveConfig saves configuration to a JSON file
func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
