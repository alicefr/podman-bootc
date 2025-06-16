package cmd

import (
	"context"
	"fmt"
	"os"
	filepath "path/filepath"

	"github.com/containers/podman-bootc/pkg/podman"
	"github.com/containers/podman-bootc/pkg/vm"
	"github.com/containers/podman-bootc/pkg/vm/domain"
	proxy "github.com/containers/podman-bootc/pkg/vsock"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/spf13/cobra"
)

const (
	// TODO: change the image tag with a proper version
	defaultImage = "quay.io/containers/bootc-vm:latest"
	diskName     = "disk.img"
	podmanSocket = "/run/user/1000/podman/podman.sock"
)

type installCmd struct {
	image            string
	vmImage          string
	bootcCmdLine     []string
	artifactsDir     string
	diskPath         string
	ctx              context.Context
	socket           string
	outputImage      string
	containerStorage string
	configPath       string
	outputPath       string
	installVM        *vm.InstallVM
}

func filterCmdlineArgs(args []string) ([]string, error) {
	sepIndex := -1
	for i, arg := range args {
		if arg == "--" {
			sepIndex = i
			break
		}
	}
	if sepIndex == -1 {
		return nil, fmt.Errorf("no command line specified")
	}

	return args[sepIndex+1:], nil
}

func NewInstallCommand() *cobra.Command {
	c := installCmd{}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the OS Containers",
		Long:  "Run bootc install to build the OS Containers. Specify the bootc cmdline after the '--'",
		RunE:  c.doInstall,
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = ""
	}
	cacheDir = filepath.Join(cacheDir, "bootc")
	cmd.PersistentFlags().StringVar(&c.vmImage, "bootc-vm", defaultImage, "bootc-vm container image containing the VM disk image")
	cmd.PersistentFlags().StringVar(&c.image, "bootc-image", "", "bootc-vm container image")
	cmd.PersistentFlags().StringVar(&c.artifactsDir, "dir", cacheDir, "directory where the artifacts are extracted")
	cmd.PersistentFlags().StringVar(&c.outputPath, "output-dir", "", "directory to store the output results")
	cmd.PersistentFlags().StringVar(&c.outputImage, "output-image", "", "path of the image to use for the installation")
	cmd.PersistentFlags().StringVar(&c.configPath, "config-dir", "", "path where to find the config.toml")
	cmd.PersistentFlags().StringVar(&c.containerStorage, "container-storage", podman.DefaultContainerStorage(), "Container storage to use")
	cmd.PersistentFlags().StringVar(&c.socket, "podman-socket", podman.DefaultPodmanSocket(), "path to the podman socket")
	if args, err := filterCmdlineArgs(os.Args); err == nil {
		c.bootcCmdLine = args
	}
	c.diskPath = filepath.Join(c.artifactsDir, diskName)

	return cmd
}

func init() {
	RootCmd.AddCommand(NewInstallCommand())
}

func (c *installCmd) validateArgs() error {
	if c.image == "" {
		return fmt.Errorf("the bootc-image cannot be empty")
	}
	if c.artifactsDir == "" {
		return fmt.Errorf("the artifacts directory path cannot be empty")
	}
	if c.outputImage == "" {
		return fmt.Errorf("the output-image needs to be set")
	}
	absPath, err := filepath.Abs(c.outputImage)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for the output image: %v", err)
	}
	c.outputImage = absPath
	if c.outputPath == "" {
		return fmt.Errorf("the output-path needs to be set")
	}
	if c.configPath == "" {
		return fmt.Errorf("the config-dir needs to be set")
	}
	if c.containerStorage == "" {
		return fmt.Errorf("the container storage cannot be empty")
	}
	if c.socket == "" {
		return fmt.Errorf("the socket for podman cannot be empty")
	}
	if len(c.bootcCmdLine) == 0 {
		return fmt.Errorf("the bootc commandline needs to be specified after the '--'")
	}
	c.ctx, err = bindings.NewConnection(context.Background(), "unix://"+c.socket)
	if err != nil {
		return fmt.Errorf("failed to connect to podman at %s: %v", c.socket, err)
	}

	return nil
}

func (c *installCmd) installBuildVM() error {
	inputPath := filepath.Join(c.artifactsDir, "disk.img")
	inputImageFormat, err := domain.GetDiskInfo(inputPath)
	if err != nil {
		return err
	}
	outputImageFormat, err := domain.GetDiskInfo(c.outputImage)
	if err != nil {
		return err
	}
	c.installVM = vm.NewInstallVM(vm.InstallOptions{
		DiskImage:            inputPath,
		OutputImage:          c.outputImage,
		InputFormat:          inputImageFormat,
		OutputFormat:         outputImageFormat,
		ContainerStoragePath: c.containerStorage,
		ConfigPath:           c.configPath,
		OutputPath:           c.outputPath,
		Root:                 false,
	})
	if err := c.installVM.Run(); err != nil {
		return err
	}

	return nil
}

func (c *installCmd) doInstall(_ *cobra.Command, _ []string) error {
	if err := c.validateArgs(); err != nil {
		return err
	}

	if err := podman.ExtractDiskImage(c.socket, c.artifactsDir, c.vmImage); err != nil {
		return err
	}
	if err := c.installBuildVM(); err != nil {
		return err
	}
	defer c.installVM.Stop()

	p := proxy.NewProxy(vm.CIDInstallVM, vm.VSOCKPort, filepath.Join(c.artifactsDir, "bootcvm.sock"))
	if err := p.Start(); err != nil {
		return err
	}
	defer p.Stop()

	if err := podman.RunPodmanCmd(p.GetSocket(), c.image, c.bootcCmdLine); err != nil {
		return err
	}

	return nil
}
