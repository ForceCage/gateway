package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/forcecage/gateway/internal/providers"
)

// ForwardProxy is the forward-proxy ingress: agents set HTTPS_PROXY to point at
// ForceCage and trust its CA. ForceCage answers CONNECT by terminating the TLS
// tunnel (MITM) so the shared Engine can inspect the plaintext request, enforce,
// forward to the real upstream, and reconcile — with zero code changes in the
// agent, just two env vars (HTTPS_PROXY + the CA bundle).
//
// This is an HTTP/1.1 MITM. Hosts that do not map to a known metered provider are
// tunnelled through untouched (blind relay), so the proxy is safe to use as the
// agent's only egress proxy.
type ForwardProxy struct {
	engine   *Engine
	registry *providers.Registry
	ca       *CertAuthority
	logger   *slog.Logger
	// upstream is the client used to reach real providers. Pinned to HTTP/1.1 to
	// keep MITM response handling simple and streaming-friendly.
	upstream *http.Client
}

func NewForwardProxy(e *Engine, registry *providers.Registry, ca *CertAuthority, logger *slog.Logger) *ForwardProxy {
	return &ForwardProxy{
		engine:   e,
		registry: registry,
		ca:       ca,
		logger:   logger,
		upstream: &http.Client{Transport: &http.Transport{}},
	}
}

// ServeHTTP handles CONNECT requests (the only method a forward proxy receives for
// HTTPS). Plain-HTTP forward proxying is intentionally not supported — metered AI
// APIs are HTTPS-only.
func (fp *ForwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "ForceCage forward proxy only accepts CONNECT", http.StatusMethodNotAllowed)
		return
	}
	fp.handleConnect(w, r)
}

func (fp *ForwardProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host // "api.openai.com:443"

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		fp.logger.Error("hijack failed", "err", err)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	provider, perr := fp.registry.GetByHost(host)
	if perr != nil {
		// Not a metered provider — relay the tunnel untouched.
		fp.blindTunnel(clientConn, host)
		return
	}

	fp.mitm(clientConn, host, provider)
}

// mitm terminates TLS with the client, then services HTTP requests on the
// decrypted connection, enforcing each one before forwarding upstream.
func (fp *ForwardProxy) mitm(clientConn net.Conn, host string, provider providers.Provider) {
	hostname := stripPort(host)
	tlsConn := tls.Server(clientConn, &tls.Config{
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return fp.ca.LeafFor(hostname)
		},
	})
	if err := tlsConn.Handshake(); err != nil {
		fp.logger.Error("tls handshake with client failed", "host", hostname, "err", err)
		return
	}
	defer tlsConn.Close()

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return // client closed the tunnel or sent garbage
		}
		if !fp.serveDecrypted(tlsConn, req, hostname, provider) {
			return // non-keep-alive or fatal error
		}
	}
}

// serveDecrypted enforces and forwards a single decrypted request. It returns
// false when the connection should be closed.
func (fp *ForwardProxy) serveDecrypted(clientConn net.Conn, req *http.Request, hostname string, provider providers.Provider) bool {
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		writeRawError(clientConn, http.StatusBadRequest, "failed to read request body")
		return false
	}

	agentID := req.Header.Get(AgentIDHeader)
	if agentID == "" {
		writeRawError(clientConn, http.StatusBadRequest, AgentIDHeader+" header is required")
		return false
	}

	decision := fp.engine.Authorize(req.Context(), agentID, provider.Name(), provider, body)
	if decision.Deny {
		writeRawJSON(clientConn, decision.StatusCode, decision.Detail)
		return false
	}

	// Build the outbound request to the real upstream over a fresh TLS connection.
	outURL := "https://" + hostname + req.URL.RequestURI()
	outReq, err := http.NewRequestWithContext(req.Context(), req.Method, outURL, bytes.NewReader(body))
	if err != nil {
		writeRawError(clientConn, http.StatusInternalServerError, "build upstream request")
		return false
	}
	copyHeader(outReq.Header, req.Header)
	outReq.Header.Del(AgentIDHeader)
	outReq.Host = hostname

	resp, err := fp.upstream.Do(outReq)
	if err != nil {
		fp.logger.Error("upstream request failed", "err", err)
		writeRawError(clientConn, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return false
	}
	defer resp.Body.Close()

	if err := fp.engine.Reconcile(resp, provider, agentID, decision.Reservations); err != nil {
		fp.logger.Error("reconcile failed", "err", err)
	}

	if err := resp.Write(clientConn); err != nil {
		return false
	}
	return true
}

// blindTunnel relays bytes both ways without inspection (non-provider hosts).
func (fp *ForwardProxy) blindTunnel(clientConn net.Conn, host string) {
	upstream, err := net.Dial("tcp", host)
	if err != nil {
		fp.logger.Error("blind tunnel dial failed", "host", host, "err", err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, upstream); done <- struct{}{} }()
	<-done
}

func stripPort(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// writeRawJSON writes an HTTP response with a JSON body directly to a net.Conn
// (used on the MITM'd connection, where there is no http.ResponseWriter).
func writeRawJSON(conn net.Conn, status int, detail map[string]any) {
	bodyBytes, _ := json.MarshalIndent(detail, "", "  ")
	resp := &http.Response{
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(bodyBytes)),
		ContentLength: int64(len(bodyBytes)),
		Close:         true,
	}
	_ = resp.Write(conn)
}

func writeRawError(conn net.Conn, status int, msg string) {
	writeRawJSON(conn, status, map[string]any{"error": map[string]any{"message": msg}})
}
