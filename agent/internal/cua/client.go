package cua

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	Binary string
	Args   []string
	Env    []string
}

type Client struct {
	session *mcp.ClientSession

	callMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

func Start(ctx context.Context, options Options) (*Client, error) {
	cmd := exec.CommandContext(ctx, options.Binary, options.Args...)
	cmd.Env = append(os.Environ(), options.Env...)

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "hadron-cua-client",
		Version: "v0.0.1",
	}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: 3 * time.Second,
	}, nil)
	if err != nil {
		return nil, err
	}

	return &Client{session: session}, nil
}

func (c *Client) Call(ctx context.Context, name string, arguments any) (*mcp.CallToolResult, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	return c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.session.Close()
	})
	return c.closeErr
}
