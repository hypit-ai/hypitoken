package config

import "testing"

func TestGPTPayDefaultsAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		enabled    bool
		port       int
	}{
		{"absent", "", false, 8321},
		{"explicit", "endpoints:\n  gptpay:\n    port: 8420\n", true, 8420},
		{"disabled", "endpoints:\n  gptpay:\n    port: 8320\n    disabled: true\n", false, 8320},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			ep := cfg.Endpoints.GPTPay
			if ep.IsEnabled() != tc.enabled || ep.Port != tc.port || ep.Host != "127.0.0.1" {
				t.Fatalf("unexpected GPTPay config: %+v", ep)
			}
		})
	}
}

func TestGPTPayRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"-1", "65536"} {
		if _, err := loadYAML(t, "endpoints:\n  gptpay:\n    port: "+port+"\n"); err == nil {
			t.Fatalf("accepted invalid port %s", port)
		}
	}
}
