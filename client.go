// Copyright 2015 Square Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	klog "github.com/square/keywhiz-fs/log"
)

// clientRefresh is the rate the client reloads itself in the background.
var clientRefresh = 10 * time.Minute

// Cipher suites enabled in the client. No RC4 or 3DES.
var ciphers = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
}

// Client basic struct.
type Client struct {
	*klog.Logger
	http   func() *http.Client
	url    *url.URL
	params httpClientParams
}

// httpClientParams are values necessary for constructing a TLS client.
type httpClientParams struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	CaBundle string `json:"ca_bundle"`
	timeout  time.Duration
}

// NewClient produces a read-to-use client struct given PEM-encoded certificate file, key file, and
// ca file with the list of trusted certificate authorities.
func NewClient(certFile, keyFile, caFile string, serverURL *url.URL, timeout time.Duration, logConfig klog.Config, ping bool) (client Client) {
	logger := klog.New("kwfs_client", logConfig)
	params := httpClientParams{certFile, keyFile, caFile, timeout}

	reqc := make(chan *http.Client)

	// Getter from channel.
	getClient := func() *http.Client {
		client := <-reqc
		return client
	}

	initial, err := params.buildClient()
	panicOnError(err)

	// Asynchronously updates client and owns current reference.
	go func() {
		current := initial
		ticker := time.Tick(clientRefresh)
		for {
			select {
			case t := <-ticker: // Periodically update client.
				logger.Infof("Updating http client at %v", t)
				if client, err := params.buildClient(); err != nil {
					logger.Errorf("Error refreshing http client: %v", err)
				} else {
					current = client
				}
			case reqc <- current: // Service request for current client.
			}
		}
	}()

	client = Client{logger, getClient, serverURL, params}
	if ping {
		if _, ok := client.SecretList(); !ok {
			log.Fatalf("Failed startup /secrets ping to %v", client.url)
		}
	}

	return client
}

// RawSecret returns raw JSON from requesting a secret.
func (c Client) RawSecret(name string) (data []byte, ok bool) {
	now := time.Now()
	// note: path.Join does not know how to properly escape for URLs!
	t := *c.url
	t.Path = path.Join(c.url.Path, "secret", name)
	resp, err := c.http().Get(t.String())
	if err != nil {
		c.Errorf("Error retrieving secret %v: %v", name, err)
		return nil, false
	}
	c.Infof("GET /secret/%v %d %v", name, resp.StatusCode, time.Since(now))
	defer resp.Body.Close()

	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		c.Errorf("Error reading response body for secret %v: %v", name, err)
		return nil, false
	}

	switch resp.StatusCode {
	case 200:
		return data, true
	case 404:
		c.Warnf("Secret %v not found", name)
		return nil, false
	default:
		msg := strings.Join(strings.Split(string(data), "\n"), " ")
		c.Errorf("Bad response code getting secret %v: (status=%v, msg='%s')", name, resp.StatusCode, msg)
		return nil, false
	}
}

// Secret returns an unmarshalled Secret struct after requesting a secret.
func (c Client) Secret(name string) (secret *Secret, ok bool) {
	data, ok := c.RawSecret(name)
	if !ok {
		return nil, false
	}

	secret, err := ParseSecret(data)
	if err != nil {
		c.Errorf("Error decoding retrieved secret %v: %v", name, err)
		return nil, false
	}

	return secret, true
}

// RawSecretList returns raw JSON from requesting a listing of secrets.
func (c Client) RawSecretList() (data []byte, ok bool) {
	now := time.Now()
	t := *c.url
	t.Path = path.Join(c.url.Path, "secrets")
	resp, err := c.http().Get(t.String())
	if err != nil {
		c.Errorf("Error retrieving secrets: %v", err)
		return nil, false
	}
	c.Infof("GET /secrets %d %v", resp.StatusCode, time.Since(now))
	defer resp.Body.Close()

	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		c.Errorf("Error reading response body for secrets: %v", err)
		return nil, false
	}

	if resp.StatusCode != 200 {
		msg := strings.Join(strings.Split(string(data), "\n"), " ")
		c.Errorf("Bad response code getting secrets: (status=%v, msg='%s')", resp.StatusCode, msg)
		return nil, false
	}
	return data, true
}

// SecretList returns a slice of unmarshalled Secret structs after requesting a listing of secrets.
func (c Client) SecretList() (secrets []Secret, ok bool) {
	data, ok := c.RawSecretList()
	if !ok {
		return nil, false
	}

	secrets, err := ParseSecretList(data)
	if err != nil {
		c.Errorf("Error decoding retrieved secrets: %v", err)
		return nil, false
	}
	return secrets, true
}

// buildClient constructs a new TLS client.
func (p httpClientParams) buildClient() (client *http.Client, err error) {
	keyPair, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
	if err != nil {
		return
	}

	caCert, err := ioutil.ReadFile(p.CaBundle)
	if err != nil {
		return
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	config := &tls.Config{
		Certificates: []tls.Certificate{keyPair},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12, // TLSv1.2 and up is required
		CipherSuites: ciphers,
	}
	config.BuildNameToCertificate()
	transport := &http.Transport{TLSClientConfig: config}
	return &http.Client{Transport: transport, Timeout: p.timeout}, nil
}
