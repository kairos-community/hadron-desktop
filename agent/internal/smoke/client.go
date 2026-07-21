package smoke

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client opens authenticated MCP sessions against one gateway endpoint. The
// same Client serves every credential class: Connect injects the presented
// bearer per session via a RoundTripper, so no bearer is ever stored on the
// Client itself.
type Client struct {
	// Endpoint is the full MCP URL (…/mcp).
	Endpoint string
	// Base is the transport every session's HTTP client wraps. For the live
	// binary it is a TLS transport whose RootCAs pool is loaded ONLY from the
	// descriptor CA file (never InsecureSkipVerify). Tests substitute the
	// httptest server's own trusting transport.
	Base http.RoundTripper
}

// NewTLSClient builds a Client for endpoint whose TLS trust is pinned to the
// single PEM certificate at caFile. InsecureSkipVerify is never set. A
// missing or unparseable CA file is a descriptor error (exit 2).
func NewTLSClient(endpoint, caFile string) (*Client, error) {
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("ca certificate %s contains no usable PEM certificate", caFile)
	}
	base := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS13,
		},
	}
	return &Client{Endpoint: endpoint, Base: base}, nil
}

// bearerRoundTripper injects a fixed bearer on every request, then delegates
// to base. It never logs the bearer.
type bearerRoundTripper struct {
	base   http.RoundTripper
	bearer string
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	if rt.bearer != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+rt.bearer)
	}
	return base.RoundTrip(req)
}

// httpClient returns an *http.Client that presents bearer on every request.
func (c *Client) httpClient(bearer string) *http.Client {
	return &http.Client{Transport: bearerRoundTripper{base: c.Base, bearer: bearer}}
}

// Connect opens an MCP session presenting bearer. The returned session must be
// closed by the caller.
func (c *Client) Connect(ctx context.Context, bearer string) (*mcp.ClientSession, error) {
	transport := &mcp.StreamableClientTransport{
		Endpoint:   c.Endpoint,
		HTTPClient: c.httpClient(bearer),
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-smoke", Version: "v1"}, nil)
	return client.Connect(ctx, transport, nil)
}

// readyURL derives the gateway's /readyz endpoint from the /mcp endpoint.
func (c *Client) readyURL() string {
	return strings.TrimSuffix(c.Endpoint, "/mcp") + "/readyz"
}

// WaitReady polls the gateway's /readyz until it reports ready or the deadline
// passes. A never-ready gateway is a readiness/transport failure (exit 3).
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	hc := c.httpClient("")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.readyURL(), nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build readiness request: %w", err)
		}
		resp, err := hc.Do(req)
		cancel()
		if err != nil {
			lastErr = err
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("readyz returned %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gateway not ready within %s", timeout)
	}
	return fmt.Errorf("gateway readiness: %w", lastErr)
}
