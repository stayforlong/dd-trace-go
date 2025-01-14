// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

// Package telemetry implements a client for sending telemetry information to
// Datadog regarding usage of an APM library such as tracing or profiling.
package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
	logger "gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/osinfo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"
)

var (
	// GlobalClient acts as a global telemetry client that the
	// tracer, profiler, and appsec products will use
	GlobalClient *Client
	// copied from dd-trace-go/profiler
	defaultHTTPClient = &http.Client{
		// We copy the transport to avoid using the default one, as it might be
		// augmented with tracing and we don't want these calls to be recorded.
		// See https://golang.org/pkg/net/http/#DefaultTransport .
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 5 * time.Second,
	}
	hostname string

	// protects agentlessURL, which may be changed for testing purposes
	agentlessEndpointLock sync.RWMutex
	// agentlessURL is the endpoint used to send telemetry in an agentless environment. It is
	// also the default URL in case connecting to the agent URL fails.
	agentlessURL = "https://instrumentation-telemetry-intake.datadoghq.com/api/v2/apmtelemetry"

	defaultHeartbeatInterval = 60 // seconds

	// LogPrefix specifies the prefix for all telemetry logging
	LogPrefix = "Instrumentation telemetry: "
)

func init() {
	h, err := os.Hostname()
	if err == nil {
		hostname = h
	}
	GlobalClient = new(Client)
	GlobalClient.fallbackOps()
}

// Client buffers and sends telemetry messages to Datadog (possibly through an
// agent). Client.Start should be called before any other methods.
//
// Client is safe to use from multiple goroutines concurrently. The client will
// send all telemetry requests in the background, in order to avoid blocking the
// caller since telemetry should not disrupt an application. Metrics are
// aggregated by the Client.
type Client struct {
	// URL for the Datadog agent or Datadog telemetry endpoint
	URL string
	// APIKey should be supplied if the endpoint is not a Datadog agent,
	// i.e. you are sending telemetry directly to Datadog
	APIKey string
	// The interval for sending a heartbeat signal to the backend.
	// Configurable with DD_TELEMETRY_HEARTBEAT_INTERVAL. Default 60s.
	heartbeatInterval time.Duration

	// e.g. "tracers", "profilers", "appsec"
	Namespace Namespace

	// App-specific information
	Service string
	Env     string
	Version string

	// Client will be used for telemetry uploads. This http.Client, if
	// provided, should be the same as would be used for any other
	// interaction with the Datadog agent, e.g. if the agent is accessed
	// over UDS, or if the user provides their own http.Client to the
	// profiler/tracer to access the agent over a proxy.
	//
	// If Client is nil, an http.Client with the same Transport settings as
	// http.DefaultTransport and a 5 second timeout will be used.
	Client *http.Client

	// mu guards all of the following fields
	mu sync.Mutex

	// debug enables the debug flag for all requests, see
	// https://dtdg.co/3bv2MMv.
	// DD_INSTRUMENTATION_TELEMETRY_DEBUG configures this field.
	debug bool
	// started is true in between when Start() returns and the next call to
	// Stop()
	started bool
	// seqID is a sequence number used to order telemetry messages by
	// the back end.
	seqID int64
	// heartbeatT is used to schedule heartbeat messages
	heartbeatT *time.Timer
	// requests hold all messages which don't need to be immediately sent
	requests []*Request
	// metrics holds un-sent metrics that will be aggregated the next time
	// metrics are sent
	metrics    map[Namespace]map[string]*metric
	newMetrics bool
}

func log(msg string, args ...interface{}) {
	// Debug level so users aren't spammed with telemetry info.
	logger.Debug(fmt.Sprintf(LogPrefix+msg, args...))
}

// Start registers that the app has begun running with the app-started event
// Start also configures the telemetry client based on the following telemetry
// environment variables: DD_INSTRUMENTATION_TELEMETRY_ENABLED,
// DD_TELEMETRY_HEARTBEAT_INTERVAL, DD_INSTRUMENTATION_TELEMETRY_DEBUG,
// and DD_TELEMETRY_DEPENDENCY_COLLECTION_ENABLED.
// TODO: implement passing in error information about tracer start
func (c *Client) Start(configuration []Configuration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if Disabled() {
		return
	}
	if c.started {
		log("attempted to start telemetry client when client has already started - ignoring attempt")
		return
	}
	// Don't start the telemetry client if there is some error configuring the client with fallback
	// options, e.g. an API key was not found but agentless telemetry is expected.
	if err := c.fallbackOps(); err != nil {
		log(err.Error())
		return
	}

	c.started = true
	c.metrics = make(map[Namespace]map[string]*metric)
	c.debug = internal.BoolEnv("DD_INSTRUMENTATION_TELEMETRY_DEBUG", false)

	payload := &AppStarted{
		Configuration: configuration,
		Products: Products{
			AppSec: ProductDetails{
				Version: version.Tag,
				Enabled: appsec.Enabled(),
			},
			// If the profiler starts, an app-product-change event will be sent
			// to signal that the profiler is enabled. It is important that we
			// send the profiler version, since product info is hashed on it's version
			// when being stored by the instrumentation telemetry backend.
			Profiler: ProductDetails{
				Version: version.Tag,
				Enabled: false,
			},
		},
	}

	appStarted := c.newRequest(RequestTypeAppStarted)
	appStarted.Body.Payload = payload
	c.scheduleSubmit(appStarted)

	if collectDependencies() {
		var depPayload Dependencies
		if deps, ok := debug.ReadBuildInfo(); ok {
			for _, dep := range deps.Deps {
				depPayload.Dependencies = append(depPayload.Dependencies,
					Dependency{
						Name:    dep.Path,
						Version: dep.Version,
					},
				)
			}
		}
		dep := c.newRequest(RequestTypeDependenciesLoaded)
		dep.Body.Payload = depPayload
		c.scheduleSubmit(dep)
	}

	c.flush()

	heartbeat := internal.IntEnv("DD_TELEMETRY_HEARTBEAT_INTERVAL", defaultHeartbeatInterval)
	if heartbeat < 1 || heartbeat > 3600 {
		log("DD_TELEMETRY_HEARTBEAT_INTERVAL=%d not in [1,3600] range, setting to default of %d", heartbeat, defaultHeartbeatInterval)
		heartbeat = defaultHeartbeatInterval
	}
	c.heartbeatInterval = time.Duration(heartbeat) * time.Second
	c.heartbeatT = time.AfterFunc(c.heartbeatInterval, c.backgroundHeartbeat)
}

// Stop notifies the telemetry endpoint that the app is closing. All outstanding
// messages will also be sent. No further messages will be sent until the client
// is started again
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return
	}
	c.started = false
	c.heartbeatT.Stop()
	// close request types have no body
	r := c.newRequest(RequestTypeAppClosing)
	c.scheduleSubmit(r)
	c.flush()
}

// Disabled returns whether instrumentation telemetry is disabled
// according to the DD_INSTRUMENTATION_TELEMETRY_ENABLED env var
func Disabled() bool {
	return !internal.BoolEnv("DD_INSTRUMENTATION_TELEMETRY_ENABLED", true)
}

// collectDependencies returns whether dependencies telemetry information is sent
func collectDependencies() bool {
	return internal.BoolEnv("DD_TELEMETRY_DEPENDENCY_COLLECTION_ENABLED", true)
}

type metricKind string

var (
	metricKindGauge metricKind = "gauge"
	metricKindCount metricKind = "count"
)

type metric struct {
	name  string
	kind  metricKind
	value float64
	// Unix timestamp
	ts     float64
	tags   []string
	common bool
}

// TODO: Can there be identically named/tagged metrics with a "common" and "not
// common" variant?

func newmetric(name string, kind metricKind, tags []string, common bool) *metric {
	return &metric{
		name:   name,
		kind:   kind,
		tags:   append([]string{}, tags...),
		common: common,
	}
}

func metricKey(name string, tags []string) string {
	return name + strings.Join(tags, "-")
}

// Gauge sets the value for a gauge with the given name and tags. If the metric
// is not language-specific, common should be set to true
func (c *Client) Gauge(namespace Namespace, name string, value float64, tags []string, common bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return
	}
	if _, ok := c.metrics[namespace]; !ok {
		c.metrics[namespace] = map[string]*metric{}
	}
	key := metricKey(name, tags)
	m, ok := c.metrics[namespace][key]
	if !ok {
		m = newmetric(name, metricKindGauge, tags, common)
		c.metrics[namespace][key] = m
	}
	m.value = value
	m.ts = float64(time.Now().Unix())
	c.newMetrics = true
}

// Count adds the value to a count with the given name and tags. If the metric
// is not language-specific, common should be set to true
func (c *Client) Count(namespace Namespace, name string, value float64, tags []string, common bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return
	}
	if _, ok := c.metrics[namespace]; !ok {
		c.metrics[namespace] = map[string]*metric{}
	}
	key := metricKey(name, tags)
	m, ok := c.metrics[namespace][key]
	if !ok {
		m = newmetric(name, metricKindCount, tags, common)
		c.metrics[namespace][key] = m
	}
	m.value += value
	m.ts = float64(time.Now().Unix())
	c.newMetrics = true
}

// flush sends any outstanding telemetry messages and aggregated metrics to be
// sent to the backend. Requests are sent in the background. Should be called
// with c.mu locked
func (c *Client) flush() {
	submissions := make([]*Request, 0, len(c.requests)+1)
	if c.newMetrics {
		c.newMetrics = false
		r := c.newRequest(RequestTypeGenerateMetrics)
		for namespace := range c.metrics {
			payload := &Metrics{
				Namespace: namespace,
			}
			for _, m := range c.metrics[namespace] {
				s := Series{
					Metric: m.name,
					Type:   string(m.kind),
					Tags:   m.tags,
					Common: m.common,
				}
				s.Points = [][2]float64{{m.ts, m.value}}
				payload.Series = append(payload.Series, s)
			}
			r.Body.Payload = payload
			submissions = append(submissions, r)
		}
	}
	// copy over requests so we can do the actual submission without holding
	// the lock. Zero out the old stuff so we don't leak references
	for i, r := range c.requests {
		submissions = append(submissions, r)
		c.requests[i] = nil
	}
	c.requests = c.requests[:0]

	go func() {
		for _, r := range submissions {
			err := r.submit()
			if err != nil {
				log("submission error: %s", err.Error())
			}
		}
	}()
}

var (
	osName        string
	osNameOnce    sync.Once
	osVersion     string
	osVersionOnce sync.Once
)

// XXX: is it actually safe to cache osName and osVersion? For example, can the
// kernel be updated without stopping execution?

func getOSName() string {
	osNameOnce.Do(func() { osName = osinfo.OSName() })
	return osName
}

func getOSVersion() string {
	osVersionOnce.Do(func() { osVersion = osinfo.OSVersion() })
	return osVersion
}

// newRequests populates a request with the common fields shared by all requests
// sent through this Client
func (c *Client) newRequest(t RequestType) *Request {
	c.seqID++
	body := &Body{
		APIVersion:  "v2",
		RequestType: t,
		TracerTime:  time.Now().Unix(),
		RuntimeID:   globalconfig.RuntimeID(),
		SeqID:       c.seqID,
		Debug:       c.debug,
		Application: Application{
			ServiceName:     c.Service,
			Env:             c.Env,
			ServiceVersion:  c.Version,
			TracerVersion:   version.Tag,
			LanguageName:    "go",
			LanguageVersion: runtime.Version(),
		},
		Host: Host{
			Hostname:     hostname,
			OS:           getOSName(),
			OSVersion:    getOSVersion(),
			Architecture: runtime.GOARCH,
			// TODO (lievan): getting kernel name, release, version TBD
		},
	}

	header := &http.Header{
		"Content-Type":               {"application/json"},
		"DD-Telemetry-API-Version":   {"v2"},
		"DD-Telemetry-Request-Type":  {string(t)},
		"DD-Client-Library-Language": {"go"},
		"DD-Client-Library-Version":  {version.Tag},
		"DD-Agent-Env":               {c.Env},
		"DD-Agent-Hostname":          {hostname},
		"Datadog-Container-ID":       {internal.ContainerID()},
	}
	if c.URL == getAgentlessURL() {
		header.Set("DD-API-KEY", c.APIKey)
	}
	client := c.Client
	if client == nil {
		client = defaultHTTPClient
	}
	return &Request{Body: body,
		Header:     header,
		HTTPClient: client,
		URL:        c.URL,
	}
}

// submit sends a telemetry request
func (r *Request) submit() error {
	retry, err := r.trySubmit()
	if retry {
		// retry telemetry submissions in instances where the telemetry client has trouble
		// connecting with the agent
		log("telemetry submission failed, retrying with agentless: %s", err)
		r.URL = getAgentlessURL()
		r.Header.Set("DD-API-KEY", defaultAPIKey())
		if _, err := r.trySubmit(); err == nil {
			return nil
		}
		log("retrying with agentless telemetry failed: %s", err)
	}
	return err
}

// agentlessRetry determines if we should retry a failed a request with
// by submitting to the agentless endpoint
func agentlessRetry(req *Request, resp *http.Response, err error) bool {
	if req.URL == getAgentlessURL() {
		// no need to retry with agentless endpoint if it already failed
		return false
	}
	if err != nil {
		// we didn't get a response which might signal a connectivity problem with
		// agent - retry with agentless
		return true
	}
	// TODO: add more status codes we do not want to retry on
	doNotRetry := []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden}
	for status := range doNotRetry {
		if resp.StatusCode == status {
			return false
		}
	}
	return true
}

// trySubmit submits a telemetry request to the specified URL
// in the Request struct. If submission fails, return whether or not
// this submission should be re-tried with the agentless endpoint
// as well as the error that occurred
func (r *Request) trySubmit() (retry bool, err error) {
	b, err := json.Marshal(r.Body)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequest(http.MethodPost, r.URL, bytes.NewReader(b))
	if err != nil {
		return false, err
	}
	req.Header = *r.Header

	req.ContentLength = int64(len(b))

	client := r.HTTPClient
	if client == nil {
		client = defaultHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return agentlessRetry(r, resp, err), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return agentlessRetry(r, resp, err), errBadStatus(resp.StatusCode)
	}
	return false, nil
}

type errBadStatus int

func (e errBadStatus) Error() string { return fmt.Sprintf("bad HTTP response status %d", e) }

// scheduleSubmit queues a request to be sent to the backend. Should be called
// with c.mu locked
func (c *Client) scheduleSubmit(r *Request) {
	c.requests = append(c.requests, r)
}

// backgroundHeartbeat is invoked at every heartbeat interval,
// sending the app-heartbeat event and flushing any outstanding
// telemetry messages
func (c *Client) backgroundHeartbeat() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return
	}
	c.scheduleSubmit(c.newRequest(RequestTypeAppHeartbeat))
	c.flush()
	c.heartbeatT.Reset(c.heartbeatInterval)
}
