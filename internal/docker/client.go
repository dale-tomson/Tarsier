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
		MaxIdleConns:       4,
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

// ListContainers queries GET /containers/json and returns a slice of normalized ContainerInfo.
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
			name = strings.TrimPrefix(item.Names[0], "/")
		}

		result = append(result, ContainerInfo{
			ID:      item.ID,
			Name:    name,
			Image:   item.Image,
			State:   strings.ToLower(item.State),
			Status:  item.Status,
			Created: item.Created,
		})
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

	inspect.Name = strings.TrimPrefix(inspect.Name, "/")
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
