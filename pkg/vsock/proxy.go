package vsock

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type Proxy struct {
	cid    uint
	port   uint
	socket string
	done   chan struct{}
}

func NewProxy(cid, port uint, socket string) *Proxy {
	return &Proxy{
		cid:    cid,
		port:   port,
		socket: socket,
		done:   make(chan struct{}),
	}
}

func (proxy *Proxy) GetSocket() string {
	return proxy.socket
}

func (proxy *Proxy) Stop() {
	select {
	case <-proxy.done:
		// already closed
	default:
		close(proxy.done)
	}
	os.Remove(proxy.socket)
	logrus.Debugf("Stopped proxy")
}

func (proxy *Proxy) Start() error {
	_ = os.Remove(proxy.socket)

	unixListener, err := net.Listen("unix", proxy.socket)
	if err != nil {
		return fmt.Errorf("Failed to listen on unix socket: %v", err)
	}
	go func() {
		defer unixListener.Close()

		for {
			select {
			case <-proxy.done:
				return
			default:
				unixConn, err := unixListener.Accept()
				if err != nil {
					logrus.Warnf("Accept error: %v", err)
					continue
				}

				go proxy.handleConnection(unixConn)
			}
		}
	}()

	logrus.Debugf("Started proxy at: %s", proxy.socket)

	return nil
}

func (proxy *Proxy) handleConnection(unixConn net.Conn) {
	defer unixConn.Close()

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		logrus.Errorf("vsock socket error: %v", err)
		return
	}

	sa := &unix.SockaddrVM{CID: uint32(proxy.cid), Port: uint32(proxy.port)}
	if err := unix.Connect(fd, sa); err != nil {
		logrus.Debugf("Failed to connect error: %v", err)
		return
	}

	vconnFile := os.NewFile(uintptr(fd), "vsock")
	if vconnFile == nil {
		logrus.Error("Failed to create os.File from fd")
		unix.Close(fd)
		return
	}
	defer vconnFile.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	go proxy.proxyFileToConn(ctx, vconnFile, unixConn, errCh)
	go proxy.proxyConnToFile(ctx, unixConn, vconnFile, errCh)

	// Wait for the first error or cancellation
	select {
	case <-proxy.done:
	case err := <-errCh:
		if err != nil && err != io.EOF {
			logrus.Errorf("proxy error: %v", err)
		}
	}
}

func (proxy *Proxy) proxyFileToConn(ctx context.Context, file *os.File, conn net.Conn, errCh chan error) {
	go func() {
		_, err := io.Copy(conn, file)
		errCh <- err
	}()
	select {
	case <-ctx.Done():
	case <-proxy.done:
	case <-errCh:
	}
}

func (proxy *Proxy) proxyConnToFile(ctx context.Context, conn net.Conn, file *os.File, errCh chan error) {
	go func() {
		_, err := io.Copy(file, conn)
		errCh <- err
	}()
	select {
	case <-ctx.Done():
	case <-proxy.done:
	case <-errCh:
	}
}
