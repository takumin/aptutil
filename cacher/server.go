package cacher

import (
	"net/http"
	"time"

	"github.com/cybozu-go/well"
)

// readHeaderTimeout bounds how long a client may take to send request
// headers, guarding against Slowloris-style connection exhaustion.
const readHeaderTimeout = 10 * time.Second

// NewServer returns HTTPServer implements go-apt-cacher handlers.
func NewServer(c *Cacher, config *Config) *well.HTTPServer {
	addr := config.Addr
	if len(addr) == 0 {
		addr = defaultAddress
	}

	return &well.HTTPServer{
		Server: &http.Server{
			Addr:              addr,
			Handler:           cacheHandler{c},
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
}
