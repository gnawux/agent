//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/hyperhq/runv/hyperstart/api/grpc"
	goudev "github.com/jochenvg/go-udev"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/sirupsen/logrus"
)

const (
	type9pFs           = "9p"
	containerMountDest = "/tmp/hyper"
	mountPerm          = os.FileMode(0755)
	devPrefix          = "/dev/"
)

// bindMount bind mounts a source in to a destination, with the recursive
// flag if needed.
func bindMount(source, destination string, recursive bool) error {
	flags := syscall.MS_BIND

	if recursive == true {
		flags |= syscall.MS_REC
	}

	return mount(source, destination, "bind", flags)
}

// mount mounts a source in to a destination. This will do some bookkeeping:
// * evaluate all symlinks
// * ensure the source exists
func mount(source, destination, fsType string, flags int) error {
	var options string
	if fsType == "xfs" {
		options = "nouuid"
	}

	absSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("Could not resolve symlink for source %v", source)
	}

	if err := ensureDestinationExists(absSource, destination, fsType); err != nil {
		return fmt.Errorf("Could not create destination mount point: %v: %v", destination, err)
	}

	if err := syscall.Mount(absSource, destination, fsType, uintptr(flags), options); err != nil {
		return fmt.Errorf("Could not bind mount %v to %v: %v", absSource, destination, err)
	}

	return nil
}

// ensureDestinationExists will recursively create a given mountpoint. If directories
// are created, their permissions are initialized to mountPerm
func ensureDestinationExists(source, destination string, fsType string) error {
	fileInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("could not stat source location: %v", source)
	}

	targetPathParent, _ := filepath.Split(destination)
	if err := os.MkdirAll(targetPathParent, mountPerm); err != nil {
		return fmt.Errorf("could not create parent directory: %v", targetPathParent)
	}

	if fsType != "bind" || fileInfo.IsDir() {
		if err := os.Mkdir(destination, mountPerm); !os.IsExist(err) {
			return err
		}
	} else {
		file, err := os.OpenFile(destination, os.O_CREATE, mountPerm)
		if err != nil {
			return err
		}

		file.Close()
	}
	return nil
}

func mountShareDir(tag, dest, fsType string) error {
	if fsType != type9pFs {
		return fmt.Errorf("Invalid fsType %q, only %q supported for now", fsType, type9pFs)
	}

	if tag == "" {
		return fmt.Errorf("Invalid mount tag, should not be empty")
	}

	if err := os.MkdirAll(dest, os.FileMode(0755)); err != nil {
		return err
	}

	return syscall.Mount(tag, dest, type9pFs, syscall.MS_MGC_VAL|syscall.MS_NODEV, "trans=virtio")
}

func unmountShareDir(shareDir string) error {
	if err := syscall.Unmount(shareDir, 0); err != nil {
		return err
	}

	return nil
}

func waitForBlockDevice(devicePath string) error {
	deviceName := strings.TrimPrefix(devicePath, devPrefix)

	if _, err := os.Stat(devicePath); err == nil {
		return nil
	}

	u := goudev.Udev{}

	// Create a monitor listening on a NetLink socket.
	monitor := u.NewMonitorFromNetlink("udev")

	// Add filter to watch for just block devices.
	if err := monitor.FilterAddMatchSubsystemDevtype("block", "disk"); err != nil {
		return err
	}

	// Create channel to signal when desired udev event has been received.
	doneListening := make(chan bool)

	ctx, cancel := context.WithCancel(context.Background())

	// Start monitor goroutine.
	ch, _ := monitor.DeviceChan(ctx)

	go func() {
		fieldLogger := agentLog.WithField("device", deviceName)

		fieldLogger.Info("Started listening for udev events for block device hotplug")

		// Check if the device already exists.
		if _, err := os.Stat(devicePath); err == nil {
			fieldLogger.Info("Device already hotplugged, quit listening")
		} else {

			for d := range ch {
				fieldLogger = fieldLogger.WithFields(logrus.Fields{
					"udev-path":  d.Syspath(),
					"udev-event": d.Action(),
				})

				fieldLogger.Info("got udev event")
				if d.Action() == "add" && filepath.Base(d.Devpath()) == deviceName {
					fieldLogger.Info("Hotplug event received")
					break
				}
			}
		}
		close(doneListening)
	}()

	select {
	case <-doneListening:
		cancel()
	case <-time.After(time.Duration(1) * time.Second):
		cancel()
		return fmt.Errorf("Timed out waiting for device %s", deviceName)
	}

	return nil
}

func mountContainerRootFs(cid string, mnt *pb.Mount, shareDir string) (string, error) {
	dest := filepath.Join(containerMountDest, cid, "root")
	if err := os.MkdirAll(dest, os.FileMode(0755)); err != nil {
		return "", err
	}

	source := mnt.Source
	if mnt.Type != "" {
		if mnt.Type == "blk" {
			if err := waitForBlockDevice(source); err != nil {
				return "", err
			}
		}

		if err := mount(mnt.Source, dest, mnt.Type, 0); err != nil {
			return "", err
		}
	} else {
		source = filepath.Join(shareDir, mnt.Source)
		if err := bindMount(source, dest, true); err != nil {
			return "", err
		}
	}

	if err := bindMount(dest, dest, true); err != nil {
		return "", err
	}

	return dest, nil
}

func addMounts(c *pb.Container, shareDir string, config *configs.Config) error {
	for _, mount := range c.Mounts {
		if mount == nil {
			continue
		}

		if mount.Rootfs {
			absRootFs, err := mountContainerRootFs(c.Id, mount, shareDir)
			if err != nil {
				return err
			}

			config.Rootfs = absRootFs
		}

		source := mount.Source
		if !filepath.IsAbs(source) {
			source = filepath.Join(shareDir, mount.Source)
		}

		newMount := &configs.Mount{
			Source:      source,
			Destination: mount.Dest,
			Device:      "bind",
			Flags:       syscall.MS_BIND | syscall.MS_REC,
		}

		if mount.ReadOnly {
			newMount.Flags |= syscall.MS_RDONLY
		}

		config.Mounts = append(config.Mounts, newMount)
	}

	return nil
}

func unmountContainerRootFs(containerID, mountingPath string) error {
	if err := syscall.Unmount(mountingPath, 0); err != nil {
		return err
	}

	containerPath := filepath.Join(containerMountDest, containerID, "root")
	if err := syscall.Unmount(containerPath, 0); err != nil {
		return err
	}

	return os.RemoveAll(containerPath)
}
