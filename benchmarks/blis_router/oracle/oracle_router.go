// Oracle Adaptive Router — complete routing.go with oracle algorithm embedded.
//
// This is a standalone copy of inference-sim/sim/routing.go with the static-weight
// scoring block in WeightedScoring.Route() replaced by the oracle adaptive affinity
// decay algorithm. The oracle section is clearly marked with ORACLE-START / ORACLE-END
// comments.
//
// What the oracle does:
//   The default BLIS router uses fixed weights (prefix-affinity:3, queue-depth:2,
//   kv-utilization:2). During bursty traffic, prefix-affinity funnels all burst
//   requests to one cached instance, creating a hotspot. The oracle dynamically
//   decays the prefix-affinity weight based on InFlightRequests imbalance:
//
//     imbalance = (max_IFR - mean_IFR) / mean_IFR
//     affinityDecay = 1 / (1 + imbalance)^2
//     adjusted_prefix_weight = original_weight * affinityDecay
//
//   Balanced load -> full prefix affinity (cache benefits preserved).
//   High imbalance -> prefix affinity decays, queue-depth drives routing (spreads load).
//
// Results (5 seeds on workload_v1.yaml, 4 instances, 55 QPS, qwen_7b):
//   E2E mean: 36.3% improvement (6618ms -> 3895ms)
//   E2E P95:  21.2% improvement (13721ms -> 10658ms)
//
// See hypo-oracle.md for full analysis and findings.

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
	// Priority is a one-shot cluster-level priority hint applied before instance injection.
	// Zero (default) means defer to instance-level PriorityPolicy entirely.
	// Non-zero value sets req.Priority for initial queue ordering only -- the instance-level
	// PriorityPolicy recomputes priority each step, so this hint affects first-step scheduling
	// but does not persist. This is intentional: it allows priority to evolve over time
	// (e.g., SLOBasedPriority ages requests) while giving routing a way to influence initial placement.
	Priority float64
}

// NewRoutingDecision creates a RoutingDecision with the given target and reason.
// Scores is nil and Priority is 0.0 (defer to instance-level PriorityPolicy).
// This is the canonical constructor for policies that do not produce per-instance scores.
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
// Priority is 0.0 (defer to instance-level PriorityPolicy).
// Used by scoring-based routing policies (e.g., WeightedScoring).
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
// Implementations receive request and cluster-wide state via *RouterState.
type RoutingPolicy interface {
	Route(req *Request, state *RouterState) RoutingDecision
}

// RoundRobin routes requests in round-robin order across instances.
type RoundRobin struct {
	counter int
}

// Route implements RoutingPolicy for RoundRobin.
func (rr *RoundRobin) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("RoundRobin.Route: empty snapshots")
	}
	target := snapshots[rr.counter%len(snapshots)]
	rr.counter++
	return NewRoutingDecision(target.ID, fmt.Sprintf("round-robin[%d]", rr.counter-1))
}

// LeastLoaded routes requests to the instance with minimum (QueueDepth + BatchSize + InFlightRequests).
// InFlightRequests prevents pile-on at high request rates where multiple routing decisions
// occur at the same timestamp before instance events process (#175).
// Ties are broken randomly when rng is non-nil; by first occurrence (lowest index) when rng is nil.
type LeastLoaded struct {
	rng *rand.Rand
}

// Route implements RoutingPolicy for LeastLoaded.
func (ll *LeastLoaded) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("LeastLoaded.Route: empty snapshots")
	}

	// Pass 1: find minimum load
	minLoad := snapshots[0].EffectiveLoad()
	for i := 1; i < len(snapshots); i++ {
		if load := snapshots[i].EffectiveLoad(); load < minLoad {
			minLoad = load
		}
	}

	// Pass 2: collect all instances tied at minimum load
	var tied []int
	for i, snap := range snapshots {
		if snap.EffectiveLoad() == minLoad {
			tied = append(tied, i)
		}
	}

	// Random tie-breaking when rng is non-nil; positional (first) when nil.
	idx := tied[0]
	if len(tied) > 1 && ll.rng != nil {
		idx = tied[ll.rng.Intn(len(tied))]
	}

	return NewRoutingDecision(snapshots[idx].ID, fmt.Sprintf("least-loaded (load=%d)", minLoad))
}

// observerFunc is called after each routing decision to update stateful scorer state.
// Used by scorers like prefix-affinity that track routing history.
type observerFunc func(req *Request, targetInstance string)

// WeightedScoring routes requests using a composable scorer pipeline.
//
// Each scorer evaluates all instances on a [0,1] scale. Scores are combined
// with configurable weights: composite = sum clamp(s_i) * w_i, then argmax.
//
// Available scorers: prefix-affinity (proportional prefix match ratio),
// precise-prefix-cache (min-max normalization of actual KV cache hits),
// no-hit-lru (cold request distribution to least-recently-used instances),
// queue-depth (min-max normalization of EffectiveLoad),
// kv-utilization (1 - KVUtilization), load-balance (1/(1 + EffectiveLoad)).
// See sim/routing_*.go for scorer implementations.
//
// Stateful scorers (prefix-affinity, no-hit-lru) register observers that update internal
// state after each routing decision. Observers are called after argmax selection.
//
// Higher scores are preferred. Ties broken randomly when rng is non-nil;
// by first occurrence (lowest index) when rng is nil.
//
// ORACLE MODIFICATION: The Route() method below uses adaptive affinity decay
// instead of static weights. See the ORACLE-START / ORACLE-END block.
type WeightedScoring struct {
	scorers   []scorerFunc
	weights   []float64 // normalized to sum to 1.0
	observers []observerFunc
	rng       *rand.Rand
}

// Route implements RoutingPolicy for WeightedScoring.
//
// ORACLE VERSION: Uses adaptive affinity decay based on InFlightRequests imbalance
// instead of static weights. The original static-weight code is replaced between
// the ORACLE-START and ORACLE-END markers below.
func (ws *WeightedScoring) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("WeightedScoring.Route: empty snapshots")
	}

	// ========================================================================
	// ORACLE-START: Adaptive affinity decay based on InFlightRequests imbalance
	//
	// Replaces the default static-weight scoring block. Instead of applying
	// ws.weights directly, we compute an imbalance metric from InFlightRequests
	// and decay the prefix-affinity weight (scorer index 0) when load is skewed.
	//
	// Signal: InFlightRequests only (synchronous/fresh on every routing call).
	// Formula: affinityDecay = 1 / (1 + imbalance)^2
	//   - Balanced (imbalance ~0): decay = 1.0 -> full prefix affinity
	//   - Moderate imbalance (1.0): decay = 0.25 -> 75% reduction
	//   - High imbalance (3.0): decay = 0.0625 -> 94% reduction
	// ========================================================================

	// Step 1: Measure InFlightRequests imbalance across instances.
	maxIFR := 0
	totalIFR := 0
	for _, snap := range snapshots {
		ifr := snap.InFlightRequests
		totalIFR += ifr
		if ifr > maxIFR {
			maxIFR = ifr
		}
	}
	meanIFR := float64(totalIFR) / float64(len(snapshots))

	// Step 2: Compute imbalance ratio (0 when balanced, grows with skew).
	imbalance := 0.0
	if meanIFR > 0 {
		imbalance = (float64(maxIFR) - meanIFR) / meanIFR
	}

	// Step 3: Compute affinity decay — smooth, no thresholds, no tuning.
	denom := 1.0 + imbalance
	affinityDecay := 1.0 / (denom * denom)

	// Step 4: Adjust weights — scale prefix-affinity (scorer index 0), renormalize.
	adjustedWeights := make([]float64, len(ws.weights))
	copy(adjustedWeights, ws.weights)
	if len(adjustedWeights) > 0 {
		adjustedWeights[0] *= affinityDecay
	}
	weightSum := 0.0
	for _, w := range adjustedWeights {
		weightSum += w
	}
	if weightSum > 0 {
		for i := range adjustedWeights {
			adjustedWeights[i] /= weightSum
		}
	}

	// Step 5: Compute composite scores with adaptive weights.
	scores := make(map[string]float64, len(snapshots))
	for i, scorer := range ws.scorers {
		dimScores := scorer(req, snapshots)
		for _, snap := range snapshots {
			s := dimScores[snap.ID]
			if s < 0 {
				s = 0
			}
			if s > 1 {
				s = 1
			}
			scores[snap.ID] += s * adjustedWeights[i]
		}
	}

	// Step 6: Argmax with random tie-breaking (same as default).
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
	// ORACLE-END
	// ========================================================================

	// Notify observers of routing decision (stateful scorers update their state).
	// Uses post-tie-breaking bestIdx so prefix-affinity records the actual target.
	for _, obs := range ws.observers {
		obs(req, snapshots[bestIdx].ID)
	}

	return NewRoutingDecisionWithScores(
		snapshots[bestIdx].ID,
		fmt.Sprintf("weighted-scoring (score=%.3f)", bestScore),
		scores,
	)
}

// AlwaysBusiest routes requests to the instance with maximum (QueueDepth + BatchSize + InFlightRequests).
// Pathological template for testing load imbalance detection.
// Ties broken by first occurrence in snapshot order (lowest index).
type AlwaysBusiest struct{}

// Route implements RoutingPolicy for AlwaysBusiest.
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

// NewRoutingPolicy creates a routing policy by name.
// Valid names are defined in validRoutingPolicies (bundle.go).
// Empty string defaults to round-robin.
// For weighted scoring, scorerConfigs configures the scorer pipeline.
// If scorerConfigs is nil/empty for "weighted", DefaultScorerConfigs() is used.
// Non-weighted policies ignore scorerConfigs.
// The rng parameter enables random tie-breaking for least-loaded and weighted policies;
// nil preserves positional tie-breaking. Ignored by round-robin and always-busiest.
// Panics on unrecognized names.
func NewRoutingPolicy(name string, scorerConfigs []ScorerConfig, blockSize int64, rng *rand.Rand) RoutingPolicy {
	return newRoutingPolicyInternal(name, scorerConfigs, blockSize, rng, nil)
}

// NewRoutingPolicyWithCache is like NewRoutingPolicy but enables the precise-prefix-cache
// and no-hit-lru scorers. cacheFn maps instance ID to a function returning the count of
// consecutive cached prefix blocks for given tokens; pass nil to disable those scorers
// (equivalent to calling NewRoutingPolicy).
func NewRoutingPolicyWithCache(name string, scorerConfigs []ScorerConfig, blockSize int64, rng *rand.Rand, cacheFn map[string]func([]int) int) RoutingPolicy {
	return newRoutingPolicyInternal(name, scorerConfigs, blockSize, rng, cacheQueryFn(cacheFn))
}

// newRoutingPolicyInternal creates a routing policy, shared by both public constructors.
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
