//
// MinIO Object Storage (c) 2021 MinIO, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package madmin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/minio/madmin-go/cgroup"
	"github.com/minio/madmin-go/kernel"
	"github.com/prometheus/procfs"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	// HealthInfoVersion0 is version 0
	HealthInfoVersion0 = ""
	// HealthInfoVersion1 is version 1
	HealthInfoVersion1 = "1"
	// HealthInfoVersion2 is version 2
	HealthInfoVersion2 = "2"
	// HealthInfoVersion3 is version 3
	HealthInfoVersion3 = "3"
	// HealthInfoVersion is current health info version.
	HealthInfoVersion = HealthInfoVersion3
)

const (
	SysErrAuditEnabled      = "audit is enabled"
	SysErrUpdatedbInstalled = "updatedb is installed"
)

const (
	SrvSELinux      = "selinux"
	SrvNotInstalled = "not-installed"
)

// NodeInfo - Interface to abstract any struct that contains address/endpoint and error fields
type NodeInfo interface {
	GetAddr() string
	SetAddr(addr string)
	SetError(err string)
}

// NodeCommon - Common fields across most node-specific health structs
type NodeCommon struct {
	Addr  string `json:"addr"`
	Error string `json:"error,omitempty"`
}

// GetAddr - return the address of the node
func (n *NodeCommon) GetAddr() string {
	return n.Addr
}

// SetAddr - set the address of the node
func (n *NodeCommon) SetAddr(addr string) {
	n.Addr = addr
}

// SetError - set the address of the node
func (n *NodeCommon) SetError(err string) {
	n.Error = err
}

// SysErrors - contains a system error
type SysErrors struct {
	NodeCommon

	Errors []string `json:"errors,omitempty"`
}

// SysServices - info about services that affect minio
type SysServices struct {
	NodeCommon

	Services []SysService `json:"services,omitempty"`
}

// SysConfig - info about services that affect minio
type SysConfig struct {
	NodeCommon

	Config map[string]interface{} `json:"config,omitempty"`
}

// SysService - name and status of a sys service
type SysService struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// CPU contains system's CPU information.
type CPU struct {
	VendorID   string   `json:"vendor_id"`
	Family     string   `json:"family"`
	Model      string   `json:"model"`
	Stepping   int32    `json:"stepping"`
	PhysicalID string   `json:"physical_id"`
	ModelName  string   `json:"model_name"`
	Mhz        float64  `json:"mhz"`
	CacheSize  int32    `json:"cache_size"`
	Flags      []string `json:"flags"`
	Microcode  string   `json:"microcode"`
	Cores      int      `json:"cores"` // computed
}

// CPUs contains all CPU information of a node.
type CPUs struct {
	NodeCommon

	CPUs          []CPU `json:"cpus,omitempty"`
	IsFreqGovPerf *bool `json:"is_freq_gov_perf,omitempty"`
}

// GetCPUs returns system's all CPU information.
func GetCPUs(ctx context.Context, addr string) CPUs {
	infos, err := cpu.InfoWithContext(ctx)
	if err != nil {
		return CPUs{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	cpuMap := map[string]CPU{}
	for _, info := range infos {
		cpu, found := cpuMap[info.PhysicalID]
		if found {
			cpu.Cores++
		} else {
			cpu = CPU{
				VendorID:   info.VendorID,
				Family:     info.Family,
				Model:      info.Model,
				Stepping:   info.Stepping,
				PhysicalID: info.PhysicalID,
				ModelName:  info.ModelName,
				Mhz:        info.Mhz,
				CacheSize:  info.CacheSize,
				Flags:      info.Flags,
				Microcode:  info.Microcode,
				Cores:      1,
			}
		}
		cpuMap[info.PhysicalID] = cpu
	}

	cpus := []CPU{}
	for _, cpu := range cpuMap {
		cpus = append(cpus, cpu)
	}

	var igp *bool
	isGovPerf, err := isFreqGovPerf()
	if err == nil {
		igp = &isGovPerf
	}

	return CPUs{
		NodeCommon:    NodeCommon{Addr: addr},
		CPUs:          cpus,
		IsFreqGovPerf: igp,
	}
}

// Partition contains disk partition's information.
type Partition struct {
	Error string `json:"error,omitempty"`

	Device       string `json:"device,omitempty"`
	Mountpoint   string `json:"mountpoint,omitempty"`
	FSType       string `json:"fs_type,omitempty"`
	MountOptions string `json:"mount_options,omitempty"`
	MountFSType  string `json:"mount_fs_type,omitempty"`
	SpaceTotal   uint64 `json:"space_total,omitempty"`
	SpaceFree    uint64 `json:"space_free,omitempty"`
	InodeTotal   uint64 `json:"inode_total,omitempty"`
	InodeFree    uint64 `json:"inode_free,omitempty"`
}

// Partitions contains all disk partitions information of a node.
type Partitions struct {
	NodeCommon

	Partitions []Partition `json:"partitions,omitempty"`
}

// GetPartitions returns all disk partitions information of a node running linux only operating system.
func GetPartitions(ctx context.Context, addr string) Partitions {
	if runtime.GOOS != "linux" {
		return Partitions{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: "unsupported operating system " + runtime.GOOS,
			},
		}
	}

	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return Partitions{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	partitions := []Partition{}

	for i := range parts {
		usage, err := disk.UsageWithContext(ctx, parts[i].Mountpoint)
		if err != nil {
			partitions = append(partitions, Partition{
				Device: parts[i].Device,
				Error:  err.Error(),
			})
		} else {
			partitions = append(partitions, Partition{
				Device:       parts[i].Device,
				Mountpoint:   parts[i].Mountpoint,
				FSType:       parts[i].Fstype,
				MountOptions: strings.Join(parts[i].Opts, ","),
				MountFSType:  usage.Fstype,
				SpaceTotal:   usage.Total,
				SpaceFree:    usage.Free,
				InodeTotal:   usage.InodesTotal,
				InodeFree:    usage.InodesFree,
			})
		}
	}

	return Partitions{
		NodeCommon: NodeCommon{Addr: addr},
		Partitions: partitions,
	}
}

// OSInfo contains operating system's information.
type OSInfo struct {
	NodeCommon

	Info    host.InfoStat          `json:"info,omitempty"`
	Sensors []host.TemperatureStat `json:"sensors,omitempty"`
}

// TimeInfo contains current time in UTC, and the
// roundtrip duration when fetching it remotely
type TimeInfo struct {
	CurrentTime       time.Time `json:"current_time"`
	RoundtripDuration int32     `json:"roundtrip_duration"`
}

// GetOSInfo returns linux only operating system's information.
func GetOSInfo(ctx context.Context, addr string) OSInfo {
	if runtime.GOOS != "linux" {
		return OSInfo{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: "unsupported operating system " + runtime.GOOS,
			},
		}
	}

	kr, err := kernel.CurrentRelease()
	if err != nil {
		return OSInfo{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	info, err := host.InfoWithContext(ctx)
	if err != nil {
		return OSInfo{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	osInfo := OSInfo{
		NodeCommon: NodeCommon{Addr: addr},
		Info:       *info,
	}
	osInfo.Info.KernelVersion = kr

	osInfo.Sensors, err = host.SensorsTemperaturesWithContext(ctx)
	if err != nil {
		if _, isWarningErr := err.(*host.Warnings); !isWarningErr {
			osInfo.Error = err.Error()
		}
	}

	return osInfo
}

// GetSysConfig returns config values from the system
// (only those affecting minio performance)
func GetSysConfig(ctx context.Context, addr string) SysConfig {
	sc := SysConfig{
		NodeCommon: NodeCommon{Addr: addr},
		Config:     map[string]interface{}{},
	}
	proc, err := procfs.Self()
	if err != nil {
		sc.Error = "rlimit: " + err.Error()
	} else {
		limits, err := proc.Limits()
		if err != nil {
			sc.Error = "rlimit: " + err.Error()
		}
		sc.Config["rlimit-max"] = limits.OpenFiles
	}

	sc.Config["time-info"] = TimeInfo{CurrentTime: time.Now().UTC()}

	return sc
}

// GetSysServices returns info of sys services that affect minio
func GetSysServices(ctx context.Context, addr string) SysServices {
	ss := SysServices{
		NodeCommon: NodeCommon{Addr: addr},
		Services:   []SysService{},
	}
	srv, e := getSELinuxInfo()
	if e != nil {
		ss.Error = e.Error()
	} else {
		ss.Services = append(ss.Services, srv)
	}

	return ss
}

func getSELinuxInfo() (SysService, error) {
	ss := SysService{Name: SrvSELinux}

	file, err := os.Open("/etc/selinux/config")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			ss.Status = SrvNotInstalled
			return ss, nil
		}
		return ss, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		tokens := strings.SplitN(strings.TrimSpace(scanner.Text()), "=", 2)
		if len(tokens) == 2 && tokens[0] == "SELINUX" {
			ss.Status = tokens[1]
			return ss, nil
		}
	}

	return ss, scanner.Err()
}

// GetSysErrors returns issues in system setup/config
func GetSysErrors(ctx context.Context, addr string) SysErrors {
	se := SysErrors{NodeCommon: NodeCommon{Addr: addr}}
	if runtime.GOOS != "linux" {
		return se
	}

	ae, err := isAuditEnabled()
	if err != nil {
		se.Error = "audit: " + err.Error()
	} else if ae {
		se.Errors = append(se.Errors, SysErrAuditEnabled)
	}

	_, err = exec.LookPath("updatedb")
	if err == nil {
		se.Errors = append(se.Errors, SysErrUpdatedbInstalled)
	} else if !strings.HasSuffix(err.Error(), exec.ErrNotFound.Error()) {
		errMsg := "updatedb: " + err.Error()
		if len(se.Error) == 0 {
			se.Error = errMsg
		} else {
			se.Error = se.Error + ", " + errMsg
		}
	}

	return se
}

// Audit is enabled if either `audit=1` is present in /proc/cmdline
// or the `kauditd` process is running
func isAuditEnabled() (bool, error) {
	file, err := os.Open("/proc/cmdline")
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "audit=1") {
			return true, nil
		}
	}

	return isKauditdRunning()
}

func isKauditdRunning() (bool, error) {
	procs, err := process.Processes()
	if err != nil {
		return false, err
	}
	for _, proc := range procs {
		pname, err := proc.Name()
		if err != nil && pname == "kauditd" {
			return true, nil
		}
	}
	return false, nil
}

// MemInfo contains system's RAM and swap information.
type MemInfo struct {
	NodeCommon

	Total          uint64 `json:"total,omitempty"`
	Available      uint64 `json:"available,omitempty"`
	SwapSpaceTotal uint64 `json:"swap_space_total,omitempty"`
	SwapSpaceFree  uint64 `json:"swap_space_free,omitempty"`
	// Limit will store cgroup limit if configured and
	// less than Total, otherwise same as Total
	Limit uint64 `json:"limit,omitempty"`
}

// Get the final system memory limit chosen by the user.
// by default without any configuration on a vanilla Linux
// system you would see physical RAM limit. If cgroup
// is configured at some point in time this function
// would return the memory limit chosen for the given pid.
func getMemoryLimit(sysLimit uint64) uint64 {
	// Following code is deliberately ignoring the error.
	cGroupLimit, err := cgroup.GetMemoryLimit(os.Getpid())
	if err == nil && cGroupLimit <= sysLimit {
		// cgroup limit is lesser than system limit means
		// user wants to limit the memory usage further
		return cGroupLimit
	}

	return sysLimit
}

// GetMemInfo returns system's RAM and swap information.
func GetMemInfo(ctx context.Context, addr string) MemInfo {
	meminfo, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return MemInfo{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	swapinfo, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return MemInfo{
			NodeCommon: NodeCommon{
				Addr:  addr,
				Error: err.Error(),
			},
		}
	}

	return MemInfo{
		NodeCommon:     NodeCommon{Addr: addr},
		Total:          meminfo.Total,
		Available:      meminfo.Available,
		SwapSpaceTotal: swapinfo.Total,
		SwapSpaceFree:  swapinfo.Free,
		Limit:          getMemoryLimit(meminfo.Total),
	}
}

// ProcInfo contains current process's information.
type ProcInfo struct {
	NodeCommon

	PID            int32                      `json:"pid,omitempty"`
	IsBackground   bool                       `json:"is_background,omitempty"`
	CPUPercent     float64                    `json:"cpu_percent,omitempty"`
	ChildrenPIDs   []int32                    `json:"children_pids,omitempty"`
	CmdLine        string                     `json:"cmd_line,omitempty"`
	NumConnections int                        `json:"num_connections,omitempty"`
	CreateTime     int64                      `json:"create_time,omitempty"`
	CWD            string                     `json:"cwd,omitempty"`
	ExecPath       string                     `json:"exec_path,omitempty"`
	GIDs           []int32                    `json:"gids,omitempty"`
	IOCounters     process.IOCountersStat     `json:"iocounters,omitempty"`
	IsRunning      bool                       `json:"is_running,omitempty"`
	MemInfo        process.MemoryInfoStat     `json:"mem_info,omitempty"`
	MemMaps        []process.MemoryMapsStat   `json:"mem_maps,omitempty"`
	MemPercent     float32                    `json:"mem_percent,omitempty"`
	Name           string                     `json:"name,omitempty"`
	Nice           int32                      `json:"nice,omitempty"`
	NumCtxSwitches process.NumCtxSwitchesStat `json:"num_ctx_switches,omitempty"`
	NumFDs         int32                      `json:"num_fds,omitempty"`
	NumThreads     int32                      `json:"num_threads,omitempty"`
	PageFaults     process.PageFaultsStat     `json:"page_faults,omitempty"`
	PPID           int32                      `json:"ppid,omitempty"`
	Status         string                     `json:"status,omitempty"`
	TGID           int32                      `json:"tgid,omitempty"`
	Times          cpu.TimesStat              `json:"times,omitempty"`
	UIDs           []int32                    `json:"uids,omitempty"`
	Username       string                     `json:"username,omitempty"`
}

// GetProcInfo returns current MinIO process information.
func GetProcInfo(ctx context.Context, addr string) ProcInfo {
	pid := int32(syscall.Getpid())

	procInfo := ProcInfo{
		NodeCommon: NodeCommon{Addr: addr},
		PID:        pid,
	}
	var err error

	proc, err := process.NewProcess(pid)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.IsBackground, err = proc.BackgroundWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.CPUPercent, err = proc.CPUPercentWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.ChildrenPIDs = []int32{}
	children, _ := proc.ChildrenWithContext(ctx)
	for i := range children {
		procInfo.ChildrenPIDs = append(procInfo.ChildrenPIDs, children[i].Pid)
	}

	procInfo.CmdLine, err = proc.CmdlineWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	connections, err := proc.ConnectionsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.NumConnections = len(connections)

	procInfo.CreateTime, err = proc.CreateTimeWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.CWD, err = proc.CwdWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.ExecPath, err = proc.ExeWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.GIDs, err = proc.GidsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	ioCounters, err := proc.IOCountersWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.IOCounters = *ioCounters

	procInfo.IsRunning, err = proc.IsRunningWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	memInfo, err := proc.MemoryInfoWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.MemInfo = *memInfo

	memMaps, err := proc.MemoryMapsWithContext(ctx, true)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.MemMaps = *memMaps

	procInfo.MemPercent, err = proc.MemoryPercentWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.Name, err = proc.NameWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.Nice, err = proc.NiceWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	numCtxSwitches, err := proc.NumCtxSwitchesWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.NumCtxSwitches = *numCtxSwitches

	procInfo.NumFDs, err = proc.NumFDsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	procInfo.NumThreads, err = proc.NumThreadsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	pageFaults, err := proc.PageFaultsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.PageFaults = *pageFaults

	procInfo.PPID, _ = proc.PpidWithContext(ctx)

	status, err := proc.StatusWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.Status = status[0]

	procInfo.TGID, err = proc.Tgid()
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	times, err := proc.TimesWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}
	procInfo.Times = *times

	procInfo.UIDs, err = proc.UidsWithContext(ctx)
	if err != nil {
		procInfo.Error = err.Error()
		return procInfo
	}

	// In certain environments, it is not possible to get username e.g. minio-operator
	// Plus it's not a serious error. So ignore error if any.
	procInfo.Username, err = proc.UsernameWithContext(ctx)
	if err != nil {
		procInfo.Username = "<non-root>"
	}

	return procInfo
}

// SysInfo - Includes hardware and system information of the MinIO cluster
type SysInfo struct {
	CPUInfo        []CPUs         `json:"cpus,omitempty"`
	Partitions     []Partitions   `json:"partitions,omitempty"`
	OSInfo         []OSInfo       `json:"osinfo,omitempty"`
	MemInfo        []MemInfo      `json:"meminfo,omitempty"`
	ProcInfo       []ProcInfo     `json:"procinfo,omitempty"`
	SysErrs        []SysErrors    `json:"errors,omitempty"`
	SysServices    []SysServices  `json:"services,omitempty"`
	SysConfig      []SysConfig    `json:"config,omitempty"`
	KubernetesInfo KubernetesInfo `json:"kubernetes"`
}

// KubernetesInfo - Information about the kubernetes platform
type KubernetesInfo struct {
	Major      string    `json:"major,omitempty"`
	Minor      string    `json:"minor,omitempty"`
	GitVersion string    `json:"gitVersion,omitempty"`
	GitCommit  string    `json:"gitCommit,omitempty"`
	BuildDate  time.Time `json:"buildDate,omitempty"`
	Platform   string    `json:"platform,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// SpeedTestResults - Includes perf test results of the MinIO cluster
type SpeedTestResults struct {
	DrivePerf []DriveSpeedTestResult `json:"drive,omitempty"`
	ObjPerf   []SpeedTestResult      `json:"obj,omitempty"`
	NetPerf   []NetperfNodeResult    `json:"net,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

// MinioConfig contains minio configuration of a node.
type MinioConfig struct {
	Error string `json:"error,omitempty"`

	Config interface{} `json:"config,omitempty"`
}

// MemStats is strip down version of runtime.MemStats containing memory stats of MinIO server.
type MemStats struct {
	Alloc      uint64
	TotalAlloc uint64
	Mallocs    uint64
	Frees      uint64
	HeapAlloc  uint64
}

// GCStats collect information about recent garbage collections.
type GCStats struct {
	LastGC     time.Time       `json:"last_gc"`     // time of last collection
	NumGC      int64           `json:"num_gc"`      // number of garbage collections
	PauseTotal time.Duration   `json:"pause_total"` // total pause for all collections
	Pause      []time.Duration `json:"pause"`       // pause history, most recent first
	PauseEnd   []time.Time     `json:"pause_end"`   // pause end times history, most recent first
}

// ServerInfo holds server information
type ServerInfo struct {
	State          string            `json:"state,omitempty"`
	Endpoint       string            `json:"endpoint,omitempty"`
	Uptime         int64             `json:"uptime,omitempty"`
	Version        string            `json:"version,omitempty"`
	CommitID       string            `json:"commitID,omitempty"`
	Network        map[string]string `json:"network,omitempty"`
	Drives         []Disk            `json:"drives,omitempty"`
	PoolNumber     int               `json:"poolNumber,omitempty"`
	MemStats       MemStats          `json:"mem_stats"`
	GOMAXPROCS     int               `json:"gomaxprocs"`
	NumCPU         int               `json:"num_cpu"`
	RuntimeVersion string            `json:"runtime_version"`
	GCStats        GCStats           `json:"gc_stats"`
	MinioEnvVars   map[string]string `json:"minio_env_vars"`
}

// MinioInfo contains MinIO server and object storage information.
type MinioInfo struct {
	Mode         string       `json:"mode,omitempty"`
	Domain       []string     `json:"domain,omitempty"`
	Region       string       `json:"region,omitempty"`
	SQSARN       []string     `json:"sqsARN,omitempty"`
	DeploymentID string       `json:"deploymentID,omitempty"`
	Buckets      Buckets      `json:"buckets,omitempty"`
	Objects      Objects      `json:"objects,omitempty"`
	Usage        Usage        `json:"usage,omitempty"`
	Services     Services     `json:"services,omitempty"`
	Backend      interface{}  `json:"backend,omitempty"`
	Servers      []ServerInfo `json:"servers,omitempty"`
	TLS          *TLSInfo     `json:"tls"`
	IsKubernetes *bool        `json:"is_kubernetes"`
	IsDocker     *bool        `json:"is_docker"`
}

type TLSInfo struct {
	TLSEnabled bool      `json:"tls_enabled"`
	Certs      []TLSCert `json:"certs,omitempty"`
}

type TLSCert struct {
	PubKeyAlgo    string    `json:"pub_key_algo"`
	SignatureAlgo string    `json:"signature_algo"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
}

// MinioHealthInfo - Includes MinIO confifuration information
type MinioHealthInfo struct {
	Error string `json:"error,omitempty"`

	Config MinioConfig `json:"config,omitempty"`
	Info   MinioInfo   `json:"info,omitempty"`
}

// HealthInfo - MinIO cluster's health Info
type HealthInfo struct {
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`

	TimeStamp time.Time        `json:"timestamp,omitempty"`
	Sys       SysInfo          `json:"sys,omitempty"`
	Perf      SpeedTestResults `json:"perf,omitempty"`
	Minio     MinioHealthInfo  `json:"minio,omitempty"`
}

func (info HealthInfo) String() string {
	data, err := json.Marshal(info)
	if err != nil {
		panic(err) // This never happens.
	}
	return string(data)
}

// JSON returns this structure as JSON formatted string.
func (info HealthInfo) JSON() string {
	data, err := json.MarshalIndent(info, " ", "    ")
	if err != nil {
		panic(err) // This never happens.
	}
	return string(data)
}

// GetError - returns error from the cluster health info
func (info HealthInfo) GetError() string {
	return info.Error
}

// GetStatus - returns status of the cluster health info
func (info HealthInfo) GetStatus() string {
	if info.Error != "" {
		return "error"
	}
	return "success"
}

// GetTimestamp - returns timestamp from the cluster health info
func (info HealthInfo) GetTimestamp() time.Time {
	return info.TimeStamp
}

// HealthDataType - Typed Health data types
type HealthDataType string

// HealthDataTypes
const (
	HealthDataTypePerfDrive   HealthDataType = "driveperf"
	HealthDataTypePerfNet     HealthDataType = "netperf"
	HealthDataTypePerfObj     HealthDataType = "objperf"
	HealthDataTypeMinioInfo   HealthDataType = "minioinfo"
	HealthDataTypeMinioConfig HealthDataType = "minioconfig"
	HealthDataTypeSysCPU      HealthDataType = "syscpu"
	HealthDataTypeSysDriveHw  HealthDataType = "sysdrivehw"
	HealthDataTypeSysDocker   HealthDataType = "sysdocker" // is this really needed?
	HealthDataTypeSysOsInfo   HealthDataType = "sysosinfo"
	HealthDataTypeSysLoad     HealthDataType = "sysload" // provides very little info. Making it TBD
	HealthDataTypeSysMem      HealthDataType = "sysmem"
	HealthDataTypeSysNet      HealthDataType = "sysnet"
	HealthDataTypeSysProcess  HealthDataType = "sysprocess"
	HealthDataTypeSysErrors   HealthDataType = "syserrors"
	HealthDataTypeSysServices HealthDataType = "sysservices"
	HealthDataTypeSysConfig   HealthDataType = "sysconfig"
)

// HealthDataTypesMap - Map of Health datatypes
var HealthDataTypesMap = map[string]HealthDataType{
	"driveperf":   HealthDataTypePerfDrive,
	"netperf":     HealthDataTypePerfNet,
	"objperf":     HealthDataTypePerfObj,
	"minioinfo":   HealthDataTypeMinioInfo,
	"minioconfig": HealthDataTypeMinioConfig,
	"syscpu":      HealthDataTypeSysCPU,
	"sysdrivehw":  HealthDataTypeSysDriveHw,
	"sysdocker":   HealthDataTypeSysDocker,
	"sysosinfo":   HealthDataTypeSysOsInfo,
	"sysload":     HealthDataTypeSysLoad,
	"sysmem":      HealthDataTypeSysMem,
	"sysnet":      HealthDataTypeSysNet,
	"sysprocess":  HealthDataTypeSysProcess,
	"syserrors":   HealthDataTypeSysErrors,
	"sysservices": HealthDataTypeSysServices,
	"sysconfig":   HealthDataTypeSysConfig,
}

// HealthDataTypesList - List of health datatypes
var HealthDataTypesList = []HealthDataType{
	HealthDataTypeMinioInfo,
	HealthDataTypeMinioConfig,
	HealthDataTypeSysCPU,
	HealthDataTypeSysDriveHw,
	HealthDataTypeSysDocker,
	HealthDataTypeSysOsInfo,
	HealthDataTypeSysLoad,
	HealthDataTypeSysMem,
	HealthDataTypeSysNet,
	HealthDataTypeSysProcess,
	HealthDataTypeSysErrors,
	HealthDataTypeSysServices,
	HealthDataTypeSysConfig,
	HealthDataTypePerfDrive,
	HealthDataTypePerfObj,
	HealthDataTypePerfNet,
}

// HealthInfoVersionStruct - struct for health info version
type HealthInfoVersionStruct struct {
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ServerHealthInfo - Connect to a minio server and call Health Info Management API
// to fetch server's information represented by HealthInfo structure
func (adm *AdminClient) ServerHealthInfo(ctx context.Context, types []HealthDataType, deadline time.Duration) (*http.Response, string, error) {
	v := url.Values{}
	v.Set("deadline", deadline.Truncate(1*time.Second).String())
	for _, d := range HealthDataTypesList { // Init all parameters to false.
		v.Set(string(d), "false")
	}
	for _, d := range types {
		v.Set(string(d), "true")
	}

	resp, err := adm.executeMethod(
		ctx, "GET", requestData{
			relPath:     adminAPIPrefix + "/healthinfo",
			queryValues: v,
		},
	)
	if err != nil {
		closeResponse(resp)
		return nil, "", err
	}

	if resp.StatusCode != http.StatusOK {
		closeResponse(resp)
		return nil, "", httpRespToErrorResponse(resp)
	}

	decoder := json.NewDecoder(resp.Body)
	var version HealthInfoVersionStruct
	if err = decoder.Decode(&version); err != nil {
		closeResponse(resp)
		return nil, "", err
	}

	if version.Error != "" {
		closeResponse(resp)
		return nil, "", errors.New(version.Error)
	}

	switch version.Version {
	case "", HealthInfoVersion2, HealthInfoVersion:
	default:
		closeResponse(resp)
		return nil, "", errors.New("Upgrade Minio Client to support health info version " + version.Version)
	}

	return resp, version.Version, nil
}
