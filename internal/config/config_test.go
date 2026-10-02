package config

import (
	"os"
	"testing"
)

func TestApplyEnv_Aliases(t *testing.T) {
	os.Setenv("PORT", "19090")
	os.Setenv("HOST", "127.0.0.2")
	os.Setenv("PRISM_COOKIE", "cookie-from-env")
	os.Setenv("PROXY_API_KEY", "sk-proxy-alias")
	os.Setenv("CORS_ORIGIN", "https://example.com")
	os.Setenv("PRISM_MODEL", "gpt-5.6-sol-custom")
	os.Setenv("PRISM_BASE_URL", "https://prism.custom.domain")

	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("HOST")
		os.Unsetenv("PRISM_COOKIE")
		os.Unsetenv("PROXY_API_KEY")
		os.Unsetenv("CORS_ORIGIN")
		os.Unsetenv("PRISM_MODEL")
		os.Unsetenv("PRISM_BASE_URL")
	}()

	cfg := Default()
	applyEnv(cfg)

	if cfg.Server.Port != 19090 {
		t.Errorf("PORT 别名未生效: %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.2" {
		t.Errorf("HOST 别名未生效: %q", cfg.Server.Host)
	}
	if cfg.Upstream.BaseURL != "https://prism.custom.domain" {
		t.Errorf("PRISM_BASE_URL 别名未生效: %q", cfg.Upstream.BaseURL)
	}
	if len(cfg.Facade.APIKeys) != 1 || cfg.Facade.APIKeys[0] != "sk-proxy-alias" {
		t.Errorf("PROXY_API_KEY 别名未生效: %+v", cfg.Facade.APIKeys)
	}
	if cfg.Server.CORSOrigin != "https://example.com" {
		t.Errorf("CORS_ORIGIN 别名未生效: %q", cfg.Server.CORSOrigin)
	}
	if cfg.Facade.DefaultModel != "gpt-5.6-sol-custom" {
		t.Errorf("PRISM_MODEL 别名未生效: %q", cfg.Facade.DefaultModel)
	}
	if len(cfg.Creds.Accounts) == 0 || cfg.Creds.Accounts[0].Cookies != "cookie-from-env" {
		t.Errorf("PRISM_COOKIE 别名未生效: %+v", cfg.Creds.Accounts)
	}
}

func TestLoad_APIKeysFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := dir + "/api-key"
	if err := os.WriteFile(keyFile, []byte("sk-one\r\nsk-two, sk-three\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvPrefix+"API_KEYS_FILE", keyFile)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"sk-one", "sk-two", "sk-three"}
	if len(cfg.Facade.APIKeys) != len(want) {
		t.Fatalf("APIKeys = %+v, want %+v", cfg.Facade.APIKeys, want)
	}
	for i := range want {
		if cfg.Facade.APIKeys[i] != want[i] {
			t.Errorf("APIKeys[%d] = %q, want %q", i, cfg.Facade.APIKeys[i], want[i])
		}
	}

	// 空文件、缺失文件、与 API_KEYS 混用都必须报错，而不是静默关闭鉴权。
	empty := dir + "/empty"
	_ = os.WriteFile(empty, []byte(" \n"), 0o600)
	t.Setenv(EnvPrefix+"API_KEYS_FILE", empty)
	if _, err := Load(""); err == nil {
		t.Error("空密钥文件应当报错")
	}
	t.Setenv(EnvPrefix+"API_KEYS_FILE", dir+"/missing")
	if _, err := Load(""); err == nil {
		t.Error("缺失的密钥文件应当报错")
	}
	t.Setenv(EnvPrefix+"API_KEYS_FILE", keyFile)
	t.Setenv(EnvPrefix+"API_KEYS", "sk-env")
	if _, err := Load(""); err == nil {
		t.Error("API_KEYS 与 API_KEYS_FILE 同时设置应当报错")
	}
}
