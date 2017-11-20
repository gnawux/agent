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
	"runtime"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	pb "github.com/hyperhq/runv/hyperstart/api/grpc"
	"github.com/mdlayher/vsock"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type process struct {
	id          string
	process     libcontainer.Process
	stdin       *os.File
	stdout      *os.File
	stderr      *os.File
	consoleSock *os.File
	termMaster  *os.File
}

type container struct {
	sync.RWMutex

	id          string
	initProcId  string
	container   libcontainer.Container
	config      configs.Config
	processes   map[string]*process
	wgProcesses sync.WaitGroup
	mounts      []string
}

type pod struct {
	sync.RWMutex

	id           string
	running      bool
	containers   map[string]*container
	channel      channel
	network      network
	shareDir     string
	wg           sync.WaitGroup
	grpcListener net.Listener
}

const (
	agentName       = "kata-agent"
	exitSuccess     = 0
	exitFailure     = 1
	fileMode0750    = 0750
	defaultLogLevel = logrus.InfoLevel
)

const (
	gRPCSockPath   = "/var/run/kata-containers/grpc.sock"
	gRPCSockScheme = "unix"
)

var agentLog = logrus.WithFields(logrus.Fields{
	"name": agentName,
	"pid":  os.Getpid(),
})

// Version is the agent version. This variable is populated at build time.
var Version = "unknown"

// This is the list of file descriptors we can properly close after the process
// has been started. When the new process is exec(), those file descriptors are
// duplicated and it is our responsibility to close them since we have opened
// them.
func (p *process) closePostStartFDs() {
	if p.process.Stdin != nil {
		p.process.Stdin.(*os.File).Close()
	}

	if p.process.Stdout != nil {
		p.process.Stdout.(*os.File).Close()
	}

	if p.process.Stderr != nil {
		p.process.Stderr.(*os.File).Close()
	}

	if p.process.ConsoleSocket != nil {
		p.process.Stderr.(*os.File).Close()
	}

	if p.consoleSock != nil {
		p.process.Stderr.(*os.File).Close()
	}
}

// This is the list of file descriptors we can properly close after the process
// has exited. These are the remaining file descriptors that we have opened and
// are no longer needed.
func (p *process) closePostExitFDs() {
	if p.termMaster != nil {
		p.termMaster.Close()
	}

	if p.stdin != nil {
		p.stdin.Close()
	}

	if p.stdout != nil {
		p.stdout.Close()
	}

	if p.stderr != nil {
		p.stderr.Close()
	}
}

func (c *container) setProcess(pid string, process *process) {
	c.Lock()
	c.processes[pid] = process
	c.Unlock()
}

func (c *container) deleteProcess(pid string) {
	c.Lock()
	delete(c.processes, pid)
	c.Unlock()
}

func (c *container) removeContainer() error {
	c.wgProcesses.Wait()

	if err := c.container.Destroy(); err != nil {
		return err
	}

	return unmountContainerRootFs(c.id, c.config.Rootfs)
}

func (c *container) getProcess(pid string) (*process, error) {
	c.RLock()
	defer c.RUnlock()

	proc, exist := c.processes[pid]
	if !exist {
		return nil, fmt.Errorf("Process %s not found (container %s)", pid, c.id)
	}

	return proc, nil
}

func (p *pod) getContainer(id string) (*container, error) {
	p.RLock()
	defer p.RUnlock()

	ctr, exist := p.containers[id]
	if !exist {
		return nil, fmt.Errorf("Container %s not found", id)
	}

	return ctr, nil
}

func (p *pod) setContainer(id string, ctr *container) {
	p.Lock()
	p.containers[id] = ctr
	p.Unlock()
}

func (p *pod) deleteContainer(id string) {
	p.Lock()
	delete(p.containers, id)
	p.Unlock()
}

func (p *pod) getRunningProcess(cid, pid string) (*process, *container, error) {
	if p.running == false {
		return nil, nil, fmt.Errorf("Pod not started")
	}

	ctr, err := p.getContainer(cid)
	if err != nil {
		return nil, nil, err
	}

	status, err := ctr.container.Status()
	if err != nil {
		return nil, nil, err
	}

	if status != libcontainer.Running {
		return nil, nil, fmt.Errorf("Container %s %s, should be %s", cid, status.String(), libcontainer.Running.String())
	}

	proc, err := ctr.getProcess(pid)
	if err != nil {
		return nil, nil, err
	}

	return proc, ctr, nil
}

func (p *pod) readStdio(cid, pid string, length int, stdout bool) ([]byte, error) {
	proc, _, err := p.getRunningProcess(cid, pid)
	if err != nil {
		return nil, err
	}

	var file *os.File
	if proc.termMaster != nil {
		file = proc.termMaster
	} else {
		if stdout {
			file = proc.stdout
		} else {
			file = proc.stderr
		}
	}

	buf := make([]byte, length)

	if _, err := file.Read(buf); err != nil {
		return nil, err
	}

	return buf, nil
}

func (p *pod) initLogger() error {
	agentLog.Logger.Formatter = &logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano}

	config := newConfig(defaultLogLevel)
	if err := config.getConfig(kernelCmdlineFile); err != nil {
		agentLog.WithError(err).Warn("Failed to get config from kernel cmdline")
	}
	config.applyConfig()

	agentLog.WithField("version", Version).Info()

	return nil
}

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

	grpcImpl := &agentGRPC{
		pod: p,
	}

	grpcServer := grpc.NewServer()
	pb.RegisterHyperstartServiceServer(grpcServer, grpcImpl)

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
			agentLog.Error(err)
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

func init() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		runtime.GOMAXPROCS(1)
		runtime.LockOSThread()
		factory, _ := libcontainer.New("")
		if err := factory.StartInitialization(); err != nil {
			agentLog.Errorf("init went wrong: %v", err)
		}
		panic("--this line should have never been executed, congratulations--")
	}
}

func main() {
	var err error

	defer func() {
		if err != nil {
			agentLog.Error(err)
			os.Exit(exitFailure)
		}

		os.Exit(exitSuccess)
	}()

	// Initialize unique pod structure.
	p := &pod{
		containers: make(map[string]*container),
		running:    false,
	}

	if err = p.initLogger(); err != nil {
		return
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
