package service

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"text/template"
	"time"

	appservice "github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/archive"
	"github.com/gravitational/gravity/lib/blob/fs"
	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/pack/encryptedpack"
	"github.com/gravitational/gravity/lib/pack/localpack"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/storage/keyval"
	fileutils "github.com/gravitational/gravity/lib/utils"

	"github.com/ghodss/yaml"
	"github.com/gravitational/license/authority"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// GetAppInstaller builds an installer package for the
// specified application and returns a reader for the contents.
//
// Steps to generate an installer:
//
//  * copy the gravity binary as ./gravity
//  * start new backend as ./gravity.db to persist package metadata
//  * start new package service in ./packages
//  * import {web-assets,gravity,dns,teleport,planet-master,planet-node,application}
//    packages from application package service into local package service running
//    in ./packages
//
func (r *applications) GetAppInstaller(req appservice.InstallerRequest) (installer io.ReadCloser, err error) {
	if err := req.Check(); err != nil {
		return nil, trace.Wrap(err)
	}

	var tempDir string
	tempDir, err = ioutil.TempDir("", "installer")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tempDir)
		}
	}()

	backendPath := filepath.Join(tempDir, "gravity.db")
	var localBackend storage.Backend
	localBackend, err = keyval.NewBolt(keyval.BoltConfig{
		Path: backendPath,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	objects, err := fs.New(filepath.Join(tempDir, defaults.PackagesDir))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var localPackages pack.PackageService
	localPackages, err = localpack.New(localpack.Config{
		Backend:     localBackend,
		UnpackedDir: filepath.Join(tempDir, defaults.PackagesDir, defaults.UnpackedDir),
		Objects:     objects,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if req.EncryptionKey != "" {
		localPackages = encryptedpack.New(localPackages, req.EncryptionKey)
	}

	localApps, err := New(Config{
		Backend:  localBackend,
		Packages: localPackages,
		StateDir: tempDir,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	_, err = localBackend.CreateAccount(req.Account)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var app *appservice.Application
	app, err = r.GetApp(req.Application)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	manifestBytes, err := yaml.Marshal(app.Manifest)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err = pullDependencies(app, localApps, r, r.FieldLogger); err != nil {
		return nil, trace.Wrap(err)
	}

	var binary *archive.Item
	binary, err = r.getGravityBinaryForApp(app)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = addCertificateAuthority(req, localPackages)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = addTrustedCluster(req, localPackages)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	reader, writer := io.Pipe()
	go func() {
		uploadScript, err := renderUploadScript(*app)
		if err != nil {
			r.Warnf("Failed to render upload script: %v.", trace.DebugReport(err))
			if errClose := writer.CloseWithError(err); errClose != nil {
				r.Warnf("Failed to close writer: %v.", errClose)
			}
			return
		}

		err = archive.CompressDirectory(
			tempDir, writer, binary,
			archive.ItemFromStringMode(
				defaults.ManifestFileName, string(manifestBytes), defaults.SharedReadMask),
			archive.ItemFromStringMode(
				installScriptFilename, installScript, defaults.SharedExecutableMask),
			archive.ItemFromStringMode(
				uploadScriptFilename, string(uploadScript), defaults.SharedExecutableMask),
			archive.ItemFromStringMode(
				upgradeScriptFilename, upgradeScript, defaults.SharedExecutableMask),
			archive.ItemFromStringMode(
				readmeFilename, readme, defaults.SharedReadMask))
		writer.CloseWithError(err)
	}()
	return &fileutils.CleanupReadCloser{
		ReadCloser: reader,
		Cleanup: func() {
			err := os.RemoveAll(tempDir)
			if err != nil {
				r.Warnf("Failed to delete %v: %v.", tempDir, trace.DebugReport(err))
			}
		},
	}, nil
}

func renderUploadScript(app appservice.Application) (uploadScript []byte, err error) {
	var buf bytes.Buffer
	err = uploadScriptTemplate.Execute(&buf, &struct{}{})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return buf.Bytes(), nil
}

func (r *applications) getGravityBinaryForApp(app *appservice.Application) (*archive.Item, error) {
	var gravityPackage *loc.Locator
	gravityPackage, err := app.Manifest.Dependencies.ByName(constants.GravityPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var envelope *pack.PackageEnvelope
	var packageBytes io.ReadCloser
	envelope, packageBytes, err = r.Packages.ReadPackage(*gravityPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return archive.ItemFromStream("gravity", packageBytes, envelope.SizeBytes, defaults.SharedExecutableMask), nil
}

// pullDependencies transitively pulls all dependent packages for app to localApps
func pullDependencies(app *appservice.Application, localApps *applications, remoteApps *applications, log log.FieldLogger) error {
	dependencies, err := appservice.GetDependencies(app, remoteApps)
	if err != nil {
		return trace.Wrap(err)
	}

	if err = pullPackages(dependencies.Packages, localApps.Packages, remoteApps.Packages, log); err != nil {
		return trace.Wrap(err)
	}

	apps := dependencies.Apps
	apps = append(apps, app.Package)
	if err = pullApplications(apps, localApps, remoteApps, log); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// pullPackages pulls package locators from remotePackages to localPackages
func pullPackages(locators []loc.Locator, localPackages pack.PackageService, remotePackages pack.PackageService, log log.FieldLogger) error {
	log.Infof("Pulling packages %v.", locators)

	for _, locator := range locators {
		envelope, reader, err := remotePackages.ReadPackage(locator)
		if err != nil {
			return trace.Wrap(err)
		}
		defer reader.Close()

		err = localPackages.UpsertRepository(locator.Repository, time.Time{})
		if err != nil {
			return trace.Wrap(err)
		}
		_, err = localPackages.CreatePackage(envelope.Locator, reader, pack.WithLabels(envelope.RuntimeLabels))
		if err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

// pullApplications pulls applications specified with locators from remoteApps to localApps
func pullApplications(locators []loc.Locator, localApps *applications, remoteApps *applications, log log.FieldLogger) error {
	log.Infof("Pulling applications %v.", locators)

	for _, locator := range locators {
		envelope, reader, err := remoteApps.Packages.ReadPackage(locator)
		if err != nil {
			return trace.Wrap(err)
		}
		defer reader.Close()

		var labels map[string]string
		_, err = localApps.CreateAppWithManifest(envelope.Locator, envelope.Manifest, reader, labels)
		if err != nil && !trace.IsAlreadyExists(err) {
			return trace.Wrap(err)
		}
	}

	return nil
}

// addCertificateAuthority makes the certificate authority package from the provided CA and key
// and puts it alongside other installer packages
func addCertificateAuthority(req appservice.InstallerRequest, destPackages pack.PackageService) error {
	if req.CACert == "" {
		return nil // nothing to do
	}
	return trace.Wrap(pack.CreateCertificateAuthority(pack.CreateCAParams{
		Packages: destPackages,
		KeyPair: authority.TLSKeyPair{
			CertPEM: []byte(req.CACert),
		}}))
}

// addTrustedCluster creates packages with trusted cluster spec provided in
// the request in the installer package service, so clusters can connect to
// it during the installation
func addTrustedCluster(req appservice.InstallerRequest, dst pack.PackageService) error {
	cluster := req.TrustedCluster
	if cluster == nil {
		return nil
	}
	// remote support will be available but turned off by default
	cluster.SetEnabled(false)
	data, err := storage.MarshalTrustedCluster(cluster)
	if err != nil {
		return trace.Wrap(err)
	}
	_, err = dst.CreatePackage(loc.TrustedCluster, bytes.NewReader(data))
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

const (
	installScript = `#!/bin/sh
#
# Installation script for Gravity-powered multi-host Linux applications.
#

REQMSG="This installer requires a 64-bit Linux desktop"

# The entry point
main() {
    case $(uname) in
        "Linux")
            arch=$(uname -m)
            if [ $arch = "x86_64" ]; then
                launchInstaller "$@"
            fi
            ;;
        "Darwin") osxError
            ;;
    esac
    echo $REQMSG
    exit 1
}

# shows a graphical UI popup to OSX users who click on this
# file in Finder
osxError() {
  osascript <<EOM
    tell app "System Events"
      display dialog "$REQMSG" buttons {"OK"} default button 1 with icon caution with title "Installer"
      return  -- Suppress result
    end tell
EOM
  exit 1
}

launchInstaller() {
    # make the directory of the script current
    # and launch the install wizard:
    cd $(dirname $0) && ./gravity wizard "$@"
    exit 0
}

main "$@"
`

	upgradeScript = `#!/bin/bash
#
# Script for upgrading the currently running application to a new version.
#

if [[ $(id -u) -ne 0 ]]; then
  echo "please run this script as root" && exit 1
fi

scriptdir=$(dirname $(realpath $0))
app=$($scriptdir/gravity app-package --state-dir=$scriptdir)
$scriptdir/upload && $scriptdir/gravity --insecure update trigger $app
`

	readme = `Requirements
============

To launch the installer you need a 64-bit Linux desktop computer
with a web browser such as Firefox or Chrome.

You also need a direct network connection to the servers
("target servers") you are installing the application to.

The target servers need to be able to connect to the computer
the installer is running on during the installation.

Starting the installer
======================

To install the application simply type in your terminal:

./install

...this should open a browser with the installer Web UI running
on localhost.

Upgrading the installed application
===================================

There are two ways to upgrade the currently running application to a new
from this tarball.

You can launch:

./upload

to upload the application update package to locally running site
and then launch the update operation from UI.

Or launch:

./upgrade

which will upload the new application version to locally running site
and start the upgrade procedure.

The upgrade operation progress can be monitored via UI or using gravity
status command.
`

	installScriptFilename = "install"
	uploadScriptFilename  = "upload"
	upgradeScriptFilename = "upgrade"
	readmeFilename        = "README"
)

var uploadScriptTemplate = template.Must(template.New("uploadScript").Parse(`#!/bin/bash
#
# Script for uploading new application version to installed site.
#
# Copyright 2016 Gravitational, Inc.
#
# This file is licensed under the Apache License, Version 2.0
# (the "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
./gravity --insecure update upload --state-dir=.
`))