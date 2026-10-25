// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
	"github.com/jaab-tech/fluxrig/pkg/snake"
)

// rawJetStream opens a plain JetStream handle to url for assertions the Bus
// interface does not expose.
func rawJetStream(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return js
}

// newDLQTestStream starts an embedded server with a business stream and a DLQ
// stream, and returns a bus whose DLQ is enabled, plus raw handles for both.
func newDLQTestStream(t *testing.T) (*NatsBus, jetstream.JetStream) {
	t.Helper()
	s, err := snake.NewServer(context.Background(), snake.Config{
		Port:        -1,
		ClusterName: t.Name(),
		StoreDir:    t.TempDir(),
	})
	require.NoError(t, err)
	t.Cleanup(s.Shutdown)

	nb := NewNatsBus("flux")
	require.NoError(t, nb.Connect(s.ClientURL(), ConnectOptions{
		Name:           t.Name(),
		ConnectTimeout: 5 * time.Second,
		ReconnectWait:  1 * time.Second,
		DLQEnabled:     true,
		DLQSubject:     "flux.dlq",
	}))
	t.Cleanup(nb.Close)

	js := rawJetStream(t, s.ClientURL())
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "flux",
		Subjects: []string{"flux.msg.>"},
	})
	require.NoError(t, err)
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "flux-dlq",
		Subjects: []string{"flux.dlq.>"},
	})
	require.NoError(t, err)
	return nb, js
}

// fetchOneDLQ reads the first message currently in the DLQ stream, failing the
// test if none arrives before the deadline.
func fetchOneDLQ(t *testing.T, js jetstream.JetStream) jetstream.Msg {
	t.Helper()
	stream, err := js.Stream(context.Background(), "flux-dlq")
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Name:          "dlq-test-reader",
		FilterSubject: "flux.dlq.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
		require.NoError(t, err)
		for m := range msgs.Messages() {
			return m
		}
		if msgs.Error() != nil {
			t.Fatalf("fetch error: %v", msgs.Error())
		}
	}
	t.Fatal("no message arrived in the dead letter stream")
	return nil
}

// A payload that cannot CBOR-decode used to be logged and Term'd with nothing
// kept. It must now land in the dead letter stream with the reason recorded and
// the original bytes unchanged, so an operator can inspect and replay it.
func TestNatsBus_DecodeFailureRoutesToDLQ(t *testing.T) {
	nb, js := newDLQTestStream(t)

	sub, err := nb.Subscribe("flux.msg.decode.test", func(ctx context.Context, m *fluxmsg.FluxMsg) error {
		t.Error("handler must not run for an undecodable payload")
		return nil
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	garbage := []byte{0xff, 0xff, 0xff, 0xff}
	require.NoError(t, nb.PublishRaw(context.Background(), "flux.msg.decode.test", garbage, uuid.New()))

	msg := fetchOneDLQ(t, js)
	assert.Equal(t, garbage, msg.Data(), "the dead letter must hold the original bytes exactly")
	h := msg.Headers()
	assert.Equal(t, "flux.msg.decode.test", h.Get(dlqHeaderOriginSubject))
	assert.Equal(t, DLQReasonDecode, h.Get(dlqHeaderReason))
	assert.NotEmpty(t, h.Get(dlqHeaderAt))
}

// A handler that fails every time used to stop after maxGuaranteedRedeliveries
// with the message left to age out unread. It must now be routed to the DLQ on
// the last delivery.
func TestNatsBus_HandlerExhaustionRoutesToDLQ(t *testing.T) {
	original := maxGuaranteedRedeliveries
	maxGuaranteedRedeliveries = 2
	defer func() { maxGuaranteedRedeliveries = original }()

	nb, js := newDLQTestStream(t)

	var attempts atomic.Int32
	sub, err := nb.Subscribe("flux.msg.poison.test", func(ctx context.Context, m *fluxmsg.FluxMsg) error {
		attempts.Add(1)
		return errors.New("deterministic failure")
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	require.NoError(t, nb.Publish(context.Background(), "flux.msg.poison.test", fluxmsg.New()))

	msg := fetchOneDLQ(t, js)
	h := msg.Headers()
	assert.Equal(t, DLQReasonRedeliveryExhausted, h.Get(dlqHeaderReason))
	assert.Equal(t, "flux.msg.poison.test", h.Get(dlqHeaderOriginSubject))
}

// With the DLQ disabled the behavior is the old one: nothing is kept.
func TestNatsBus_DLQDisabledKeepsOldBehavior(t *testing.T) {
	nb, js := newDLQTestStream(t)
	nb.dlqEnabled = false

	handled := make(chan struct{}, 1)
	sub, err := nb.Subscribe("flux.msg.nodlq.test", func(ctx context.Context, m *fluxmsg.FluxMsg) error {
		handled <- struct{}{}
		return nil
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// The undecodable message, then a valid one on the same subject. Delivery
	// is in order, so once the valid one reaches the handler, the first has
	// been settled, and a routed copy would already be in the DLQ.
	require.NoError(t, nb.PublishRaw(context.Background(), "flux.msg.nodlq.test", []byte{0xff, 0xff}, uuid.New()))
	require.NoError(t, nb.Publish(context.Background(), "flux.msg.nodlq.test", fluxmsg.New()))
	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("the valid message after the undecodable one was not delivered")
	}

	stream, err := js.Stream(context.Background(), "flux-dlq")
	require.NoError(t, err)
	info, err := stream.Info(context.Background())
	require.NoError(t, err)
	assert.Zero(t, info.State.Msgs, "a disabled DLQ must keep nothing")
}

// The deadLetterSubject builder must not double dots or drop the origin.
func TestDeadLetterSubject(t *testing.T) {
	assert.Equal(t, "flux.dlq.flux.msg.rack-1.gear.out", deadLetterSubject("flux.dlq", "flux.msg.rack-1.gear.out"))
	assert.Equal(t, "flux.dlq.x", deadLetterSubject("flux.dlq.", "x"), "a trailing dot must not double")
	assert.Equal(t, "flux.dlq.x", deadLetterSubject("", "x"), "an empty prefix uses the default")
}

// stalledJetStream is a server that takes a dead letter and never answers.
// Only PublishMsg is implemented: it holds until the caller's context ends.
type stalledJetStream struct {
	jetstream.JetStream
}

func (stalledJetStream) PublishMsg(ctx context.Context, _ *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A dead letter publish used to run on context.Background(), so a server
// that never answered held the consumer for as long as the client library
// chose. It must give up after the configured timeout.
func TestDeadLetter_StalledServerReturnsWithinTimeout(t *testing.T) {
	nb := &NatsBus{
		js:                stalledJetStream{},
		dlqEnabled:        true,
		dlqSubject:        "flux.dlq",
		dlqPublishTimeout: 50 * time.Millisecond,
	}

	done := make(chan bool, 1)
	go func() {
		done <- nb.deadLetter(context.Background(), "flux.msg.stalled", []byte{0xff}, DLQReasonDecode, 1)
	}()

	select {
	case routed := <-done:
		assert.False(t, routed, "a dead letter the server never took is not routed")
	case <-time.After(10 * time.Second):
		t.Fatal("the dead letter publish did not return within its configured timeout")
	}
}

// The timeout bounds the publish, and so does the caller's context: a parent
// that ends first stops the publish without waiting for the timeout.
func TestRouteToDLQ_BoundedByTimeoutAndParent(t *testing.T) {
	run := func(ctx context.Context, timeout time.Duration) <-chan error {
		out := make(chan error, 1)
		go func() {
			out <- routeToDLQ(ctx, stalledJetStream{}, "flux.dlq", "flux.msg.x", []byte{0xff}, DLQReasonDecode, 1, timeout)
		}()
		return out
	}
	wait := func(ch <-chan error) error {
		select {
		case err := <-ch:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("routeToDLQ did not return")
			return nil
		}
	}

	require.ErrorIs(t, wait(run(context.Background(), 50*time.Millisecond)), context.DeadlineExceeded)

	parent, cancel := context.WithCancel(context.Background())
	ch := run(parent, time.Hour)
	cancel()
	require.ErrorIs(t, wait(ch), context.Canceled)
}
