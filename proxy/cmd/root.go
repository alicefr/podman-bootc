package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/containers/podman-bootc/pkg/vsock"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type rootCmd struct {
	proxy    *vsock.Proxy
	logLevel string
}

func NewRootCmd() *cobra.Command {
	c := rootCmd{}
	cmd := &cobra.Command{
		Use:               "proxy",
		Short:             "Proxy the VSOCK connection to a UNIX socket",
		Long:              "Proxy the VSOCK connection from the CID and VSOCK to a local unix socket",
		PersistentPreRunE: c.preExec,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.run()
		},
	}

	var (
		cid    uint
		port   uint
		socket string
	)
	cmd.PersistentFlags().UintVarP(&cid, "cid", "c", 0, "CID allocated by the VM")
	cmd.PersistentFlags().UintVarP(&port, "port", "p", 0, "Port for the VSOCK on the VM")
	cmd.PersistentFlags().StringVarP(&socket, "socket", "s", "", "Socket for the proxy")
	cmd.PersistentFlags().StringVarP(&c.logLevel, "log-level", "", "", "Set log level")
	cmd.MarkPersistentFlagRequired("cid")
	cmd.MarkPersistentFlagRequired("port")
	cmd.MarkPersistentFlagRequired("socket")

	return cmd
}

func (c *rootCmd) preExec(cmd *cobra.Command, args []string) error {
	if c.logLevel != "" {
		level, err := log.ParseLevel(c.logLevel)
		if err != nil {
			return err
		}
		log.SetLevel(level)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	cid, _ := cmd.Flags().GetUint("cid")
	port, _ := cmd.Flags().GetUint("port")
	socket, _ := cmd.Flags().GetString("socket")

	if socket == "" {
		return fmt.Errorf("the socket needs to be set")
	}

	c.proxy = vsock.NewProxy(cid, port, socket)

	if c.proxy.GetSocket() == "" {
		return fmt.Errorf("the socket needs to be set")
	}

	return nil
}

func (c *rootCmd) run() error {
	if err := c.proxy.Start(); err != nil {
		return err
	}
	defer c.proxy.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	return nil
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
