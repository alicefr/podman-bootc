package podman

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	_ "embed"

	"github.com/containers/podman-bootc/pkg/utils"
	"github.com/containers/podman-bootc/pkg/vm"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
	log "github.com/sirupsen/logrus"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"
)

type RunVMContainerOptions struct {
	ContainerStoragePath string
	OutputDir            string
	Command              []string
}

func detectLocalPodman() string {
	return ""
}

type VMContainer struct {
	contID     string
	image      string
	socketPath string
	opts       *RunVMContainerOptions
}

func NewVMContainer(image, socketPath string, opts *RunVMContainerOptions) *VMContainer {
	return &VMContainer{
		image:      image,
		socketPath: socketPath,
		opts:       opts,
	}
}

func (c *VMContainer) Stop() error {
	ctx, err := connectPodman(c.socketPath)
	if err != nil {
		return fmt.Errorf("Failed to connect to Podman service: %v", err)
	}
	if err := containers.Stop(ctx, c.contID, &containers.StopOptions{}); err != nil {
		return fmt.Errorf("failed to stop the bootc container: %v", err)
	}
	if _, err := containers.Remove(ctx, c.contID, &containers.RemoveOptions{}); err != nil {
		return fmt.Errorf("failed to remove the bootc container: %v", err)
	}

	return nil
}

func (c *VMContainer) Run() error {
	ctx, err := connectPodman(c.socketPath)
	if err != nil {
		return fmt.Errorf("Failed to connect to Podman service: %v", err)
	}

	c.contID, err = createVMContainer(ctx, c.image, c.opts)
	if err != nil {
		return err
	}

	if err := containers.Start(ctx, c.contID, &containers.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start the bootc container: %v", err)
	}

	isRunning, err := isContainerRunning(ctx, c.contID)
	if err != nil {
		return err
	}
	if !isRunning {
		return fmt.Errorf("the VM container %s isn't running", c.contID)
	}
	return err
}

func (c *VMContainer) Wait() error {
	ctx, err := connectPodman(c.socketPath)
	if err != nil {
		return fmt.Errorf("Failed to connect to Podman service: %v", err)
	}
	return fetchLogsAfterExit(ctx, c.contID)
}

func isContainerRunning(ctx context.Context, name string) (bool, error) {
	inspectData, err := containers.Inspect(ctx, name, nil)
	if err != nil {
		return false, fmt.Errorf("failed to inspect container: %w", err)
	}

	// Check if it's running
	return inspectData.State.Running, nil
}

func createVMContainer(ctx context.Context, image string, opts *RunVMContainerOptions) (string, error) {
	cmd := opts.Command
	if len(cmd) == 0 {
		cmd = []string{"/entrypoint.sh"}
	}
	specGen := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command: cmd,
			Stdin:   utils.Ptr(true),
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: vm.VMImage,
			ImageVolumes: []*specgen.ImageVolume{
				{
					Destination: vm.BootcDir,
					Source:      image,
					ReadWrite:   true,
				},
			},
			Devices: []ocispec.LinuxDevice{
				{
					Path: "/dev/kvm",
					Type: "char",
				},
				{
					Path: "/dev/vhost-net",
					Type: "char",
				},
				{
					Path: "/dev/vhost-vsock",
					Type: "char",
				},
			},
			Mounts: []ocispec.Mount{
				{
					Destination: vm.ContainerStoragePath,
					Source:      opts.ContainerStoragePath,
					Type:        "bind",
				},
				{
					Destination: vm.OutputDir,
					Source:      opts.OutputDir,
					Type:        "bind",
				},
			},
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			Privileged:  utils.Ptr(true),
			SelinuxOpts: []string{"type:unconfined_t"},
		},
		ContainerCgroupConfig: specgen.ContainerCgroupConfig{},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			PublishExposedPorts: utils.Ptr(true),
			Expose:              map[uint16]string{uint16(vm.VNCPort): "tcp"},
		},
	}
	if err := specGen.Validate(); err != nil {
		return "", err
	}
	response, err := containers.CreateWithSpec(ctx, specGen, &containers.CreateOptions{})
	if err != nil {
		return "", err
	}

	log.Debugf("Run VM container ID: %s", response.ID)

	return response.ID, nil
}

func connectPodman(socketPath string) (context.Context, error) {
	const (
		retryInterval = 5 * time.Second
		timeout       = 5 * time.Minute
	)

	deadline := time.Now().Add(timeout)

	var ctx context.Context
	var err error

	for time.Now().Before(deadline) {
		ctx, err = bindings.NewConnection(context.Background(), fmt.Sprintf("unix:%s", socketPath))
		if err == nil {
			log.Debugf("Connected to Podman successfully!")
			return ctx, nil
		}

		log.Debugf("Failed to connect to Podman. Retrying in %s seconds...", retryInterval.String())
		time.Sleep(retryInterval)
	}

	return nil, fmt.Errorf("Unable to connect to Podman after %v: %v", timeout, err)
}

func createBootcContainer(ctx context.Context, image string, bootcCmdLine []string) (string, error) {
	log.Debugf("Create bootc container with cmdline: %v", bootcCmdLine)
	specGen := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command: bootcCmdLine,
			Stdin:   utils.Ptr(true),
			PidNS: specgen.Namespace{
				NSMode: specgen.Host,
			},
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: image,
			Mounts: []ocispec.Mount{
				{
					Destination: "/var/lib/containers",
					Source:      "/var/lib/containers",
					Type:        "bind",
				},
				{
					Destination: "/var/lib/containers/storage",
					Source:      vm.ContainerStoragePath,
					Type:        "bind",
				},
				{
					Destination: "/dev",
					Source:      "/dev",
					Type:        "bind",
				},
				{
					Destination: "/output",
					Source:      vm.OutputDir,
					Type:        "bind",
				},
			},
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			Privileged:  utils.Ptr(true),
			SelinuxOpts: []string{"type:unconfined_t"},
		},
		ContainerCgroupConfig: specgen.ContainerCgroupConfig{},
	}
	if err := specGen.Validate(); err != nil {
		return "", err
	}
	response, err := containers.CreateWithSpec(ctx, specGen, &containers.CreateOptions{})
	if err != nil {
		return "", err
	}

	return response.ID, nil
}

func fetchLogsAfterExit(ctx context.Context, containerID string) error {
	stdoutCh := make(chan string)
	stderrCh := make(chan string)

	// Start log streaming
	go func() {
		logOpts := new(containers.LogOptions).WithFollow(true).WithStdout(true).WithStderr(true)

		err := containers.Logs(ctx, containerID, logOpts, stdoutCh, stderrCh)
		if err != nil {
			log.Errorf("Error streaming logs: %v\n", err)
		}
		close(stdoutCh)
		close(stderrCh)
	}()

	go func() {
		for line := range stdoutCh {
			fmt.Fprintf(os.Stdout, "%s", line)
		}
	}()
	go func() {
		for line := range stderrCh {
			fmt.Fprintf(os.Stderr, "%s", line)
		}
	}()

	exitCode, err := containers.Wait(ctx, containerID, new(containers.WaitOptions).
		WithCondition([]define.ContainerStatus{define.ContainerStateExited}))
	if err != nil {
		return fmt.Errorf("failed to wait for container: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("bootc command failed: %d", exitCode)
	}

	return nil
}

func RunPodmanCmd(socketPath string, image string, bootcCmdLine []string) error {
	ctx, err := connectPodman(socketPath)
	if err != nil {
		return fmt.Errorf("Failed to connect to Podman service: %v", err)
	}

	name, err := createBootcContainer(ctx, image, bootcCmdLine)
	if err != nil {
		return fmt.Errorf("failed to create the bootc container: %v", err)
	}

	if err := containers.Start(ctx, name, &containers.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start the bootc container: %v", err)
	}

	if err := fetchLogsAfterExit(ctx, name); err != nil {
		return fmt.Errorf("failed executing bootc: %v", err)
	}

	return nil
}

func DefaultPodmanSocket() string {
	if envSock := os.Getenv("DOCKER_HOST"); envSock != "" {
		return envSock
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir != "" {
		return filepath.Join(runtimeDir, "podman", "podman.sock")
	}
	usr, err := user.Current()
	if err == nil && usr.Uid != "0" {
		return "/run/user/" + usr.Uid + "/podman/podman.sock"
	}

	return "/run/podman/podman.sock"
}

func DefaultContainerStorage() string {
	usr, err := user.Current()
	if err == nil && usr.Uid != "0" {
		homeDir := os.Getenv("HOME")
		if homeDir != "" {
			return filepath.Join(homeDir, ".local/share/containers/storage")
		}
	}

	return "/var/lib/containers/storage"
}
