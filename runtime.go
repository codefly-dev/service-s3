package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
)

type Runtime struct {
	services.RuntimeServer
	*Service

	// internal
	runnerEnvironment *dockerrun.DockerEnvironment

	// nixRuntime is set instead of runnerEnvironment when the caller requests
	// RuntimeContextNix — minio runs natively from a nix-provisioned binary.
	nixRuntime *nixMinio

	s3Port uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find TCP endpoint")
			}
			s.TcpEndpoint = endpoint
			return nil
		},
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	configuration := req.GetConfiguration()

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	// 9000 is the MinIO S3 API port. The console (9001) is not
	// exposed — operators who want the UI can `docker port` it
	// manually; codefly only manages one TCP endpoint per service.
	s.s3Port = 9000

	// Create connection string resources for the network instance
	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}
	s.Wool.Debug("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	// Load creds from configuration (defaults to MinIO defaults). Needed by both
	// runtimes.
	err = s.LoadConfiguration(ctx, configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// MinIO refuses to start if the root password is shorter than 8
	// chars. We surface that early with a wool error rather than
	// letting the container crash-loop with an opaque message.
	if len(s.rootPassword) < 8 {
		return s.Runtime.InitError(w.NewError("MINIO_ROOT_PASSWORD must be >= 8 chars"))
	}

	// Nix runtime: run minio natively from a nix-provisioned binary instead of a
	// Docker container — selected when the caller requests RuntimeContextNix
	// (e.g. a host without Docker). minio binds the assigned port directly, so
	// WaitForReady is unchanged.
	if rc := req.GetRuntimeContext(); rc != nil && rc.Kind == resources.RuntimeContextNix {
		s.Infof("using nix runtime for minio on port %d", instance.Port)
		nixm, errNix := newNixMinio(ctx, s.Location, uint16(instance.Port), s.rootUser, s.rootPassword, s.Wool)
		if errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		if errNix = nixm.Init(ctx); errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		s.nixRuntime = nixm
	} else {
		// Docker
		runner, errDocker := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
		if errDocker != nil {
			return s.Runtime.InitError(errDocker)
		}
		runner.WithOutput(s.Wool)
		runner.WithPortMapping(ctx, uint16(instance.Port), s.s3Port)
		runner.WithEnvironmentVariables(ctx,
			resources.Env("MINIO_ROOT_USER", s.rootUser),
			resources.Env("MINIO_ROOT_PASSWORD", s.rootPassword),
		)
		// `server /data` is MinIO's standard single-node command. We omit
		// --console-address — codefly only exposes the S3 API port.
		runner.WithCommand("server", "/data")
		s.runnerEnvironment = runner
		w.Debug("init for runner environment: will start container")
		if errDocker = s.runnerEnvironment.Init(ctx); errDocker != nil {
			return s.Runtime.InitError(errDocker)
		}
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

// minIOReadyURL is MinIO's documented liveness probe — returns 200
// once the server is accepting traffic, anything else means "not
// ready yet". Distinct from /minio/health/cluster (cluster-mode probe)
// which we'd add only if we ever ran multi-node MinIO.
const minIOReadyURL = "/minio/health/live"

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot find network instance")
	}

	probeURL := fmt.Sprintf("http://%s%s", instance.Address, minIOReadyURL)
	s.Wool.Debug("waiting for minio to be ready", wool.Field("url", probeURL))

	client := &http.Client{Timeout: 2 * time.Second}

	// MinIO usually accepts traffic 1-3s after the container starts;
	// 30 attempts × 2s = 60s budget which is generous for a slow
	// laptop or a fresh image pull on the first run.
	const maxRetry = 30
	var lastErr error
	for retry := 0; retry < maxRetry; retry++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				s.Wool.Debug("minio is ready", wool.Field("status", resp.StatusCode))
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		s.Wool.Debug("minio not ready yet", wool.Field("retry", retry), wool.ErrField(lastErr))
		time.Sleep(2 * time.Second)
	}
	return s.Wool.NewError("minio is not ready: %v", lastErr)
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Nix runtime: terminate the native minio process; there is no container.
	if s.nixRuntime != nil {
		if err := s.nixRuntime.Stop(ctx); err != nil {
			return s.Runtime.DestroyError(err)
		}
		return s.Runtime.DestroyResponse()
	}

	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	err = runner.Shutdown(ctx)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}
