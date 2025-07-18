package cmd

import (
	"context"
	"fmt"
	"os"
	filepath "path/filepath"

	"github.com/containers/podman-bootc/pkg/podman"
	"github.com/containers/podman-bootc/pkg/vm"
	"github.com/containers/podman/v5/pkg/bindings"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type installCmd struct {
	image            string
	bootcCmdLine     []string
	ctx              context.Context
	socket           string
	outputImage      string
	containerStorage string
	outputPath       string
	podmanSocketDir  string
	logLevel         string
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
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			lvl, err := log.ParseLevel(c.logLevel)
			if err != nil {
				return err
			}
			log.SetLevel(lvl)
			log.SetFormatter(&log.TextFormatter{
				FullTimestamp: true,
			})
			return nil
		},
		RunE: c.doInstall,
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = ""
	}
	cacheDir = filepath.Join(cacheDir, "bootc")
	cmd.PersistentFlags().StringVar(&c.image, "bootc-image", "", "bootc-vm container image")
	cmd.PersistentFlags().StringVar(&c.outputPath, "output-dir", "", "directory to store the output results")
	cmd.PersistentFlags().StringVar(&c.outputImage, "output-image", "", "path of the image to use for the installation")
	cmd.PersistentFlags().StringVar(&c.containerStorage, "container-storage", podman.DefaultContainerStorage(), "Container storage to use")
	cmd.PersistentFlags().StringVar(&c.socket, "podman-socket", podman.DefaultPodmanSocket(), "path to the podman socket")
	cmd.PersistentFlags().StringVar(&c.logLevel, "log-level", "info", "set the log level (trace, debug, info, warn, error, fatal, panic)")
	if args, err := filterCmdlineArgs(os.Args); err == nil {
		c.bootcCmdLine = args
	}

	return cmd
}

func init() {
	RootCmd.AddCommand(NewInstallCommand())
}

func (c *installCmd) validateArgs() error {
	if c.image == "" {
		return fmt.Errorf("the bootc-image cannot be empty")
	}
	if c.outputImage == "" {
		return fmt.Errorf("the output-image needs to be set")
	}
	if c.outputPath == "" {
		return fmt.Errorf("the output-path needs to be set")
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
	var err error
	c.ctx, err = bindings.NewConnection(context.Background(), "unix://"+c.socket)
	if err != nil {
		return fmt.Errorf("failed to connect to podman at %s: %v", c.socket, err)
	}

	return nil
}

func (c *installCmd) setupAndRunVMContainer() (*podman.VMContainer, error) {
	entrypointCmd := []string{
		"/usr/bin/entrypoint",
		"--bootc-image", c.image,
		"--output-dir", vm.OutputDir,
		"--output-image", c.outputImage,
		"--log-level", c.logLevel,
	}
	entrypointCmd = append(entrypointCmd, "--")
	entrypointCmd = append(entrypointCmd, c.bootcCmdLine...)

	vmCont := podman.NewVMContainer(c.image, c.socket, &podman.RunVMContainerOptions{
		ContainerStoragePath: c.containerStorage,
		OutputDir:            c.outputPath,
		Command:              entrypointCmd,
	})
	if err := vmCont.Run(); err != nil {
		return nil, err
	}
	return vmCont, nil
}

func (c *installCmd) doInstall(_ *cobra.Command, _ []string) error {
	if err := c.validateArgs(); err != nil {
		return err
	}

	vmCont, err := c.setupAndRunVMContainer()
	if err != nil {
		return err
	}
	defer vmCont.Stop()

	if err := vmCont.Wait(); err != nil {
		return err
	}

	return nil
}
