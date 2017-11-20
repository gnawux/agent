//
// Copyright (c) 2017 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"

	gpb "github.com/golang/protobuf/ptypes/empty"
	pb "github.com/hyperhq/runv/hyperstart/api/grpc"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/utils"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

type agentGRPC struct {
	pod *pod
}

// PCI scanning
const (
	pciBusRescanFile = "/sys/bus/pci/rescan"
	pciBusMode       = 0220
)

// Signals
const (
	// If a process terminates because of signal "n"
	// The exit code is "128 + signal_number"
	// http://tldp.org/LDP/abs/html/exitcodes.html
	exitSignalOffset = 128
)

// CPU and Memory hotplug
const (
	sysfsCPUOnlinePath = "/sys/devices/system/cpu"
	sysfsMemOnlinePath = "/sys/devices/system/memory"
	cpuRegexpPattern   = "cpu[0-9]*"
	memRegexpPattern   = "memory[0-9]*"
)

type onlineResource struct {
	sysfsOnlinePath string
	regexpPattern   string
}

var defaultCapsList = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_RAW",
	"CAP_SYS_CHROOT",
	"CAP_MKNOD",
	"CAP_AUDIT_WRITE",
	"CAP_SETFCAP",
}

var fullCapsList = []string{
	"CAP_AUDIT_CONTROL",
	"CAP_AUDIT_READ",
	"CAP_AUDIT_WRITE",
	"CAP_BLOCK_SUSPEND",
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_DAC_READ_SEARCH",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_IPC_LOCK",
	"CAP_IPC_OWNER",
	"CAP_KILL",
	"CAP_LEASE",
	"CAP_LINUX_IMMUTABLE",
	"CAP_MAC_ADMIN",
	"CAP_MAC_OVERRIDE",
	"CAP_MKNOD",
	"CAP_NET_ADMIN",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_BROADCAST",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_SETUID",
	"CAP_SYS_ADMIN",
	"CAP_SYS_BOOT",
	"CAP_SYS_CHROOT",
	"CAP_SYS_MODULE",
	"CAP_SYS_NICE",
	"CAP_SYS_PACCT",
	"CAP_SYS_PTRACE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_RESOURCE",
	"CAP_SYS_TIME",
	"CAP_SYS_TTY_CONFIG",
	"CAP_SYSLOG",
	"CAP_WAKE_ALARM",
}

var emptyResp = &gpb.Empty{}

func onlineCPUMem() error {
	resourceList := []onlineResource{
		{
			sysfsOnlinePath: sysfsCPUOnlinePath,
			regexpPattern:   cpuRegexpPattern,
		},
		{
			sysfsOnlinePath: sysfsMemOnlinePath,
			regexpPattern:   memRegexpPattern,
		},
	}

	for _, resource := range resourceList {
		files, err := ioutil.ReadDir(resource.sysfsOnlinePath)
		if err != nil {
			return err
		}

		for _, file := range files {
			matched, err := regexp.MatchString(resource.regexpPattern, file.Name())
			if err != nil {
				return err
			}

			if !matched {
				continue
			}

			cpuOnlinePath := filepath.Join(sysfsCPUOnlinePath, file.Name(), "online")
			ioutil.WriteFile(cpuOnlinePath, []byte("1"), 0600)
		}
	}

	return nil
}

func setConsoleCarriageReturn(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}

	termios.Oflag |= unix.ONLCR

	return unix.IoctlSetTermios(fd, unix.TCSETS, termios)
}

func buildProcess(agentProcess *pb.Process) (*process, error) {
	var envList []string
	for key, env := range agentProcess.Envs {
		envList = append(envList, fmt.Sprintf("%s=%s", key, env))
	}

	// we can specify the user and the group separated by :
	user := fmt.Sprintf("%s:%s", agentProcess.User.Uid, agentProcess.User.Gid)

	proc := &process{
		id:      agentProcess.Id,
		process: libcontainer.Process{
			Cwd:              agentProcess.Workdir,
			Args:             agentProcess.Args,
			Env:              envList,
			User:             user,
			AdditionalGroups: agentProcess.User.AdditionalGids,
		},
	}

	if agentProcess.Terminal {
		parentSock, childSock, err := utils.NewSockPair("console")
		if err != nil {
			return nil, err
		}

		proc.process.ConsoleSocket = childSock
		proc.consoleSock = parentSock

		return proc, nil
	}

	rStdin, wStdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	rStdout, wStdout, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	rStderr, wStderr, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	proc.process.Stdin = rStdin
	proc.process.Stdout = wStdout
	proc.process.Stderr = wStderr

	proc.stdin = wStdin
	proc.stdout = rStdout
	proc.stderr = rStderr

	return proc, nil
}

// Shared function between StartContainer and ExecProcess, because those expect
// a process to be run. The difference being the process does not exist yet in
// case of ExecProcess.
func (a *agentGRPC) runProcess(cid string, agentProcess *pb.Process) (err error) {
	if a.pod.running == false {
		return fmt.Errorf("Pod not started")
	}

	ctr, err := a.pod.getContainer(cid)
	if err != nil {
		return err
	}

	status, err := ctr.container.Status()
	if err != nil {
		return err
	}

	var proc *process
	if agentProcess != nil {
		if status != libcontainer.Running {
			return fmt.Errorf("Container %s status %s, should be %s", cid, status.String(), libcontainer.Running.String())
		}

		if _, err := ctr.getProcess(agentProcess.Id); err == nil {
			return fmt.Errorf("Process %s already exists", agentProcess.Id)
		}

		proc, err = buildProcess(agentProcess)
		if err != nil {
			return err
		}
	} else {
		if status != libcontainer.Created {
			return fmt.Errorf("Container %s status %s, should be %s", cid, status.String(), libcontainer.Created.String())
		}

		proc, err = ctr.getProcess(ctr.initProcId);
		if err != nil {
			return err
		}
	}

	ctr.wgProcesses.Add(1)

	if err := ctr.container.Run(&(proc.process)); err != nil {
		return fmt.Errorf("Could not run process: %v", err)
	}
	defer proc.closePostStartFDs()

	// Setup terminal if enabled.
	if proc.consoleSock != nil {
		termMaster, err := utils.RecvFd(proc.consoleSock)
		if err != nil {
			return err
		}

		if err := setConsoleCarriageReturn(int(termMaster.Fd())); err != nil {
			return err
		}

		proc.termMaster = termMaster
	}

	// Update process info.
	ctr.setProcess(proc.id, proc)

	return nil
}

func (a *agentGRPC) CreateContainer(ctx context.Context, req *pb.CreateContainerRequest) (*gpb.Empty, error) {
	if a.pod.running == false {
		return emptyResp, fmt.Errorf("Pod not started, impossible to run a new container")
	}

	if _, err := a.pod.getContainer(req.Container.Id); err == nil {
		return emptyResp, fmt.Errorf("Container %s already exists, impossible to create", req.Container.Id)
	}

	// re-scan PCI bus
	// looking for hidden devices
	if err := ioutil.WriteFile(pciBusRescanFile, []byte("1"), pciBusMode); err != nil {
		agentLog.WithError(err).Warnf("Could not rescan PCI bus")
	}

	defaultMountFlags := syscall.MS_NOEXEC | syscall.MS_NOSUID | syscall.MS_NODEV

	config := configs.Config{
		Capabilities: &configs.Capabilities{
			Bounding:    defaultCapsList,
			Effective:   defaultCapsList,
			Inheritable: defaultCapsList,
			Permitted:   defaultCapsList,
			Ambient:     defaultCapsList,
		},
		Namespaces: configs.Namespaces([]configs.Namespace{
			{Type: configs.NEWNS},
			{Type: configs.NEWUTS},
			{Type: configs.NEWIPC},
			{Type: configs.NEWPID},
		}),
		Cgroups: &configs.Cgroup{
			Name:   req.Container.Id,
			Parent: "system",
			Resources: &configs.Resources{
				MemorySwappiness: nil,
				AllowAllDevices:  nil,
				AllowedDevices:   configs.DefaultAllowedDevices,
			},
		},
		Devices: configs.DefaultAutoCreatedDevices,

		Hostname: a.pod.id,
		Mounts: []*configs.Mount{
			{
				Source:      "proc",
				Destination: "/proc",
				Device:      "proc",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "tmpfs",
				Destination: "/dev",
				Device:      "tmpfs",
				Flags:       syscall.MS_NOSUID | syscall.MS_STRICTATIME,
				Data:        "mode=755",
			},
			{
				Source:      "devpts",
				Destination: "/dev/pts",
				Device:      "devpts",
				Flags:       syscall.MS_NOSUID | syscall.MS_NOEXEC,
				Data:        "newinstance,ptmxmode=0666,mode=0620,gid=5",
			},
			{
				Device:      "tmpfs",
				Source:      "shm",
				Destination: "/dev/shm",
				Data:        "mode=1777,size=65536k",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "mqueue",
				Destination: "/dev/mqueue",
				Device:      "mqueue",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "sysfs",
				Destination: "/sys",
				Device:      "sysfs",
				Flags:       defaultMountFlags,
			},
		},

		NoNewKeyring:    true,
		NoNewPrivileges: true,
	}

	// Fill config.Rootfs and populate config.Mounts with additional mounts.
	if err := addMounts(req.Container, a.pod.shareDir, &config); err != nil {
		return emptyResp, err
	}

	containerPath := filepath.Join("/tmp/libcontainer", a.pod.id)
	factory, err := libcontainer.New(containerPath, libcontainer.Cgroupfs)
	if err != nil {
		return emptyResp, err
	}

	libContContainer, err := factory.Create(req.Container.Id, &config)
	if err != nil {
		return emptyResp, err
	}

	builtProcess, err := buildProcess(req.Init)
	if err != nil {
		return emptyResp, err
	}

	processes := make(map[string]*process)
	processes[req.Init.Id] = builtProcess

	container := &container{
		id:         req.Container.Id,
		initProcId: req.Init.Id,
		container:  libContContainer,
		config:     config,
		processes:  processes,
	}

	a.pod.setContainer(req.Container.Id, container)

	return emptyResp, nil
}

func (a *agentGRPC) StartContainer(ctx context.Context, req *pb.StartContainerRequest) (*gpb.Empty, error) {
	return emptyResp, a.runProcess(req.ContainerId, nil)
}

func (a *agentGRPC) ExecProcess(ctx context.Context, req *pb.ExecProcessRequest) (*gpb.Empty, error) {
	return emptyResp, a.runProcess(req.ContainerId, req.Process)
}

func (a *agentGRPC) SignalProcess(ctx context.Context, req *pb.SignalProcessRequest) (*gpb.Empty, error) {
	if a.pod.running == false {
		return emptyResp, fmt.Errorf("Pod not started, impossible to signal the container")
	}

	ctr, err := a.pod.getContainer(req.ContainerId)
	if err != nil {
		return emptyResp, fmt.Errorf("Could not signal process %s: %v", req.ProcessId, err)
	}

	status, err := ctr.container.Status()
	if err != nil {
		return emptyResp, err
	}

	signal := syscall.Signal(req.Signal)

	if status == libcontainer.Stopped {
		agentLog.Info("Container %s is Stopped on pod %s, discard signal %s", req.ContainerId, a.pod.id, signal.String())
		return emptyResp, nil
	}

	proc, err := ctr.getProcess(req.ProcessId)
	if err != nil {
		return emptyResp, fmt.Errorf("Could not signal process: %v", err)
	}

	if err := proc.process.Signal(signal); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) WaitProcess(ctx context.Context, req *pb.WaitProcessRequest) (*pb.WaitProcessResponse, error) {
	proc, ctr, err := a.pod.getRunningProcess(req.ContainerId, req.ProcessId)
	if err != nil {
		return &pb.WaitProcessResponse{}, err
	}

	defer func() {
		proc.closePostExitFDs()
		ctr.deleteProcess(proc.id)
		ctr.wgProcesses.Done()
	}()

	fieldLogger := agentLog.WithField("container-pid", proc.id)

	processState, err := proc.process.Wait()
	// Ignore error if process fails because of an unsuccessful exit code
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		fieldLogger.WithError(err).Error("Process wait failed")
	}

	// Get exit code
	exitCode := 255
	if processState != nil {
		fieldLogger = fieldLogger.WithField("process-state", fmt.Sprintf("%+v", processState))
		fieldLogger.Info("Got process state")

		if waitStatus, ok := processState.Sys().(syscall.WaitStatus); ok {
			exitStatus := waitStatus.ExitStatus()

			if waitStatus.Signaled() {
				exitCode = exitSignalOffset + int(waitStatus.Signal())
				fieldLogger.WithField("exit-code", exitCode).Info("process was signaled")
			} else {
				exitCode = exitStatus
				fieldLogger.WithField("exit-code", exitCode).Info("got wait exit code")
			}
		}

	} else {
		fieldLogger.Error("Process state is nil could not get process exit code")
	}

	return &pb.WaitProcessResponse{
		Status: int32(exitCode),
	}, nil
}

func (a *agentGRPC) RemoveContainer(ctx context.Context, req *pb.RemoveContainerRequest) (*gpb.Empty, error) {
	ctr, err := a.pod.getContainer(req.ContainerId)
	if err != nil {
		return emptyResp, err
	}

	a.pod.Lock()
	defer a.pod.Unlock()
	if err := ctr.removeContainer(); err != nil {
		return emptyResp, err
	}

	delete(a.pod.containers, ctr.id)

	return emptyResp, nil
}

func (a *agentGRPC) WriteStdin(ctx context.Context, req *pb.WriteStreamRequest) (*pb.WriteStreamResponse, error) {
	proc, _, err := a.pod.getRunningProcess(req.ContainerId, req.ProcessId)
	if err != nil {
		return &pb.WriteStreamResponse{}, err
	}

	var file *os.File
	if proc.termMaster != nil {
		file = proc.termMaster
	} else {
		file = proc.stdin
	}

	n, err := file.Write(req.Data)
	if err != nil {
		return &pb.WriteStreamResponse{}, err
	}

	return &pb.WriteStreamResponse{
		Len: uint32(n),
	}, nil
}

func (a *agentGRPC) ReadStdout(ctx context.Context, req *pb.ReadStreamRequest) (*pb.ReadStreamResponse, error) {
	data, err := a.pod.readStdio(req.ContainerId, req.ProcessId, int(req.Len), true)
	if err != nil {
		return &pb.ReadStreamResponse{}, err
	}

	return &pb.ReadStreamResponse{
		Data: data,
	}, nil
}

func (a *agentGRPC) ReadStderr(ctx context.Context, req *pb.ReadStreamRequest) (*pb.ReadStreamResponse, error) {
	data, err := a.pod.readStdio(req.ContainerId, req.ProcessId, int(req.Len), false)
	if err != nil {
		return &pb.ReadStreamResponse{}, err
	}

	return &pb.ReadStreamResponse{
		Data: data,
	}, nil
}

func (a *agentGRPC) CloseStdin(ctx context.Context, req *pb.CloseStdinRequest) (*gpb.Empty, error) {
	proc, _, err := a.pod.getRunningProcess(req.ContainerId, req.ProcessId)
	if err != nil {
		return emptyResp, err
	}

	var file *os.File
	if proc.termMaster != nil {
		file = proc.termMaster
	} else {
		file = proc.stdin
	}

	if err := file.Close(); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) TtyWinResize(ctx context.Context, req *pb.TtyWinResizeRequest) (*gpb.Empty, error) {
	proc, _, err := a.pod.getRunningProcess(req.ContainerId, req.ProcessId)
	if err != nil {
		return emptyResp, err
	}

	if proc.termMaster == nil {
		return emptyResp, fmt.Errorf("Terminal is not set, impossible to resize it")
	}

	fd := int(proc.termMaster.Fd())

	// Get current terminal size.
	winSize, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return emptyResp, err
	}

	winSize.Row = uint16(req.Row)
	winSize.Col = uint16(req.Column)

	// Set new terminal size.
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, winSize); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) StartSandbox(ctx context.Context, req *pb.StartSandboxRequest) (*gpb.Empty, error) {
	if a.pod.running == true {
		return emptyResp, fmt.Errorf("Pod already started, impossible to start again")
	}

	a.pod.id = req.Hostname
	a.pod.network.dns = req.Dns
	a.pod.running = true

	if err := mountShareDir(req.Storage.Source, req.Storage.MountPoint, req.Storage.Fstype); err != nil {
		return emptyResp, err
	}

	a.pod.shareDir = req.Storage.MountPoint

	if err := setupDNS(a.pod.network.dns); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) DestroySandbox(ctx context.Context, req *pb.DestroySandboxRequest) (*gpb.Empty, error) {
	if a.pod.running == false {
		agentLog.WithField("pod", a.pod.id).Info("Pod not started, this is a no-op")
		return emptyResp, nil
	}

	a.pod.Lock()
	for key, c := range a.pod.containers {
		if err := c.removeContainer(); err != nil {
			return emptyResp, err
		}

		delete(a.pod.containers, key)
	}
	a.pod.Unlock()

	if err := a.pod.removeNetwork(); err != nil {
		return emptyResp, err
	}

	if err := unmountShareDir(a.pod.shareDir); err != nil {
		return emptyResp, err
	}

	a.pod.id = ""
	a.pod.containers = make(map[string]*container)
	a.pod.running = false
	a.pod.network = network{}
	a.pod.shareDir = ""

	// Synchronize the caches on the system. This is needed to ensure
	// there is no pending transactions left before the VM is shut down.
	syscall.Sync()

	return emptyResp, nil
}

func (a *agentGRPC) AddInterfaces(ctx context.Context, req *pb.AddInterfacesRequest) (*gpb.Empty, error) {
	if err := a.pod.addInterfaces(req.Ifaces); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) AddRoutes(ctx context.Context, req *pb.AddRoutesRequest) (*gpb.Empty, error) {
	if err := a.pod.addRoutes(req.Routes); err != nil {
		return emptyResp, err
	}

	return emptyResp, nil
}

func (a *agentGRPC) OnlineCPUMem(ctx context.Context, req *pb.OnlineCPUMemRequest) (*gpb.Empty, error) {
	go onlineCPUMem()

	return emptyResp, nil
}
