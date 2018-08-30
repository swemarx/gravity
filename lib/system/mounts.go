package system

import (
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/systemservice"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// Mount creates a new mount based on the given configuration.
// The mount is created as a systemd mount unit named service.
func Mount(config MountConfig, service string, services systemservice.ServiceManager) error {
	spec := systemservice.MountServiceSpec{
		Where: config.Where,
		What:  storage.DeviceName(config.What).Path(),
		Type:  config.Filesystem,
	}
	req := systemservice.NewMountServiceRequest{
		ServiceSpec: spec,
		Name:        service,
	}

	err := services.StopService(service)
	if err != nil {
		log.Warnf("Error stopping service %v: %v.", service, trace.DebugReport(err))
	}

	err = services.InstallMountService(req)
	if err != nil {
		return trace.Wrap(err, "failed to install mount service %q", service)
	}
	return nil
}

// Unmount uninstalls the specified mount service.
func Unmount(service string, services systemservice.ServiceManager) error {
	status, err := services.StatusService(service)
	if err != nil {
		return trace.Wrap(err)
	}

	log.Debugf("Mount service is %q.", status)
	err = services.UninstallService(service)
	if err != nil {
		return trace.Wrap(err, "failed to uninstall mount service %q", service)
	}

	return nil
}

// MountConfig describes configuration to mount a directory
// on a specific device and filesystem
//
// See https://www.freedesktop.org/software/systemd/man/systemd.mount.html
type MountConfig struct {
	// What specifies defines the absolute path of a device node, file or other resource to mount
	What storage.DeviceName
	// Where specifies the absolute path of a directory for the mount point
	Where string
	// Filesystem specifies the file system type
	Filesystem string
	// Options lists mount options to use when mounting
	Options []string
}