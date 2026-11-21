package docker

import (
	"errors"
	"time"
)

var (
	// ErrSocketUnavailable indicates the Docker Unix socket does not exist or refused connection.
	ErrSocketUnavailable = errors.New("docker socket unavailable")

	// ErrContainerNotFound indicates Docker Engine returned 404 Not Found for the requested container ID or name.
	ErrContainerNotFound = errors.New("container not found")
)

// ContainerInfo represents discovered container metadata.
type ContainerInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"` // Normalized without leading '/'
	Image        string `json:"image"`
	State        string `json:"state"` // "running", "exited", "restarting", "paused", "dead", "created"
	Status       string `json:"status"`
	Created      int64  `json:"created"`
	RestartCount int    `json:"restart_count"`
	MemoryLimit  int64  `json:"memory_limit"` // Bytes (0 if unlimited)
	TTY          bool   `json:"tty"`
}

// ContainerInspect represents minimal inspect details for a container.
type ContainerInspect struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"` // Normalized without leading '/'
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
	HostConfig struct {
		Memory   int64 `json:"Memory"`
		NanoCPUs int64 `json:"NanoCpus"`
	} `json:"HostConfig"`
}

// DockerStatsPayload captures cgroup CPU, Memory, Network, and Block I/O statistics
// returned by Docker's GET /containers/{id}/stats?stream=false.
type DockerStatsPayload struct {
	Read     time.Time `json:"read"`
	PreRead  time.Time `json:"preread"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}
