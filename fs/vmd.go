// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

const (
	vmdInitialVersion = 1
	vmdCopies         = 3
)

type (
	fsDeviceMD struct {
		MountPath string `json:"mpath"`
		FsType    string `json:"fs_type"`
		Enabled   bool   `json:"enabled"`
	}

	// Short for VolumeMetaData.
	VMD struct {
		Devices  map[string]*fsDeviceMD `json:"devices"` // Mpath => MD
		DaemonID string                 `json:"daemon_id"`
		Version  uint                   `json:"version"` // formatting version for backward compatibility
		cksum    *cmn.Cksum             // checksum of VMD
	}
)

func newVMD(expectedSize int) *VMD {
	return &VMD{
		Devices: make(map[string]*fsDeviceMD, expectedSize),
		Version: vmdInitialVersion,
	}
}

func CreateNewVMD(daemonID string) (*VMD, error) {
	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	var (
		available, disabled = Get()
		vmd                 = newVMD(len(available))
	)

	vmd.DaemonID = daemonID

	for _, mPath := range available {
		vmd.Devices[mPath.Path] = &fsDeviceMD{
			MountPath: mPath.Path,
			FsType:    mPath.FileSystem,
			Enabled:   true,
		}
	}

	for _, mPath := range disabled {
		vmd.Devices[mPath.Path] = &fsDeviceMD{
			MountPath: mPath.Path,
			FsType:    mPath.FileSystem,
			Enabled:   false,
		}
	}
	return vmd, vmd.persist()
}

// LoadVMD loads VMD from given paths:
// - Returns error in case of validation errors or failed to load existing VMD
// - Returns nil if VMD not present on any path
func LoadVMD(mpaths cmn.StringSet) (mainVMD *VMD, err error) {
	for path := range mpaths {
		fpath := filepath.Join(path, VmdPersistedFileName)
		vmd := newVMD(len(mpaths))
		vmd.cksum, err = jsp.Load(fpath, vmd, jsp.CCSign())
		if err != nil && os.IsNotExist(err) {
			continue
		}

		if err != nil {
			err = newVMDLoadErr(path, err)
			return nil, err
		}

		if err = vmd.Validate(); err != nil {
			err = newVMDValidationErr(path, err)
			return nil, err
		}

		if mainVMD != nil {
			if !mainVMD.cksum.Equal(vmd.cksum) {
				err = newVMDMismatchErr(mainVMD, vmd, path)
				return nil, err
			}
			continue
		}
		mainVMD = vmd
	}

	if mainVMD == nil {
		glog.Infof("VMD not found on any of %d mountpaths", len(mpaths))
	}
	return mainVMD, nil
}

func (vmd VMD) persist() error {
	// Checksum, compress and sign, as a VMD might be quite large.
	if cnt, availMpaths := PersistOnMpaths(VmdPersistedFileName, "", vmd, vmdCopies, jsp.CCSign()); availMpaths == 0 {
		glog.Errorf("failed to persist VMD no available mountpaths")
	} else if cnt == 0 {
		return fmt.Errorf("failed to persist VMD on any of mountpaths (%d)", availMpaths)
	}
	return nil
}

func (vmd VMD) Validate() error {
	// TODO: Add versions handling.
	if vmd.Version != vmdInitialVersion {
		return fmt.Errorf("invalid VMD version %q", vmd.Version)
	}
	cmn.Assert(vmd.cksum != nil)
	cmn.Assert(vmd.DaemonID != "")
	return nil
}

func (vmd VMD) HasPath(path string) (exists bool) {
	_, exists = vmd.Devices[path]
	return
}

// LoadDaemonID loads the daemon ID present as xattr on given mount paths.
func LoadDaemonID(mpaths cmn.StringSet) (mDaeID string, err error) {
	for mp := range mpaths {
		daeID, err := LoadDaemonIDXattr(mp)
		if err != nil {
			return "", err
		}
		if daeID == "" {
			continue
		}
		if mDaeID != "" {
			if mDaeID != daeID {
				return "", newMpathIDMismatchErr(mDaeID, daeID, mp)
			}
			continue
		}
		mDaeID = daeID
	}
	return
}

func LoadDaemonIDXattr(mpath string) (daeID string, err error) {
	b, err := GetXattr(mpath, daemonIDXattr)
	if err == nil {
		daeID = string(b)
		return
	}
	if cmn.IsErrXattrNotFound(err) {
		err = nil
	}
	return
}
