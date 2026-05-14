// Package bridge connects Discord threads to Forge sessions.
package bridge

import (
	"encoding/json"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// ChannelConfig describes a single configured Forge channel.
type ChannelConfig struct {
	ChannelID         string   `json:"channelId"`
	RepoPath          string   `json:"repoPath"`
	DefaultBaseBranch string   `json:"defaultBaseBranch"`
	AllowedUserIDs    []string `json:"allowedUserIds"`
}

// ChannelsConfig is the top-level config file structure.
type ChannelsConfig struct {
	Channels []ChannelConfig `json:"channels"`
}

// Config holds runtime configuration for the bridge.
type Config struct {
	GuildID         string
	ForgeGatewayURL string
	ShowThinking    bool
	RevealSessionID bool
	AdminToken      string

	mu       sync.RWMutex
	channels ChannelsConfig
}

// LoadChannels reads and parses the channels config file.
func (c *Config) LoadChannels(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg ChannelsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	c.mu.Lock()
	c.channels = cfg
	c.mu.Unlock()
	return nil
}

// GetChannelConfig returns the config for a channel, or nil if not configured.
func (c *Config) GetChannelConfig(channelID string) *ChannelConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ch := range c.channels.Channels {
		if ch.ChannelID == channelID {
			cc := ch // copy
			return &cc
		}
	}
	return nil
}

// IsForgeChannel returns true if the channel is a configured Forge channel.
func (c *Config) IsForgeChannel(channelID string) bool {
	return c.GetChannelConfig(channelID) != nil
}

// IsUserAllowed checks if a user is allowed to start tasks in a channel.
func (c *Config) IsUserAllowed(channelID, userID string) bool {
	cc := c.GetChannelConfig(channelID)
	if cc == nil {
		return false
	}
	if cc.AllowedUserIDs == nil {
		return true // nil = anyone allowed
	}
	for _, id := range cc.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// WatchConfig reloads channels config on SIGHUP.
func (c *Config) WatchConfig(path string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			if err := c.LoadChannels(path); err != nil {
				// Log error but don't crash
				continue
			}
		}
	}()
}
