//
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/ciao-project/ciao/deviceinfo"
	"github.com/intel/ccloudvm/types"
	"github.com/pkg/errors"
)

type backend interface {
	createInstance(context.Context, chan interface{}, chan<- downloadRequest, *types.CreateArgs) error
	start(context.Context, string, *types.VMSpec) error
	stop(context.Context, string) error
	quit(context.Context, string) error
	status(context.Context, string) (*types.InstanceDetails, error)
	deleteInstance(context.Context, string) error
}

type ccvmBackend struct{}

func checkMemAvailable(in *types.VMSpec) error {
	_, available := deviceinfo.GetMemoryInfo()
	if available == -1 {
		return fmt.Errorf("Unable to compute memory statistics of host device")
	}

	if in.MemMiB > available {
		return fmt.Errorf("Host device has only %d MiB of RAM available", available)
	}

	return nil
}

func prepareCreate(ctx context.Context, args *types.CreateArgs) (*workload, *workspace, *http.Transport, error) {
	ws, err := prepareEnv(ctx, args.Name)
	if err != nil {
		return nil, nil, nil, err
	}

	ws.HTTPProxy = args.HTTPProxy
	ws.HTTPSProxy = args.HTTPSProxy
	ws.NoProxy = args.NoProxy
	ws.GoPath = args.GoPath

	transport := getHTTPTransport(ws.HTTPProxy, ws.HTTPSProxy, ws.NoProxy)

	wkld, err := createWorkload(ctx, ws, args.WorkloadName, transport)
	if err != nil {
		return nil, nil, nil, err
	}

	in := &wkld.spec.VM

	err = in.MergeCustom(&args.CustomSpec)
	if err != nil {
		return nil, nil, nil, err
	}

	ws.Mounts = in.Mounts
	ws.Hostname = args.Name
	if ws.NoProxy != "" {
		ws.NoProxy = fmt.Sprintf("%s,%s,10.0.2.2", ws.Hostname, ws.NoProxy)
	} else if ws.HTTPProxy != "" || ws.HTTPSProxy != "" {
		ws.NoProxy = fmt.Sprintf("%s,10.0.2.2", ws.Hostname)
	}

	ws.PackageUpgrade = "false"
	if args.Update {
		ws.PackageUpgrade = "true"
	}

	if wkld.spec.NeedsNestedVM && !hostSupportsNestedKVM() {
		return nil, nil, nil, fmt.Errorf("nested KVM is not enabled.  Please enable and try again")
	}

	if err := checkMemAvailable(in); err != nil {
		return nil, nil, nil, err
	}

	return wkld, ws, transport, nil
}

func downloadProgress(resultCh chan interface{}, p progress) {
	if p.totalMB >= 0 {
		resultCh <- types.CreateResult{
			Line: fmt.Sprintf("Downloaded %d MB of %d\n", p.downloadedMB, p.totalMB),
		}
	} else {
		resultCh <- types.CreateResult{
			Line: fmt.Sprintf("Downloaded %d MB\n", p.downloadedMB),
		}
	}
}

func (c ccvmBackend) createInstance(ctx context.Context, resultCh chan interface{},
	downloadCh chan<- downloadRequest, args *types.CreateArgs) error {
	var err error

	wkld, ws, transport, err := prepareCreate(ctx, args)
	if err != nil {
		return err
	}

	_, err = os.Stat(ws.instanceDir)
	if err == nil {
		return fmt.Errorf("instance already exists")
	}

	err = os.MkdirAll(ws.instanceDir, 0755)
	if err != nil {
		return errors.Wrap(err, "unable to create cache dir")
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(ws.instanceDir)
		}
	}()

	err = prepareSSHKeys(ctx, ws)
	if err != nil {
		return err
	}

	listener, port, err := createLocalListener()
	if err != nil {
		return err
	}
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
	}()
	ws.HTTPServerPort = port

	err = wkld.generateCloudConfig(ws)
	if err != nil {
		return errors.Wrap(err, "Error applying template to user-data")
	}

	err = wkld.save(ws.instanceDir)
	if err != nil {
		return errors.Wrap(err, "Unable to save instance state")
	}

	resultCh <- types.CreateResult{
		Line: fmt.Sprintf("Downloading %s\n", wkld.spec.BaseImageName),
	}

	qcowPath, err := downloadFile(ctx, downloadCh, transport, wkld.spec.BaseImageURL, func(p progress) {
		downloadProgress(resultCh, p)
	})
	if err != nil {
		return err
	}

	err = buildISOImage(ctx, resultCh, wkld.mergedUserData, ws, args.Debug)
	if err != nil {
		return err
	}

	spec := wkld.spec.VM
	err = createRootfs(ctx, qcowPath, ws.instanceDir, spec.DiskGiB)
	if err != nil {
		return err
	}

	resultCh <- types.CreateResult{
		Line: fmt.Sprintf("Booting VM with %d MiB RAM and %d cpus\n", spec.MemMiB, spec.CPUs),
	}

	err = bootVM(ctx, ws, args.Name, &spec)
	if err != nil {
		return err
	}

	err = manageInstallation(ctx, resultCh, downloadCh, transport, listener, ws.instanceDir)

	// Ownership of listener passes to manageInstallation
	listener = nil
	if err != nil {
		return err
	}

	resultCh <- types.CreateResult{
		Line: fmt.Sprintf("VM successfully created!\n"),
	}

	return nil
}

func (c ccvmBackend) start(ctx context.Context, name string, customSpec *types.VMSpec) error {
	ws, err := prepareEnv(ctx, name)
	if err != nil {
		return err
	}

	wkld, err := restoreWorkload(ws)
	if err != nil {
		return err
	}
	in := &wkld.spec.VM

	err = in.MergeCustom(customSpec)
	if err != nil {
		return err
	}

	defaults := defaultVMSpec()
	if in.MemMiB == 0 {
		in.MemMiB = defaults.MemMiB
	}
	if in.CPUs == 0 {
		in.CPUs = defaults.CPUs
	}

	if wkld.spec.NeedsNestedVM && !hostSupportsNestedKVM() {
		return fmt.Errorf("nested KVM is not enabled.  Please enable and try again")
	}

	if err := checkMemAvailable(in); err != nil {
		return err
	}

	if err := wkld.save(ws.instanceDir); err != nil {
		fmt.Printf("Warning: Failed to update instance state: %v", err)
	}

	fmt.Printf("Booting VM with %d MiB RAM and %d cpus\n", in.MemMiB, in.CPUs)

	err = bootVM(ctx, ws, name, in)
	if err != nil {
		return err
	}

	fmt.Println("VM Started")

	return nil
}

func (c ccvmBackend) stop(ctx context.Context, name string) error {
	ws, err := prepareEnv(ctx, name)
	if err != nil {
		return err
	}

	err = stopVM(ctx, ws.instanceDir)
	if err != nil {
		return err
	}

	fmt.Println("VM Stopped")

	return nil
}

func (c ccvmBackend) quit(ctx context.Context, name string) error {
	ws, err := prepareEnv(ctx, name)
	if err != nil {
		return err
	}

	err = quitVM(ctx, ws.instanceDir)
	if err != nil {
		return err
	}

	fmt.Println("VM Quit")

	return nil
}

func (c ccvmBackend) status(ctx context.Context, name string) (*types.InstanceDetails, error) {
	ws, err := prepareEnv(ctx, name)
	if err != nil {
		return nil, err
	}

	wkld, err := restoreWorkload(ws)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to load instance state")
	}
	in := &wkld.spec.VM

	sshPort, err := in.SSHPort()
	if err != nil {
		return nil, fmt.Errorf("Instance does not have SSH port open.  Unable to determine status")
	}

	return &types.InstanceDetails{
		Name: name,
		SSH: types.SSHDetails{
			KeyPath: ws.keyPath,
			Port:    sshPort,
		},
		Workload: wkld.spec.WorkloadName,
		VMSpec:   *in,
	}, nil
}

func (c ccvmBackend) deleteInstance(ctx context.Context, name string) error {
	ws, err := prepareEnv(ctx, name)
	if err != nil {
		return err
	}

	_ = quitVM(ctx, ws.instanceDir)
	err = os.RemoveAll(ws.instanceDir)
	if err != nil {
		return errors.Wrap(err, "unable to delete instance")
	}

	return nil
}
