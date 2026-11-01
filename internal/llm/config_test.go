package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// setHome points HOME at a temp dir and clears the SEANCE_LLM_* variables.
func setHome(t *testing.T, configYAML string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{envBaseURL, envAPIKey, envModel} {
		t.Setenv(k, "")
	}
	if configYAML == "" {
		return
	}
	dir := filepath.Join(home, ".k8s-seance")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want Config
	}{
		{
			name: "no file and no env is empty, not an error",
			want: Config{},
		},
		{
			name: "file only",
			yaml: "llm:\n  base_url: https://example.invalid/v1\n  api_key: file-key\n  model: file-model\n",
			want: Config{BaseURL: "https://example.invalid/v1", APIKey: "file-key", Model: "file-model"},
		},
		{
			name: "env overrides file per field",
			yaml: "llm:\n  base_url: https://file.invalid/v1\n  api_key: file-key\n  model: file-model\n",
			env:  map[string]string{envBaseURL: "https://env.invalid/v1", envModel: "env-model"},
			want: Config{BaseURL: "https://env.invalid/v1", APIKey: "file-key", Model: "env-model"},
		},
		{
			name: "env without file",
			env:  map[string]string{envBaseURL: "https://env.invalid/v1", envAPIKey: "k", envModel: "m"},
			want: Config{BaseURL: "https://env.invalid/v1", APIKey: "k", Model: "m"},
		},
		{
			name: "blank env value does not override file",
			yaml: "llm:\n  model: file-model\n",
			env:  map[string]string{envModel: "   "},
			want: Config{Model: "file-model"},
		},
		{
			name: "comments, quotes and other sections are handled",
			yaml: "# top comment\nother:\n  base_url: wrong\nllm:\n  base_url: \"https://q.invalid/v1\" # trailing\n  api_key: 'sk-quoted'\n\n  model: m-1\n",
			want: Config{BaseURL: "https://q.invalid/v1", APIKey: "sk-quoted", Model: "m-1"},
		},
		{
			name: "keys outside llm section are ignored",
			yaml: "base_url: wrong\nllm:\n  model: m\nmodel: also-wrong\n",
			want: Config{Model: "m"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHome(t, tt.yaml)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadConfig = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigMalformedFile(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"line without colon", "llm:\n  base_url https://x\n"},
		{"top-level line without colon", "just words\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHome(t, tt.yaml)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig: want error for malformed file, got nil")
			}
		})
	}
}

func TestConfigComplete(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"all set", Config{BaseURL: "b", APIKey: "k", Model: "m"}, true},
		{"missing base URL", Config{APIKey: "k", Model: "m"}, false},
		{"missing key", Config{BaseURL: "b", Model: "m"}, false},
		{"missing model", Config{BaseURL: "b", APIKey: "k"}, false},
		{"empty", Config{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Complete(); got != tt.want {
				t.Errorf("Complete() = %v, want %v", got, tt.want)
			}
		})
	}
}
