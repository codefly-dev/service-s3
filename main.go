package main

import (
	"context"
	"embed"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

// Settings — MinIO root credentials. Both default to "minioadmin"
// (the upstream MinIO default) so a brand-new dev workspace just
// works; production deployments override via codefly secret.
//
// MinIO refuses to start if root-password is shorter than 8 chars,
// so the validation lives at start time (Init) rather than here —
// keeping Settings unmarshal lossless for round-tripping.
type Settings struct {
	RootUser     string `yaml:"root-user"`
	RootPassword string `yaml:"root-password"`
}

// image — digest-pinned MinIO release. NEVER :latest (project policy):
// the bucket layout, IAM behaviour, and admin endpoints can shift
// between releases; an unpinned tag means a re-pull silently changes
// behaviour. Bump deliberately when upgrading.
//
// MinIO is the local S3-compatible store; same API surface as AWS
// S3, R2, GCS-S3-mode — production swaps the endpoint env without
// touching application code.
var image = &resources.DockerImage{
	Repository: "cgr.dev/chainguard",
	Name:       "minio",
	Tag:        "RELEASE.2026-07-17T12-07-51Z",
	Digest:     "sha256:fa23f6a6f62645654530ff94aa077d1cc0d0e44c8f1cce02ab039873612edc72",
}

type Service struct {
	*services.Base

	// Settings
	*Settings

	// Resolved MinIO root creds — pulled from configuration via
	// LoadConfiguration. Defaults to MinIO's upstream default
	// ("minioadmin" / "minioadmin") so dev workspaces work with no
	// codefly secret wired. Production overrides through the standard
	// configuration flow.
	rootUser     string
	rootPassword string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Nix:    true,
			Docker: true,
		},
		Config: []*agentv0.ConfigurationValueDetail{
			{
				Name: "s3", Description: "s3 connection details",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "s3:// URL form (legacy)"},
					{Name: "endpoint", Description: "host:port (no scheme)"},
					{Name: "access_key", Description: "MinIO root user / S3 access key id"},
					{Name: "secret_key", Description: "MinIO root password / S3 secret"},
					{Name: "region", Description: "AWS-style region (defaults to us-east-1; MinIO ignores)"},
				},
			},
		},
		ReadMe: readme,
	}.Build(), nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

// minioDefaultUser / minioDefaultPassword — the upstream MinIO
// defaults. Used when no codefly secret is configured so a fresh dev
// workspace works on first `codefly run`.
const (
	minioDefaultUser     = "minioadmin"
	minioDefaultPassword = "minioadmin"
)

// LoadConfiguration pulls MINIO_ROOT_USER + MINIO_ROOT_PASSWORD from
// the s3 secret group, falling back to the MinIO defaults when no
// configuration is wired. Production sets these through the standard
// codefly secret flow.
func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	// Defaults first; any successful key read overrides.
	s.rootUser = minioDefaultUser
	s.rootPassword = minioDefaultPassword

	if conf == nil {
		return nil
	}

	if u, err := resources.GetConfigurationValue(ctx, conf, "s3", "MINIO_ROOT_USER"); err == nil && u != "" {
		s.rootUser = u
	}
	if p, err := resources.GetConfigurationValue(ctx, conf, "s3", "MINIO_ROOT_PASSWORD"); err == nil && p != "" {
		s.rootPassword = p
	}
	return nil
}

// connectionURL returns an s3:// URL with the resolved creds. Kept
// for backward compatibility — consumers should prefer the structured
// keys (endpoint / access_key / secret_key) below.
func (s *Service) connectionURL(_ context.Context, address string) string {
	return fmt.Sprintf("s3://%s:%s@%s", s.rootUser, s.rootPassword, address)
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.LoadConfiguration(ctx, conf)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot load configuration")
	}

	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos:          []*basev0.ConfigurationInformation{s.buildConnectionInfo(ctx, instance.Address)},
	}
	return outputConf, nil
}

// buildConnectionInfo emits the s3 ConfigurationInformation block.
// Split out from CreateConnectionConfiguration so it's testable without
// a fully-loaded Service identity (Base.Unique() panics until Load
// runs). Downstream consumers (audit-exporter, app code reading
// endpoint/access_key/secret_key) depend on this exact key shape;
// renames here are breaking changes for them.
//
// Why both `connection` and the structured keys: connection is the
// legacy s3:// URL form some libs accept directly. endpoint /
// access_key / secret_key / region are the canonical names that
// minio-go and the AWS SDK consume natively.
func (s *Service) buildConnectionInfo(ctx context.Context, address string) *basev0.ConfigurationInformation {
	return &basev0.ConfigurationInformation{
		Name: "s3",
		ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: "connection", Value: s.connectionURL(ctx, address), Secret: true},
			{Key: "endpoint", Value: address},
			{Key: "access_key", Value: s.rootUser, Secret: true},
			{Key: "secret_key", Value: s.rootPassword, Secret: true},
			// MinIO accepts any region; the AWS SDK requires one set.
			{Key: "region", Value: "us-east-1"},
		},
	}
}

func main() {
	svc := NewService()
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
