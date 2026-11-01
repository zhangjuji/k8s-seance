package llm

import "testing"

func TestSelectProvider(t *testing.T) {
	complete := Config{BaseURL: "https://example.invalid/v1", APIKey: "k", Model: "m"}
	tests := []struct {
		name     string
		cfg      Config
		mode     string
		env      string
		wantType string // "http" or "mock"
	}{
		{"empty mode, complete config uses http", complete, "", "", "http"},
		{"empty mode, incomplete config uses mock", Config{BaseURL: "x"}, "", "", "mock"},
		{"explicit http with complete config", complete, "http", "", "http"},
		{"explicit http with incomplete config falls back to mock", Config{}, "http", "", "mock"},
		{"explicit mock wins over complete config", complete, "mock", "", "mock"},
		{"replay is mock until task 4.4", complete, "replay", "", "mock"},
		{"unknown mode falls back to mock", complete, "bogus", "", "mock"},
		{"flag beats env", complete, "mock", "http", "mock"},
		{"env used when flag empty", complete, "", "mock", "mock"},
		{"env http with complete config", complete, "", "http", "http"},
		{"mode is case and space insensitive", complete, "  HTTP ", "", "http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SEANCE_LLM_MODE", tt.env)
			got := SelectProvider(tt.cfg, tt.mode)
			var gotType string
			switch p := got.(type) {
			case HTTPProvider:
				gotType = "http"
				if p.Config != tt.cfg {
					t.Errorf("HTTPProvider config = %+v, want %+v", p.Config, tt.cfg)
				}
			case MockProvider:
				gotType = "mock"
			default:
				t.Fatalf("unexpected provider type %T", got)
			}
			if gotType != tt.wantType {
				t.Errorf("SelectProvider(%+v, %q) = %s, want %s", tt.cfg, tt.mode, gotType, tt.wantType)
			}
		})
	}
}
