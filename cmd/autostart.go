package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type autoStartSpec struct {
	Binary    string `json:"binary"`
	Port      string `json:"port"`
	ConfigDir string `json:"config_dir"`
	DataDir   string `json:"data_dir"`
}

func autoStartManifestPath(dataDir string) string {
	return filepath.Join(dataDir, "run", "autostart.json")
}

func loadAutoStartManifest(dataDir string) (autoStartSpec, error) {
	b, err := os.ReadFile(autoStartManifestPath(dataDir))
	if err != nil {
		return autoStartSpec{}, err
	}
	var spec autoStartSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return autoStartSpec{}, err
	}
	return spec, nil
}

func autoStartPort(dataDir string) string {
	spec, err := loadAutoStartManifest(dataDir)
	if err != nil {
		return ""
	}
	return spec.Port
}

func ensureAutoStart(dataDir string, spec autoStartSpec) error {
	current, err := loadAutoStartManifest(dataDir)
	if err == nil && current.Port != spec.Port {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(autoStartManifestPath(dataDir))
	}
	return setAutoStart(dataDir, true, spec)
}

func setAutoStart(dataDir string, enabled bool, spec autoStartSpec) error {
	if !enabled {
		if err := removeAutoStart(); err != nil {
			return err
		}
		if err := os.Remove(autoStartManifestPath(dataDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := installAutoStart(spec); err != nil {
		return err
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(autoStartManifestPath(dataDir), append(b, '\n'), 0o600)
}
