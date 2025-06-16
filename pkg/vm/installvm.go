package vm

import (
	"fmt"

	"math/rand"

	"github.com/containers/podman-bootc/pkg/vm/domain"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
)

const (
	CIDInstallVM = 3
	VSOCKPort    = 1234
)
const (
	mac    = "52:54:00:0b:dd:1e"
	imodel = "e1000"
)

const (
	StorageVirtiofsTarget = "storage"
	ConfigVirtiofsTarget  = "config"
	OutputVirtiofsTarget  = "output"
)

type InstallOptions struct {
	DiskImage            string
	OutputImage          string
	InputFormat          domain.DiskDriverType
	OutputFormat         domain.DiskDriverType
	ContainerStoragePath string
	ConfigPath           string
	OutputPath           string
	Root                 bool
}

type InstallVM struct {
	libvirtURI string
	domain     string
	opts       InstallOptions
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOQRSTUVWXYZ0123456789"

func RandomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func NewInstallVM(opts InstallOptions) *InstallVM {
	uri := "qemu:///session"
	if opts.Root {
		uri = "qemu:///system"
	}
	name := "bootc-" + RandomString(5)
	return &InstallVM{
		domain:     name,
		libvirtURI: uri,
		opts:       opts,
	}
}

func (vm *InstallVM) newDomain() *libvirtxml.Domain {
	return domain.NewDomain(
		domain.WithName(vm.domain),
		domain.WithUUID(uuid.New().String()),
		domain.WithKVM(),
		domain.WithOS(),
		domain.WithMemory(2048),
		domain.WithMemoryBackingForVirtiofs(),
		domain.WithCPUHostModel(),
		domain.WithVCPUs(2),
		domain.WithSerialConsole(),
		domain.WithVSOCK(CIDInstallVM),
		domain.WithInterface(mac, imodel),
		domain.WithDisk(vm.opts.DiskImage, "input", "vda", vm.opts.InputFormat, domain.DiskBusVirtio),
		domain.WithDisk(vm.opts.OutputImage, "output", "vdb", vm.opts.OutputFormat, domain.DiskBusVirtio),
		domain.WithFilesystem(vm.opts.ContainerStoragePath, StorageVirtiofsTarget),
		domain.WithFilesystem(vm.opts.ConfigPath, ConfigVirtiofsTarget),
		domain.WithFilesystem(vm.opts.OutputPath, OutputVirtiofsTarget),
	)
}

func (vm *InstallVM) Run() error {
	domainXML, err := vm.newDomain().Marshal()
	if err != nil {
		return err
	}
	conn, err := libvirt.NewConnect(vm.libvirtURI)
	if err != nil {
		return err
	}
	_, err = conn.DomainDefineXMLFlags(domainXML, libvirt.DOMAIN_DEFINE_VALIDATE)
	if err != nil {
		return fmt.Errorf("unable to define virtual machine domain: %w", err)
	}
	dom, err := conn.LookupDomainByName(vm.domain)
	if err != nil {
		return err
	}
	defer dom.Free()
	err = dom.Create()
	if err != nil {
		return fmt.Errorf("Failed to start domain: %v", err)
	}
	logrus.Debugf("Domain %s started successfully.", vm.domain)

	return nil
}

func (vm *InstallVM) Stop() error {
	conn, err := libvirt.NewConnect(vm.libvirtURI)
	if err != nil {
		return err
	}
	dom, err := conn.LookupDomainByName(vm.domain)
	if err != nil {
		return err
	}
	defer dom.Free()
	if err := dom.Destroy(); err != nil {
		logrus.Warningf("Failed to destroy the domain %s, maybe already stopped: %v", vm.domain, err)
	}
	if err := dom.Undefine(); err != nil {
		return fmt.Errorf("Undefine failed: %v", err)
	}
	logrus.Debugf("Domain %s stopped and deleted successfully", vm.domain)

	return nil
}
