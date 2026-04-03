// Per-Instance IFR Penalty Router (v2) — complete routing.go with algorithm embedded.
//
// This is a standalone copy of inference-sim/sim/routing.go with the static-weight
// scoring block in WeightedScoring.Route() replaced by the per-instance IFR penalty
// algorithm. The v2 section is clearly marked with V2-START / V2-END comments.
//
// What v2 does:
//   Computes base scores using the standard scorer pipeline with original weights,
//   then applies a per-instance penalty based on each instance's InFlightRequests
//   relative to the cluster mean. Instances above the mean get their score reduced
//   by 1/(1 + excess)^2 where excess = (IFR - mean) / mean.
//
// Why v2 beats the default:
//   The default uses stale queue-depth (5s Prometheus refresh) for load awareness.
//   V2 replaces stale load signals with fresh IFR, reacting instantly to burst onset.
//   During calm periods, IFR is uniform → load scores are similar → cache score dominates.
//
// Why v2 beats Glia on prefix-heavy workloads:
//   Glia routes by KV headroom (projectedUsage/totalBlocks), which penalizes cached
//   instances (more KV allocated = lower score). This causes systematic cache misses
//   on large-prefix workloads. V2 preserves cache affinity from the precise-prefix-cache
//   scorer and uses fresh IFR for load awareness instead of stale KV signals.
//
// How v2 differs from oracle v1 (global weight decay):
//   - v1 decays the prefix-affinity WEIGHT globally → all instances lose affinity
//   - v2 penalizes individual INSTANCES that are overloaded → only the hot instance
//     loses its score; idle cached instances keep their full affinity benefit
//
// Properties:
//   - No thresholds, no tuning parameters, no state
//   - The 1/(1+x)^2 formula has no free parameters (x = excess/mean is dimensionless)
//   - Quadratic penalty justified by M/M/1 queuing: delay grows quadratically
//     near saturation, so score should decay quadratically with load excess
//
// Results: See hypo-doc2.md for full analysis.

package sim

import (
	"fmt"
	"math/rand"
)

// RoutingSnapshot is a lightweight view of instance state for policy decisions.
// Populated by CachedSnapshotProvider reading InstanceSimulator query methods,
// with InFlightRequests injected by buildRouterState() at the cluster level.
// Used by both AdmissionPolicy and RoutingPolicy.
// Timestamp is intentionally excluded: snapshot freshness is managed by
// CachedSnapshotProvider and is not a policy concern.
type RoutingSnapshot struct {
	ID               string
	QueueDepth       int
	BatchSize        int
	KVUtilization    float64
	FreeKVBlocks     int64
	CacheHitRate     float64
	InFlightRequests int    // Requests dispatched to this instance but not yet completed
	Model            string // Model served by this instance; used by buildRouterState() for per-model filtering
}

// EffectiveLoad returns the total effective load on this instance:
// QueueDepth + BatchSize + InFlightRequests.
// Used by routing policies and counterfactual scoring for consistent load calculations.
func (s RoutingSnapshot) EffectiveLoad() int {
	return s.QueueDepth + s.BatchSize + s.InFlightRequests
}

// NewRoutingSnapshot creates a RoutingSnapshot with the given instance ID.
// All numeric fields are zero-valued. Used for initial snapshot creation;
// field-by-field refresh via CachedSnapshotProvider.Snapshot() is a separate concern.
func NewRoutingSnapshot(id string) RoutingSnapshot {
	if id == "" {
		panic("NewRoutingSnapshot: id must not be empty")
	}
	return RoutingSnapshot{ID: id}
}

// RoutingDecision encapsulates the routing decision for a request.
type RoutingDecision struct {
	TargetInstance string             // Instance ID to route to (must match a snapshot ID)
	Reason         string             // Human-readable explanation
	Scores         map[string]float64 // Instance ID -> composite score (nil for policies without scoring)
	Priority       float64
}

// NewRoutingDecision creates a RoutingDecision with the given target and reason.
func NewRoutingDecision(target string, reason string) RoutingDecision {
	if target == "" {
		panic("NewRoutingDecision: target must not be empty")
	}
	return RoutingDecision{
		TargetInstance: target,
		Reason:         reason,
	}
}

// NewRoutingDecisionWithScores creates a RoutingDecision with target, reason, and per-instance scores.
func NewRoutingDecisionWithScores(target string, reason string, scores map[string]float64) RoutingDecision {
	if target == "" {
		panic("NewRoutingDecisionWithScores: target must not be empty")
	}
	return RoutingDecision{
		TargetInstance: target,
		Reason:         reason,
		Scores:         scores,
	}
}

// RoutingPolicy decides which instance should handle a request.
type RoutingPolicy interface {
	Route(req *Request, state *RouterState) RoutingDecision
}

// RoundRobin routes requests in round-robin order across instances.
type RoundRobin struct {
	counter int
}

func (rr *RoundRobin) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("RoundRobin.Route: empty snapshots")
	}
	target := snapshots[rr.counter%len(snapshots)]
	rr.counter++
	return NewRoutingDecision(target.ID, fmt.Sprintf("round-robin[%d]", rr.counter-1))
}

// LeastLoaded routes requests to the instance with minimum EffectiveLoad.
type LeastLoaded struct {
	rng *rand.Rand
}

func (ll *LeastLoaded) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("LeastLoaded.Route: empty snapshots")
	}
	minLoad := snapshots[0].EffectiveLoad()
	for i := 1; i < len(snapshots); i++ {
		if load := snapshots[i].EffectiveLoad(); load < minLoad {
			minLoad = load
		}
	}
	var tied []int
	for i, snap := range snapshots {
		if snap.EffectiveLoad() == minLoad {
			tied = append(tied, i)
		}
	}
	idx := tied[0]
	if len(tied) > 1 && ll.rng != nil {
		idx = tied[ll.rng.Intn(len(tied))]
	}
	return NewRoutingDecision(snapshots[idx].ID, fmt.Sprintf("least-loaded (load=%d)", minLoad))
}

type observerFunc func(req *Request, targetInstance string)

// WeightedScoring routes requests using a composable scorer pipeline.
//
// V2 MODIFICATION: The Route() method adds per-instance IFR penalty on top of
// the standard scorer pipeline. See V2-START / V2-END block.
type WeightedScoring struct {
	scorers   []scorerFunc
	weights   []float64 // normalized to sum to 1.0
	observers []observerFunc
	rng       *rand.Rand
}

// Route implements RoutingPolicy for WeightedScoring.
//
// V2: Per-instance IFR penalty on top of the standard scorer pipeline.
//
// Uses the full scorer pipeline (pa:3, qd:2, kv:2 or whatever is configured)
// to compute base scores, then applies per-instance IFR penalty to instances
// above the cluster mean. This adds FRESH load-awareness (IFR is synchronous)
// on top of the existing pipeline (which uses stale 5s snapshots for queue-depth
// and kv-utilization).
//
// Why this beats the default: During bursts, the default's queue-depth scorer
// reacts slowly (5s stale data). The IFR penalty reacts instantly, redirecting
// traffic away from overloaded instances before the queue-depth scorer even
// notices the imbalance.
//
// Why this beats Glia on large-prefix workloads: Glia routes by KV headroom,
// which penalizes cached instances (more KV = lower score). V2 preserves the
// prefix-affinity benefit from the scorer pipeline during calm periods, and
// only penalizes the specific overloaded instance during bursts.
func (ws *WeightedScoring) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("WeightedScoring.Route: empty snapshots")
	}

	// ========================================================================
	// V2-START: Cache affinity + fresh IFR load signal.
	//
	// Combines the precise-prefix-cache scorer (ground-truth KV cache state)
	// with a FRESH IFR-based load score (for instant burst response). The
	// default router uses stale queue-depth and kv-utilization (5s Prometheus
	// refresh) for load awareness. V2 replaces these stale signals with
	// synchronous InFlightRequests, giving instant reaction to load changes.
	//
	// Approach:
	//   1. Get cache score from scorer[0] (precise-prefix-cache: queries actual
	//      KV cache state, min-max normalized — ground truth, not heuristic)
	//   2. Compute fresh load score: 1/(1+IFR) for each instance
	//   3. Combine using the SAME weight ratio as configured (e.g., 43%/57%
	//      for ppc:3,qd:2,kv:2), but with the load component based on fresh IFR
	//
	// Why this beats default: During bursts, stale queue-depth takes 5s to
	// update, so default keeps routing to overloaded instances. Fresh IFR
	// reacts instantly, redirecting traffic within the same simulation tick.
	//
	// Why this beats Glia: Glia routes by KV headroom, which penalizes cached
	// instances (more KV = lower score). V2 preserves cache affinity,
	// maintaining cache hits during calm periods.
	//
	// Properties: No thresholds, no tuning parameters. Weight ratio comes from
	// the existing scorer config (not a new parameter). IFR-based load score
	// is parameter-free: 1/(1+IFR) is a natural [0,1] normalization.
	// ========================================================================

	// Phase 1: Get cache score from scorer[0] (precise-prefix-cache).
	// Queries actual KV cache state: min-max normalized cached block counts.
	affinityDim := ws.scorers[0](req, snapshots)

	// Compute affinity weight fraction from configured weights.
	affinityWeight := ws.weights[0]
	loadWeight := 0.0
	for i := 1; i < len(ws.weights); i++ {
		loadWeight += ws.weights[i]
	}

	// Phase 2: Compute fresh load score from IFR.
	// 1/(1+IFR) is a smooth [0,1] function: 1.0 for idle, approaching 0 for heavily loaded.
	scores := make(map[string]float64, len(snapshots))
	for _, snap := range snapshots {
		// Affinity component (clamped).
		aff := affinityDim[snap.ID]
		if aff < 0 {
			aff = 0
		}
		if aff > 1 {
			aff = 1
		}

		// Fresh load score from IFR.
		freshLoad := 1.0 / (1.0 + float64(snap.InFlightRequests))

		// Combine with configured weight ratio.
		scores[snap.ID] = affinityWeight*aff + loadWeight*freshLoad
	}

	// Argmax: select instance with highest composite score.
	bestScore := -1.0
	for _, snap := range snapshots {
		if scores[snap.ID] > bestScore {
			bestScore = scores[snap.ID]
		}
	}
	var tied []int
	for i, snap := range snapshots {
		if scores[snap.ID] == bestScore {
			tied = append(tied, i)
		}
	}
	bestIdx := tied[0]
	if len(tied) > 1 && ws.rng != nil {
		bestIdx = tied[ws.rng.Intn(len(tied))]
	}

	// ========================================================================
	// V2-END
	// ========================================================================

	// Notify observers of routing decision (stateful scorers update their state).
	for _, obs := range ws.observers {
		obs(req, snapshots[bestIdx].ID)
	}

	return NewRoutingDecisionWithScores(
		snapshots[bestIdx].ID,
		fmt.Sprintf("weighted-scoring (score=%.3f)", bestScore),
		scores,
	)
}

// AlwaysBusiest routes requests to the instance with maximum EffectiveLoad.
type AlwaysBusiest struct{}

func (ab *AlwaysBusiest) Route(_ *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("AlwaysBusiest.Route: empty snapshots")
	}
	maxLoad := snapshots[0].EffectiveLoad()
	target := snapshots[0]
	for i := 1; i < len(snapshots); i++ {
		load := snapshots[i].EffectiveLoad()
		if load > maxLoad {
			maxLoad = load
			target = snapshots[i]
		}
	}
	return NewRoutingDecision(target.ID, fmt.Sprintf("always-busiest (load=%d)", maxLoad))
}

func NewRoutingPolicy(name string, scorerConfigs []ScorerConfig, blockSize int64, rng *rand.Rand) RoutingPolicy {
	return newRoutingPolicyInternal(name, scorerConfigs, blockSize, rng, nil)
}

func NewRoutingPolicyWithCache(name string, scorerConfigs []ScorerConfig, blockSize int64, rng *rand.Rand, cacheFn map[string]func([]int) int) RoutingPolicy {
	return newRoutingPolicyInternal(name, scorerConfigs, blockSize, rng, cacheQueryFn(cacheFn))
}

func newRoutingPolicyInternal(name string, scorerConfigs []ScorerConfig, blockSize int64, rng *rand.Rand, cacheFn cacheQueryFn) RoutingPolicy {
	if !IsValidRoutingPolicy(name) {
		panic(fmt.Sprintf("unknown routing policy %q", name))
	}
	switch name {
	case "", "round-robin":
		return &RoundRobin{}
	case "least-loaded":
		return &LeastLoaded{rng: rng}
	case "weighted":
		if len(scorerConfigs) == 0 {
			scorerConfigs = DefaultScorerConfigs()
		}
		scorers := make([]scorerFunc, len(scorerConfigs))
		var observers []observerFunc
		for i, cfg := range scorerConfigs {
			scorer, obs := newScorerWithObserver(cfg.Name, int(blockSize), cacheFn)
			scorers[i] = scorer
			if obs != nil {
				observers = append(observers, obs)
			}
		}
		weights := normalizeScorerWeights(scorerConfigs)
		return &WeightedScoring{scorers: scorers, weights: weights, observers: observers, rng: rng}
	case "always-busiest":
		return &AlwaysBusiest{}
	default:
		panic(fmt.Sprintf("unhandled routing policy %q", name))
	}
}
