package internal

import (
	"context"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/internal"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// WSReceiver implements the WebSocket client's receiving portion of OpAMP protocol.
type WSReceiver struct {
	conn      *websocket.Conn
	logger    types.Logger
	sender    *WSSender
	callbacks types.Callbacks
	processor receivedProcessor

	// Indicates that the receiver has fully stopped.
	stopped chan struct{}
}

// NewWSReceiver creates a new Receiver that uses WebSocket to receive
// messages from the server.
func NewWSReceiver(
	logger types.Logger,
	callbacks types.Callbacks,
	conn *websocket.Conn,
	sender *WSSender,
	clientSyncedState *ClientSyncedState,
	packagesStateProvider types.PackagesStateProvider,
	capabilities protobufs.AgentCapabilities,
	packageSyncMutex *sync.Mutex,
) *WSReceiver {
	w := &WSReceiver{
		conn:      conn,
		logger:    logger,
		sender:    sender,
		callbacks: callbacks,
		processor: newReceivedProcessor(logger, callbacks, sender, clientSyncedState, packagesStateProvider, capabilities, packageSyncMutex),
		stopped:   make(chan struct{}),
	}

	return w
}

// Start starts the receiver loop.
func (r *WSReceiver) Start(ctx context.Context) {
	go r.ReceiverLoop(ctx)
}

// IsStopped returns a channel that's closed when the receiver is stopped.
func (r *WSReceiver) IsStopped() <-chan struct{} {
	return r.stopped
}

// ReceiverLoop runs the receiver loop.
// To stop the receiver cancel the context and close the websocket connection
func (r *WSReceiver) ReceiverLoop(ctx context.Context) {
	type receivedMessage struct {
		message *protobufs.ServerToAgent
		err     error
	}

	defer func() { close(r.stopped) }()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			result := make(chan receivedMessage, 1)

			// To stop this goroutine, close the websocket connection
			go func() {
				var message protobufs.ServerToAgent
				err := r.receiveMessage(&message)
				result <- receivedMessage{&message, err}
			}()

			select {
			case <-ctx.Done():
				return
			case res := <-result:
				if res.err != nil {
					if !websocket.IsCloseError(res.err, websocket.CloseNormalClosure) {
						r.logger.Errorf(ctx, "Unexpected error while receiving: %v", res.err)
					}
					return
				}
				r.processor.ProcessReceivedMessage(ctx, res.message)
			}
		}
	}
}

func (r *WSReceiver) receiveMessage(msg *protobufs.ServerToAgent) error {
	_, bytes, err := r.conn.ReadMessage()
	if err != nil {
		return err
	}
	err = internal.DecodeWSMessage(bytes, msg)
	if err != nil {
		return fmt.Errorf("cannot decode received message: %w", err)
	}
	return err
}
