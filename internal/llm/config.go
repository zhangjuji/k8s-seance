package llm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that override the config file, one per field.
const (
	envBaseURL = "SEANCE_LLM_BASE_URL"
	envAPIKey  = "SEANCE_LLM_API_KEY"
	envModel   = "SEANCE_LLM_MODEL"
)

// Config holds the connection settings of an OpenAI-compatible endpoint.
// There are no vendor defaults: every field must come from the user.
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Complete reports whether all three fields are set.
func (c Config) Complete() bool {
	return c.BaseURL != "" && c.APIKey != "" && c.Model != ""
}

// LoadConfig reads the LLM settings. Each field comes from its environment
// variable if that is set and non-empty, otherwise from the llm section of
// ~/.k8s-seance/config.yaml. A missing config file is not an error.
// The result may be incomplete; check Complete.
func LoadConfig() (Config, error) {
	var fileCfg Config
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		fileCfg, err = parseConfigFile(data)
		if err != nil {
			return Config{}, fmt.Errorf("llm: read %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No file: fall through with empty file values.
	default:
		return Config{}, fmt.Errorf("llm: read %s: %w", path, err)
	}

	return Config{
		BaseURL: envOr(envBaseURL, fileCfg.BaseURL),
		APIKey:  envOr(envAPIKey, fileCfg.APIKey),
		Model:   envOr(envModel, fileCfg.Model),
	}, nil
}

// configPath returns ~/.k8s-seance/config.yaml.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("llm: find home directory: %w", err)
	}
	return filepath.Join(home, ".k8s-seance", "config.yaml"), nil
}

// envOr returns the trimmed environment value when non-empty, else fallback.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// parseConfigFile reads the llm section of the config file. It supports only the
// shape shown in design.md: a top-level "llm:" mapping with base_url, api_key and
// model. Other top-level keys are ignored. Values may be bare or quoted, and
// "#" starts a comment outside quotes. This avoids adding a YAML dependency.
func parseConfigFile(data []byte) (Config, error) {
	var cfg Config
	inLLM := false
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		if line[0] != ' ' && line[0] != '\t' {
			// Top-level key: "llm:" opens the section, anything else closes it.
			key, value, ok := splitKeyValue(line)
			if !ok {
				return Config{}, fmt.Errorf("line %d: expected key: value", lineNo)
			}
			inLLM = key == "llm" && value == ""
			continue
		}

		if !inLLM {
			continue
		}
		key, value, ok := splitKeyValue(strings.TrimSpace(line))
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		switch key {
		case "base_url":
			cfg.BaseURL = value
		case "api_key":
			cfg.APIKey = value
		case "model":
			cfg.Model = value
		}
	}
	return cfg, nil
}

// splitKeyValue splits "key: value" and returns the cleaned value.
func splitKeyValue(line string) (key, value string, ok bool) {
	k, v, found := strings.Cut(line, ":")
	k = strings.TrimSpace(k)
	if !found || k == "" || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	return k, parseScalar(strings.TrimSpace(v)), true
}

// parseScalar strips a trailing comment and matching surrounding quotes.
func parseScalar(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
