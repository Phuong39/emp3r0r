package tun

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/fatih/color"
	"golang.org/x/net/http2"
)

var (

	// CACrt for TLS server cert signing
	// fill our CA pem text when compiling
	// taken care by build.py
	CACrt = []byte(`
[emp3r0r_ca]
		`)
)

// EmpHTTPClient add our CA to trusted CAs, while keeps TLS InsecureVerify on
func EmpHTTPClient(proxyServer string) *http.Client {
	// start with an empty pool
	rootCAs := x509.NewCertPool()

	// add our cert
	if ok := rootCAs.AppendCertsFromPEM(CACrt); !ok {
		FatalError("No CA certs appended")
	}

	// Trust the augmented cert pool in our TLS client
	config := &tls.Config{
		InsecureSkipVerify: false,
		RootCAs:            rootCAs,
	}

	// transport of our http client, with configured TLS client
	tr := &http.Transport{
		TLSClientConfig:     config,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	// use a proxy for our HTTP client
	if proxyServer != "" {
		proxyUrl, err := url.Parse(proxyServer)
		if err != nil {
			FatalError("Invalid proxy: %v", err)
		}
		tr.Proxy = http.ProxyURL(proxyUrl)
	}
	err := http2.ConfigureTransport(tr) // upgrade to HTTP2, while keeping http.Transport
	if err != nil {
		FatalError("Cannot switch to HTTP2: %v", err)
	}

	return &http.Client{Transport: tr}
}

// FatalError print log in red, and exit
func FatalError(format string, a ...interface{}) {
	errorColor := color.New(color.Bold, color.FgHiRed)
	log.Fatal(errorColor.Sprintf(format, a...))
}
