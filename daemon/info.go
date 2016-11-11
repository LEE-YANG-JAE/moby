package daemon

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/container"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/docker/pkg/parsers/operatingsystem"
	"github.com/docker/docker/pkg/platform"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/utils"
	"github.com/docker/docker/volume/drivers"
	"github.com/docker/go-connections/sockets"
)

// SystemInfo returns information about the host server the daemon is running on.
func (daemon *Daemon) SystemInfo() (*types.Info, error) {
	kernelVersion := "<unknown>"
	if kv, err := kernel.GetKernelVersion(); err != nil {
		logrus.Warnf("Could not get kernel version: %v", err)
	} else {
		kernelVersion = kv.String()
	}

	operatingSystem := "<unknown>"
	if s, err := operatingsystem.GetOperatingSystem(); err != nil {
		logrus.Warnf("Could not get operating system name: %v", err)
	} else {
		operatingSystem = s
	}

	// Don't do containerized check on Windows
	if runtime.GOOS != "windows" {
		if inContainer, err := operatingsystem.IsContainerized(); err != nil {
			logrus.Errorf("Could not determine if daemon is containerized: %v", err)
			operatingSystem += " (error determining if containerized)"
		} else if inContainer {
			operatingSystem += " (containerized)"
		}
	}

	meminfo, err := system.ReadMemInfo()
	if err != nil {
		logrus.Errorf("Could not read system memory info: %v", err)
		meminfo = &system.MemInfo{}
	}

	sysInfo := sysinfo.New(true)

	var cRunning, cPaused, cStopped int32
	daemon.containers.ApplyAll(func(c *container.Container) {
		switch c.StateString() {
		case "paused":
			atomic.AddInt32(&cPaused, 1)
		case "running":
			atomic.AddInt32(&cRunning, 1)
		default:
			atomic.AddInt32(&cStopped, 1)
		}
	})

	securityOptions := []types.SecurityOpt{}
	if sysInfo.AppArmor {
		securityOptions = append(securityOptions, types.SecurityOpt{Key: "Name", Value: "apparmor"})
	}
	if sysInfo.Seccomp && supportsSeccomp {
		profile := daemon.seccompProfilePath
		if profile == "" {
			profile = "default"
		}
		securityOptions = append(securityOptions,
			types.SecurityOpt{Key: "Name", Value: "seccomp"},
			types.SecurityOpt{Key: "Profile", Value: profile},
		)
	}
	if selinuxEnabled() {
		securityOptions = append(securityOptions, types.SecurityOpt{Key: "Name", Value: "selinux"})
	}
	uid, gid := daemon.GetRemappedUIDGID()
	if uid != 0 || gid != 0 {
		securityOptions = append(securityOptions, types.SecurityOpt{Key: "Name", Value: "userns"})
	}

	v := &types.InfoBase{
		ID:                 daemon.ID,
		Containers:         int(cRunning + cPaused + cStopped),
		ContainersRunning:  int(cRunning),
		ContainersPaused:   int(cPaused),
		ContainersStopped:  int(cStopped),
		Images:             len(daemon.imageStore.Map()),
		Driver:             daemon.GraphDriverName(),
		DriverStatus:       daemon.layerStore.DriverStatus(),
		Plugins:            daemon.showPluginsInfo(),
		IPv4Forwarding:     !sysInfo.IPv4ForwardingDisabled,
		BridgeNfIptables:   !sysInfo.BridgeNFCallIPTablesDisabled,
		BridgeNfIP6tables:  !sysInfo.BridgeNFCallIP6TablesDisabled,
		Debug:              utils.IsDebugEnabled(),
		NFd:                fileutils.GetTotalUsedFds(),
		NGoroutines:        runtime.NumGoroutine(),
		SystemTime:         time.Now().Format(time.RFC3339Nano),
		LoggingDriver:      daemon.defaultLogConfig.Type,
		CgroupDriver:       daemon.getCgroupDriver(),
		NEventsListener:    daemon.EventsService.SubscribersCount(),
		KernelVersion:      kernelVersion,
		OperatingSystem:    operatingSystem,
		IndexServerAddress: registry.IndexServer,
		OSType:             platform.OSType,
		Architecture:       platform.Architecture,
		RegistryConfig:     daemon.RegistryService.ServiceConfig(),
		NCPU:               sysinfo.NumCPU(),
		MemTotal:           meminfo.MemTotal,
		DockerRootDir:      daemon.configStore.Root,
		Labels:             daemon.configStore.Labels,
		ExperimentalBuild:  daemon.configStore.Experimental,
		ServerVersion:      dockerversion.Version,
		ClusterStore:       daemon.configStore.ClusterStore,
		ClusterAdvertise:   daemon.configStore.ClusterAdvertise,
		HTTPProxy:          sockets.GetProxyEnv("http_proxy"),
		HTTPSProxy:         sockets.GetProxyEnv("https_proxy"),
		NoProxy:            sockets.GetProxyEnv("no_proxy"),
		LiveRestoreEnabled: daemon.configStore.LiveRestoreEnabled,
		Isolation:          daemon.defaultIsolation,
	}

	// TODO Windows. Refactor this more once sysinfo is refactored into
	// platform specific code. On Windows, sysinfo.cgroupMemInfo and
	// sysinfo.cgroupCpuInfo will be nil otherwise and cause a SIGSEGV if
	// an attempt is made to access through them.
	if runtime.GOOS != "windows" {
		v.MemoryLimit = sysInfo.MemoryLimit
		v.SwapLimit = sysInfo.SwapLimit
		v.KernelMemory = sysInfo.KernelMemory
		v.OomKillDisable = sysInfo.OomKillDisable
		v.CPUCfsPeriod = sysInfo.CPUCfsPeriod
		v.CPUCfsQuota = sysInfo.CPUCfsQuota
		v.CPUShares = sysInfo.CPUShares
		v.CPUSet = sysInfo.Cpuset
		v.Runtimes = daemon.configStore.GetAllRuntimes()
		v.DefaultRuntime = daemon.configStore.GetDefaultRuntimeName()
		v.InitBinary = daemon.configStore.GetInitPath()

		v.ContainerdCommit.Expected = dockerversion.ContainerdCommitID
		if sv, err := daemon.containerd.GetServerVersion(context.Background()); err == nil {
			v.ContainerdCommit.ID = sv.Revision
		} else {
			logrus.Warnf("failed to retrieve containerd version: %v", err)
			v.ContainerdCommit.ID = "N/A"
		}

		v.RuncCommit.Expected = dockerversion.RuncCommitID
		if rv, err := exec.Command(DefaultRuntimeBinary, "--version").Output(); err == nil {
			parts := strings.Split(strings.TrimSpace(string(rv)), "\n")
			if len(parts) == 3 {
				parts = strings.Split(parts[1], ": ")
				if len(parts) == 2 {
					v.RuncCommit.ID = strings.TrimSpace(parts[1])
				}
			}

			if v.RuncCommit.ID == "" {
				logrus.Warnf("failed to retrieve %s version: unknown output format: %s", DefaultRuntimeBinary, string(rv))
				v.RuncCommit.ID = "N/A"
			}
		} else {
			logrus.Warnf("failed to retrieve %s version: %v", DefaultRuntimeBinary, err)
			v.RuncCommit.ID = "N/A"
		}

		v.InitCommit.Expected = dockerversion.InitCommitID
		if rv, err := exec.Command(DefaultInitBinary, "--version").Output(); err == nil {
			parts := strings.Split(strings.TrimSpace(string(rv)), " - ")
			if len(parts) == 2 {
				if dockerversion.InitCommitID[0] == 'v' {
					vs := strings.TrimPrefix(parts[0], "tini version ")
					v.InitCommit.ID = "v" + vs
				} else {
					// Get the sha1
					gitParts := strings.Split(parts[1], ".")
					if len(gitParts) == 2 && gitParts[0] == "git" {
						v.InitCommit.ID = gitParts[1]
						v.InitCommit.Expected = dockerversion.InitCommitID[0:len(gitParts[1])]
					}
				}
			}

			if v.InitCommit.ID == "" {
				logrus.Warnf("failed to retrieve %s version: unknown output format: %s", DefaultInitBinary, string(rv))
				v.InitCommit.ID = "N/A"
			}
		} else {
			logrus.Warnf("failed to retrieve %s version", DefaultInitBinary)
			v.InitCommit.ID = "N/A"
		}
	}

	hostname := ""
	if hn, err := os.Hostname(); err != nil {
		logrus.Warnf("Could not get hostname: %v", err)
	} else {
		hostname = hn
	}
	v.Name = hostname

	i := &types.Info{
		InfoBase:        v,
		SecurityOptions: securityOptions,
	}

	return i, nil
}

// SystemVersion returns version information about the daemon.
func (daemon *Daemon) SystemVersion() types.Version {
	v := types.Version{
		Version:       dockerversion.Version,
		GitCommit:     dockerversion.GitCommit,
		MinAPIVersion: api.MinVersion,
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		BuildTime:     dockerversion.BuildTime,
		Experimental:  daemon.configStore.Experimental,
	}

	kernelVersion := "<unknown>"
	if kv, err := kernel.GetKernelVersion(); err != nil {
		logrus.Warnf("Could not get kernel version: %v", err)
	} else {
		kernelVersion = kv.String()
	}
	v.KernelVersion = kernelVersion

	return v
}

func (daemon *Daemon) showPluginsInfo() types.PluginsInfo {
	var pluginsInfo types.PluginsInfo

	pluginsInfo.Volume = volumedrivers.GetDriverList()
	pluginsInfo.Network = daemon.GetNetworkDriverList()
	pluginsInfo.Authorization = daemon.configStore.AuthorizationPlugins

	return pluginsInfo
}
