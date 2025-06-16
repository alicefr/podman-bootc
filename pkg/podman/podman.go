package podman

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/containers/podman-bootc/pkg/utils"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
	log "github.com/sirupsen/logrus"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"
)

func createContainer(ctx context.Context, vmImage string) (string, error) {
	specGen := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command: []string{"/"},
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: vmImage,
		},
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

func extractTar(reader io.Reader, dest string) error {
	tr := tar.NewReader(reader)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dest, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}

	return nil
}

func ExtractDiskImage(socketPath, dir, vmImage string) error {
	if err := os.Mkdir(dir, 0750); err != nil && !os.IsExist(err) {
		return err
	}
	ctx, err := bindings.NewConnection(context.Background(), fmt.Sprintf("unix:%s", socketPath))
	if err != nil {
		return err
	}

	containerName, err := createContainer(ctx, vmImage)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()
		err := containers.Export(ctx, containerName, pw, &containers.ExportOptions{})
		if err != nil {
			// If an error occurs, propagate it to the pipe reader
			pw.CloseWithError(err)
		}
	}()
	if err := extractTar(pr, dir); err != nil {
		return err
	}
	log.Debugf("Extracted disk at: %s", dir)

	return nil
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
					Source:      "/usr/lib/bootc/storage",
					Type:        "bind",
				},
				{
					Destination: "/dev",
					Source:      "/dev",
					Type:        "bind",
				},
				{
					Destination: "/output",
					Source:      "/usr/lib/bootc/output",
					Type:        "bind",
				},
				{
					Destination: "/config",
					Source:      "/usr/lib/bootc/config",
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
