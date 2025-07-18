package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/containers/podman-bootc/pkg/podman"
	"github.com/containers/podman-bootc/pkg/vm"
	"github.com/containers/podman-bootc/pkg/vm/domain"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type entrypointCmd struct {
	image        string
	bootcCmdLine []string
	outputImage  string
	outputPath   string
	installVM    *vm.InstallVM
	logLevel     string
	remoteSocket string
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

func NewEntrypointCommand() *cobra.Command {
	c := entrypointCmd{
		remoteSocket: "/run/podman/podman-vm.sock",
	}
	cmd := &cobra.Command{
		Use:    "entrypoint",
		Short:  "Internal command to run the installation",
		Hidden: true,
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
		RunE: c.doEntrypoint,
	}

	cmd.PersistentFlags().StringVar(&c.image, "bootc-image", "", "bootc-vm container image")
	cmd.PersistentFlags().StringVar(&c.outputPath, "output-dir", "", "directory to store the output results")
	cmd.PersistentFlags().StringVar(&c.outputImage, "output-image", "", "path of the image to use for the installation")
	cmd.PersistentFlags().StringVar(&c.logLevel, "log-level", "info", "set the log level (trace, debug, info, warn, error, fatal, panic)")

	if args, err := filterCmdlineArgs(os.Args); err == nil {
		c.bootcCmdLine = args
	}
	return cmd
}

func (c *entrypointCmd) validateArgs() error {
	if c.image == "" {
		return fmt.Errorf("the bootc-image cannot be empty")
	}
	if c.outputImage == "" {
		return fmt.Errorf("the output-image needs to be set")
	}
	if c.outputPath == "" {
		return fmt.Errorf("the output-path needs to be set")
	}
	if len(c.bootcCmdLine) == 0 {
		return fmt.Errorf("the bootc commandline needs to be specified after the '--'")
	}
	return nil
}

func copyFile(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	if err != nil {
		return err
	}
	return os.Chmod(dst, sourceFileStat.Mode())
}

func (c *entrypointCmd) injectFiles() error {
	bootcRoot := vm.BootcDir
	vmFilesDir := "/vm_files"

	dirs := []string{
		filepath.Join(bootcRoot, "/etc/sysusers.d"),
		filepath.Join(bootcRoot, "/usr/lib/containers/storage"),
		filepath.Join(bootcRoot, "/etc/systemd/system"),
		filepath.Join(bootcRoot, "/usr/local/bin"),
		filepath.Join(bootcRoot, "/etc/containers"),
		filepath.Join(bootcRoot, "/etc/selinux"),
		filepath.Join(bootcRoot, "/etc/sudoers.d"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	files := map[string]string{
		filepath.Join(vmFilesDir, "bootc.conf"):                 filepath.Join(bootcRoot, "/etc/sysusers.d/bootc.conf"),
		filepath.Join(vmFilesDir, "podman-vsock-proxy.service"): filepath.Join(bootcRoot, "/etc/systemd/system/podman-vsock-proxy.service"),
		filepath.Join(vmFilesDir, "mount-vfsd-targets.service"): filepath.Join(bootcRoot, "/etc/systemd/system/mount-vfsd-targets.service"),
		filepath.Join(vmFilesDir, "mount-vfsd-targets.sh"):      filepath.Join(bootcRoot, "/usr/local/bin/mount-vfsd-targets.sh"),
		filepath.Join(vmFilesDir, "container-storage.conf"):     filepath.Join(bootcRoot, "/etc/containers/storage.conf"),
		filepath.Join(vmFilesDir, "selinux-config"):             filepath.Join(bootcRoot, "/etc/selinux/config"),
		filepath.Join(vmFilesDir, "sudoers-bootc"):              filepath.Join(bootcRoot, "/etc/sudoers.d/bootc"),
		"/usr/local/bin/vsock-proxy":                            filepath.Join(bootcRoot, "/usr/local/bin/vsock-proxy"),
	}

	for src, dst := range files {
		log.Infof("Copying %s to %s", src, dst)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("failed to copy %s to %s: %w", src, dst, err)
		}
	}

	log.Info("Creating empty password for bootc user")
	shadowFile := filepath.Join(bootcRoot, "/etc/shadow")
	f, err := os.OpenFile(shadowFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open shadow file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString("bootc::20266::::::\n"); err != nil {
		return fmt.Errorf("failed to write to shadow file: %w", err)
	}

	services := []string{
		"mount-vfsd-targets",
		"podman.socket",
		"podman-vsock-proxy.service",
	}

	for _, service := range services {
		log.Infof("Enabling systemd service: %s", service)
		cmd := exec.Command("chroot", bootcRoot, "systemctl", "enable", service)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to enable service %s: %w, output: %s", service, err, string(output))
		}
	}

	return nil
}

func (c *entrypointCmd) installBuildVM(kernel, initrd string, outputImageFormat domain.DiskDriverType) error {
	c.installVM = vm.NewInstallVM(vm.InstallOptions{
		OutputFormat: outputImageFormat,
		OutputImage:  filepath.Join(vm.OutputDir, c.outputImage),
		Kernel:       kernel,
		Initrd:       initrd,
	})
	if err := c.installVM.Run(); err != nil {
		return err
	}
	return nil
}

func findFile(root string, pattern string) (string, error) {
	var foundPath string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == pattern {
			foundPath = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if foundPath == "" {
		return "", fmt.Errorf("file %s not found in %s", pattern, root)
	}
	return foundPath, nil
}


func ensureBaseDirExists(path string) error {
	baseDir := filepath.Dir(path)

	// Check if directory exists
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		err := os.MkdirAll(baseDir, 0755)
		if err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return nil
}

func (c *entrypointCmd) doEntrypoint(_ *cobra.Command, _ []string) error {
	if err := c.validateArgs(); err != nil {
		return err
	}

	if err := c.injectFiles(); err != nil {
		return err
	}

	log.Info("Starting virtlogd...")
	virtlogdCmd := exec.Command("/usr/bin/virtlogd")
	if err := virtlogdCmd.Start(); err != nil {
		return fmt.Errorf("failed to start virtlogd: %w", err)
	}
	defer func() {
		if virtlogdCmd.ProcessState != nil {
			return
		}
		if err := virtlogdCmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Warnf("failed to terminate virtlogd: %v", err)
			return
		}
		log.Info("virtlogd terminated")
	}()

	virtlogdErr := make(chan error, 1)
	go func() {
		virtlogdErr <- virtlogdCmd.Wait()
	}()

	log.Info("Starting virtstoraged...")
	virtstoragedCmd := exec.Command("/usr/bin/virtstoraged")
	if err := virtstoragedCmd.Start(); err != nil {
		return fmt.Errorf("failed to start virtstoraged: %w", err)
	}
	defer func() {
		if virtstoragedCmd.ProcessState != nil {
			return
		}
		if err := virtstoragedCmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Warnf("failed to terminate virtstoraged: %v", err)
			return
		}
		log.Info("virtstoraged terminated")
	}()

	virtstoragedErr := make(chan error, 1)
	go func() {
		virtstoragedErr <- virtstoragedCmd.Wait()
	}()

	log.Info("Starting virtqemud...")
	virtqemudCmd := exec.Command("/usr/sbin/virtqemud", "-v", "-t", "0")
	if err := virtqemudCmd.Start(); err != nil {
		return fmt.Errorf("failed to start virtqemud: %w", err)
	}
	defer func() {
		if virtqemudCmd.ProcessState != nil {
			return
		}
		if err := virtqemudCmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Warnf("failed to terminate virtqemud: %v", err)
			return
		}
		log.Info("virtqemud terminated")
	}()

	virtqemudErr := make(chan error, 1)
	go func() {
		virtqemudErr <- virtqemudCmd.Wait()
	}()

	if err := ensureBaseDirExists(c.remoteSocket); err != nil {
		return err
	}
	log.Info("Starting vsock-proxy...")
	args := []string{"--log-level", "debug",
		"-s", c.remoteSocket,
		"-p", "1234",
		"--cid", "3",
		"--listen-mode", "unixToVsock"}
	vsockProxyCmd := exec.Command("vsock-proxy", args...)
	vsockProxyCmd.Stdout = os.Stdout
	vsockProxyCmd.Stderr = os.Stderr

	if err := vsockProxyCmd.Start(); err != nil {
		return fmt.Errorf("failed to start vsock-proxy: %w", err)
	}
	defer func() {
		if vsockProxyCmd.ProcessState != nil {
			return
		}
		if err := vsockProxyCmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Warnf("failed to terminate vsock-proxy: %v", err)
			return
		}
		log.Info("vsock-proxy terminated")
	}()

	vsockProxyErr := make(chan error, 1)
	go func() {
		vsockProxyErr <- vsockProxyCmd.Wait()
	}()

	image := filepath.Join(c.outputPath, c.outputImage)
	outputImageFormat, err := domain.GetDiskInfo(image)
	if err != nil {
		return err
	}

	kernel, err := findFile(vm.BootcDir, "vmlinuz")
	if err != nil {
		return err
	}
	initrd, err := findFile(vm.BootcDir, "initramfs.img")
	if err != nil {
		return err
	}
	log.Debugf("Boot artifacts kernel: %s and initrd: %s", kernel, initrd)

	if err := c.installBuildVM(kernel, initrd, outputImageFormat); err != nil {
		return err
	}
	defer c.installVM.Stop()

	installErr := make(chan error, 1)
	go func() {
		installErr <- podman.RunPodmanCmd(c.remoteSocket, c.image, c.bootcCmdLine)
	}()

	vmErr := make(chan error, 1)
	go func() {
		vmErr <- c.installVM.Wait()
	}()

	select {
	case err := <-installErr:
		if err != nil {
			return fmt.Errorf("installation failed: %w", err)
		}
		log.Info("Installation finished successfully")
	case err := <-vmErr:
		return fmt.Errorf("installer VM exited unexpectedly: %v", err)
	case err := <-virtlogdErr:
		return fmt.Errorf("virtlogd exited unexpectedly: %v", err)
	case err := <-virtstoragedErr:
		return fmt.Errorf("virtstoraged exited unexpectedly: %v", err)
	case err := <-virtqemudErr:
		return fmt.Errorf("virtqemud exited unexpectedly: %v", err)
	case err := <-vsockProxyErr:
		return fmt.Errorf("vsock-proxy exited unexpectedly: %v", err)
	}

	return nil
}

func main() {
	if err := NewEntrypointCommand().Execute(); err != nil {
		log.Fatalf("entrypoint command failed: %v", err)
	}
}
