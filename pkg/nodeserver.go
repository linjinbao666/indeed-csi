package pkg

import (
	"fmt"
	"os"
	"strings"

	"indeed-csi/pkg/state"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	"k8s.io/utils/mount"
)

const TopologyKeyNode = "topology.indeed.csi/node"

func (ind *indeed) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

	// Check arguments
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	targetPath := req.GetTargetPath()
	ephemeralVolume := req.GetVolumeContext()["csi.storage.k8s.io/ephemeral"] == "true" ||
		req.GetVolumeContext()["csi.storage.k8s.io/ephemeral"] == "" && ind.config.Ephemeral // Kubernetes 1.15 doesn't have csi.storage.k8s.io/ephemeral.

	if req.GetVolumeCapability().GetBlock() != nil &&
		req.GetVolumeCapability().GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument, "cannot have both block and mount access type")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	// if ephemeral is specified, create volume here to avoid errors
	if ephemeralVolume {
		volID := req.GetVolumeId()
		volName := fmt.Sprintf("ephemeral-%s", volID)
		kind := req.GetVolumeContext()[storageKind]
		// Configurable size would be nice. For now we use a small, fixed volume size of 100Mi.
		volSize := int64(100 * 1024 * 1024)
		vol, err := ind.createVolume(req.GetVolumeId(), volName, volSize, state.MountAccess, ephemeralVolume, kind)
		if err != nil && !os.IsExist(err) {
			glog.Error("ephemeral mode failed to create volume: ", err)
			return nil, err
		}
		glog.V(4).Infof("ephemeral mode: created volume: %s", vol.VolPath)
	}

	vol, err := ind.state.GetVolumeByID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	if !ephemeralVolume {
		if vol.Staged.Empty() {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %q must be staged before publishing", vol.VolID)
		}
		if !vol.Staged.Has(req.GetStagingTargetPath()) {
			return nil, status.Errorf(codes.InvalidArgument, "volume %q was staged at %v, not %q", vol.VolID, vol.Staged, req.GetStagingTargetPath())
		}
	}

	if req.GetVolumeCapability().GetBlock() != nil {
		if vol.VolAccessType != state.BlockAccess {
			return nil, status.Error(codes.InvalidArgument, "cannot publish a non-block volume as block volume")
		}

		volPathHandler := volumepathhandler.VolumePathHandler{}

		// Get loop device from the volume path.
		loopDevice, err := volPathHandler.GetLoopDevice(vol.VolPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get the loop device: %w", err)
		}

		mounter := mount.New("")

		// Check if the target path exists. Create if not present.
		_, err = os.Lstat(targetPath)
		if os.IsNotExist(err) {
			if err = makeFile(targetPath); err != nil {
				return nil, fmt.Errorf("failed to create target path: %w", err)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to check if the target block file exists: %w", err)
		}

		// Check if the target path is already mounted. Prevent remounting.
		notMount, err := mount.IsNotMountPoint(mounter, targetPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("error checking path %s for mount: %w", targetPath, err)
			}
			notMount = true
		}
		if !notMount {
			// It's already mounted.
			glog.V(5).Infof("Skipping bind-mounting subpath %s: already mounted", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}

		options := []string{"bind"}
		if err := mount.New("").Mount(loopDevice, targetPath, "", options); err != nil {
			return nil, fmt.Errorf("failed to mount block device: %s at %s: %w", loopDevice, targetPath, err)
		}
	} else if req.GetVolumeCapability().GetMount() != nil {
		if vol.VolAccessType != state.MountAccess {
			return nil, status.Error(codes.InvalidArgument, "cannot publish a non-mount volume as mount volume")
		}

		notMnt, err := mount.IsNotMountPoint(mount.New(""), targetPath)
		if err != nil {
			if os.IsNotExist(err) {
				if err = os.Mkdir(targetPath, 0750); err != nil {
					return nil, fmt.Errorf("create target path: %w", err)
				}
				notMnt = true
			} else {
				return nil, fmt.Errorf("check target path: %w", err)
			}
		}

		if !notMnt {
			return &csi.NodePublishVolumeResponse{}, nil
		}

		fsType := req.GetVolumeCapability().GetMount().GetFsType()

		deviceId := ""
		if req.GetPublishContext() != nil {
			deviceId = req.GetPublishContext()[deviceID]
		}

		readOnly := req.GetReadonly()
		volumeId := req.GetVolumeId()
		attrib := req.GetVolumeContext()
		mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()

		glog.V(4).Infof("target %v\nfstype %v\ndevice %v\nreadonly %v\nvolumeId %v\nattributes %v\nmountflags %v\n",
			targetPath, fsType, deviceId, readOnly, volumeId, attrib, mountFlags)

		options := []string{"bind"}
		if readOnly {
			options = append(options, "ro")
		}
		mounter := mount.New("")
		path := ind.getVolumePath(volumeId)

		if err := mounter.Mount(path, targetPath, "", options); err != nil {
			var errList strings.Builder
			errList.WriteString(err.Error())
			if vol.Ephemeral {
				if rmErr := os.RemoveAll(path); rmErr != nil && !os.IsNotExist(rmErr) {
					errList.WriteString(fmt.Sprintf(" :%s", rmErr.Error()))
				}
			}
			return nil, fmt.Errorf("failed to mount device: %s at %s: %s", path, targetPath, errList.String())
		}
	}

	vol.NodeID = ind.config.NodeID
	vol.Published.Add(targetPath)
	if err := ind.state.UpdateVolume(vol); err != nil {
		return nil, err
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ind *indeed) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	vol, err := ind.state.GetVolumeByID(volumeID)
	if err != nil {
		return nil, err
	}

	if !vol.Published.Has(targetPath) {
		glog.V(4).Infof("Volume %q is not published at %q, nothing to do.", volumeID, targetPath)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	// Unmount only if the target path is really a mount point.
	if notMnt, err := mount.IsNotMountPoint(mount.New(""), targetPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("check target path: %w", err)
		}
	} else if !notMnt {
		// Unmounting the image or filesystem.
		err = mount.New("").Unmount(targetPath)
		if err != nil {
			return nil, fmt.Errorf("unmount target path: %w", err)
		}
	}
	// Delete the mount point.
	// Does not return error for non-existent path, repeated calls OK for idempotency.
	if err = os.RemoveAll(targetPath); err != nil {
		return nil, fmt.Errorf("remove target path: %w", err)
	}
	glog.V(4).Infof("hostpath: volume %s has been unpublished.", targetPath)

	if vol.Ephemeral {
		glog.V(4).Infof("deleting volume %s", volumeID)
		if err := ind.deleteVolume(volumeID); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to delete volume: %w", err)
		}
	} else {
		vol.Published.Remove(targetPath)
		if err := ind.state.UpdateVolume(vol); err != nil {
			return nil, err
		}
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ind *indeed) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	stagingTargetPath := req.GetStagingTargetPath()
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capability missing in request")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	vol, err := ind.state.GetVolumeByID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if ind.config.EnableAttach && !vol.Attached {
		return nil, status.Errorf(codes.Internal, "ControllerPublishVolume must be called on volume '%s' before staging on node",
			vol.VolID)
	}

	if vol.Staged.Has(stagingTargetPath) {
		glog.V(4).Infof("Volume %q is already staged at %q, nothing to do.", req.VolumeId, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if !vol.Staged.Empty() {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %q is already staged at %v", req.VolumeId, vol.Staged)
	}

	vol.Staged.Add(stagingTargetPath)
	if err := ind.state.UpdateVolume(vol); err != nil {
		return nil, err
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ind *indeed) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	stagingTargetPath := req.GetStagingTargetPath()
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	vol, err := ind.state.GetVolumeByID(req.VolumeId)
	if err != nil {
		return nil, err
	}

	if !vol.Staged.Has(stagingTargetPath) {
		glog.V(4).Infof("Volume %q is not staged at %q, nothing to do.", req.VolumeId, stagingTargetPath)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	if !vol.Published.Empty() {
		return nil, status.Errorf(codes.Internal, "volume %q is still published at %q on node %q", vol.VolID, vol.Published, vol.NodeID)
	}
	vol.Staged.Remove(stagingTargetPath)
	if err := ind.state.UpdateVolume(vol); err != nil {
		return nil, err
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ind *indeed) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{
		NodeId:            ind.config.NodeID,
		MaxVolumesPerNode: ind.config.MaxVolumesPerNode,
	}

	if ind.config.EnableTopology {
		resp.AccessibleTopology = &csi.Topology{
			Segments: map[string]string{TopologyKeyNode: ind.config.NodeID},
		}
	}

	if ind.config.AttachLimit > 0 {
		resp.MaxVolumesPerNode = ind.config.AttachLimit
	}

	return resp, nil
}

func (ind *indeed) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
		},
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
				},
			},
		},
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
				},
			},
		},
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
				},
			},
		},
	}
	if ind.config.EnableVolumeExpansion && !ind.config.DisableNodeExpansion {
		caps = append(caps, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
				},
			},
		})
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (ind *indeed) NodeGetVolumeStats(ctx context.Context, in *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(in.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}
	if len(in.GetVolumePath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume Path not provided")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	volume, err := ind.state.GetVolumeByID(in.GetVolumeId())
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(in.GetVolumePath()); err != nil {
		return nil, status.Errorf(codes.NotFound, "Could not get file information from %s: %+v", in.GetVolumePath(), err)
	}

	healthy, msg := ind.doHealthCheckInNodeSide(in.GetVolumeId())
	glog.V(3).Infof("Healthy state: %+v Volume: %+v", volume.VolName, healthy)
	available, capacity, used, inodes, inodesFree, inodesUsed, err := getPVStats(in.GetVolumePath())
	if err != nil {
		return nil, fmt.Errorf("get volume stats failed: %w", err)
	}

	glog.V(3).Infof("Capacity: %+v Used: %+v Available: %+v Inodes: %+v Free inodes: %+v Used inodes: %+v", capacity, used, available, inodes, inodesFree, inodesUsed)
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: available,
				Used:      used,
				Total:     capacity,
				Unit:      csi.VolumeUsage_BYTES,
			}, {
				Available: inodesFree,
				Used:      inodesUsed,
				Total:     inodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
		VolumeCondition: &csi.VolumeCondition{
			Abnormal: !healthy,
			Message:  msg,
		},
	}, nil
}

// NodeExpandVolume is only implemented so the driver can be used for e2e testing.
func (ind *indeed) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if !ind.config.EnableVolumeExpansion {
		return nil, status.Error(codes.Unimplemented, "NodeExpandVolume is not supported")
	}

	volID := req.GetVolumeId()
	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	ind.mutex.Lock()
	defer ind.mutex.Unlock()

	vol, err := ind.state.GetVolumeByID(volID)
	if err != nil {
		return nil, err
	}

	volPath := req.GetVolumePath()
	if len(volPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume path not provided")
	}

	capRange := req.GetCapacityRange()
	if capRange == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range not provided")
	}
	capacity := int64(capRange.GetRequiredBytes())
	if capacity > ind.config.MaxVolumeExpansionSizeNode {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", capacity, ind.config.MaxVolumeExpansionSizeNode)
	}

	info, err := os.Stat(volPath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Could not get file information from %s: %v", volPath, err)
	}

	switch m := info.Mode(); {
	case m.IsDir():
		if vol.VolAccessType != state.MountAccess {
			return nil, status.Errorf(codes.InvalidArgument, "Volume %s is not a directory", volID)
		}
	case m&os.ModeDevice != 0:
		if vol.VolAccessType != state.BlockAccess {
			return nil, status.Errorf(codes.InvalidArgument, "Volume %s is not a block device", volID)
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "Volume %s is invalid", volID)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// makeFile ensures that the file exists, creating it if necessary.
// The parent directory must exist.
func makeFile(pathname string) error {
	f, err := os.OpenFile(pathname, os.O_CREATE, os.FileMode(0644))
	defer f.Close()
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}