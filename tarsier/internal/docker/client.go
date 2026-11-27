package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client is a lightweight, zero-dependency Docker Engine API client
// communicating over a Unix domain socket.
type Client struct {
	socketPath string
	httpClient *http.Client
	streamHTTP *http.Client // For long-lived streaming connections (e.g. /events)
}

// NewClient initializes a Client dialing the specified Unix domain socket path.
// If socketPath is empty, it defaults to "/var/run/docker.sock".
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: false,
		MaxIdleConns:      4,
		IdleConnTimeout:   30 * time.Second,
	}

	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
		streamHTTP: &http.Client{
			Transport: transport,
			Timeout:   0, // Long-polling, governed by context.Context
		},
	}
}

// SocketPath returns the configured Unix socket path.
func (c *Client) SocketPath() string {
	return c.socketPath
}

// StreamHTTPClient returns the underlying http.Client configured for streaming without timeout.
func (c *Client) StreamHTTPClient() *http.Client {
	return c.streamHTTP
}

// Ping checks whether the Docker daemon is responding via GET /_ping.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_ping", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected ping status: %d", resp.StatusCode)
	}

	lr := io.LimitReader(resp.Body, 1024)
	body, err := io.ReadAll(lr)
	if err != nil {
		return fmt.Errorf("read ping response: %w", err)
	}

	if strings.TrimSpace(string(body)) != "OK" {
		return fmt.Errorf("unexpected ping response body: %q", string(body))
	}

	return nil
}

// ListContainers queries GET /containers/json and enriches each container with
// InspectContainer metadata (RestartCount, State, MemoryLimit, TTY) using bounded
// concurrency, falling back to basic listing metadata if inspect fails.
func (c *Client) ListContainers(ctx context.Context, all bool) ([]ContainerInfo, error) {
	endpoint := "http://localhost/containers/json"
	if all {
		endpoint += "?all=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return nil, fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list containers unexpected status: %d", resp.StatusCode)
	}

	// 4 MiB limit reader cap prevents unbounded memory allocation on large container fleets
	lr := io.LimitReader(resp.Body, 4<<20)

	type rawContainer struct {
		ID      string   `json:"Id"`
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
		Created int64    `json:"Created"`
	}

	var rawList []rawContainer
	if err := json.NewDecoder(lr).Decode(&rawList); err != nil {
		return nil, fmt.Errorf("decode containers json: %w", err)
	}

	result := make([]ContainerInfo, 0, len(rawList))
	for _, item := range rawList {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(strings.TrimSpace(item.Names[0]), "/")
		}

		result = append(result, ContainerInfo{
			ID:      item.ID,
			Name:    name,
			Image:   strings.TrimPrefix(strings.TrimSpace(item.Image), "/"),
			State:   strings.ToLower(item.State),
			Status:  item.Status,
			Created: item.Created,
		})
	}

	if len(result) > 0 {
		workers := 6
		if len(result) < workers {
			workers = len(result)
		}

		jobs := make(chan int, len(result))
		for i := range result {
			jobs <- i
		}
		close(jobs)

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range jobs {
					if ctx.Err() != nil {
						return
					}
					inspectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					inspect, inspectErr := c.InspectContainer(inspectCtx, result[idx].ID)
					cancel()
					if inspectErr != nil {
						continue
					}

					result[idx].RestartCount = inspect.RestartCount
					if st := strings.ToLower(strings.TrimSpace(inspect.State.Status)); st != "" {
						result[idx].State = st
					}
					result[idx].MemoryLimit = inspect.HostConfig.Memory
					result[idx].TTY = inspect.Config.Tty
				}
			}()
		}
		wg.Wait()
	}

	return result, nil
}

// InspectContainer queries GET /containers/{id}/json and decodes minimal container inspection metadata.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	if strings.TrimSpace(id) == "" {
		return ContainerInspect{}, errors.New("container id must not be empty")
	}

	endpoint := fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ContainerInspect{}, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return ContainerInspect{}, fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return ContainerInspect{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ContainerInspect{}, ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ContainerInspect{}, fmt.Errorf("inspect container unexpected status: %d", resp.StatusCode)
	}

	// 512 KiB limit reader cap for individual container inspect payloads
	lr := io.LimitReader(resp.Body, 512<<10)

	var inspect ContainerInspect
	if err := json.NewDecoder(lr).Decode(&inspect); err != nil {
		return ContainerInspect{}, fmt.Errorf("decode inspect json: %w", err)
	}

	inspect.Name = strings.TrimPrefix(strings.TrimSpace(inspect.Name), "/")
	inspect.Image = strings.TrimPrefix(strings.TrimSpace(inspect.Image), "/")
	return inspect, nil
}

// GetContainerStats queries GET /containers/{id}/stats?stream=false and decodes the cgroup metrics payload.
func (c *Client) GetContainerStats(ctx context.Context, id string) (DockerStatsPayload, error) {
	if strings.TrimSpace(id) == "" {
		return DockerStatsPayload{}, errors.New("container id must not be empty")
	}

	endpoint := fmt.Sprintf("http://localhost/containers/%s/stats?stream=false", url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return DockerStatsPayload{}, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return DockerStatsPayload{}, fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return DockerStatsPayload{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return DockerStatsPayload{}, ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return DockerStatsPayload{}, fmt.Errorf("container stats unexpected status: %d", resp.StatusCode)
	}

	// 512 KiB limit reader cap for individual container stats payloads
	lr := io.LimitReader(resp.Body, 512<<10)

	var payload DockerStatsPayload
	if err := json.NewDecoder(lr).Decode(&payload); err != nil {
		return DockerStatsPayload{}, fmt.Errorf("decode stats json: %w", err)
	}

	return payload, nil
}

// ContainerLogsOptions specifies parameters for streaming container logs.
type ContainerLogsOptions struct {
	Follow     bool
	Stdout     bool
	Stderr     bool
	Timestamps bool
	Since      int64  // Unix epoch seconds
	Tail       string // e.g. "all", "100"
}

// GetContainerLogsStream queries GET /containers/{id}/logs and returns a streaming io.ReadCloser response body.
func (c *Client) GetContainerLogsStream(ctx context.Context, id string, opts ContainerLogsOptions) (io.ReadCloser, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("container id must not be empty")
	}

	params := url.Values{}
	if opts.Follow {
		params.Set("follow", "1")
	}
	if opts.Stdout {
		params.Set("stdout", "1")
	}
	if opts.Stderr {
		params.Set("stderr", "1")
	}
	if opts.Timestamps {
		params.Set("timestamps", "1")
	}
	if opts.Since > 0 {
		params.Set("since", fmt.Sprintf("%d", opts.Since))
	}
	if opts.Tail != "" {
		params.Set("tail", opts.Tail)
	}

	endpoint := fmt.Sprintf("http://localhost/containers/%s/logs?%s", url.PathEscape(id), params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		if isSocketUnavailable(err) {
			return nil, fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
		}
		return nil, err
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		lr := io.LimitReader(resp.Body, 4<<10)
		body, _ := io.ReadAll(lr)
		resp.Body.Close()
		return nil, fmt.Errorf("container logs unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return resp.Body, nil
}

func isSocketUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSocketUnavailable) || os.IsNotExist(err) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "connect: ")
}
