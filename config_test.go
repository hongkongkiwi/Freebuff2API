package main

import (
	"strings"
	"testing"
)

func TestOverrideIntValidation(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		want      int
		wantError string
	}{
		{"empty is no-op", "", 42, ""},
		{"valid applies", "7", 7, ""},
		{"zero applies", "0", 0, ""},
		{"non-numeric fails", "abc", 42, "must be an integer"},
		{"negative fails", "-1", 42, "cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_OVERRIDE_INT", tc.value)
			target := 42
			err := overrideInt(&target, "TEST_OVERRIDE_INT")
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if target != tc.want {
					t.Fatalf("target = %d, want %d", target, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantError)
			}
			if target != 42 {
				t.Fatalf("failed override must not modify target: %d", target)
			}
		})
	}
}

func TestOverrideBoolValidation(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		want      bool
		wantError string
	}{
		{"empty is no-op", "", true, ""},
		{"true applies", "true", true, ""},
		{"false applies", "false", false, ""},
		{"one applies", "1", true, ""},
		{"invalid fails", "yes", true, "must be a boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_OVERRIDE_BOOL", tc.value)
			target := true
			err := overrideBool(&target, "TEST_OVERRIDE_BOOL")
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if target != tc.want {
					t.Fatalf("target = %v, want %v", target, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantError)
			}
		})
	}
}

func TestLoadConfigFailsFastOnMalformedEnv(t *testing.T) {
	t.Setenv("AUTH_TOKENS", "token1")
	t.Setenv("TOOL_CAMOUFLAGE", "yes")

	_, err := loadConfig("")
	if err == nil || !strings.Contains(err.Error(), "TOOL_CAMOUFLAGE") {
		t.Fatalf("err = %v, want a TOOL_CAMOUFLAGE validation error", err)
	}
}

func TestLoadConfigEnvOverrideApplies(t *testing.T) {
	t.Setenv("AUTH_TOKENS", "token1,token2")
	t.Setenv("MAX_REQUEST_BODY_MB", "16")
	t.Setenv("FORCE_UPSTREAM_STREAM", "false")

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.AuthTokens) != 2 {
		t.Fatalf("auth tokens = %v", cfg.AuthTokens)
	}
	if cfg.MaxRequestBodyMB != 16 {
		t.Fatalf("max body MB = %d, want 16", cfg.MaxRequestBodyMB)
	}
	if cfg.ForceUpstreamStream {
		t.Fatal("FORCE_UPSTREAM_STREAM=false was not applied")
	}
}
