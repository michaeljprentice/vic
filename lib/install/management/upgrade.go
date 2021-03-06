// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package management

import (
	"context"
	"fmt"
	"path"
	"strings"

	log "github.com/Sirupsen/logrus"

	"github.com/docker/docker/opts"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/vic/lib/config"
	"github.com/vmware/vic/lib/install/data"
	"github.com/vmware/vic/pkg/errors"
	"github.com/vmware/vic/pkg/trace"
	"github.com/vmware/vic/pkg/vsphere/extraconfig"
	"github.com/vmware/vic/pkg/vsphere/extraconfig/vmomi"
	"github.com/vmware/vic/pkg/vsphere/tasks"
	"github.com/vmware/vic/pkg/vsphere/vm"
)

const (
	UpgradePrefix = "upgrade for"
)

// Upgrade will try to upgrade vch appliance to new version. If failed will try to roll back to original status.
func (d *Dispatcher) Upgrade(vch *vm.VirtualMachine, conf *config.VirtualContainerHostConfigSpec, settings *data.InstallerData) (err error) {
	defer trace.End(trace.Begin(conf.Name))

	d.appliance = vch

	// update the displayname to the actual folder name used
	if d.vmPathName, err = d.appliance.FolderName(d.ctx); err != nil {
		log.Errorf("Failed to get canonical name for appliance: %s", err)
		return err
	}

	ds, err := d.session.Finder.Datastore(d.ctx, conf.ImageStores[0].Host)
	if err != nil {
		err = errors.Errorf("Failed to find image datastore %q", conf.ImageStores[0].Host)
		return err
	}
	d.session.Datastore = ds
	if !conf.HostCertificate.IsNil() {
		d.VICAdminProto = "https"
		d.DockerPort = fmt.Sprintf("%d", opts.DefaultTLSHTTPPort)
	} else {
		d.VICAdminProto = "http"
		d.DockerPort = fmt.Sprintf("%d", opts.DefaultHTTPPort)
	}

	if err = d.uploadImages(settings.ImageFiles); err != nil {
		return errors.Errorf("Uploading images failed with %s. Exiting...", err)
	}

	conf.BootstrapImagePath = fmt.Sprintf("[%s] %s/%s", conf.ImageStores[0].Host, d.vmPathName, settings.BootstrapISO)

	// ensure that we wait for components to come up
	for _, s := range conf.ExecutorConfig.Sessions {
		s.Started = ""
	}

	snapshotName := fmt.Sprintf("%s %s", UpgradePrefix, conf.Version.BuildNumber)
	snapshotName = strings.TrimSpace(snapshotName)
	snapshotRefID, err := d.createSnapshot(snapshotName, "upgrade snapshot")
	if err != nil {
		d.deleteUpgradeImages(ds, settings)
		return err
	}
	defer func() {
		if err == nil {
			// do clean up aggressively, even the previous operation failed with context deadline excceeded.
			d.deleteSnapshot(*snapshotRefID, snapshotName, conf.Name)
		}
	}()

	if err = d.update(conf, settings); err == nil {
		return nil
	}
	log.Errorf("Failed to upgrade: %s", err)
	log.Infof("Rolling back upgrade")

	// reset timeout, to make sure rollback still happens in case of deadline exceeded error in previous step
	var cancel context.CancelFunc
	d.ctx, cancel = context.WithTimeout(context.Background(), settings.RollbackTimeout)
	defer cancel()

	if rerr := d.rollback(conf, snapshotName); rerr != nil {
		log.Errorf("Failed to revert appliance to snapshot: %s", rerr)
		// return the error message for upgrade, instead of rollback
		return err
	}

	d.deleteUpgradeImages(ds, settings)
	log.Infof("Appliance is rollback to old version")
	return err
}

func (d *Dispatcher) deleteSnapshot(id types.ManagedObjectReference, snapshotName string, applianceName string) error {
	defer trace.End(trace.Begin(snapshotName))
	log.Infof("Deleting upgrade snapshot %q", snapshotName)
	// do clean up aggressively, even the previous operation failed with context deadline excceeded.
	d.ctx = context.Background()
	if _, err := tasks.WaitForResult(d.ctx, func(ctx context.Context) (tasks.Task, error) {
		return d.appliance.RemoveSnapshot(ctx, id, true, true)
	}); err != nil {
		log.Errorf("Failed to clean up appliance upgrade snapshot %q: %s.", snapshotName, err)
		log.Errorf("Snapshot %q of appliance virtual machine %q MUST be removed manually before upgrade again", snapshotName, applianceName)
		return err
	}
	return nil
}

func (d *Dispatcher) createSnapshot(name string, desc string) (*types.ManagedObjectReference, error) {
	defer trace.End(trace.Begin(name))
	log.Infof("Creating snapshot %s", name)

	snapRefID, err := d.tryCreateSnapshot(name, desc)
	if err == nil {
		log.Infof("created snapshot %s", snapRefID)
		return snapRefID, nil
	}
	log.Error(err)
	return nil, err
}

// tryCreateSnapshot try to create upgrade snapshot. It will check if upgrade snapshot already exists. If exists, return error.
// if succeed, return snapshot refID
func (d *Dispatcher) tryCreateSnapshot(name, desc string) (*types.ManagedObjectReference, error) {
	defer trace.End(trace.Begin(name))

	upgrading, snapshot, err := d.appliance.UpgradeInProgress(d.ctx, UpgradePrefix)
	if err != nil {
		return nil, err
	}
	if upgrading {
		return nil, errors.Errorf("Detected another upgrade process in progress. If this is incorrect, manually remove appliance snapshot %q and restart upgrade", snapshot)
	}

	taskInfo, err := tasks.WaitForResult(d.ctx, func(ctx context.Context) (tasks.Task, error) {
		return d.appliance.CreateSnapshot(d.ctx, name, desc, true, false)
	})
	if err != nil {
		return nil, errors.Errorf("Failed to create upgrade snapshot %q: %s.", name, err)
	}
	ref := taskInfo.Result.(types.ManagedObjectReference)
	return &ref, nil
}

func (d *Dispatcher) deleteUpgradeImages(ds *object.Datastore, settings *data.InstallerData) {
	defer trace.End(trace.Begin(""))

	log.Infof("Deleting upgrade images")

	// do clean up aggressively, even the previous operation failed with context deadline excceeded.
	d.ctx = context.Background()

	m := object.NewFileManager(ds.Client())

	file := ds.Path(path.Join(d.vmPathName, settings.ApplianceISO))
	if err := d.deleteVMFSFiles(m, ds, file); err != nil {
		log.Warnf("Image file %q is not removed for %s. Use the vSphere UI to delete content", file, err)
	}

	file = ds.Path(path.Join(d.vmPathName, settings.BootstrapISO))
	if err := d.deleteVMFSFiles(m, ds, file); err != nil {
		log.Warnf("Image file %q is not removed for %s. Use the vSphere UI to delete content", file, err)
	}
}

func (d *Dispatcher) update(conf *config.VirtualContainerHostConfigSpec, settings *data.InstallerData) error {
	defer trace.End(trace.Begin(conf.Name))

	power, err := d.appliance.PowerState(d.ctx)
	if err != nil {
		log.Errorf("Failed to get vm power status %q: %s", d.appliance.Reference(), err)
		return err
	}
	if power != types.VirtualMachinePowerStatePoweredOff {
		if _, err = tasks.WaitForResult(d.ctx, func(ctx context.Context) (tasks.Task, error) {
			return d.appliance.PowerOff(ctx)
		}); err != nil {
			log.Errorf("Failed to power off appliance: %s", err)
			return err
		}
	}

	if err = d.reconfigVCH(conf, fmt.Sprintf("[%s] %s/%s", conf.ImageStores[0].Host, d.vmPathName, settings.ApplianceISO)); err != nil {
		return err
	}

	return d.startAppliance(conf)
}

func (d *Dispatcher) rollback(conf *config.VirtualContainerHostConfigSpec, snapshot string) error {
	defer trace.End(trace.Begin(fmt.Sprintf("old appliance iso: %q, snapshot: %q", d.oldApplianceISO, snapshot)))

	// do not power on appliance in this snapsthot revert
	log.Infof("Reverting to snapshot %s", snapshot)
	if _, err := tasks.WaitForResult(d.ctx, func(ctx context.Context) (tasks.Task, error) {
		return d.appliance.RevertToSnapshot(d.ctx, snapshot, true)
	}); err != nil {
		return errors.Errorf("Failed to roll back upgrade: %s.", err)
	}

	return d.ensureRollbackReady(conf)
}

func (d *Dispatcher) ensureRollbackReady(conf *config.VirtualContainerHostConfigSpec) error {
	defer trace.End(trace.Begin(conf.Name))

	power, err := d.appliance.PowerState(d.ctx)
	if err != nil {
		log.Errorf("Failed to get vm power status %q after rollback: %s", d.appliance.Reference(), err)
		return err
	}
	if power == types.VirtualMachinePowerStatePoweredOff {
		log.Infof("Roll back finished - Appliance is kept in powered off status")
		return nil
	}
	return d.startAppliance(conf)
}

func (d *Dispatcher) reconfigVCH(conf *config.VirtualContainerHostConfigSpec, isoFile string) error {
	defer trace.End(trace.Begin(isoFile))

	spec := &types.VirtualMachineConfigSpec{}

	deviceChange, err := d.switchISO(isoFile)
	if err != nil {
		return err
	}

	spec.DeviceChange = deviceChange

	if conf != nil {
		cfg := make(map[string]string)
		extraconfig.Encode(extraconfig.MapSink(cfg), conf)
		spec.ExtraConfig = append(spec.ExtraConfig, vmomi.OptionValueFromMap(cfg)...)
	}

	if spec.DeviceChange == nil && spec.ExtraConfig == nil {
		// nothing need to do
		log.Debugf("Nothing changed, no need to reconfigure appliance")
		return nil
	}

	// reconfig
	log.Infof("Setting VM configuration")
	info, err := tasks.WaitForResult(d.ctx, func(ctx context.Context) (tasks.Task, error) {
		return d.appliance.Reconfigure(ctx, *spec)
	})

	if err != nil {
		log.Errorf("Error while reconfiguring appliance: %s", err)
		return err
	}
	if info.State != types.TaskInfoStateSuccess {
		log.Errorf("Reconfiguring appliance reported: %s", info.Error.LocalizedMessage)
		return err
	}
	return nil
}

func (d *Dispatcher) switchISO(filePath string) ([]types.BaseVirtualDeviceConfigSpec, error) {
	defer trace.End(trace.Begin(filePath))

	var devices object.VirtualDeviceList
	var err error

	log.Infof("Switching appliance iso to %s", filePath)
	devices, err = d.appliance.Device(d.ctx)
	if err != nil {
		log.Errorf("Failed to get vm devices for appliance: %s", err)
		return nil, err
	}
	// find the single cdrom
	cd, err := devices.FindCdrom("")
	if err != nil {
		log.Errorf("Failed to get CD rom device from appliance: %s", err)
		return nil, err
	}

	oldApplianceISO := cd.Backing.(*types.VirtualCdromIsoBackingInfo).FileName
	if oldApplianceISO == filePath {
		log.Debugf("Target file name %q is same to old one, no need to change.")
		return nil, nil
	}
	cd = devices.InsertIso(cd, filePath)
	changedDevices := object.VirtualDeviceList([]types.BaseVirtualDevice{cd})

	deviceChange, err := changedDevices.ConfigSpec(types.VirtualDeviceConfigSpecOperationEdit)
	if err != nil {
		log.Errorf("Failed to create config spec for appliance: %s", err)
		return nil, err
	}

	d.oldApplianceISO = oldApplianceISO
	return deviceChange, nil
}
