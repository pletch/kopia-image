package acmecert

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/libdns/libdns"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()

	fname := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(fname, []byte(content), 0o600))

	return fname
}

func TestTokenSource_Success(t *testing.T) {
	cases := map[string]TokenSource{
		"static":                StaticToken("abc_DEF-123"),
		"static with spaces":    StaticToken("  abc_DEF-123\n"),
		"file":                  TokenFromFile(writeTokenFile(t, "abc_DEF-123")),
		"file with line ending": TokenFromFile(writeTokenFile(t, "abc_DEF-123\r\n")),
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			token, err := src()
			require.NoError(t, err)
			require.Equal(t, "abc_DEF-123", token)
		})
	}
}

func TestTokenSource_Errors(t *testing.T) {
	const secret = "s3cr3t-part"

	cases := map[string]TokenSource{
		"empty static":      StaticToken(" "),
		"embedded space":    StaticToken(secret + " " + secret),
		"embedded newline":  StaticToken(secret + "\nX-Injected: 1"),
		"non-ASCII":         StaticToken(secret + "é"),
		"missing file":      TokenFromFile(filepath.Join(t.TempDir(), "missing")),
		"empty file":        TokenFromFile(writeTokenFile(t, "\n")),
		"multi-line file":   TokenFromFile(writeTokenFile(t, secret+"\n"+secret+"\n")),
		"oversized file":    TokenFromFile(writeTokenFile(t, strings.Repeat(secret, maxTokenFileSize))),
		"directory as file": TokenFromFile(t.TempDir()),
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := src()
			require.Error(t, err)
			require.NotContains(t, err.Error(), secret, "errors must not leak the token")
		})
	}
}

// recordingClient rejects every request the way Cloudflare rejects a bad token
// and remembers the credentials that were presented.
type recordingClient struct {
	mu sync.Mutex
	// +checklocks:mu
	authorizations []string
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.authorizations = append(c.authorizations, req.Header.Get("Authorization"))

	return &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)),
		Request:    req,
	}, nil
}

func (c *recordingClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.authorizations...)
}

func TestCloudflareDNSProvider_ReadsTokenForEveryChange(t *testing.T) {
	ctx := testlogging.Context(t)
	tokenFile := writeTokenFile(t, "first-token\n")
	client := &recordingClient{}

	p := &cloudflareDNSProvider{token: TokenFromFile(tokenFile), httpClient: client}
	recs := []libdns.Record{libdns.TXT{Name: "_acme-challenge", Text: "value"}}

	_, err := p.AppendRecords(ctx, "example.com.", recs)
	require.ErrorContains(t, err, "Invalid access token")
	require.NotContains(t, err.Error(), "first-token")

	// a rotated token is used without restarting.
	require.NoError(t, os.WriteFile(tokenFile, []byte("second-token\n"), 0o600))

	_, err = p.DeleteRecords(ctx, "example.com.", recs)
	require.Error(t, err)

	require.Equal(t, []string{"Bearer first-token", "Bearer second-token"}, client.seen())
}

func TestCloudflareDNSProvider_FailsWithoutCallingAPIWhenTokenUnavailable(t *testing.T) {
	ctx := testlogging.Context(t)
	client := &recordingClient{}

	p := &cloudflareDNSProvider{token: TokenFromFile(filepath.Join(t.TempDir(), "missing")), httpClient: client}
	recs := []libdns.Record{libdns.TXT{Name: "_acme-challenge", Text: "value"}}

	_, err := p.AppendRecords(ctx, "example.com.", recs)
	require.ErrorContains(t, err, "Cloudflare API token")

	_, err = p.DeleteRecords(ctx, "example.com.", recs)
	require.ErrorContains(t, err, "Cloudflare API token")

	require.Empty(t, client.seen())
}
