package config_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

func TestFromLookupAI(t *testing.T) {
	t.Parallel()
	c, err := config.FromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if c.AI.Listen != "127.0.0.1:4600" || c.AI.PublicURL != "" || len(c.AI.Platforms) == 0 ||
		c.AI.TCPListen != "127.0.0.1:4601" || c.AI.TCPDomain != "ai.localhost" || c.AI.TLSDir == "" {
		t.Fatalf("default AI config = %+v", c.AI)
	}

	env := map[string]string{
		"NEBIUS_MOCKER_AI_LISTEN":     "0.0.0.0:4601",
		"NEBIUS_MOCKER_AI_PUBLIC_URL": "https://mocker:4601",
		"NEBIUS_MOCKER_AI_TCP_LISTEN": "0.0.0.0:4602",
		"NEBIUS_MOCKER_AI_TCP_DOMAIN": "ai.example.test",
		"NEBIUS_MOCKER_AI_TLS_DIR":    "/tmp/ca",
		"NEBIUS_MOCKER_AI_PLATFORMS":  `[{"name":"cpu-x","cpu":"arm","presets":[{"name":"2vcpu","vcpus":2}]}]`,
	}
	c, err = config.FromLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if c.AI.Listen != "0.0.0.0:4601" || c.AI.PublicURL != "https://mocker:4601" ||
		c.AI.TCPListen != "0.0.0.0:4602" || c.AI.TCPDomain != "ai.example.test" || c.AI.TLSDir != "/tmp/ca" {
		t.Fatalf("AI config = %+v", c.AI)
	}
	if len(c.AI.Platforms) != 1 || c.AI.Platforms[0].Name != "cpu-x" || c.AI.Platforms[0].Presets[0].VCPUs != 2 {
		t.Fatalf("platforms = %+v", c.AI.Platforms)
	}
}

func TestFromLookupAIPlatformsErrors(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"not json", "[]", `[{"name":"p"}]`, `[{"presets":[{"name":"x"}]}]`} {
		_, err := config.FromLookup(func(k string) (string, bool) {
			return v, k == "NEBIUS_MOCKER_AI_PLATFORMS"
		})
		if err == nil {
			t.Errorf("NEBIUS_MOCKER_AI_PLATFORMS=%q: no error", v)
		}
	}
}
