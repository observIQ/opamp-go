package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gorilla/websocket"

	"github.com/open-telemetry/opamp-go/client/internal"
	"github.com/open-telemetry/opamp-go/client/types"
	sharedinternal "github.com/open-telemetry/opamp-go/internal"
	"github.com/open-telemetry/opamp-go/protobufs"
)

const (
	defaultShutdownTimeout = 5 * time.Second
)

// wsClient is an OpAMP Client implementation for WebSocket transport.
// See specification: https://github.com/open-telemetry/opamp-spec/blob/main/specification.md#websocket-transport
type wsClient struct {
	common internal.ClientCommon

	// OpAMP Server URL.
	url *url.URL

	// HTTP request headers to use when connecting to OpAMP Server.
	getHeader func() http.Header

	// Websocket dialer and connection.
	dialer    websocket.Dialer
	conn      *websocket.Conn
	connMutex sync.RWMutex

	// The sender is responsible for sending portion of the OpAMP protocol.
	senders map[types.InstanceUid]*internal.WSSender

	// last non-nil internal error that was encountered in the conn retry loop,
	// currently used only for testing.
	lastInternalErr atomic.Pointer[error]

	// Network connection timeout used for the WebSocket closing handshake.
	// This field is currently only modified during testing.
	connShutdownTimeout time.Duration

	// responseChain is used for the "via" argument in CheckRedirect.
	// It is appended to with every redirect followed, and zeroed on a succesful
	// connection. responseChain should only be referred to by the goroutine that
	// runs tryConnectOnce and its synchronous callees.
	responseChain []*http.Response
}

// NewWebSocket creates a new OpAMP Client that uses WebSocket transport.
func NewWebSocket(logger types.Logger) *wsClient {
	if logger == nil {
		logger = &sharedinternal.NopLogger{}
	}

	w := &wsClient{
		common:              internal.NewClientCommon(logger),
		connShutdownTimeout: defaultShutdownTimeout,
	}
	return w
}

func (c *wsClient) PrepareStart(ctx context.Context, settings types.StartSettings) error {
	for _, agent := range settings.Agents {
		// sender is shared between this client and common client
		sender := internal.NewSender(c.common.Logger)
		c.senders[agent.InstanceUid] = sender
		c.common.Agents[agent.InstanceUid].Sender = sender
		c.common.SetAgentDescription(agent.InstanceUid, agent.AgentDescription)
		c.common.SetHealth(agent.InstanceUid, &protobufs.ComponentHealth{Healthy: false})
	}

	return c.common.PrepareStart(ctx, settings)
}

func (c *wsClient) Start(ctx context.Context, settings types.StartSettings) error {
	// Prepare connection settings.
	c.dialer = *websocket.DefaultDialer

	var err error
	c.url, err = url.Parse(settings.OpAMPServerURL)
	if err != nil {
		return err
	}

	c.dialer.EnableCompression = settings.EnableCompression

	if settings.TLSConfig != nil {
		c.url.Scheme = "wss"
	}
	c.dialer.TLSClientConfig = settings.TLSConfig

	headerFunc := settings.HeaderFunc
	if headerFunc == nil {
		headerFunc = func(h http.Header) http.Header {
			return h
		}
	}

	baseHeader := settings.Header
	if baseHeader == nil {
		baseHeader = http.Header{}
	}

	c.getHeader = func() http.Header {
		return headerFunc(baseHeader.Clone())
	}

	c.common.StartConnectAndRun(c.runUntilStopped)

	return nil
}

func (c *wsClient) Stop(ctx context.Context) error {
	return c.common.Stop(ctx)
}

func (c *wsClient) AgentDescription(agentId types.InstanceUid) *protobufs.AgentDescription {
	return c.common.AgentDescription(agentId)
}

func (c *wsClient) SetAgentDescription(agentId types.InstanceUid, descr *protobufs.AgentDescription) error {
	return c.common.SetAgentDescription(agentId, descr)
}

func (c *wsClient) RequestConnectionSettings(agentId types.InstanceUid, request *protobufs.ConnectionSettingsRequest) error {
	return c.common.RequestConnectionSettings(agentId, request)
}

func (c *wsClient) SetHealth(agentId types.InstanceUid, health *protobufs.ComponentHealth) error {
	return c.common.SetHealth(agentId, health)
}

func (c *wsClient) UpdateEffectiveConfig(agentId types.InstanceUid, ctx context.Context) error {
	return c.common.UpdateEffectiveConfig(agentId, ctx)
}

func (c *wsClient) SetRemoteConfigStatus(agentId types.InstanceUid, status *protobufs.RemoteConfigStatus) error {
	return c.common.SetRemoteConfigStatus(agentId, status)
}

func (c *wsClient) SetPackageStatuses(agentId types.InstanceUid, statuses *protobufs.PackageStatuses) error {
	return c.common.SetPackageStatuses(agentId, statuses)
}

func (c *wsClient) SetCustomCapabilities(agentId types.InstanceUid, customCapabilities *protobufs.CustomCapabilities) error {
	return c.common.SetCustomCapabilities(agentId, customCapabilities)
}

func (c *wsClient) SetFlags(agentId types.InstanceUid, flags protobufs.AgentToServerFlags) {
	c.common.SetFlags(agentId, flags)
}

func (c *wsClient) SendCustomMessage(agentId types.InstanceUid, message *protobufs.CustomMessage) (messageSendingChannel chan struct{}, err error) {
	return c.common.SendCustomMessage(agentId, message)
}

// SetAvailableComponents implements OpAMPClient.SetAvailableComponents
func (c *wsClient) SetAvailableComponents(agentId types.InstanceUid, components *protobufs.AvailableComponents) error {
	return c.common.SetAvailableComponents(agentId, components)
}

func viaReq(resps []*http.Response) []*http.Request {
	reqs := make([]*http.Request, 0, len(resps))
	for _, resp := range resps {
		reqs = append(reqs, resp.Request)
	}
	return reqs
}

// handleRedirect checks a failed websocket upgrade response for a 3xx response
// and a Location header. If found, it sets the URL to the location found in the
// header so that it is tried on the next retry, instead of the current URL.
func (c *wsClient) handleRedirect(ctx context.Context, resp *http.Response) error {
	// append to the responseChain so that subsequent redirects will have access
	c.responseChain = append(c.responseChain, resp)

	// very liberal handling of 3xx that largely ignores HTTP semantics
	redirect, err := resp.Location()
	if err != nil {
		c.common.Logger.Errorf(ctx, "%d redirect, but no valid location: %s", resp.StatusCode, err)
		return err
	}

	// It's slightly tricky to make CheckRedirect work. The WS HTTP request is
	// formed within the websocket library. To work around that, copy the
	// previous request, available in the response, and set the URL to the new
	// location. It should then result in the same URL that the websocket
	// library will form.
	nextRequest := resp.Request.Clone(ctx)
	nextRequest.URL = redirect

	// if CheckRedirect results in an error, it gets returned, terminating
	// redirection. As with stdlib, the error is wrapped in url.Error.
	if c.common.Callbacks.CheckRedirect != nil {
		if err := c.common.Callbacks.CheckRedirect(nextRequest, viaReq(c.responseChain), c.responseChain); err != nil {
			return &url.Error{
				Op:  "Get",
				URL: nextRequest.URL.String(),
				Err: err,
			}
		}
	}

	// rewrite the scheme for the sake of tolerance
	if redirect.Scheme == "http" {
		redirect.Scheme = "ws"
	} else if redirect.Scheme == "https" {
		redirect.Scheme = "wss"
	}
	c.common.Logger.Debugf(ctx, "%d redirect to %s", resp.StatusCode, redirect)

	// Set the URL to the redirect, so that it connects to it on the
	// next cycle.
	c.url = redirect

	return nil
}

// Try to connect once. Returns an error if connection fails and optional retryAfter
// duration to indicate to the caller to retry after the specified time as instructed
// by the Server.
func (c *wsClient) tryConnectOnce(ctx context.Context) (retryAfter sharedinternal.OptionalDuration, err error) {
	var resp *http.Response
	var redirecting bool
	defer func() {
		if err != nil && !redirecting {
			c.responseChain = nil
			if !c.common.IsStopping() {
				c.common.Callbacks.OnConnectFailed(ctx, err)
			}
		}
	}()
	conn, resp, err := c.dialer.DialContext(ctx, c.url.String(), c.getHeader())
	if err != nil {
		if !c.common.IsStopping() {
			c.common.Callbacks.OnConnectFailed(ctx, err)
		}
		if resp != nil {
			duration := sharedinternal.ExtractRetryAfterHeader(resp)
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				redirecting = true
				if err := c.handleRedirect(ctx, resp); err != nil {
					return duration, err
				}
			} else {
				c.common.Logger.Errorf(ctx, "Server responded with status=%v", resp.Status)
			}
			return duration, err
		}
		return sharedinternal.OptionalDuration{Defined: false}, err
	}

	// Successfully connected.
	c.connMutex.Lock()
	c.conn = conn
	c.connMutex.Unlock()
	c.common.Callbacks.OnConnect(ctx)

	return sharedinternal.OptionalDuration{Defined: false}, nil
}

// Continuously try until connected. Will return nil when successfully
// connected. Will return error if it is cancelled via context.
func (c *wsClient) ensureConnected(ctx context.Context) error {
	infiniteBackoff := backoff.NewExponentialBackOff()

	// Make ticker run forever.
	infiniteBackoff.MaxElapsedTime = 0

	interval := time.Duration(0)

	for {
		timer := time.NewTimer(interval)
		interval = infiniteBackoff.NextBackOff()

		select {
		case <-timer.C:
			{
				if retryAfter, err := c.tryConnectOnce(ctx); err != nil {
					c.lastInternalErr.Store(&err)
					if errors.Is(err, context.Canceled) {
						c.common.Logger.Debugf(ctx, "Client is stopped, will not try anymore.")
						return err
					} else {
						c.common.Logger.Errorf(ctx, "Connection failed (%v), will retry.", err)
					}
					// Retry again a bit later.

					if retryAfter.Defined && retryAfter.Duration > interval {
						// If the Server suggested connecting later than our interval
						// then honour Server's request, otherwise wait at least
						// as much as we calculated.
						interval = retryAfter.Duration
					}

					continue
				}
				// Connected successfully.
				return nil
			}

		case <-ctx.Done():
			c.common.Logger.Debugf(ctx, "Client is stopped, will not try anymore.")
			timer.Stop()
			return ctx.Err()
		}
	}
}

// runOneCycle performs the following actions:
//  1. connect (try until succeeds).
//  2. send first status report.
//  3. start the sender to wait for scheduled messages and send them to the server.
//  4. start the receiver to receive and process messages until an error happens.
//  5. wait until both the sender and receiver are stopped.
//
// runOneCycle will close the connection it created before it return.
//
// When Stop() is called (ctx is cancelled, isStopping is set), wsClient will shutdown gracefully:
//  1. sender will be cancelled by the ctx, send the close message to server and return the error via sender.Err().
//  2. runOneCycle will handle that error and wait for the close message from server until timeout.
func (c *wsClient) runOneCycle(ctx context.Context) {
	if err := c.ensureConnected(ctx); err != nil {
		// Can't connect, so can't move forward. This currently happens when we
		// are being stopped.
		return
	}
	// Close the underlying connection.
	defer c.conn.Close()

	if c.common.IsStopping() {
		return
	}

	// Prepare the first status report.
	err := c.common.PrepareFirstMessage(ctx)
	if err != nil {
		c.common.Logger.Errorf(ctx, "cannot prepare the first message:%v", err)
		return
	}

	// Create a cancellable context for background processors.
	senderCtx, stopSender := context.WithCancel(ctx)
	defer stopSender()

	// Connected successfully. Start the sender. This will also send the first
	// status report.
	for _, sender := range c.senders {
		if err := sender.Start(senderCtx, c.conn); err != nil {
			c.common.Logger.Errorf(senderCtx, "Failed to send first status report: %v", err)
			return
		}
	}

	// When the wsclient is closed, the context passed to runOneCycle will be canceled.
	// The receiver should keep running and processing messages
	// until it received a Close message from the server which means the server has no more messages.
	receiverCtx, stopReceiver := context.WithCancel(context.Background())
	defer stopReceiver()

	// First status report sent. Now loop to receive and process messages.
	// var receivers []*internal.WSReceiver
	for id, sender := range c.senders {
		r := internal.NewWSReceiver(
			c.common.Logger,
			c.common.Callbacks,
			c.conn,
			sender,
			&c.common.Agents[id].ClientSyncedState,
			c.common.Agents[id].PackagesStateProvider,
			c.common.Agents[id].Capabilities,
			&c.common.PackageSyncMutex,
		)
		r.Start(receiverCtx)
		go func() {
			select {
			case <-sender.IsStopped():
				// sender will send close message to initiate the close handshake
				if err := sender.StoppingErr(); err != nil {
					c.common.Logger.Debugf(ctx, "Error stopping the sender: %v", err)

					stopReceiver()
					<-r.IsStopped()
					break
				}

				c.common.Logger.Debugf(ctx, "Waiting for receiver to stop.")
				select {
				case <-r.IsStopped():
					c.common.Logger.Debugf(ctx, "Receiver stopped.")
				case <-time.After(c.connShutdownTimeout):
					c.common.Logger.Debugf(ctx, "Timeout waiting for receiver to stop.")
					stopReceiver()
					<-r.IsStopped()
				}
			case <-r.IsStopped():
				// If we exited receiverLoop it means there is a connection error, we cannot
				// read messages anymore. We need to start over.

				stopSender()
				<-sender.IsStopped()
			}
		}()
	}
}

func (c *wsClient) runUntilStopped(ctx context.Context) {
	// Iterates until we detect that the client is stopping.
	for {
		if c.common.IsStopping() {
			return
		}

		c.runOneCycle(ctx)
	}
}
