// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jaab-tech/fluxrig/pkg/controller"
	"github.com/jaab-tech/fluxrig/pkg/fluxmsg"
	"github.com/jaab-tech/fluxrig/pkg/idgen"
	"github.com/jaab-tech/fluxrig/pkg/registry"
	"github.com/jaab-tech/fluxrig/pkg/registry/kvstore"
)

// pushRecorder is a scenario bus that records the subject of every push.
type pushRecorder struct {
	mu       sync.Mutex
	subjects []string
}

func (b *pushRecorder) Publish(_ context.Context, subject string, _ *fluxmsg.FluxMsg) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subjects = append(b.subjects, subject)
	return nil
}

func (b *pushRecorder) pushesTo(rack string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.subjects {
		if s == fluxmsg.SubjectScenarioPrefix+rack+fluxmsg.SubjectScenarioSuffix {
			n++
		}
	}
	return n
}

// approvalFixture is an API server over an in-memory registry with one
// pending Rack called rack-pending, and a scenario controller that has
// activated a scenario with a global gear. The activation reached no Rack:
// the only Rack is pending.
func approvalFixture(t *testing.T) (*Server, uuid.UUID, *pushRecorder) {
	t.Helper()
	ctx := context.Background()
	mixerID := uuid.New()
	reg := kvstore.New(slog.Default(), kvstore.NewMemoryBucket(0), kvstore.NewMemoryBucket(90*time.Second), nil, mixerID,
		kvstore.Config{MaxValueSize: 1 << 20, WriteRetries: 3})
	reg.SetBootstrapSecret("sec")
	rack, err := reg.Register(ctx, uuid.New(), "rack-pending", "sec", "ip", 80, "v1", nil, mixerID)
	require.NoError(t, err)
	require.Equal(t, "pending", rack.Status, "auto-adopt is off: the Rack must start pending")

	gen, err := idgen.New(mixerID)
	require.NoError(t, err)
	sc := controller.NewScenarioController(slog.Default(), nil, reg, reg, gen, mixerID, 200*time.Millisecond)
	bus := &pushRecorder{}
	sc.SetBus(bus)

	name, err := sc.Import(ctx, []byte(`
meta:
  name: global-only
  version: "1.0.0"
gears:
  - name: "everywhere"
    type: "io_tcp"
`), false)
	require.NoError(t, err)
	require.NoError(t, sc.Activate(ctx, name))
	require.Zero(t, bus.pushesTo("rack-pending"), "a pending Rack gets nothing before it is approved")

	s := NewServer(reg, nil, nil, sc, nil, mixerID, uuid.Nil, nil, nil, nil)
	return s, rack.MachineID, bus
}

func approve(t *testing.T, s *Server, id uuid.UUID, name string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/racks/"+id.String()+"/approve", strings.NewReader(`{"name":"`+name+`"}`))
	req.SetPathValue("id", id.String())
	req.SetPathValue("action", "approve")
	w := httptest.NewRecorder()
	s.handleRackAction(w, req)
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
}

// A Rack that was pending when a scenario with a global gear was activated
// gets that scenario when an operator approves it. Before, nothing pushed it,
// and the Rack ran no global gear until something else triggered a push.
func TestApprovePushesTheActiveScenarioToTheRack(t *testing.T) {
	s, id, bus := approvalFixture(t)

	approve(t, s, id, "rack-pending")
	assert.Equal(t, 1, bus.pushesTo("rack-pending"), "the approved Rack did not get the active scenario")

	// Approving a Rack that is already active changes nothing it runs: a
	// second push would only restart its gears.
	approve(t, s, id, "rack-pending")
	assert.Equal(t, 1, bus.pushesTo("rack-pending"), "approving an active Rack pushed the scenario again")
}

// An approval that renames the Rack pushes to the name it was approved
// under, which is the name it subscribes with from then on.
func TestApprovePushesUnderTheApprovedName(t *testing.T) {
	s, id, bus := approvalFixture(t)

	approve(t, s, id, "edge-1")
	assert.Equal(t, 1, bus.pushesTo("edge-1"))
	assert.Zero(t, bus.pushesTo("rack-pending"))
}

// pendingRackServer is a Server over a real in-memory registry holding one
// pending Rack, with sc as its scenario controller.
func pendingRackServer(t *testing.T, sc controller.ScenarioManager) (*Server, uuid.UUID, registry.Registry) {
	t.Helper()
	mixerID := uuid.New()
	reg := kvstore.New(slog.Default(), kvstore.NewMemoryBucket(0), kvstore.NewMemoryBucket(90*time.Second), nil, mixerID,
		kvstore.Config{MaxValueSize: 1 << 20, WriteRetries: 3})
	reg.SetBootstrapSecret("sec")
	rack, err := reg.Register(context.Background(), uuid.New(), "rack-pending", "sec", "ip", 80, "v1", nil, mixerID)
	require.NoError(t, err)
	return NewServer(reg, nil, nil, sc, nil, mixerID, uuid.Nil, nil, nil, nil), rack.MachineID, reg
}

// The push does not end with the request. A client that disconnects right
// after it asks for the approval must not cancel the push to the Rack.
func TestApprovePushOutlivesACancelledRequest(t *testing.T) {
	var pushCtxErr error
	pushed := false
	sc := &MockScenarioController{PushActiveToRackSinceFunc: func(ctx context.Context, _ string, _ time.Time) error {
		pushed = true
		pushCtxErr = ctx.Err()
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline, "the push must be bounded by api.approval_push_timeout")
		return nil
	}}
	s, id, _ := pendingRackServer(t, sc)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/v1/racks/"+id.String()+"/approve", strings.NewReader(`{"name":"rack-pending"}`)).WithContext(reqCtx)
	req.SetPathValue("id", id.String())
	req.SetPathValue("action", "approve")
	w := httptest.NewRecorder()
	s.handleRackAction(w, req)

	require.True(t, pushed, "the approval did not push")
	assert.NoError(t, pushCtxErr, "the request's cancellation reached the push")
}

// A push that fails does not undo the approval: the Rack is active and the
// operator gets the approval's answer.
func TestApproveSucceedsWhenThePushFails(t *testing.T) {
	sc := &MockScenarioController{PushActiveToRackSinceFunc: func(context.Context, string, time.Time) error {
		return errors.New("bus unavailable")
	}}
	s, id, reg := pendingRackServer(t, sc)

	approve(t, s, id, "rack-pending")
	rack, err := reg.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "active", rack.Status)
}
