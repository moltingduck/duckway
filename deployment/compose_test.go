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

func TestDockerBuildContextExcludesProductionSecrets(t *testing.T) {
	body, err := os.ReadFile("../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	for _, pattern := range []string{".prod.env", ".secrets", "backups", "live-credentials", "secrets", "test_auth*.json", "auth.json"} {
		if !slices.Contains(lines, pattern) {
			t.Errorf(".dockerignore does not exclude %q", pattern)
		}
	}
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

func TestTailscaleServeConfigsForwardTailnetTCPToLoopback(t *testing.T) {
	configs := map[string]string{
		"../tailscale/ts-combined.json": "127.0.0.1:8080",
		"../tailscale/ts-admin.json":    "127.0.0.1:9091",
		"../tailscale/ts-gateway.json":  "127.0.0.1:8080",
	}
	for path, target := range configs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		config := string(body)
		for _, want := range []string{`"80"`, `"TCPForward"`, target} {
			if !strings.Contains(config, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		for _, forbidden := range []string{`"HTTP"`, `"HTTPS"`, `"Web"`, "TS_CERT_DOMAIN"} {
			if strings.Contains(config, forbidden) {
				t.Errorf("%s must not depend on HTTP host or certificate matching (%s)", path, forbidden)
			}
		}
	}
}

func TestPostgresComposeIsPrivateAndUsesSecrets(t *testing.T) {
	body, err := os.ReadFile("../docker-compose.postgres.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Ports     []string                     `yaml:"ports"`
			Secrets   []string                     `yaml:"secrets"`
			Volumes   []string                     `yaml:"volumes"`
			DependsOn map[string]composeDependency `yaml:"depends_on"`
		} `yaml:"services"`
		Secrets map[string]yaml.Node `yaml:"secrets"`
		Volumes map[string]yaml.Node `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(body, &compose); err != nil {
		t.Fatal(err)
	}
	postgres, ok := compose.Services["postgres"]
	if !ok {
		t.Fatal("missing PostgreSQL service")
	}
	if len(postgres.Ports) != 0 {
		t.Fatal("PostgreSQL must not publish a host port")
	}
	if !slices.Contains(postgres.Secrets, "postgres_password") || !slices.Contains(postgres.Volumes, "duckway-postgres-data:/var/lib/postgresql/data") {
		t.Error("PostgreSQL must use the password secret and dedicated volume")
	}
	if _, ok := compose.Secrets["postgres_password"]; !ok {
		t.Error("postgres_password secret is not declared")
	}
	if _, ok := compose.Volumes["duckway-postgres-data"]; !ok {
		t.Error("duckway-postgres-data volume is not declared")
	}
	for _, name := range []string{"admin-prod", "gateway-prod", "admin-tailscale", "gateway-tailscale"} {
		service, ok := compose.Services[name]
		dependency, depends := service.DependsOn["postgres"]
		if !ok || !slices.Contains(service.Secrets, "postgres_password") || !depends || dependency.Condition != "service_healthy" {
			t.Errorf("%s is not wired to PostgreSQL secret and health dependency", name)
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
