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
	InFlightRequests int // Requests dispatched to this instance but not yet completed
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
	Scores         map[string]float64 // Instance ID → composite score (nil for policies without scoring)
	// Priority is a one-shot cluster-level priority hint applied before instance injection.
	// Zero (default) means defer to instance-level PriorityPolicy entirely.
	// Non-zero value sets req.Priority for initial queue ordering only — the instance-level
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
// with configurable weights: composite = Σ clamp(s_i) × w_i, then argmax.
//
// Available scorers: prefix-affinity (proportional prefix match ratio),
// queue-depth (min-max normalization of EffectiveLoad),
// kv-utilization (1 - KVUtilization), load-balance (1/(1 + EffectiveLoad)).
// See sim/routing_scorers.go and sim/routing_prefix_scorer.go for implementations.
//
// Stateful scorers (prefix-affinity) register observers that update internal
// state after each routing decision. Observers are called after argmax selection.
//
// Higher scores are preferred. Ties broken randomly when rng is non-nil;
// by first occurrence (lowest index) when rng is nil.
type WeightedScoring struct {
	scorers   []scorerFunc
	weights   []float64 // normalized to sum to 1.0
	observers []observerFunc
	rng       *rand.Rand
}

// Route implements RoutingPolicy for WeightedScoring.
func (ws *WeightedScoring) Route(req *Request, state *RouterState) RoutingDecision {
	snapshots := state.Snapshots
	if len(snapshots) == 0 {
		panic("WeightedScoring.Route: empty snapshots")
	}

	// EVOLVE-BLOCK-START

	// Step 1: compute all scorer dimensions
	allDimScores := make([]map[string]float64, len(ws.scorers))
	for i, scorer := range ws.scorers {
		allDimScores[i] = scorer(req, snapshots)
	}

	// Step 2: cluster load analysis (fresh signals only)
	minInflight := snapshots[0].InFlightRequests
	maxInflight := snapshots[0].InFlightRequests
	for _, snap := range snapshots {
		if snap.InFlightRequests < minInflight {
			minInflight = snap.InFlightRequests
		}
		if snap.InFlightRequests > maxInflight {
			maxInflight = snap.InFlightRequests
		}
	}

	// Step 3: find best prefix instance
	bestPrefixID := ""
	bestPrefixScore := -1.0
	bestPrefixInflight := 0
	if len(allDimScores) > 0 {
		for _, snap := range snapshots {
			if ps := allDimScores[0][snap.ID]; ps > bestPrefixScore {
				bestPrefixScore = ps
				bestPrefixID = snap.ID
				bestPrefixInflight = snap.InFlightRequests
			}
		}
	}

	// Step 4: Two-mode routing — CALM vs BURST
	//
	// Key insight: prefix affinity creates a "rich get richer" dynamic.
	// During calm periods this is great (cache hits save 100s of ms).
	// During bursts it's catastrophic (all burst traffic piles on one instance).
	//
	// Burst detection: the cached instance has >1.5x the load of the lightest.
	// This is a strong signal that a burst is hitting the cached instance.
	//
	// During CALM: boost prefix weight ABOVE the baseline 0.5 → 0.70.
	// This gives better cache utilization than 1:1, reducing per-request latency.
	//
	// During BURST: hard switch to load-aware routing with only 0.1 prefix weight.
	// The fast, decisive switch prevents burst damage from accumulating.
	aw := make([]float64, len(ws.weights))
	copy(aw, ws.weights)

	if len(ws.weights) >= 2 && bestPrefixID != "" && bestPrefixScore > 0.05 {
		ratio := float64(bestPrefixInflight+1) / float64(minInflight+1)

		if ratio > 1.5 {
			// BURST MODE: cached instance is heavily overloaded.
			// Near-complete switch to load-balance.
			burstIntensity := (ratio - 1.5) / 2.0 // 0→1 as ratio goes 1.5x→3.5x
			if burstIntensity > 1.0 {
				burstIntensity = 1.0
			}
			// Prefix weight decays from 0.5 to 0.05 as burst intensifies
			aw[0] = ws.weights[0] * (0.1 + 0.9*(1.0-burstIntensity))
			aw[1] = 1.0 - aw[0]
		} else {
			// CALM MODE: boost prefix affinity above baseline.
			// 1:1 uses 0.5/0.5. We use 0.70/0.30 to get stronger cache hits.
			aw[0] = 0.70
			aw[1] = 0.30
		}
	}

	// Step 5: composite scores
	scores := make(map[string]float64, len(snapshots))
	for i := range ws.scorers {
		for _, snap := range snapshots {
			s := allDimScores[i][snap.ID]
			if s < 0 {
				s = 0
			}
			if s > 1 {
				s = 1
			}
			scores[snap.ID] += s * aw[i]
		}
	}

	// Step 6: KV pressure penalty (graduated, starts at 75%)
	for _, snap := range snapshots {
		if snap.KVUtilization > 0.75 {
			pressure := (snap.KVUtilization - 0.75) / 0.25
			scores[snap.ID] -= 0.35 * pressure * pressure
		}
	}

	// Step 7: fresh load tiebreaker
	for _, snap := range snapshots {
		scores[snap.ID] += 0.02 / (1.0 + float64(snap.InFlightRequests))
	}

	// Step 8: argmax with tie-breaking
	bestScore := -1e9
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
	// EVOLVE-BLOCK-END

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
			scorer, obs := newScorerWithObserver(cfg.Name, int(blockSize))
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
