package config

import (
	"testing"
)

func TestParseModelMap(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "xopglm51:glm-5.1", map[string]string{"xopglm51": "glm-5.1"}, false},
		{"multiple", "xopglm51:glm-5.1,xopglm52:glm-5.2", map[string]string{"xopglm51": "glm-5.1", "xopglm52": "glm-5.2"}, false},
		{"spaces_trimmed", " xopglm51 : glm-5.1 , xopglm52:glm-5.2 ", map[string]string{"xopglm51": "glm-5.1", "xopglm52": "glm-5.2"}, false},
		{"value_with_colon", "xopglm51:a:b", map[string]string{"xopglm51": "a:b"}, false},
		{"trailing_comma", "xopglm51:glm-5.1,", map[string]string{"xopglm51": "glm-5.1"}, false},
		{"missing_colon", "xopglm51", nil, true},
		{"empty_key", ":glm-5.1", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MODEL_MAP", c.env)
			got, err := parseModelMap("MODEL_MAP")
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; result=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d; got=%v", len(got), len(c.want), got)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("map[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseRewriteMode(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{"empty_defaults_off", "", "off", false},
		{"off", "off", "off", false},
		{"passthrough", "passthrough", "passthrough", false},
		{"default", "default", "default", false},
		{"uppercase_normalized", "DEFAULT", "default", false},
		{"invalid", "rewrite", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MODEL_REWRITE_MODE", c.env)
			got, err := parseRewriteMode("MODEL_REWRITE_MODE")
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestLoadModelRewriteConfig 验证 Load 对 model 改写三配置的解析与校验。
// 必填上游 URL 与改写无关，仅用于满足 Load 的前置条件。
func TestLoadModelRewriteConfig(t *testing.T) {
	t.Setenv("UPSTREAM_OPENAI_URL", "https://openai.example.com")
	t.Setenv("UPSTREAM_ANTHROPIC_URL", "https://anthropic.example.com")

	t.Run("default_off_when_unset", func(t *testing.T) {
		t.Setenv("MODEL_REWRITE_MODE", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModelRewriteMode != "off" {
			t.Errorf("mode = %q, want off", cfg.ModelRewriteMode)
		}
	})

	t.Run("passthrough_with_map", func(t *testing.T) {
		t.Setenv("MODEL_REWRITE_MODE", "passthrough")
		t.Setenv("MODEL_MAP", "xopglm51:glm-5.1")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModelRewriteMode != "passthrough" {
			t.Errorf("mode = %q, want passthrough", cfg.ModelRewriteMode)
		}
		if cfg.ModelMap["xopglm51"] != "glm-5.1" {
			t.Errorf("map = %v, want xopglm51→glm-5.1", cfg.ModelMap)
		}
	})

	t.Run("default_mode_requires_model_default", func(t *testing.T) {
		t.Setenv("MODEL_REWRITE_MODE", "default")
		t.Setenv("MODEL_DEFAULT", "")
		if _, err := Load(); err == nil {
			t.Fatal("expected error when MODEL_DEFAULT missing in default mode")
		}
	})

	t.Run("default_mode_with_default_ok", func(t *testing.T) {
		t.Setenv("MODEL_REWRITE_MODE", "default")
		t.Setenv("MODEL_DEFAULT", "glm-default")
		t.Setenv("MODEL_MAP", "xopglm51:glm-5.1")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModelDefault != "glm-default" {
			t.Errorf("default = %q, want glm-default", cfg.ModelDefault)
		}
	})

	t.Run("invalid_mode_errors", func(t *testing.T) {
		t.Setenv("MODEL_REWRITE_MODE", "bogus")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for invalid mode")
		}
	})
}
