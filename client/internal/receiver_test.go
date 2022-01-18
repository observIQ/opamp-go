package internal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type TestLogger struct {
	*testing.T
}

func (logger TestLogger) Debugf(format string, v ...interface{}) {
	logger.Logf(format, v...)
}

func TestRequestRestart(t *testing.T) {
	called := false

	callbacks := types.CallbacksStruct{
		OnRestartRequestedFunc: func() error {
			called = true
			return nil
		},
	}
	receiver := NewReceiver(TestLogger{t}, callbacks, nil, nil)
	receiver.processReceivedMessage(context.Background(), &protobufs.ServerToAgent{
		Flags: protobufs.ServerToAgent_RequestRestart,
	})
	assert.Equal(t, true, called, "requestRestart flag should trigger the OnRestartRequested callback")
}

func TestNoRequestRestart(t *testing.T) {
	called := false
	callbacks := types.CallbacksStruct{
		OnRestartRequestedFunc: func() error {
			called = true
			return nil
		},
	}
	receiver := NewReceiver(TestLogger{t}, callbacks, nil, nil)
	receiver.processReceivedMessage(context.Background(), &protobufs.ServerToAgent{
		Flags: 0,
	})
	assert.Equal(t, false, called, "without RequestRestart flag, do not trigger the OnRestartRequested callback")
}
