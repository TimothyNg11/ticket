package main

import (
	"crypto/tls"
	"net/http"
)

// insecureTransport skips certificate checks for the local kind ingress, whose
// certificate is self-signed. Never used against anything but a local cluster.
func insecureTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in flag for local self-signed TLS only
	return t
}
