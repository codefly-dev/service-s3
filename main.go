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
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
)

type Settings struct {
	Password    string `yaml:"password"`
	RequirePass bool   `yaml:"require-pass"`
}

// image — pinned MinIO release. NEVER :latest (project policy):
// the bucket layout, IAM behaviour, and admin endpoints can shift
// between releases; an unpinned tag means a re-pull silently changes
// behaviour. Bump deliberately when upgrading.
//
// MinIO is the local S3-compatible store; same API surface as AWS
// S3, R2, GCS-S3-mode — production swaps the endpoint env without
// touching application code.
var image = &resources.DockerImage{Name: "minio/minio", Tag: "RELEASE.2025-04-08T15-41-24Z"}

type Service struct {
	*services.Base

	// Settings
	*Settings

	s3Password string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{
			{
				Name: "s3", Description: "s3 connection details",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "connection string"},
				},
			},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	// Configuration is optional — if no S3_PASSWORD is provided, run
	// without auth. This is the sensible default for local dev + test
	// environments; production deployments set the password via the
	// standard configuration flow.
	if conf == nil {
		s.s3Password = ""
		return nil
	}
	pw, err := resources.GetConfigurationValue(ctx, conf, "s3", "S3_PASSWORD")
	if err != nil {
		// Missing key is fine — empty password means no auth. Only
		// surface genuine errors (malformed config, etc.) — but
		// GetConfigurationValue returns an error for "not found" too, so
		// treat any err as "no password configured".
		s.s3Password = ""
		return nil
	}
	s.s3Password = pw
	return nil
}

func (s *Service) createConnectionString(_ context.Context, address string) string {
	if s.s3Password != "" {
		return fmt.Sprintf("s3://:%s@%s", s.s3Password, address)
	}
	return fmt.Sprintf("s3://%s", address)
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.LoadConfiguration(ctx, conf)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot load configuration")
	}

	connection := s.createConnectionString(ctx, instance.Address)

	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "s3",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
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
