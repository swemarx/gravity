package service

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/app/docker"
	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/utils"
	"github.com/gravitational/trace"

	log "github.com/sirupsen/logrus"
)

// SyncRequest describes a request to sync an application with registry
type SyncRequest struct {
	PackService  pack.PackageService
	AppService   app.Applications
	ImageService docker.ImageService
	Package      loc.Locator
}

// SyncApp syncs an application and all its dependencies with registry
func SyncApp(ctx context.Context, req SyncRequest) error {
	application, err := req.AppService.GetApp(req.Package)
	if err != nil {
		return trace.Wrap(err)
	}

	// sync base app
	base := application.Manifest.Base()
	if base != nil {
		err = SyncApp(ctx, SyncRequest{
			PackService:  req.PackService,
			AppService:   req.AppService,
			ImageService: req.ImageService,
			Package:      *base,
		})
		if err != nil {
			return trace.Wrap(err)
		}
	}

	// sync dependencies
	for _, dep := range application.Manifest.Dependencies.Apps {
		err = SyncApp(ctx, SyncRequest{
			PackService:  req.PackService,
			AppService:   req.AppService,
			ImageService: req.ImageService,
			Package:      dep.Locator,
		})
		if err != nil {
			return trace.Wrap(err)
		}
	}

	// the app will be unpacked at this dir
	dir, err := ioutil.TempDir("", "sync")
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		err := os.RemoveAll(dir)
		if err != nil {
			log.Warningf("failed to remove %v: %v", dir, trace.DebugReport(err))
		}
	}()

	// unpack the app and sync its registry with the local registry
	unpackedPath := pack.PackagePath(dir, req.Package)
	if err = pack.Unpack(req.PackService, req.Package, unpackedPath, nil); err != nil {
		return trace.Wrap(err)
	}

	syncPath := filepath.Join(unpackedPath, "registry")

	// check if the registry dir exists at all
	if exists, _ := utils.IsDirectory(syncPath); !exists {
		log.Warningf("registry dir does not exist, skipping sync: %v", syncPath)
		return nil
	}

	// registry dir exists, check if it has any contents
	empty, err := utils.IsDirectoryEmpty(syncPath)
	if err != nil {
		return trace.Wrap(err)
	}
	if empty {
		log.Warningf("registry directory is empty, skipping sync: %v", syncPath)
		return nil
	}

	log.Infof("syncing %v", req.Package)

	if _, err = req.ImageService.Sync(ctx, syncPath); err != nil {
		return trace.Wrap(err)
	}

	return nil
}