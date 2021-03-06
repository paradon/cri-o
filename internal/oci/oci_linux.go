// +build linux

package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containers/libpod/pkg/cgroups"
	"github.com/cri-o/cri-o/utils"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func createUnitName(prefix, name string) string {
	return fmt.Sprintf("%s-%s.scope", prefix, name)
}

func createConmonUnitName(name string) string {
	return createUnitName("crio-conmon", name)
}

func (r *runtimeOCI) createContainerPlatform(c *Container, cgroupParent string, pid int) {
	// Move conmon to specified cgroup
	if r.config.ConmonCgroup == "pod" || r.config.ConmonCgroup == "" {
		switch r.config.CgroupManager {
		case SystemdCgroupsManager:
			logrus.Debugf("Running conmon under slice %s and unitName %s", cgroupParent, createConmonUnitName(c.id))
			if err := utils.RunUnderSystemdScope(pid, cgroupParent, createConmonUnitName(c.id)); err != nil {
				logrus.Warnf("Failed to add conmon to systemd sandbox cgroup: %v", err)
			}
		case CgroupfsCgroupsManager:
			cgroupPath := filepath.Join(cgroupParent, "/crio-conmon-"+c.id)
			control, err := cgroups.New(cgroupPath, &rspec.LinuxResources{})
			if err != nil {
				logrus.Warnf("Failed to add conmon to cgroupfs sandbox cgroup: %v", err)
			}
			if control == nil {
				break
			}
			// Record conmon's cgroup path in the container, so we can properly
			// clean it up when removing the container.
			c.conmonCgroupfsPath = cgroupPath
			// Here we should defer a crio-connmon- cgroup hierarchy deletion, but it will
			// always fail as conmon's pid is still there.
			// Fortunately, kubelet takes care of deleting this for us, so the leak will
			// only happens in corner case where one does a manual deletion of the container
			// through e.g. runc. This should be handled by implementing a conmon monitoring
			// routine that does the cgroup cleanup once conmon is terminated.
			if err := control.AddPid(pid); err != nil {
				logrus.Warnf("Failed to add conmon to cgroupfs sandbox cgroup: %v", err)
			}
		default:
			// error for an unknown cgroups manager
			logrus.Errorf("unknown cgroups manager %q for sandbox cgroup", r.config.CgroupManager)
		}
	} else if strings.HasSuffix(r.config.ConmonCgroup, ".slice") {
		logrus.Debugf("Running conmon under custom slice %s and unitName %s", r.config.ConmonCgroup, createConmonUnitName(c.id))
		if err := utils.RunUnderSystemdScope(pid, r.config.ConmonCgroup, createConmonUnitName(c.id)); err != nil {
			logrus.Warnf("Failed to add conmon to custom systemd sandbox cgroup: %v", err)
		}
	}
}

func sysProcAttrPlatform() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// newPipe creates a unix socket pair for communication
func newPipe() (parent, child *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[1]), "parent"), os.NewFile(uintptr(fds[0]), "child"), nil
}

func (r *runtimeOCI) containerStats(ctr *Container, cgroup string) (*ContainerStats, error) {
	cg, err := cgroups.Load(cgroup)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to load cgroup at %s", cgroup)
	}

	cgroupStats, err := cg.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to obtain cgroup stats")
	}

	stats := &ContainerStats{}
	stats.Container = ctr.ID()
	stats.CPUNano = cgroupStats.CPU.Usage.Total
	stats.SystemNano = time.Now().UnixNano()
	stats.CPU = calculateCPUPercent(cgroupStats)
	stats.MemUsage = cgroupStats.Memory.Usage.Usage
	stats.MemLimit = getMemLimit(cgroupStats.Memory.Usage.Limit)
	stats.MemPerc = float64(stats.MemUsage) / float64(stats.MemLimit)
	stats.PIDs = cgroupStats.Pids.Current
	stats.BlockInput, stats.BlockOutput = calculateBlockIO(cgroupStats)

	if ctr.state != nil {
		netNsPath := fmt.Sprintf("/proc/%d/ns/net", ctr.state.Pid)
		stats.NetInput, stats.NetOutput = getContainerNetIO(netNsPath)
	}

	return stats, nil
}

func metricsToCtrStats(c *Container, m *cgroups.Metrics) *ContainerStats {
	var (
		cpu         float64
		cpuNano     uint64
		memUsage    uint64
		memLimit    uint64
		memPerc     float64
		netInput    uint64
		netOutput   uint64
		blockInput  uint64
		blockOutput uint64
		pids        uint64
	)

	if m != nil {
		pids = m.Pids.Current

		cpuNano = m.CPU.Usage.Total
		cpu = genericCalculateCPUPercent(cpuNano, m.CPU.Usage.PerCPU)

		memUsage = m.Memory.Usage.Usage
		memLimit = getMemLimit(m.Memory.Usage.Limit)
		memPerc = float64(memUsage) / float64(memLimit)

		for _, entry := range m.Blkio.IoServiceBytesRecursive {
			switch strings.ToLower(entry.Op) {
			case "read":
				blockInput += entry.Value
			case "write":
				blockOutput += entry.Value
			}
		}
	}

	return &ContainerStats{
		Container:   c.ID(),
		CPU:         cpu,
		CPUNano:     cpuNano,
		SystemNano:  time.Now().UnixNano(),
		MemUsage:    memUsage,
		MemLimit:    memLimit,
		MemPerc:     memPerc,
		NetInput:    netInput,
		NetOutput:   netOutput,
		BlockInput:  blockInput,
		BlockOutput: blockOutput,
		PIDs:        pids,
	}
}
