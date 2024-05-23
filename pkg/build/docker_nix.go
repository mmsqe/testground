package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/testground/testground/pkg/api"
	"github.com/testground/testground/pkg/docker"
	"github.com/testground/testground/pkg/rpc"

	"github.com/docker/docker/client"
)

var _ api.Builder = &DockerNixBuilder{}

type DockerNixBuilderConfig struct {
	Enabled bool
	Name    string
	System  string
}

type DockerNixBuilder struct{}

func (d DockerNixBuilder) ID() string {
	return "docker:nix"
}

func (d DockerNixBuilder) Build(ctx context.Context, in *api.BuildInput, ow *rpc.OutputWriter) (*api.BuildOutput, error) {
	cfg, ok := in.BuildConfig.(*DockerNixBuilderConfig)
	if !ok {
		return nil, fmt.Errorf("expected configuration type DockerNixBuilderConfig, was: %T", in.BuildConfig)
	}

	cliopts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}

	var (
		basesrc  = in.UnpackedSources.PlanDir
		cli, err = client.NewClientWithOpts(cliopts...)
	)
	if err != nil {
		return nil, err
	}

	// fill default configs
	if len(cfg.System) == 0 {
		switch runtime.GOARCH {
		case "arm64":
			cfg.System = "aarch64-linux"
		case "amd64":
			cfg.System = "x86_64-linux"
		default:
			return nil, fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
		}
	}

	if len(cfg.Name) == 0 {
		cfg.Name = in.TestPlan + "-image"
	}

	buildStart := time.Now()

	// spawn nix build
	cmd := exec.Command(
		"nix",
		"build",
		fmt.Sprintf("%s#legacyPackages.%s.%s", basesrc, cfg.System, cfg.Name),
		"--no-link",
		"--print-out-paths",
	)
	ow.Infow("nix build", "target", fmt.Sprintf("%s#legacyPackages.%s.%s", basesrc, cfg.System, cfg.Name))
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			ow.Errorw("nix build fail result", "stderr", string(ee.Stderr))
		}
		return nil, fmt.Errorf("nix build failed: %w", err)
	}

	path := strings.TrimRight(string(stdout), "\r\n")
	ow.Infow("nix build completed", "path", path)

	// somehow we need to wait a little bit for the file to be ready for reading.
	time.Sleep(100 * time.Millisecond)

	tarball, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("couldnt open tarball: %s, %w", path, err)
	}
	defer tarball.Close()
	loadResponse, err := cli.ImageLoad(ctx, tarball, false)
	if err != nil {
		return nil, fmt.Errorf("docker image load failed: %w", err)
	}
	rsp, err := docker.PipeOutput(loadResponse.Body, ow.StdoutWriter())
	if err != nil {
		return nil, fmt.Errorf("couldnt read docker image load response: %w", err)
	}

	defaultTag := strings.TrimRight(strings.TrimPrefix(rsp, "Loaded image: "), "\r\n")
	ow.Infow("build completed", "default_tag", defaultTag, "took", time.Since(buildStart).Truncate(time.Second))

	imageID, err := docker.GetImageID(ctx, cli, defaultTag)
	if err != nil {
		return nil, fmt.Errorf("couldnt get docker image id: %s, %w", rsp, err)
	}

	ow.Infow("got docker image id", "image_id", imageID)

	out := &api.BuildOutput{
		ArtifactPath: imageID,
	}

	// Testplan image tag
	testplanImageTag := fmt.Sprintf("%s:%s", in.TestPlan, imageID)

	ow.Infow("tagging image", "image_id", imageID, "tag", testplanImageTag)
	if err = cli.ImageTag(ctx, out.ArtifactPath, testplanImageTag); err != nil {
		return out, err
	}

	return out, err
}

func (d DockerNixBuilder) Purge(ctx context.Context, testplan string, ow *rpc.OutputWriter) error {
	return fmt.Errorf("purge not implemented for docker:nix")
}

func (d DockerNixBuilder) ConfigType() reflect.Type {
	return reflect.TypeOf(DockerNixBuilderConfig{})
}
