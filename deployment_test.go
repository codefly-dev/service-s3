package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)
	secret, err := os.ReadFile(filepath.Join(destination, "overlays", "test", "secret.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret), "kind: Secret") || !strings.Contains(string(secret), "c2VjcmV0") {
		t.Fatalf("ephemeral profile did not render populated Secret:\n%s", secret)
	}
}

func TestPromotableGitOpsDeployment(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "s3",
		Version:   "1.2.3",
	}
	if err := builder.HeadlessLoad(ctx, identity); err != nil {
		t.Fatal(err)
	}
	builder.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("s3.example", 9000)
	instance.Access = resources.NewPublicNetworkAccess()
	destination := t.TempDir()

	response, err := builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "test"},
		NetworkMappings: []*basev0.NetworkMapping{
			{
				Endpoint: builder.TcpEndpoint,
				Instances: []*basev0.NetworkInstance{
					instance,
				},
			},
		},
		Deployment: &builderv0.Deployment{
			Kind: &builderv0.Deployment_Kubernetes{
				Kubernetes: &builderv0.KubernetesDeployment{
					Namespace:   "codefly-test",
					Destination: destination,
					Profile:     builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
					SecretReferences: map[string]*builderv0.KubernetesSecretKeyReference{
						"CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER": {
							Name: "s3-credentials",
							Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER",
						},
						"CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD": {
							Name: "s3-credentials",
							Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD",
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}
	output := response.GetDeployment().GetKubernetes()
	if output.GetProfile() != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		t.Fatalf("output profile: got %s", output.GetProfile())
	}
	if output.GetContractVersion() != services.KubernetesManifestContractVersion {
		t.Fatalf("contract version: got %q", output.GetContractVersion())
	}
	if output.GetValidation().GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("static validation: %v", output.GetValidation().GetViolations())
	}
	if !output.GetValidation().GetPromotable() {
		t.Fatalf("deployment is not promotable: %v", output.GetValidation().GetViolations())
	}

	for _, relative := range []string{
		filepath.Join("base", "namespace.yaml"),
		filepath.Join("overlays", "test", "secret.yaml"),
	} {
		content, readErr := os.ReadFile(filepath.Join(destination, relative))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(string(content)) != "" {
			t.Fatalf("%s must be empty for GitOps:\n%s", relative, content)
		}
	}
	statefulSet, err := os.ReadFile(filepath.Join(destination, "base", "stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(statefulSet)
	for _, expected := range []string{
		image.FullName(),
		"name: MINIO_ROOT_USER",
		"name: MINIO_ROOT_PASSWORD",
		"name: s3-credentials",
		"key: CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER",
		"key: CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD",
	} {
		if !strings.Contains(manifest, expected) {
			t.Errorf("StatefulSet missing %q:\n%s", expected, manifest)
		}
	}
	if strings.Contains(manifest, "envFrom:") {
		t.Fatalf("GitOps StatefulSet uses ephemeral Secret envFrom:\n%s", manifest)
	}
}

func TestPromotableGitOpsDeploymentRequiresCredentialReferences(t *testing.T) {
	validReferences := func() map[string]*builderv0.KubernetesSecretKeyReference {
		return map[string]*builderv0.KubernetesSecretKeyReference{
			"CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER": {
				Name: "s3-credentials",
				Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER",
			},
			"CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD": {
				Name: "s3-credentials",
				Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD",
			},
		}
	}
	tests := []struct {
		name       string
		references func() map[string]*builderv0.KubernetesSecretKeyReference
		wantError  string
	}{
		{
			name:       "no references",
			references: func() map[string]*builderv0.KubernetesSecretKeyReference { return nil },
			wantError:  `requires exactly two canonical MinIO credential references`,
		},
		{
			name: "missing root user",
			references: func() map[string]*builderv0.KubernetesSecretKeyReference {
				references := validReferences()
				delete(references, "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER")
				return references
			},
			wantError: `requires exactly two canonical MinIO credential references`,
		},
		{
			name: "missing root password",
			references: func() map[string]*builderv0.KubernetesSecretKeyReference {
				references := validReferences()
				delete(references, "CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD")
				return references
			},
			wantError: `requires exactly two canonical MinIO credential references`,
		},
		{
			name: "optional root user",
			references: func() map[string]*builderv0.KubernetesSecretKeyReference {
				references := validReferences()
				references["CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_USER"].Optional = true
				return references
			},
			wantError: `"MINIO_ROOT_USER" secret reference must not be optional`,
		},
		{
			name: "optional root password",
			references: func() map[string]*builderv0.KubernetesSecretKeyReference {
				references := validReferences()
				references["CODEFLY__SERVICE_SECRET_CONFIGURATION__MODULE__S3__S3__MINIO_ROOT_PASSWORD"].Optional = true
				return references
			},
			wantError: `"MINIO_ROOT_PASSWORD" secret reference must not be optional`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := NewBuilder()
			identity := &resources.ServiceIdentity{
				Workspace: "workspace",
				Module:    "module",
				Name:      "s3",
				Version:   "1.2.3",
			}
			builder.Identity = identity
			builder.Information = &services.Information{
				Service: resources.ToServiceWithCase(identity),
				Module:  resources.ToModuleWithCase(identity),
			}
			builder.EnvironmentVariables.SetIdentity(&basev0.ServiceIdentity{
				Workspace: identity.Workspace,
				Module:    identity.Module,
				Name:      identity.Name,
				Version:   identity.Version,
			})
			builder.TcpEndpoint = &basev0.Endpoint{
				Name:    "tcp",
				Module:  identity.Module,
				Service: identity.Name,
				Api:     "tcp",
			}
			instance := resources.NewNetworkInstance("s3.example", 9000)
			instance.Access = resources.NewPublicNetworkAccess()
			destination := t.TempDir()

			response, err := builder.Deploy(context.Background(), &builderv0.DeploymentRequest{
				Environment: &basev0.Environment{Name: "test"},
				NetworkMappings: []*basev0.NetworkMapping{
					{
						Endpoint:  builder.TcpEndpoint,
						Instances: []*basev0.NetworkInstance{instance},
					},
				},
				Deployment: &builderv0.Deployment{
					Kind: &builderv0.Deployment_Kubernetes{
						Kubernetes: &builderv0.KubernetesDeployment{
							Namespace:        "codefly-test",
							Destination:      destination,
							Profile:          builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
							SecretReferences: test.references(),
						},
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.GetState().GetState() != builderv0.DeploymentStatus_ERROR {
				t.Fatalf("deployment status: got %s, want %s", response.GetState().GetState(), builderv0.DeploymentStatus_ERROR)
			}
			if !strings.Contains(response.GetState().GetMessage(), test.wantError) {
				t.Fatalf("deployment error: got %q, want it to contain %q", response.GetState().GetMessage(), test.wantError)
			}
			entries, err := os.ReadDir(destination)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("deployment wrote manifests before rejecting invalid credentials: %v", entries)
			}
		})
	}
}
