package deployment

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeDependency struct {
	Condition string `yaml:"condition"`
	Restart   bool   `yaml:"restart"`
}

type composeHealthcheck struct {
	Test []string `yaml:"test"`
}

type composeService struct {
	Environment []string                     `yaml:"environment"`
	Volumes     []string                     `yaml:"volumes"`
	Devices     []string                     `yaml:"devices"`
	CapAdd      []string                     `yaml:"cap_add"`
	Ports       []string                     `yaml:"ports"`
	Networks    []string                     `yaml:"networks"`
	Profiles    []string                     `yaml:"profiles"`
	NetworkMode string                       `yaml:"network_mode"`
	DependsOn   map[string]composeDependency `yaml:"depends_on"`
	Healthcheck composeHealthcheck           `yaml:"healthcheck"`
}

func TestProductionTailscaleProfilesUseUserspaceServe(t *testing.T) {
	body, err := os.ReadFile("../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]composeService `yaml:"services"`
		Networks map[string]any            `yaml:"networks"`
	}
	if err := yaml.Unmarshal(body, &compose); err != nil {
		t.Fatal(err)
	}

	sidecars := map[string]struct {
		network, serveConfig string
	}{
		"tailscale-server":  {"tailscale-server-net", "TS_SERVE_CONFIG=/config/ts-combined.json"},
		"tailscale-admin":   {"tailscale-admin-net", "TS_SERVE_CONFIG=/config/ts-admin.json"},
		"tailscale-gateway": {"tailscale-gateway-net", "TS_SERVE_CONFIG=/config/ts-gateway.json"},
	}
	for name, want := range sidecars {
		service := requireService(t, compose.Services, name)
		for _, environment := range []string{"TS_USERSPACE=true", "TS_ENABLE_HEALTH_CHECK=true", want.serveConfig} {
			if !slices.Contains(service.Environment, environment) {
				t.Errorf("%s missing environment %q", name, environment)
			}
		}
		if len(service.Devices) != 0 || len(service.CapAdd) != 0 || len(service.Ports) != 0 {
			t.Errorf("%s must remain unprivileged and publish no ports", name)
		}
		if len(service.Networks) != 1 || service.Networks[0] != want.network {
			t.Errorf("%s networks=%v, want only %s", name, service.Networks, want.network)
		}
		if _, ok := compose.Networks[want.network]; !ok {
			t.Errorf("top-level network %s is not declared", want.network)
		}
		if !slices.Contains(service.Volumes, "./tailscale:/config:ro") {
			t.Errorf("%s does not mount its Serve config", name)
		}
		if !slices.Contains(service.Healthcheck.Test, "http://127.0.0.1:9002/healthz") {
			t.Errorf("%s does not check Tailscale readiness", name)
		}
	}

	apps := map[string]struct {
		sidecar, listen, profile string
	}{
		"server-tailscale":  {"tailscale-server", "DUCKWAY_LISTEN=127.0.0.1:8080", "tailscale-combined"},
		"admin-tailscale":   {"tailscale-admin", "DUCKWAY_ADMIN_LISTEN=127.0.0.1:9091", "tailscale"},
		"gateway-tailscale": {"tailscale-gateway", "DUCKWAY_GATEWAY_LISTEN=127.0.0.1:8080", "tailscale"},
	}
	for name, want := range apps {
		service := requireService(t, compose.Services, name)
		if service.NetworkMode != "service:"+want.sidecar {
			t.Errorf("%s network_mode=%q, want service:%s", name, service.NetworkMode, want.sidecar)
		}
		if !slices.Contains(service.Environment, want.listen) {
			t.Errorf("%s must listen only on loopback (%s)", name, want.listen)
		}
		if len(service.Ports) != 0 || len(service.Networks) != 0 {
			t.Errorf("%s must not publish ports or join an additional network", name)
		}
		if !slices.Equal(service.Profiles, []string{want.profile}) {
			t.Errorf("%s profiles=%v, want only %s", name, service.Profiles, want.profile)
		}
		dependency, ok := service.DependsOn[want.sidecar]
		if !ok || dependency.Condition != "service_healthy" || !dependency.Restart {
			t.Errorf("%s must wait for and restart with %s", name, want.sidecar)
		}
	}
}

func TestTailscaleServeConfigsExposeHTTPToLoopbackOnly(t *testing.T) {
	configs := map[string]string{
		"../tailscale/ts-combined.json": "http://127.0.0.1:8080",
		"../tailscale/ts-admin.json":    "http://127.0.0.1:9091",
		"../tailscale/ts-gateway.json":  "http://127.0.0.1:8080",
	}
	for path, target := range configs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		config := string(body)
		for _, want := range []string{`"80"`, `"HTTP": true`, target} {
			if !strings.Contains(config, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		if strings.Contains(config, `"HTTPS": true`) {
			t.Errorf("%s must not require a Tailscale HTTPS certificate", path)
		}
	}
}

func requireService(t *testing.T, services map[string]composeService, name string) composeService {
	t.Helper()
	service, ok := services[name]
	if !ok {
		t.Fatalf("missing compose service %q", name)
	}
	return service
}
