package proxy

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Client talks to a running proxy over its control socket.
type Client struct{ http *http.Client }

func NewClient(vopsHome string) *Client {
	sock := ControlSocket(vopsHome)
	return &Client{&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 2,
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://proxy"+path, r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("proxy: %s", strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.UnmarshalRead(resp.Body, out)
}

func (c *Client) Ping(ctx context.Context) error { return c.do(ctx, "GET", "/ping", nil, nil) }

func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, "GET", "/status", nil, &st)
	return st, err
}

// Push replaces the proxy's table; it returns once the proxy serves it.
func (c *Client) Push(ctx context.Context, routes []Route) (Status, error) {
	var st Status
	err := c.do(ctx, "PUT", "/routes", tableFile{routes}, &st)
	return st, err
}

// Reload makes the proxy re-read the config lock; restart is true when it re-execs for new listeners.
func (c *Client) Reload(ctx context.Context) (restart bool, err error) {
	var out struct {
		Restart bool `json:"restart"`
	}
	err = c.do(ctx, "POST", "/reload", nil, &out)
	return out.Restart, err
}
