package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nbd-wtf/go-nostr"
)

// RoomSpec is one chat or group in persisted preferences (JSON shape unchanged).
type RoomSpec struct {
	Name     string   `json:"name"`
	IsGroup  bool     `json:"is_group"`
	Children []string `json:"children"`
	PoW      int      `json:"pow,omitempty"`
}

type blockedPeer struct {
	PubKey string `json:"pubkey"`
	Nick   string `json:"nick,omitempty"`
}

type textRule struct {
	Pattern string `json:"pattern"`
	Enabled bool   `json:"enabled"`
}

// profile mirrors config.json on disk.
type profile struct {
	PrivateKey             string        `json:"private_key"`
	Nick                   string        `json:"nick,omitempty"`
	Views                  []RoomSpec    `json:"views"`
	ActiveViewName         string        `json:"active_view_name"`
	AnchorRelays           []string      `json:"anchor_relays,omitempty"`
	BlockedUsers           []blockedPeer `json:"blocked_users,omitempty"`
	Filters                []textRule    `json:"filters,omitempty"`
	Mutes                  []textRule    `json:"mutes,omitempty"`
	HistoryLookbackMinutes int           `json:"history_lookback_minutes,omitempty"`
	path                   string        `json:"-"`
}

func loadProfile() (*profile, error) {
	appConfigDir, err := getAppConfigDir()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(appConfigDir, "config.json")
	conf := &profile{path: configPath}

	file, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return seedProfile(configPath)
		}
		return nil, fmt.Errorf("could not open config file: %w", err)
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(conf); err != nil {
		return nil, fmt.Errorf("could not decode config file: %w", err)
	}

	return conf, nil
}

func (p *profile) save() error {
	dirPerm := os.FileMode(0755)
	filePerm := os.FileMode(0644)

	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		dirPerm = 0700
		filePerm = 0600
	}

	if err := os.MkdirAll(filepath.Dir(p.path), dirPerm); err != nil {
		return fmt.Errorf("could not create config directory: %w", err)
	}

	file, err := os.OpenFile(p.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("could not create config file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(p); err != nil {
		return fmt.Errorf("could not encode config file: %w", err)
	}

	return nil
}

func seedProfile(path string) (*profile, error) {
	sk := nostr.GeneratePrivateKey()
	conf := &profile{
		PrivateKey:     sk,
		Views:          []RoomSpec{},
		ActiveViewName: "",
		AnchorRelays:   []string{},
		BlockedUsers:   []blockedPeer{},
		Filters:        []textRule{},
		Mutes:          []textRule{},
		path:           path,
	}
	return conf, conf.save()
}

func getAppConfigDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not get user config directory: %w", err)
	}
	return filepath.Join(configDir, "ephemeral"), nil
}
