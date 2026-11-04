package mcpproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidConfiguration = errors.New("MCP proxy configuration is invalid")

type Configuration struct {
	ListenAddress   string
	ControlURL      string
	ArchivistHostID string
	Credential      string
	Client          *http.Client
}

type Proxy struct {
	listenAddress   string
	controlURL      string
	archivistHostID string
	credential      string
	client          *http.Client
	router          http.Handler
}

func New(configuration Configuration) (*Proxy, error) {
	listen, err := netip.ParseAddrPort(strings.TrimSpace(configuration.ListenAddress))
	if err != nil || !listen.Addr().IsLoopback() {
		return nil, ErrInvalidConfiguration
	}
	control, err := url.Parse(strings.TrimRight(strings.TrimSpace(configuration.ControlURL), "/"))
	if err != nil || control.Host == "" || control.User != nil || control.RawQuery != "" || control.Fragment != "" ||
		(control.Scheme != "https" && !(control.Scheme == "http" && loopbackHostname(control.Hostname()))) ||
		strings.TrimSpace(configuration.ArchivistHostID) == "" || strings.TrimSpace(configuration.Credential) == "" {
		return nil, ErrInvalidConfiguration
	}
	client := configuration.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	proxy := &Proxy{
		listenAddress: listen.String(), controlURL: strings.TrimRight(control.String(), "/"),
		archivistHostID: strings.TrimSpace(configuration.ArchivistHostID), credential: configuration.Credential, client: client,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mcp/v1/tools", proxy.forward)
	mux.HandleFunc("POST /mcp/v1/call", proxy.forward)
	proxy.router = mux
	return proxy, nil
}

func (proxy *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	proxy.router.ServeHTTP(writer, request)
}

func (proxy *Proxy) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", proxy.listenAddress)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		return <-result
	case err := <-result:
		return err
	}
}

func (proxy *Proxy) forward(writer http.ResponseWriter, request *http.Request) {
	var body io.Reader
	if request.Method == http.MethodPost {
		if request.Header.Get("Content-Type") != "application/json" {
			http.Error(writer, "JSON_REQUIRED", http.StatusUnsupportedMediaType)
			return
		}
		body = http.MaxBytesReader(writer, request.Body, 128<<10)
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, proxy.controlURL+request.URL.Path, body)
	if err != nil {
		http.Error(writer, "MCP_PROXY_FAILED", http.StatusBadGateway)
		return
	}
	if body != nil {
		upstream.Header.Set("Content-Type", "application/json")
	}
	upstream.Header.Set("Authorization", "Bearer "+proxy.credential)
	upstream.Header.Set("X-Archivist-Host-ID", proxy.archivistHostID)
	response, err := proxy.client.Do(upstream)
	if err != nil {
		http.Error(writer, "MCP_PROXY_FAILED", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, io.LimitReader(response.Body, 2<<20))
}

func loopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}
