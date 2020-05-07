package policy

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad-autoscaler/plugins"
	"github.com/hashicorp/nomad-autoscaler/plugins/manager"
	targetpkg "github.com/hashicorp/nomad-autoscaler/plugins/target"
)

// Handler monitors a policy for changes and controls when them are sent for
// evaluation.
type Handler struct {
	log hclog.Logger

	// policyID is the ID of the policy the handler is responsible for.
	policyID PolicyID

	// pluginManager is used to retrieve an instance of the target plugin used
	// by the policy.
	pluginManager *manager.PluginManager

	// policySource is used to monitor for changes to the policy the handler
	// is responsible for.
	policySource Source

	// ticker controls the frequency the policy is sent for evaluation.
	ticker *time.Ticker

	// cooldownCh is used to notify the handler that it should enter a cooldown
	// period.
	cooldownCh chan time.Duration

	// running is used to help keep track if the handler is active or not.
	running     bool
	runningLock sync.RWMutex

	// ch is used to listen for policy updates.
	ch chan Policy

	// errCh is used to listen for errors from the policy source.
	errCh chan error

	// doneCh is used to signal the handler to stop.
	doneCh chan struct{}
}

// NewHandler returns a new handler for a policy.
func NewHandler(ID PolicyID, log hclog.Logger, pm *manager.PluginManager, ps Source) *Handler {
	return &Handler{
		policyID:      ID,
		log:           log.Named("policy_handler").With("policy_id", ID),
		pluginManager: pm,
		policySource:  ps,
		ch:            make(chan Policy),
		errCh:         make(chan error),
		doneCh:        make(chan struct{}),
		cooldownCh:    make(chan time.Duration),
	}
}

// Run starts the handler and periodically sends the policy for evaluation.
//
// This function blocks until the context provided is canceled or the handler
// is stopped with the Stop() method.
func (h *Handler) Run(ctx context.Context, evalCh chan<- *Evaluation) {
	h.log.Trace("starting policy handler")

	defer h.Stop()

	// Mark the handler as running.
	h.runningLock.Lock()
	h.running = true
	h.runningLock.Unlock()

	// Store a local copy of the policy so we can compare it for changes.
	var currentPolicy *Policy

	// Start with a long ticker until we receive the right interval.
	// TODO(luiz): make this a config param
	policyReadTimeout := 3 * time.Minute
	h.ticker = time.NewTicker(policyReadTimeout)

	// Create separate context so we can stop the monitoring Go routine if
	// doneCh is closed, but ctx is still valid.
	monitorCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start monitoring the policy for changes.
	go h.policySource.MonitorPolicy(monitorCtx, h.policyID, h.ch, h.errCh)

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.doneCh:
			return
		case err := <-h.errCh:
			merr, ok := err.(*multierror.Error)
			if ok && len(merr.Errors) > 1 {
				// Transform Errors into a slice of strings to avoid logging
				// empty objects when using JSON format.
				errors := make([]string, len(merr.Errors))
				for i, e := range merr.Errors {
					errors[i] = e.Error()
				}
				h.log.Error(errors[0], "errors", errors[1:])
				return
			}

			h.log.Error(err.Error())
			return
		case p := <-h.ch:
			h.updateHandler(currentPolicy, &p)
			currentPolicy = &p
		case <-h.ticker.C:

			// Timestamp the invocation of this evaluation run. This can be
			// used when checking cooldown or emitting metrics to ensure some
			// consistency.
			curTime := time.Now().UTC().UnixNano()

			eval, err := h.generateEvaluation(currentPolicy)
			if err != nil {
				h.log.Error(err.Error())
				return
			}

			// If the evaluation is nil there is nothing to be done this time
			// around.
			if eval == nil {
				continue
			}

			// If the target status includes a last event meta key, check for cooldown
			// due to out-of-band events. This is also useful if the Autoscaler has
			// been re-deployed.
			if ts, ok := eval.TargetStatus.Meta[targetpkg.MetaKeyLastEvent]; ok {

				// Convert the last event string. If an error occurs, just log and
				// continue with the evaluation. A malformed timestamp shouldn't mean
				// we skip scaling.
				lastTS, err := strconv.ParseUint(ts, 10, 64)
				if err != nil {
					h.log.Error("failed to parse last event timestamp as uint64", "error", err)
				} else {

					// If the handler should be placed into cooldown due to scaling
					// outside of the Autoscaler process, enforce.
					if h.isInCooldown(currentPolicy.Cooldown, curTime, lastTS) {

						cdPeriod := h.calculateRemainingCooldown(currentPolicy.Cooldown, curTime, int64(lastTS))

						// Enforce the cooldown which will block until complete.
						if !h.enforceCooldown(ctx, cdPeriod) {
							return
						}

						// After the cooldown, the evaluation data is
						// potentially stale. Therefore continue and allow a
						// new tick to occur.
						continue
					}
				}
			}

			// If we got this far, the evaluation can be sent to the channel
			// for processing.
			evalCh <- eval

		case ts := <-h.cooldownCh:

			// Enforce the cooldown which will block until complete.
			if !h.enforceCooldown(ctx, ts) {
				return
			}
		}
	}
}

// Stop stops the handler and the monitoring Go routine.
func (h *Handler) Stop() {
	h.runningLock.Lock()
	defer h.runningLock.Unlock()

	if h.running {
		h.log.Trace("stopping handler")
		h.ticker.Stop()
		close(h.doneCh)
	}

	h.running = false
}

// generateEvaluation returns an evaluation if the policy needs to be evaluated.
// Returning an error will stop the handler.
func (h *Handler) generateEvaluation(policy *Policy) (*Evaluation, error) {
	h.log.Trace("tick")

	if policy == nil {
		// Initial ticker ticked without a policy being set, assume we are not able
		// to retrieve the policy and exit.
		return nil, fmt.Errorf("timeout: failed to read policy in time")
	}

	// Exit early if the policy is not enabled.
	if !policy.Enabled {
		h.log.Debug("policy is not enabled")
		return nil, nil
	}

	// Dispense an instance of target plugin used by the policy.
	targetPlugin, err := h.pluginManager.Dispense(policy.Target.Name, plugins.PluginTypeTarget)
	if err != nil {
		return nil, err
	}

	targetInst, ok := targetPlugin.Plugin().(targetpkg.Target)
	if !ok {
		err := fmt.Errorf("plugin %s (%T) is not a target plugin", policy.Target.Name, targetPlugin.Plugin())
		return nil, err
	}

	// Get target status.
	h.log.Trace("getting target status")

	status, err := targetInst.Status(policy.Target.Config)
	if err != nil {
		h.log.Warn("failed to get target status", "error", err)
		return nil, nil
	}

	// A nil status indicates the target doesn't exist, so we don't need to
	// monitor the policy anymore.
	if status == nil {
		h.log.Trace("target doesn't exist anymore", "target", policy.Target.Config)
		h.Stop()
		return nil, nil
	}

	// Exit early if the target is not ready yet.
	if !status.Ready {
		h.log.Trace("target is not ready")
		return nil, nil
	}

	// Send policy for evaluation.
	h.log.Trace("sending policy for evaluation")
	return &Evaluation{
		Policy:       policy,
		TargetStatus: status,
	}, nil
}

// updateHandler updates the handler's internal state based on the changes in
// the policy being monitored.
func (h *Handler) updateHandler(current, next *Policy) {
	if current == nil {
		h.log.Trace("received policy")
	} else {
		h.log.Trace("received policy change")
		h.log.Trace(cmp.Diff(current, next))
	}

	// Update ticker if it's the first time we receive the policy or if the
	// policy's evaluation interval has changed.
	if current == nil || current.EvaluationInterval != next.EvaluationInterval {
		h.ticker.Stop()
		h.ticker = time.NewTicker(next.EvaluationInterval)
	}
}

// enforceCooldown blocks until the cooldown period has been reached, or the
// handler has been instructed to exit. The boolean return details whether or
// not the cooldown period passed without being interrupted.
func (h *Handler) enforceCooldown(ctx context.Context, t time.Duration) bool {

	// Log that cooldown is being enforced. This is very useful as cooldown
	// blocks the ticker making this the only indication of cooldown to
	// operators.
	h.log.Debug("scaling policy has been placed into cooldown", "cooldown", t)

	// Using a timer directly is mentioned to be more efficient than
	// time.After() as long as we ensure to call Stop(). So setup a timer for
	// use and defer the stop.
	timer := time.NewTimer(t)
	defer timer.Stop()

	// Cooldown should not mean we miss other handler control signals. So wait
	// on all the channels desired here.
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-h.doneCh:
		return false

	}
}

// isInCooldown is used to check whether the handler should be in cooldown or
// not. This is used when performing a policy evaluation and allows the
// autoscaler to enforce a cooldown on objects that have been scaled outside of
// the autoscaler.
func (h *Handler) isInCooldown(cd time.Duration, ts int64, lastEvent uint64) bool {
	return int64(lastEvent)+cd.Nanoseconds() >= ts
}

// calculateRemainingCooldown calculates the remaining cooldown based on the
// time since the last event.
func (h *Handler) calculateRemainingCooldown(cd time.Duration, ts, lastEvent int64) time.Duration {
	return cd - time.Duration(ts-lastEvent)
}
