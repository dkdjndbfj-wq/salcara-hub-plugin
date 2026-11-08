package standalone

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const deploymentModeFile = "deployment-mode"

// ensureDeploymentMode makes the deployment purpose part of the persistent
// volume contract. A volume containing Station state cannot silently become a
// Bot relay, and a Bot volume cannot be opened by a Station process.
func ensureDeploymentMode(dataDir, mode string) error {
	path := filepath.Join(dataDir, deploymentModeFile)
	b, err := os.ReadFile(path)
	if err == nil {
		if strings.TrimSpace(string(b)) != mode {
			return errors.New("persistent data volume belongs to a different Hub deployment mode")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mode == "bot" {
		if _, statErr := os.Stat(filepath.Join(dataDir, "devices.json")); statErr == nil {
			return errors.New("refusing to switch an existing Station data volume to Bot mode")
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(mode+"\n"), 0o600)
}
