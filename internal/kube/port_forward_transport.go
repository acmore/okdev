package kube

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport/spdy"
)

// The upstream SPDY round tripper reads the upgrade response without watching
// cancellation. Keep its TLS/proxy dialer, and close that connection on cancel.
type portForwardSPDYRoundTripper struct {
	*httpstreamspdy.SpdyRoundTripper
	conn net.Conn
}

func portForwardSPDYTransport(cfg *rest.Config) (http.RoundTripper, spdy.Upgrader, error) {
	tlsConfig, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, nil, err
	}
	proxy := cfg.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	base, err := httpstreamspdy.NewRoundTripperWithProxy(tlsConfig, proxy)
	if err != nil {
		return nil, nil, err
	}
	rt := &portForwardSPDYRoundTripper{SpdyRoundTripper: base}
	wrapped, err := rest.HTTPWrappersForConfig(cfg, rt)
	return wrapped, rt, err
}

func (t *portForwardSPDYRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Add(httpstream.HeaderUpgrade, httpstreamspdy.HeaderSpdy31)
	conn, err := t.Dial(req)
	if err != nil {
		return nil, err
	}
	context.AfterFunc(req.Context(), func() { _ = conn.Close() })
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	t.conn = conn
	return resp, nil
}

func (t *portForwardSPDYRoundTripper) NewConnection(resp *http.Response) (httpstream.Connection, error) {
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.Contains(strings.ToLower(resp.Header.Get(httpstream.HeaderConnection)), strings.ToLower(httpstream.HeaderUpgrade)) ||
		!strings.Contains(strings.ToLower(resp.Header.Get(httpstream.HeaderUpgrade)), strings.ToLower(httpstreamspdy.HeaderSpdy31)) {
		defer t.conn.Close()
		// Preserve client-go's decoding of Kubernetes error responses.
		return t.SpdyRoundTripper.NewConnection(resp)
	}
	return httpstreamspdy.NewClientConnectionWithPings(t.conn, 5*time.Second)
}
