//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"
)

type channelType string

const (
	serialChannelName = "agent.channel.0"
	virtIOPath        = "/sys/class/virtio-ports"
	devRootPath       = "/dev"
	vSockDevPath      = "/dev/vsock"
)

type channel interface {
	getType() channelType
}

const (
	vSockChannelType  channelType = "vsock"
	serialChannelType channelType = "serial"
)

type vSockChannel struct {
}

func (c *vSockChannel) getType() channelType {
	return vSockChannelType
}

type serialChannel struct {
	serialConn   *os.File
	grpcConn     net.Conn
	yamuxSession *yamux.Session
}

func (c *serialChannel) getType() channelType {
	return serialChannelType
}

func isAFVSockSupported() (bool, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		// This case is valid. It means AF_VSOCK is not a supported
		// domain on this system.
		if err == unix.EAFNOSUPPORT {
			return false, nil
		}

		return false, err
	}

	if err := unix.Close(fd); err != nil {
		return true, err
	}

	return true, nil
}

func vSockPathExist() (bool, error) {
	if _, err := os.Stat(vSockDevPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (p *pod) isVSock() bool {
	if p.channel.getType() == vSockChannelType {
		return true
	}

	return false
}

func findVirtualSerialPath(serialName string) (string, error) {
	dir, err := os.Open(virtIOPath)
	if err != nil {
		return "", err
	}

	defer dir.Close()

	ports, err := dir.Readdirnames(0)
	if err != nil {
		return "", err
	}

	for _, port := range ports {
		path := filepath.Join(virtIOPath, port, "name")
		content, err := ioutil.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("Skip parsing of non-existent file %s\n", path)
				continue
			}
			return "", err
		}

		if strings.Contains(string(content), serialName) == true {
			return filepath.Join(devRootPath, port), nil
		}
	}

	return "", fmt.Errorf("Could not find virtio port %s", serialName)
}

func openSerialChannel() (*os.File, error) {
	serialPath, err := findVirtualSerialPath(serialChannelName)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(serialPath, os.O_RDWR, os.ModeDevice)
	if err != nil {
		return nil, err
	}

	return file, nil
}
