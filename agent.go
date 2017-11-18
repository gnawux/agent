//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	grpc "google.golang.org/grpc"
	"github.com/hashicorp/yamux"
	"github.com/mdlayher/vsock"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type process struct {
	process     libcontainer.Process
	stdin       *os.File
	stdout      *os.File
	stderr      *os.File
	seqStdio    uint64
	seqStderr   uint64
	consoleSock *os.File
	termMaster  *os.File
}

type container struct {
	container     libcontainer.Container
	config        configs.Config
	processes     map[string]*process
	pod           *pod
	processesLock sync.RWMutex
	wgProcesses   sync.WaitGroup
}

type pod struct {
	id           string
	running      bool
	containers   map[string]*container
	channel      channel
	stdinLock    sync.Mutex
	ttyLock      sync.Mutex
	podLock      sync.RWMutex
	wg           sync.WaitGroup
	grpcListener net.Listener
}

const (
	exitSuccess  = 0
	exitFailure  = 1
	fileMode0750 = 0750
)

const (
	gRPCSockPath   = "/var/run/kata-containers/grpc.sock"
	gRPCSockScheme = "unix"
)

func (p *pod) initChannel() error {
	// Check for vsock support.
	vSockSupported, err := isAFVSockSupported()
	if err != nil {
		return err
	}

	if vSockSupported {
		// Check if vsock socket exists. We want to cover the case
		// where the guest OS can support vsock, but the runtime is
		// still using virtio serial connection.
		exist, err := vSockPathExist()
		if err != nil {
			return err
		}

		if exist {
			// Fill the pod with vsock channel information.
			p.channel = &vSockChannel{}

			return nil
		}
	}

	// Open serial channel.
	file, err := openSerialChannel()
	if err != nil {
		return err
	}

	// Fill the pod with serial channel information.
	p.channel = &serialChannel{
		serialConn: file,
	}

	return nil
}

func (p *pod) startGRPC() (err error) {
	var l net.Listener

	if p.isVSock() {
		l, err = vsock.Listen(1024)
		if err != nil {
			return err
		}
	} else {
		socketDir := filepath.Dir(gRPCSockPath)
		if err = os.MkdirAll(socketDir, fileMode0750); err != nil {
			return fmt.Errorf("Couldn't create socket directory: %v", err)
		}

		l, err = net.Listen(gRPCSockScheme, gRPCSockPath)
		if err != nil {
			return err
		}
	}
	p.grpcListener = l

	grpcServer := grpc.NewServer()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		grpcServer.Serve(l)
	}()

	return nil
}

func (p *pod) loopYamux(session *yamux.Session, grpcConn net.Conn) {
	defer p.wg.Done()

	for {
		stream, err := session.Accept()
		if err != nil {
			fmt.Printf("%v\n", err)
			break
		}
		defer stream.Close()

		go func() {
			io.Copy(grpcConn, stream)
		}()

		go func() {
			io.Copy(stream, grpcConn)
		}()
	}
}

func (p *pod) startYamux() error {
	// If vsock is used, no need to go through yamux.
	if p.isVSock() {
		return nil
	}

	serialChan, ok := p.channel.(*serialChannel)
	if !ok {
		return fmt.Errorf("Could not cast into serialChannel type")
	}

	// Connect gRPC socket.
	conn, err := net.Dial(gRPCSockScheme, gRPCSockPath)
	if err != nil {
		return err
	}

	serialChan.grpcConn = conn

	// Initialize Yamux server.
	session, err := yamux.Server(serialChan.serialConn, nil)
	if err != nil {
		return err
	}

	serialChan.yamuxSession = session

	p.wg.Add(1)
	go p.loopYamux(session, conn)

	return nil
}

func (p *pod) tearDown() error {
	p.grpcListener.Close()

	if p.isVSock() {
		return nil
	}

	serialChan, ok := p.channel.(*serialChannel)
	if !ok {
		return fmt.Errorf("Could not cast into serialChannel type")
	}

	serialChan.yamuxSession.Close()
	serialChan.grpcConn.Close()
	serialChan.serialConn.Close()

	return nil
}

func main() {
	var err error

	defer func() {
		if err != nil {
			fmt.Printf("%v\n", err)
			os.Exit(exitFailure)
		}

		os.Exit(exitSuccess)
	}()

	// Initialize unique pod structure.
	p := &pod{
		containers: make(map[string]*container),
		running:    false,
	}

	// Check for vsock vs serial. This will fill the pod structure with
	// information about the channel.
	if err = p.initChannel(); err != nil {
		return
	}

	// Start gRPC server.
	if err = p.startGRPC(); err != nil {
		return
	}

	// Start yamux server.
	if err = p.startYamux(); err != nil {
		return
	}

	p.wg.Wait()

	// Tear down properly.
	if err = p.tearDown(); err != nil {
		return
	}
}
