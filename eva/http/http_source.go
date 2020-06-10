// Copyright 2018-2019 Workiva Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/Workiva/eva-client-go/edn"
	"github.com/Workiva/eva-client-go/eva"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (

	// XFormContentType defines the form encoding mime type.
	XFormContentType = "application/x-www-form-urlencoded"

	ErrUnsupportedType = edn.ErrorMessage("Unsupported type")

	// ErrNoServiceImpl defines a service implementation error.
	ErrNoServiceImpl = edn.ErrorMessage("No service implementation")

	// ErrInvalidCertificate defines an invalid certificate.
	ErrInvalidCertificate = edn.ErrorMessage("Invalid certificate")

	// SourceName defines the http source type name.
	SourceName = "http"

	// defaultRetryPauseTimeout defines the default retry amount.
	defaultRetryPauseTimeout = 5000 // ms
)

// httpDoer is the thing that does the client call.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type httpClientInvoker func(httpDoer, *http.Request) (*http.Response, error)

// httpSourceImpl defines the http source.
type httpSourceImpl struct {
	*eva.BaseSource
	retryPause time.Duration
	retryTimes int
	protocol   string
	server     string
	port       int
	version    string
	roots      *x509.CertPool
	callClient httpClientInvoker
}

// initialize the source.
func init() {
	eva.PanicOnError(func() error {
		return eva.AddSourceFactory(SourceName, initHttpSource)
	})
}

// initHttpSource initialized an http source.
func initHttpSource(config eva.Configuration, tenant eva.Tenant) (source eva.Source, err error) {

	if config != nil {

		var has bool
		var server string
		var roots *x509.CertPool

		retries := 1 // We should try just once by default
		retryPause := defaultRetryPauseTimeout
		protocol := "http"

		var srcConfig eva.SourceConfiguration
		if srcConfig, err = config.Source(); err == nil {
			if server, has = srcConfig.Setting("server"); has {
				var certData string
				if certData, has = srcConfig.Setting("cert"); has {
					roots = x509.NewCertPool()
					if !roots.AppendCertsFromPEM([]byte(certData)) {
						err = edn.MakeError(ErrInvalidCertificate, nil)
					} else {
						protocol = "https"
					}
				}
			} else {
				err = edn.MakeError(eva.ErrInvalidConfiguration, "No server")
			}

			if err == nil {
				if toRetry, has := srcConfig.Setting("retries"); has {

					// this can be in two parts:
					//   "10" or "10@5000"
					switch split := strings.Index(toRetry, "@"); split {
					case -1:
						retries, err = strconv.Atoi(toRetry)
					case 0:
						err = &edn.Error{} // place holder... will be overwritten.
					default:
						if retries, err = strconv.Atoi(toRetry[:split]); err == nil {
							retryPause, err = strconv.Atoi(toRetry[split+1:])
						}
					}

					if err != nil {
						err = edn.MakeError(eva.ErrInvalidConfiguration, "retries in not in the right format")
					}
				}
			}

		}

		if err == nil {
			httpSource := &httpSourceImpl{
				protocol:   protocol,
				server:     server,
				version:    "v.1",
				roots:      roots,
				retryTimes: retries,
				retryPause: time.Millisecond * time.Duration(retryPause),
				callClient: func(c httpDoer, r *http.Request) (*http.Response, error) { return c.Do(r) },
			}

			if err == nil {
				if httpSource.BaseSource, err = eva.NewBaseSource(config, tenant, httpSource, newHttpConnChannel, httpSource.queryImpl); err == nil {
					source = httpSource
				}
			}
		}
	} else {
		err = edn.MakeError(eva.ErrInvalidConfiguration, "nil")
	}

	return source, err
}

// formulateUrl from the request and operation.
func (source *httpSourceImpl) formulateUrl(operation string) string {
	return fmt.Sprintf(
		"%s://%s/eva/%s/%s/%s/%s",
		source.protocol,
		source.server,
		source.version,
		operation,
		source.Tenant().Name(),
		source.BaseSource.Category())
}

// NewHTTPClient creates a unique HTTP client to be used on a per client basis.
//
// There is a known issue in the golang base http client where if a connection to a
// remote host fails in an unexpected way, if the request set a custom http header, the
// caller not only blocks, but it leaves in place a mutex preventing any other new
// connections on the same http client from reaching that same remote host.
//
// See https://github.com/golang/go/issues/33006 for more details.
//
// This client is designed to not share any of the global resources with go's builtin
// http.DefaultClient so that clients can be isolated from this shared failure state.
//
// By default, this client also sets a Timeout for all connections running through it.
//
// Keep in mind that this timeout should be the MAXIMUM timeout for this client.
// If you would like a lower, per-request timeout, please set request's Context
// using req.WithContext(ctx): https://golang.org/pkg/net/http/#Request.WithContext
func newHTTPClient() *http.Client {
	// borrowed from net/http::DefaultTransport to ensure we don't have shared globals
	return &http.Client{
		Transport: newHTTPTransport(),

		// Default timeout for all requests going through this client.
		Timeout: 60 * time.Second,
	}
}

// NewHTTPTransport creates a unique http transport to be used in an http client.
//
// The returned transport has all the same settings as the default global
// http transport, with the exception of MaxIdleConnsPerHost, which is set to 100.
func newHTTPTransport() *http.Transport {
	// borrowed from net/http::DefaultTransport to ensure we don't have shared globals
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true, // added in go 1.13
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100, // note: different from default global transport
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// call the uri with the provided form.
func (source *httpSourceImpl) call(method string, uri string, form url.Values) (result eva.Result, err error) {

	if source.callClient != nil {
		var req *http.Request
		client := newHTTPClient()

		var serializer edn.Serializer
		if serializer, err = source.Serializer(); err == nil {
			if req, err = http.NewRequest(method, uri, strings.NewReader(form.Encode())); err == nil {

				if corrId, has := source.Tenant().CorrelationId(); has {
					req.Header.Add("_cid", corrId)
				}

				req.Header.Add("Content-Type", XFormContentType)
				req.Header.Add("Accept", serializer.MimeType().String())

				if source.roots != nil {
					client.Transport = &http.Transport{
						TLSClientConfig: &tls.Config{
							RootCAs: source.roots,
						},
					}
				}
			}
		}

		if err == nil {
			done := false
			for tries := 0; tries < source.retryTimes && err == nil && !done; tries++ {

				var resp *http.Response
				if resp, err = source.callClient(client, req); err == nil {

					done = true // At this point the request was made and server responded.
					result, err = newHttpResult(req, form, resp)
				}

				switch e := err.(type) {
				case *url.Error:
					switch errMsg := e.Err.Error(); {
					case

						// The client service is up, but it is ne responding to request yet.
						strings.Contains(errMsg, "EOF"),

						// The server responded with a strange invalid body.
						strings.Contains(errMsg, "http: ContentLength=") && strings.Contains(errMsg, " with Body length 0"),

						// The path is not reachable.
						strings.Contains(errMsg, "connect: connection refused"):

						// For all these cases, just pause and try again.
						time.Sleep(source.retryPause)
					}
				}

				// clear the error if needed.
				if err != nil {
					if tries+1 < source.retryTimes {
						err = nil
					}
				}
			}
		}

	} else {
		err = edn.MakeError(ErrNoServiceImpl, "")
	}

	return result, err
}

// queryImpl implements the query.
func (source *httpSourceImpl) queryImpl(query interface{}, parameters ...interface{}) (result eva.Result, err error) {
	form := url.Values{}

	if err == nil {
		var trx string
		switch q := query.(type) {
		case string:
			trx = q
		case edn.Serializable:
			var serializer edn.Serializer
			if serializer, err = source.Serializer(); err == nil {
				trx, err = q.Serialize(serializer)
			}
		default:
			err = edn.MakeErrorWithFormat(ErrUnsupportedType, "query type: %T", q)
		}
		if err == nil {
			form.Add("query", trx)
			if err = source.fillForm(form, parameters...); err == nil {
				uri := source.formulateUrl("q")
				result, err = source.call(http.MethodPost, uri, form)
			}
		}
	}

	return result, err
}

// fillForm fills out a form.
func (source *httpSourceImpl) fillForm(form url.Values, parameters ...interface{}) (err error) {

	var serializer edn.Serializer
	if serializer, err = source.Serializer(); err == nil {
		for index, param := range parameters {

			var v string
			switch val := param.(type) {
			case string:
				v = val
			case edn.Serializable:
				v, err = val.Serialize(serializer)
			default:
				err = edn.MakeErrorWithFormat(ErrUnsupportedType, "parameter type: %T", val)
			}

			if err != nil {
				break
			} else {
				form.Add(fmt.Sprintf("p[%d]", index), v)
			}
		}
	}

	return err
}
