// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build (linux && 386) || (linux && amd64)
// +build linux,386 linux,amd64

package socket

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"

	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/cfgwarn"
	"github.com/elastic/beats/v7/metricbeat/mb"
	"github.com/elastic/beats/v7/x-pack/auditbeat/module/system"
	"github.com/elastic/beats/v7/x-pack/auditbeat/module/system/socket/guess"
	"github.com/elastic/beats/v7/x-pack/auditbeat/module/system/socket/helper"
	"github.com/elastic/beats/v7/x-pack/auditbeat/tracing"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/go-perf"
	"github.com/elastic/go-sysinfo"
	"github.com/elastic/go-sysinfo/providers/linux"

	"github.com/elastic/beats/v7/x-pack/auditbeat/module/system/socket/dns"
	// Register dns capture implementations
	_ "github.com/elastic/beats/v7/x-pack/auditbeat/module/system/socket/dns/afpacket"
)

const (
	moduleName      = "system"
	metricsetName   = "socket"
	fullName        = moduleName + "/" + metricsetName
	namespace       = "system.audit.socket"
	detailSelector  = metricsetName + "detailed"
	groupNamePrefix = "auditbeat_"
	// Magic value to detect clock-sync events generated by the metricset.
	clockSyncMagic uint64 = 0x42DEADBEEFABCDEF
)

var (
	groupName     = fmt.Sprintf("%s%d", groupNamePrefix, os.Getpid())
	kernelVersion string
	eventCount    uint64
)

var defaultMounts = []*mountPoint{
	{fsType: "tracefs", path: "/sys/kernel/tracing"},
	{fsType: "debugfs", path: "/sys/kernel/debug"},
}

// MetricSet for system/socket.
type MetricSet struct {
	system.SystemMetricSet
	templateVars mapstr.M
	config       Config
	log          *logp.Logger
	detailLog    *logp.Logger
	installer    helper.ProbeInstaller
	sniffer      dns.Sniffer
	perfChannel  *tracing.PerfChannel
	mountedFS    *mountPoint
	isDebug      bool
	isDetailed   bool
	terminated   sync.WaitGroup
}

func init() {
	mb.Registry.MustAddMetricSet(moduleName, metricsetName, New,
		mb.DefaultMetricSet(),
		mb.WithNamespace(namespace),
	)
	var err error
	if kernelVersion, err = linux.KernelVersion(); err != nil {
		logp.Err("Failed fetching Linux kernel version: %v", err)
	}
}

var (
	// Singleton to instantiate one socket dataset at a time.
	instance      *MetricSet
	instanceMutex sync.Mutex
)

// New constructs a new MetricSet.
func New(base mb.BaseMetricSet) (mb.MetricSet, error) {
	instanceMutex.Lock()
	defer instanceMutex.Unlock()

	config := defaultConfig
	if err := base.Module().UnpackConfig(&config); err != nil {
		return nil, fmt.Errorf("failed to unpack the %s config: %w", fullName, err)
	}
	if instance != nil {
		// Do not instantiate a new dataset if the config hasn't changed.
		// This is necessary when run under config reloader even though the
		// reloader itself already checks the config for changes, because
		// the first time it runs it will allocate two consecutive instances
		// (one for checking the config, one for running). This saves
		// running the guesses twice on startup.
		if config.Equals(instance.config) {
			return instance, nil
		}
		instance.terminated.Wait()
	}
	var err error
	instance, err = newSocketMetricset(config, base)
	return instance, err
}

func newSocketMetricset(config Config, base mb.BaseMetricSet) (*MetricSet, error) {
	cfgwarn.Beta("The %s dataset is beta.", fullName)
	logger := logp.NewLogger(metricsetName)
	sniffer, err := dns.NewSniffer(base, logger)
	if err != nil {
		return nil, fmt.Errorf("unable to create DNS sniffer: %w", err)
	}
	ms := &MetricSet{
		SystemMetricSet: system.NewSystemMetricSet(base),
		templateVars:    make(mapstr.M),
		config:          config,
		log:             logger,
		isDebug:         logp.IsDebug(metricsetName),
		detailLog:       logp.NewLogger(detailSelector),
		isDetailed:      logp.HasSelector(detailSelector),
		sniffer:         sniffer,
	}
	// Setup the metricset before Run() so that startup can be halted in case of
	// error.
	if err = ms.Setup(); err != nil {
		return nil, fmt.Errorf("%s dataset setup failed: %w", fullName, err)
	}
	return ms, nil
}

// Run the metricset. This will loop until the passed reporter is cancelled.
func (m *MetricSet) Run(r mb.PushReporterV2) {
	m.terminated.Add(1)
	defer m.log.Infof("%s terminated.", fullName)
	defer m.terminated.Done()
	defer m.Cleanup()

	st := NewState(r,
		m.log,
		m.config.FlowInactiveTimeout,
		m.config.SocketInactiveTimeout,
		m.config.FlowTerminationTimeout,
		m.config.ClockMaxDrift)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.sniffer.Monitor(ctx, func(tr dns.Transaction) {
		if err := st.OnDNSTransaction(tr); err != nil {
			m.log.Errorf("Unable to store DNS transaction %+v: %v", tr, err)
		}
	}); err != nil {
		err = fmt.Errorf("unable to start DNS sniffer: %w", err)
		r.Error(err)
		m.log.Error(err)
		return
	}

	if err := m.perfChannel.Run(); err != nil {
		err = fmt.Errorf("unable to start perf channel: %w", err)
		r.Error(err)
		m.log.Error(err)
		return
	}
	// Launch the clock-synchronization ticker.
	go m.clockSyncLoop(m.config.ClockSyncPeriod, r.Done())

	if procs, err := sysinfo.Processes(); err != nil {
		m.log.Error("Failed to bootstrap process table using /proc", err)
	} else {
		for _, p := range procs {
			if i, err := p.Info(); err == nil {
				if len(i.Name) == 16 && len(i.Args) != 0 {
					// github.com/prometheus/procfs uses /proc/<pid>/stat for
					// the process name which is truncated to 16 bytes, so get
					// the name from the cmdline data if it might be truncated.
					// The guard for length of i.Args is for cases where there
					// is no command line reported by proc fs; this should never
					// happen, but does.
					i.Name = filepath.Base(i.Args[0])
				}
				process := &process{
					name:        i.Name,
					pid:         uint32(i.PID),
					args:        i.Args,
					createdTime: i.StartTime,
					path:        i.Exe,
				}

				if user, err := p.User(); err == nil {
					toUint32 := func(id string) uint32 {
						num, _ := strconv.Atoi(id)
						return uint32(num)
					}
					process.uid = toUint32(user.UID)
					process.euid = toUint32(user.EUID)
					process.gid = toUint32(user.GID)
					process.egid = toUint32(user.EGID)
					process.hasCreds = true
				}

				st.CreateProcess(process)

				if m.HostID() != "" && !process.createdTime.IsZero() {
					process.entityID = entityID(m.HostID(), process)
				}
			}
		}
		m.log.Info("Bootstrapped process table using /proc")
	}

	m.log.Infof("%s dataset is running.", fullName)
	// Dispatch loop.
	for running := true; running; {
		select {
		case <-r.Done():
			running = false

		case iface, ok := <-m.perfChannel.C():
			if !ok {
				running = false
				break
			}
			v, ok := iface.(event)
			if !ok {
				m.log.Errorf("Received an event of wrong type: %T", iface)
				continue
			}
			if m.isDetailed {
				m.detailLog.Debug(v.String())
			}
			if err := v.Update(st); err != nil && m.isDetailed {
				// These errors are seldom interesting, as the flow state engine
				// doesn't have many error conditions and all benign enough to
				// not be worth logging them by default.
				m.detailLog.Warnf("Issue while processing event '%s': %v", v.String(), err)
			}
			atomic.AddUint64(&eventCount, 1)

		case err := <-m.perfChannel.ErrC():
			m.log.Errorf("Error received from perf channel: %v", err)
			running = false

		case numLost := <-m.perfChannel.LostC():
			if numLost != ^uint64(0) {
				m.log.Warnf("Lost %d events", numLost)
			} else {
				m.log.Warn("Lost the whole ringbuffer")
			}
		}
	}
}

// entityID creates an ID that uniquely identifies this process across machines.
func entityID(hostID string, p *process) string {
	h := system.NewEntityHash()
	h.Write([]byte(hostID))
	binary.Write(h, binary.LittleEndian, int64(p.pid))
	binary.Write(h, binary.LittleEndian, int64(p.createdTime.Nanosecond()))
	return h.Sum()
}

// Setup performs all the initialisations required for KProbes monitoring.
func (m *MetricSet) Setup() (err error) {
	m.log.Infof("Setting up %s for kernel %s", fullName, kernelVersion)

	//
	// Validate that tracefs / debugfs is present and kprobes are available
	//
	var traceFS *tracing.TraceFS
	if m.config.TraceFSPath == nil {
		if err := tracing.IsTraceFSAvailable(); err != nil {
			m.log.Debugf("tracefs/debugfs not found. Attempting to mount")
			for _, mount := range defaultMounts {
				if err = mount.mount(); err != nil {
					m.log.Debugf("Mount %s returned %v", mount, err)
					continue
				}
				if tracing.IsTraceFSAvailable() != nil {
					m.log.Warnf("Mounted %s but no kprobes available", mount, err)
					mount.unmount()
					continue
				}
				m.log.Debugf("Mounted %s", mount)
				m.mountedFS = mount
				break
			}
		}
		traceFS, err = tracing.NewTraceFS()
	} else {
		traceFS, err = tracing.NewTraceFSWithPath(*m.config.TraceFSPath)
	}
	if err != nil {
		return fmt.Errorf("tracefs/debugfs is not mounted or not writeable: %w", err)
	}

	//
	// Setup initial template variables
	//
	m.templateVars.Update(baseTemplateVars)
	m.templateVars.Update(archVariables)

	//
	// Detect IPv6 support
	//

	hasIPv6, err := detectIPv6()
	if err != nil {
		m.log.Debugf("Error detecting IPv6 support: %v", err)
		hasIPv6 = false
	}
	m.log.Debugf("IPv6 supported: %v", hasIPv6)
	if m.config.EnableIPv6 != nil {
		if *m.config.EnableIPv6 && !hasIPv6 {
			return errors.New("requested IPv6 support but IPv6 is disabled in the system")
		}
		hasIPv6 = *m.config.EnableIPv6
	}
	m.log.Debugf("IPv6 enabled: %v", hasIPv6)
	m.templateVars["HAS_IPV6"] = hasIPv6

	//
	// Create probe installer
	//
	extra := WithNoOp()
	if m.config.DevelopmentMode {
		extra = WithFilterPort(22)
	}
	m.installer = newProbeInstaller(traceFS,
		WithGroup(groupName),
		WithTemplates(m.templateVars),
		extra)
	defer func() {
		if err != nil {
			m.installer.UninstallInstalled()
		}
	}()

	//
	// remove dangling KProbes from terminated Auditbeat processes.
	// Not a fatal error if they can't be removed.
	//
	if err = m.installer.UninstallIf(isDeadAuditbeat); err != nil {
		m.log.Debugf("Removing existing probes from terminated instances: %+v", err)
	}

	//
	// remove existing Auditbeat KProbes that match the current PID.
	//
	if err = m.installer.UninstallIf(isThisAuditbeat); err != nil {
		return fmt.Errorf("unable to delete existing KProbes for group %s: %w", groupName, err)
	}

	//
	// Load available kernel functions for tracing
	//
	functions, err := LoadTracingFunctions(traceFS)
	if err != nil {
		m.log.Debugf("Can't load available_tracing_functions. Using alternative. err=%v", err)
	}

	//
	// Resolve function names from alternatives
	//
	for varName, alternatives := range functionAlternatives {
		if exists, _ := m.templateVars.HasKey(varName); exists {
			return fmt.Errorf("variable %s overwrites existing key", varName)
		}
		found := false
		var selected string
		for _, selected = range alternatives {
			if found = m.isKernelFunctionAvailable(selected, functions); found {
				break
			}
		}
		if !found {
			return fmt.Errorf("none of the required functions for %s is found. One of %v is required", varName, alternatives)
		}
		if m.isDebug {
			m.log.Debugf("Selected kernel function %s for %s", selected, varName)
		}
		m.templateVars[varName] = selected
	}

	//
	// Make sure all the required kernel functions are available
	//
	for _, probeDef := range getKProbes(hasIPv6) {
		if slices.Index(m.config.DisableKprobe, probeDef.Probe.Name) != -1 {
			continue
		}
		probeDef = probeDef.ApplyTemplate(m.templateVars)
		name := probeDef.Probe.Address
		if !m.isKernelFunctionAvailable(name, functions) {
			return fmt.Errorf("required function '%s' is not available for tracing in the current kernel (%s)", name, kernelVersion)
		}
	}

	//
	// Guess all the required parameters
	//
	if err = guess.GuessAll(m.installer,
		guess.Context{
			Log:     m.log,
			Vars:    m.templateVars,
			Timeout: m.config.GuessTimeout,
		}); err != nil {
		return fmt.Errorf("unable to guess one or more required parameters: %w", err)
	}

	if m.isDebug {
		names := make([]string, 0, len(m.templateVars))
		for name := range m.templateVars {
			names = append(names, name)
		}
		sort.Strings(names)
		m.log.Debugf("%d template variables in use:", len(m.templateVars))
		for _, key := range names {
			m.log.Debugf("  %s = %v", key, m.templateVars[key])
		}
	}

	//
	// Create perf channel
	//
	m.perfChannel, err = tracing.NewPerfChannel(
		tracing.WithBufferSize(m.config.PerfQueueSize),
		tracing.WithErrBufferSize(m.config.ErrQueueSize),
		tracing.WithLostBufferSize(m.config.LostQueueSize),
		tracing.WithRingSizeExponent(m.config.RingSizeExp),
		tracing.WithTID(perf.AllThreads),
		tracing.WithTimestamp())
	if err != nil {
		return fmt.Errorf("unable to create perf channel: %w", err)
	}

	//
	// Register Kprobes
	//
	for _, probeDef := range getKProbes(hasIPv6) {
		if slices.Index(m.config.DisableKprobe, probeDef.Probe.Name) != -1 {
			continue
		}
		format, decoder, err := m.installer.Install(probeDef)
		if err != nil {
			return fmt.Errorf("unable to register probe %s: %w", probeDef.Probe.String(), err)
		}
		if err = m.perfChannel.MonitorProbe(format, decoder); err != nil {
			return fmt.Errorf("unable to monitor probe %s: %w", probeDef.Probe.String(), err)
		}
	}
	return nil
}

// Cleanup must be called so that kprobes are not left around after exit.
func (m *MetricSet) Cleanup() {
	if m.perfChannel != nil {
		if err := m.perfChannel.Close(); err != nil {
			m.log.Warnf("Failed to close perf channel on exit: %v", err)
		}
	}
	if m.installer != nil {
		if err := m.installer.UninstallIf(isThisAuditbeat); err != nil {
			m.log.Warnf("Failed to remove KProbes on exit: %v", err)
		}
	}
	if m.mountedFS != nil {
		if err := m.mountedFS.unmount(); err != nil {
			m.log.Errorf("Failed to umount %s: %v", m.mountedFS, err)
		} else {
			m.log.Debugf("Unmounted %s", m.mountedFS)
		}
	}
}

func (m *MetricSet) clockSyncLoop(interval time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	triggerClockSync()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			triggerClockSync()
		}
	}
}

func (m *MetricSet) isKernelFunctionAvailable(name string, tracingFns common.StringSet) bool {
	if tracingFns.Count() != 0 {
		return tracingFns.Has(name)
	}
	defer m.installer.UninstallInstalled()
	checkProbe := helper.ProbeDef{
		Probe: tracing.Probe{
			Name:      "check_" + name,
			Address:   name,
			Fetchargs: "%ax:u64", // dump decoder needs it.
		},
		Decoder: tracing.NewDumpDecoder,
	}
	_, _, err := m.installer.Install(checkProbe)
	return err == nil
}

func triggerClockSync() {
	// This generates a uname (SYS_UNAME) syscall event that contains
	// clockSyncMagic at the first 8 bytes of the passed buffer and
	// the current UNIX nano timestamp at the following 8 bytes.
	//
	// The magic bytes are used to filter-out legitimate uname() calls
	// from this process and the timestamp is used as a reference point for
	// synchronization with the internal clock that the kernel uses for stamping
	// the tracing events it produces.
	var buf unix.Utsname
	tracing.MachineEndian.PutUint64(buf.Sysname[:], clockSyncMagic)
	tracing.MachineEndian.PutUint64(buf.Sysname[8:], uint64(time.Now().UnixNano()))
	unix.Uname(&buf)
}

func isRunningAuditbeat(pid int) bool {
	path := fmt.Sprintf("/proc/%d/exe", pid)
	exePath, err := os.Readlink(path)
	if err != nil {
		// Not a running process
		return false
	}
	exeName := filepath.Base(exePath)
	return strings.HasPrefix(exeName, "auditbeat")
}

func isDeadAuditbeat(probe tracing.Probe) bool {
	if strings.HasPrefix(probe.Group, groupNamePrefix) && probe.Group != groupName {
		if pid, err := strconv.Atoi(probe.Group[len(groupNamePrefix):]); err == nil && !isRunningAuditbeat(pid) {
			return true
		}
	}
	return false
}

func isThisAuditbeat(probe tracing.Probe) bool {
	return probe.Group == groupName
}

type mountPoint struct {
	fsType string
	path   string
}

func (m mountPoint) mount() error {
	return unix.Mount(m.fsType, m.path, m.fsType, 0, "")
}

func (m mountPoint) unmount() error {
	return syscall.Unmount(m.path, 0)
}

func (m *mountPoint) String() string {
	return m.fsType + " at " + m.path
}

func detectIPv6() (bool, error) {
	// Check that AF_INET6 is available.
	// This fails when the kernel is booted with ipv6.disable=1
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		return false, nil
	}
	unix.Close(fd)
	loopback, err := helper.NewIPv6Loopback()
	if err != nil {
		return false, err
	}
	defer loopback.Cleanup()
	_, err = loopback.AddRandomAddress()
	// Assume that all failures for Add..() are caused by missing IPv6 support.
	return err == nil, nil
}
