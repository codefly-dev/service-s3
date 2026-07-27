package main

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func TestRuntimeImagePin(t *testing.T) {
	const want = "cgr.dev/chainguard/minio@sha256:fa23f6a6f62645654530ff94aa077d1cc0d0e44c8f1cce02ab039873612edc72"
	if got := image.FullName(); got != want {
		t.Fatalf("runtime image: got %q, want %q", got, want)
	}
}

// TestNewService_EmbedsBase — wiring smoke test: the Service struct
// must compose services.Base or none of the gRPC plumbing the agent
// runtime relies on works.
func TestNewService_EmbedsBase(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
}

// TestSettings_YAMLRoundTrip — the Settings struct is the public
// interface in service.codefly.yaml; a tag rename here is a breaking
// change for every consumer service that already declared it.
func TestSettings_YAMLRoundTrip(t *testing.T) {
	src := []byte(`
root-user: "ops"
root-password: "supersecret"
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if s.RootUser != "ops" {
		t.Errorf("RootUser: got %q, want %q", s.RootUser, "ops")
	}
	if s.RootPassword != "supersecret" {
		t.Errorf("RootPassword: got %q, want %q", s.RootPassword, "supersecret")
	}
}

// TestLoadConfiguration_DefaultsWhenNil — fresh dev workspaces ship
// with no codefly secret wired; the defaults MUST land minioadmin /
// minioadmin so the FE/api can connect on first `codefly run`.
func TestLoadConfiguration_DefaultsWhenNil(t *testing.T) {
	svc := NewService()
	if err := svc.LoadConfiguration(context.Background(), nil); err != nil {
		t.Fatalf("LoadConfiguration(nil) error: %v", err)
	}
	if svc.rootUser != minioDefaultUser {
		t.Errorf("rootUser: got %q, want %q", svc.rootUser, minioDefaultUser)
	}
	if svc.rootPassword != minioDefaultPassword {
		t.Errorf("rootPassword: got %q, want %q", svc.rootPassword, minioDefaultPassword)
	}
}

// TestLoadConfiguration_OverridesFromSecret — when the operator wires
// MINIO_ROOT_USER + MINIO_ROOT_PASSWORD via codefly secret, those
// MUST take precedence over the defaults.
func TestLoadConfiguration_OverridesFromSecret(t *testing.T) {
	svc := NewService()
	conf := &basev0.Configuration{
		Infos: []*basev0.ConfigurationInformation{
			{
				Name: "s3",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "MINIO_ROOT_USER", Value: "ops"},
					{Key: "MINIO_ROOT_PASSWORD", Value: "supersecret"},
				},
			},
		},
	}
	if err := svc.LoadConfiguration(context.Background(), conf); err != nil {
		t.Fatalf("LoadConfiguration error: %v", err)
	}
	if svc.rootUser != "ops" {
		t.Errorf("rootUser: got %q, want %q", svc.rootUser, "ops")
	}
	if svc.rootPassword != "supersecret" {
		t.Errorf("rootPassword: got %q", svc.rootPassword)
	}
}

// TestBuildConnectionInfo_EmitsStructuredKeys — downstream consumers
// (audit-exporter, app code reading endpoint/access_key/secret_key)
// depend on this exact key shape. Any rename is a breaking change
// for them. Pinning the contract here.
func TestBuildConnectionInfo_EmitsStructuredKeys(t *testing.T) {
	svc := NewService()
	svc.rootUser = "ops"
	svc.rootPassword = "supersecret"

	info := svc.buildConnectionInfo(context.Background(), "127.0.0.1:9000")

	got := map[string]string{}
	for _, kv := range info.ConfigurationValues {
		got[kv.Key] = kv.Value
	}

	want := map[string]string{
		"connection": "s3://ops:supersecret@127.0.0.1:9000",
		"endpoint":   "127.0.0.1:9000",
		"access_key": "ops",
		"secret_key": "supersecret",
		"region":     "us-east-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("conf[%q]: got %q, want %q", k, got[k], v)
		}
	}
}

// TestBuildConnectionInfo_DefaultsAreMinioDefaults — when LoadConfiguration
// hasn't been called or produced no override, the emitted creds match
// the MinIO upstream defaults. This is the dev-mode inner loop: a
// fresh `codefly run` with no secret wired must still produce a
// usable s3 connection.
func TestBuildConnectionInfo_DefaultsAreMinioDefaults(t *testing.T) {
	svc := NewService()
	if err := svc.LoadConfiguration(context.Background(), nil); err != nil {
		t.Fatalf("LoadConfiguration: %v", err)
	}
	info := svc.buildConnectionInfo(context.Background(), "127.0.0.1:9000")

	for _, kv := range info.ConfigurationValues {
		if kv.Key == "access_key" && kv.Value != minioDefaultUser {
			t.Errorf("access_key default: got %q, want %q", kv.Value, minioDefaultUser)
		}
		if kv.Key == "secret_key" && kv.Value != minioDefaultPassword {
			t.Errorf("secret_key default: got %q", kv.Value)
		}
	}
}

// TestBuildConnectionInfo_SecretsAreMarkedSecret — the `connection`,
// `access_key`, and `secret_key` fields contain credentials and must
// be flagged Secret=true so codefly's secret/config flows treat them
// correctly (e.g. don't echo to logs, route to vault). The endpoint
// and region fields are public infrastructure metadata.
func TestBuildConnectionInfo_SecretsAreMarkedSecret(t *testing.T) {
	svc := NewService()
	svc.rootUser = "ops"
	svc.rootPassword = "supersecret"

	info := svc.buildConnectionInfo(context.Background(), "127.0.0.1:9000")

	wantSecret := map[string]bool{
		"connection": true,
		"endpoint":   false,
		"access_key": true,
		"secret_key": true,
		"region":     false,
	}
	for _, kv := range info.ConfigurationValues {
		want, ok := wantSecret[kv.Key]
		if !ok {
			continue
		}
		if kv.Secret != want {
			t.Errorf("conf[%q].Secret: got %v, want %v", kv.Key, kv.Secret, want)
		}
	}
}

// Sanity — the package compiles with the resources import we need
// for the integration test below to build, and the connection
// configuration message uses the proto schema we expect.
var _ = resources.Env
