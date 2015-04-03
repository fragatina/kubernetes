/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws_pd

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider/aws"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/types"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/exec"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/mount"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/volume"
	"github.com/golang/glog"
)

// This is the primary entrypoint for volume plugins.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{&awsPersistentDiskPlugin{nil}}
}

type awsPersistentDiskPlugin struct {
	host volume.VolumeHost
}

var _ volume.VolumePlugin = &awsPersistentDiskPlugin{}

const (
	awsPersistentDiskPluginName = "kubernetes.io/aws-pd"
)

func (plugin *awsPersistentDiskPlugin) Init(host volume.VolumeHost) {
	plugin.host = host
}

func (plugin *awsPersistentDiskPlugin) Name() string {
	return awsPersistentDiskPluginName
}

func (plugin *awsPersistentDiskPlugin) CanSupport(spec *api.Volume) bool {
	if spec.AWSPersistentDisk != nil {
		return true
	}
	return false
}

func (plugin *awsPersistentDiskPlugin) GetAccessModes() []api.AccessModeType {
	return []api.AccessModeType{
		api.ReadWriteOnce,
	}
}

func (plugin *awsPersistentDiskPlugin) NewBuilder(spec *api.Volume, podRef *api.ObjectReference) (volume.Builder, error) {
	// Inject real implementations here, test through the internal function.
	return plugin.newBuilderInternal(spec, podRef.UID, &AWSDiskUtil{}, mount.New())
}

func (plugin *awsPersistentDiskPlugin) newBuilderInternal(spec *api.Volume, podUID types.UID, manager pdManager, mounter mount.Interface) (volume.Builder, error) {
	pdName := spec.AWSPersistentDisk.PDName
	fsType := spec.AWSPersistentDisk.FSType
	partition := ""
	if spec.AWSPersistentDisk.Partition != 0 {
		partition = strconv.Itoa(spec.AWSPersistentDisk.Partition)
	}
	readOnly := spec.AWSPersistentDisk.ReadOnly

	return &awsPersistentDisk{
		podUID:      podUID,
		volName:     spec.Name,
		pdName:      pdName,
		fsType:      fsType,
		partition:   partition,
		readOnly:    readOnly,
		manager:     manager,
		mounter:     mounter,
		diskMounter: &awsSafeFormatAndMount{mounter, exec.New()},
		plugin:      plugin,
	}, nil
}

func (plugin *awsPersistentDiskPlugin) NewCleaner(volName string, podUID types.UID) (volume.Cleaner, error) {
	// Inject real implementations here, test through the internal function.
	return plugin.newCleanerInternal(volName, podUID, &AWSDiskUtil{}, mount.New())
}

func (plugin *awsPersistentDiskPlugin) newCleanerInternal(volName string, podUID types.UID, manager pdManager, mounter mount.Interface) (volume.Cleaner, error) {
	return &awsPersistentDisk{
		podUID:      podUID,
		volName:     volName,
		manager:     manager,
		mounter:     mounter,
		diskMounter: &awsSafeFormatAndMount{mounter, exec.New()},
		plugin:      plugin,
	}, nil
}

// Abstract interface to PD operations.
type pdManager interface {
	// Attaches the disk to the kubelet's host machine.
	AttachAndMountDisk(pd *awsPersistentDisk, globalPDPath string) error
	// Detaches the disk from the kubelet's host machine.
	DetachDisk(pd *awsPersistentDisk) error
}

// awsPersistentDisk volumes are disk resources provided by Google Compute Engine
// that are attached to the kubelet's host machine and exposed to the pod.
type awsPersistentDisk struct {
	volName string
	podUID  types.UID
	// Unique name of the PD, used to find the disk resource in the provider.
	pdName string
	// Filesystem type, optional.
	fsType string
	// Specifies the partition to mount
	partition string
	// Specifies whether the disk will be attached as read-only.
	readOnly bool
	// Utility interface that provides API calls to the provider to attach/detach disks.
	manager pdManager
	// Mounter interface that provides system calls to mount the global path to the pod local path.
	mounter mount.Interface
	// diskMounter provides the interface that is used to mount the actual block device.
	diskMounter mount.Interface
	plugin      *awsPersistentDiskPlugin
}

func detachDiskLogError(pd *awsPersistentDisk) {
	err := pd.manager.DetachDisk(pd)
	if err != nil {
		glog.Warningf("Failed to detach disk: %v (%v)", pd, err)
	}
}

// getVolumeProvider returns the AWS Volumes interface
func (pd *awsPersistentDisk) getVolumeProvider() (aws_cloud.Volumes, error) {
	name := "aws"
	cloud, err := cloudprovider.GetCloudProvider(name, nil)
	if err != nil {
		return nil, err
	}
	volumes, ok := cloud.(aws_cloud.Volumes)
	if !ok {
		return nil, fmt.Errorf("Cloud provider does not support volumes")
	}
	return volumes, nil
}

// SetUp attaches the disk and bind mounts to the volume path.
func (pd *awsPersistentDisk) SetUp() error {
	return pd.SetUpAt(pd.GetPath())
}

// SetUpAt attaches the disk and bind mounts to the volume path.
func (pd *awsPersistentDisk) SetUpAt(dir string) error {
	// TODO: handle failed mounts here.
	mountpoint, err := mount.IsMountPoint(dir)
	glog.V(4).Infof("PersistentDisk set up: %s %v %v", dir, mountpoint, err)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if mountpoint {
		return nil
	}

	globalPDPath := makeGlobalPDName(pd.plugin.host, pd.pdName)
	if err := pd.manager.AttachAndMountDisk(pd, globalPDPath); err != nil {
		return err
	}

	flags := uintptr(0)
	if pd.readOnly {
		flags = mount.FlagReadOnly
	}

	if err := os.MkdirAll(dir, 0750); err != nil {
		// TODO: we should really eject the attach/detach out into its own control loop.
		detachDiskLogError(pd)
		return err
	}

	// Perform a bind mount to the full path to allow duplicate mounts of the same PD.
	err = pd.mounter.Mount(globalPDPath, dir, "", mount.FlagBind|flags, "")
	if err != nil {
		mountpoint, mntErr := mount.IsMountPoint(dir)
		if mntErr != nil {
			glog.Errorf("isMountpoint check failed: %v", mntErr)
			return err
		}
		if mountpoint {
			if mntErr = pd.mounter.Unmount(dir, 0); mntErr != nil {
				glog.Errorf("Failed to unmount: %v", mntErr)
				return err
			}
			mountpoint, mntErr := mount.IsMountPoint(dir)
			if mntErr != nil {
				glog.Errorf("isMountpoint check failed: %v", mntErr)
				return err
			}
			if mountpoint {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				glog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", dir)
				return err
			}
		}
		os.Remove(dir)
		// TODO: we should really eject the attach/detach out into its own control loop.
		detachDiskLogError(pd)
		return err
	}

	return nil
}

func makeGlobalPDName(host volume.VolumeHost, devName string) string {
	// Clean up the URI to be more fs-friendly
	name := devName
	name = strings.Replace(name, "://", "/", -1)
	return path.Join(host.GetPluginDir(awsPersistentDiskPluginName), "mounts", name)
}

func getPdNameFromGlobalMount(host volume.VolumeHost, globalPath string) (string, error) {
	basePath := path.Join(host.GetPluginDir(awsPersistentDiskPluginName), "mounts")
	rel, err := filepath.Rel(basePath, globalPath)
	if err != nil {
		return "", err
	}
	if strings.Contains(rel, "../") {
		return "", fmt.Errorf("Unexpected mount path: " + globalPath)
	}
	// Reverse the :// replacement done in makeGlobalPDName
	name := rel
	if strings.HasPrefix(name, "aws/") {
		name = strings.Replace(name, "aws/", "aws://")
	}
	return name, nil
}

func (pd *awsPersistentDisk) GetPath() string {
	name := awsPersistentDiskPluginName
	return pd.plugin.host.GetPodVolumeDir(pd.podUID, util.EscapeQualifiedNameForDisk(name), pd.volName)
}

// Unmounts the bind mount, and detaches the disk only if the PD
// resource was the last reference to that disk on the kubelet.
func (pd *awsPersistentDisk) TearDown() error {
	return pd.TearDownAt(pd.GetPath())
}

// Unmounts the bind mount, and detaches the disk only if the PD
// resource was the last reference to that disk on the kubelet.
func (pd *awsPersistentDisk) TearDownAt(dir string) error {
	mountpoint, err := mount.IsMountPoint(dir)
	if err != nil {
		glog.V(2).Info("Error checking if mountpoint ", dir, ": ", err)
		return err
	}
	if !mountpoint {
		glog.V(2).Info("Not mountpoint, deleting")
		return os.Remove(dir)
	}

	refs, err := mount.GetMountRefs(pd.mounter, dir)
	if err != nil {
		glog.V(2).Info("Error getting mountrefs for ", dir, ": ", err)
		return err
	}
	// Unmount the bind-mount inside this pod
	if err := pd.mounter.Unmount(dir, 0); err != nil {
		glog.V(2).Info("Error unmounting dir ", dir, ": ", err)
		return err
	}
	// If len(refs) is 1, then all bind mounts have been removed, and the
	// remaining reference is the global mount. It is safe to detach.
	if len(refs) == 1 {
		// pd.pdName is not initially set for volume-cleaners, so set it here.
		pd.pdName, err = getPdNameFromGlobalMount(refs[0])
		if err != nil {
			glog.V(2).Info("Could not determine pdName from mountpoint ", refs[0], ": ", err)
			return err
		}
		if err := pd.manager.DetachDisk(pd); err != nil {
			glog.V(2).Info("Error detaching disk ", pd.pdName, ": ", err)
			return err
		}
	}
	mountpoint, mntErr := mount.IsMountPoint(dir)
	if mntErr != nil {
		glog.Errorf("isMountpoint check failed: %v", mntErr)
		return err
	}
	if !mountpoint {
		if err := os.Remove(dir); err != nil {
			glog.V(2).Info("Error removing mountpoint ", dir, ": ", err)
			return err
		}
	}
	return nil
}
