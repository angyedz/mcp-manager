package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

const ServiceName = "mcp-manager"

// ProjectConfig represents a managed MCP workspace config
type ProjectConfig struct {
	Name       string `mapstructure:"name" yaml:"name"`
	Path       string `mapstructure:"path" yaml:"path"`
	TunnelType string `mapstructure:"tunnel_type" yaml:"tunnel_type"` // "ngrok" or "cloudflare"
}

func getSecretsFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mcp-manager", "secrets.json"), nil
}

func saveSecretToFile(key, val string) error {
	path, err := getSecretsFilePath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	secrets := make(map[string]string)
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err == nil {
			_ = json.Unmarshal(data, &secrets)
		}
	}

	secrets[key] = val
	data, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

func getSecretFromFile(key string) (string, error) {
	path, err := getSecretsFilePath()
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", keyring.ErrNotFound
		}
		return "", err
	}

	secrets := make(map[string]string)
	if err := json.Unmarshal(data, &secrets); err != nil {
		return "", err
	}

	val, ok := secrets[key]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return val, nil
}

// SaveSecret stores a secret in the system keyring, falling back to a local file if keyring fails.
func SaveSecret(key, val string) error {
	err := keyring.Set(ServiceName, key, val)
	if err != nil {
		// Keyring failed (e.g. run via sudo, headless system, or missing dbus)
		return saveSecretToFile(key, val)
	}
	return nil
}

// GetSecret retrieves a secret from the system keyring, falling back to a local file if needed.
func GetSecret(key string) (string, error) {
	val, err := keyring.Get(ServiceName, key)
	if err == nil {
		return val, nil
	}
	fileVal, fileErr := getSecretFromFile(key)
	if fileErr == nil {
		return fileVal, nil
	}
	return "", err
}

// InitConfig initializes Viper configuration
func InitConfig() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configDir := filepath.Join(home, ".config", "mcp-manager")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	viper.AddConfigPath(configDir)
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")

	configPath := filepath.Join(configDir, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Write an empty config file
		f, err := os.Create(configPath)
		if err != nil {
			return err
		}
		_, _ = f.Write([]byte("{}"))
		f.Close()
	}

	return viper.ReadInConfig()
}

// GetProjects retrieves all configured projects
func GetProjects() []ProjectConfig {
	var projects []ProjectConfig
	_ = viper.UnmarshalKey("projects", &projects)
	return projects
}

// SaveProject appends or updates a project config in the YAML file
func SaveProject(p ProjectConfig) error {
	projects := GetProjects()
	updated := false
	for i, existing := range projects {
		if existing.Name == p.Name {
			projects[i] = p
			updated = true
			break
		}
	}
	if !updated {
		projects = append(projects, p)
	}
	viper.Set("projects", projects)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".config", "mcp-manager", "config.yaml")
	return viper.WriteConfigAs(configPath)
}

// DeleteProject removes a project config from the YAML file
func DeleteProject(name string) error {
	projects := GetProjects()
	idx := -1
	for i, p := range projects {
		if p.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("project %s not found", name)
	}
	// Remove the project from slice
	projects = append(projects[:idx], projects[idx+1:]...)
	viper.Set("projects", projects)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".config", "mcp-manager", "config.yaml")
	return viper.WriteConfigAs(configPath)
}

