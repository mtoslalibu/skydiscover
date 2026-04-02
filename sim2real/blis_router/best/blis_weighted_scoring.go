// FILE: llm-d-inference-scheduler/pkg/plugins/scorer/blis_weighted_scoring.go
package scorer

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/scheduling"
)

const (
	// BlisWeightedScoringType is the unique type name for this scorer.
	BlisWeightedScoringType = "blis-weighted-scoring-scorer"
)

// Compile-time type assertion.
var _ scheduling.Scorer = &BlisWeightedScoring{}

// BlisWeightedScoring implements the evolved BLIS weighted routing algorithm.
//
// The EVOLVE-BLOCK describes a composite weighted-scorer router that combines
// two sub-scorers (prefix-affinity and load-balance) using adaptive weights.
//
// Since prefix affinity is not available in the production metric set,
// bestPrefixScore = 0.0, which means the EVOLVE-BLOCK's condition
// (bestPrefixScore > 0.1) is NEVER true, so the adaptive weight decay
// block is NEVER entered. The weights remain at their base values:
//   - aw[0] = prefixWeight (0.6) for prefix-affinity (always contributes 0)
//   - aw[1] = loadWeight (0.4) for load-balance
//
// For each endpoint the algorithm computes:
//  1. Load-balance score: 1.0 / (1.0 + float64(inFlightRequests))
//  2. Prefix-affinity score: 0.0 (not available)
//  3. combined = aw[0] * 0.0 + aw[1] * loadScore = loadWeight * loadScore
//  4. Clamp each sub-score to [0, 1] before weighting (per EVOLVE-BLOCK)
//  5. Apply KV pressure penalty: if KVUtilization > 0.9, subtract
//     kvPenaltyCoefficient * (kvUtil - 0.9) / 0.1 from the combined score.
//  6. Add tiebreaker bonus: tiebreakerCoefficient / (1.0 + float64(inFlightRequests)).
//  7. Do NOT clamp final score — the EVOLVE-BLOCK allows negative scores.
//
// Signal mapping (from algorithm_summary.json and signal coverage):
//   - InFlightRequests → endpoint.GetMetrics().RunningRequestsSize
//   - KVUtilization → endpoint.GetMetrics().KVCacheUsagePercent / 100.0
type BlisWeightedScoring struct {
	typedName plugin.TypedName
	enabled   bool

	// kvPressureThreshold is the KV utilization level above which the
	// pressure penalty is applied. The EVOLVE-BLOCK hardcodes 0.9.
	kvPressureThreshold float64

	// kvPenaltyCoefficient is the multiplier for the KV pressure penalty (0.5).
	kvPenaltyCoefficient float64

	// kvPenaltyDenominator is the fixed denominator for the KV penalty.
	// The EVOLVE-BLOCK hardcodes 0.1.
	kvPenaltyDenominator float64

	// tiebreakerCoefficient is the small bonus for tie-breaking (0.01).
	tiebreakerCoefficient float64

	// prefixWeight is the base weight for the prefix-affinity sub-scorer (0.6).
	// Since prefix affinity is unavailable (score=0), this weight's contribution
	// is always zero. Kept for algorithm fidelity.
	prefixWeight float64

	// loadWeight is the base weight for the load-balance sub-scorer (0.4).
	loadWeight float64

	// decayCoefficient is the coefficient in the adaptive decay formula (0.6).
	// Since the decay branch is never entered (bestPrefixScore=0 fails >0.1 check),
	// this parameter has no effect. Kept for algorithm fidelity.
	decayCoefficient float64
}

// BlisWeightedScoringParameters holds the configurable parameters for the scorer.
type BlisWeightedScoringParameters struct {
	KVPressureThreshold   float64 `json:"kvPressureThreshold"`
	KVPenaltyCoefficient  float64 `json:"kvPenaltyCoefficient"`
	KVPenaltyDenominator  float64 `json:"kvPenaltyDenominator"`
	TiebreakerCoefficient float64 `json:"tiebreakerCoefficient"`
	PrefixWeight          float64 `json:"prefixWeight"`
	LoadWeight            float64 `json:"loadWeight"`
	DecayCoefficient      float64 `json:"decayCoefficient"`
	Enabled               bool    `json:"enabled"`
}

// BlisWeightedScoringFactory defines the factory function for the BlisWeightedScoring scorer.
func BlisWeightedScoringFactory(name string, rawParameters json.RawMessage, handle plugin.Handle) (plugin.Plugin, error) {
	params := BlisWeightedScoringParameters{
		KVPressureThreshold:   0.9,
		KVPenaltyCoefficient:  0.5,
		KVPenaltyDenominator:  0.1,
		TiebreakerCoefficient: 0.01,
		PrefixWeight:          0.6,
		LoadWeight:            0.4,
		DecayCoefficient:      0.6,
		Enabled:               true,
	}
	if rawParameters != nil {
		if err := json.Unmarshal(rawParameters, &params); err != nil {
			return nil, fmt.Errorf("failed to parse parameters for '%s' scorer: %w", BlisWeightedScoringType, err)
		}
	}

	return NewBlisWeightedScoring(handle.Context(), params).WithName(name), nil
}

// NewBlisWeightedScoring creates a new BlisWeightedScoring scorer with the given parameters.
func NewBlisWeightedScoring(ctx context.Context, params BlisWeightedScoringParameters) *BlisWeightedScoring {
	logger := log.FromContext(ctx)

	if params.KVPressureThreshold <= 0 || params.KVPressureThreshold > 1.0 {
		params.KVPressureThreshold = 0.9
		logger.V(logutil.DEFAULT).Info("kvPressureThreshold must be in (0, 1], using default 0.9")
	}
	if params.KVPenaltyCoefficient < 0 {
		params.KVPenaltyCoefficient = 0.5
		logger.V(logutil.DEFAULT).Info("kvPenaltyCoefficient must be non-negative, using default 0.5")
	}
	if params.KVPenaltyDenominator <= 0 {
		params.KVPenaltyDenominator = 0.1
		logger.V(logutil.DEFAULT).Info("kvPenaltyDenominator must be positive, using default 0.1")
	}
	if params.TiebreakerCoefficient < 0 {
		params.TiebreakerCoefficient = 0.01
		logger.V(logutil.DEFAULT).Info("tiebreakerCoefficient must be non-negative, using default 0.01")
	}
	if params.PrefixWeight < 0 {
		params.PrefixWeight = 0.6
		logger.V(logutil.DEFAULT).Info("prefixWeight must be non-negative, using default 0.6")
	}
	if params.LoadWeight < 0 {
		params.LoadWeight = 0.4
		logger.V(logutil.DEFAULT).Info("loadWeight must be non-negative, using default 0.4")
	}
	if params.DecayCoefficient < 0 {
		params.DecayCoefficient = 0.6
		logger.V(logutil.DEFAULT).Info("decayCoefficient must be non-negative, using default 0.6")
	}

	return &BlisWeightedScoring{
		typedName:             plugin.TypedName{Type: BlisWeightedScoringType},
		enabled:               params.Enabled,
		kvPressureThreshold:   params.KVPressureThreshold,
		kvPenaltyCoefficient:  params.KVPenaltyCoefficient,
		kvPenaltyDenominator:  params.KVPenaltyDenominator,
		tiebreakerCoefficient: params.TiebreakerCoefficient,
		prefixWeight:          params.PrefixWeight,
		loadWeight:            params.LoadWeight,
		decayCoefficient:      params.DecayCoefficient,
	}
}

// TypedName returns the plugin's type and instance name.
func (s *BlisWeightedScoring) TypedName() plugin.TypedName {
	return s.typedName
}

// WithName sets the instance name.
func (s *BlisWeightedScoring) WithName(name string) *BlisWeightedScoring {
	s.typedName.Name = name
	return s
}

// Category returns the scoring category.
// This scorer distributes load across endpoints (not session-affinity).
func (s *BlisWeightedScoring) Category() scheduling.ScorerCategory {
	return scheduling.Distribution
}

// clampSubScore clamps a sub-scorer output to [0, 1] as specified by the EVOLVE-BLOCK.
func clampSubScore(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Score computes per-endpoint scores following the EVOLVE-BLOCK algorithm.
//
// The load signal uses InFlightRequests mapped to RunningRequestsSize.
// KVUtilization is mapped from KVCacheUsagePercent (divided by 100).
//
// Since prefix affinity is unavailable (bestPrefixScore = 0.0), the EVOLVE-BLOCK's
// adaptive weight decay condition (bestPrefixScore > 0.1) is never satisfied.
// Therefore, aw[0] = prefixWeight and aw[1] = loadWeight (no decay applied).
// The effective score simplifies to: loadWeight * loadScore + penalties + tiebreaker.
//
// Scores are NOT clamped — the EVOLVE-BLOCK allows negative scores (KV penalty
// can drive scores below zero).
func (s *BlisWeightedScoring) Score(ctx context.Context, _ *scheduling.CycleState, request *scheduling.LLMRequest, endpoints []scheduling.Endpoint) map[scheduling.Endpoint]float64 {
	logger := log.FromContext(ctx)

	if !s.enabled {
		return nil
	}

	type endpointData struct {
		inFlightRequests int
		kvUtilization    float64
		hasMetrics       bool
	}

	data := make([]endpointData, len(endpoints))

	for i, endpoint := range endpoints {
		metrics := endpoint.GetMetrics()
		if metrics == nil {
			data[i] = endpointData{hasMetrics: false}
			continue
		}

		// Signal: InFlightRequests → RunningRequestsSize
		inFlight := metrics.RunningRequestsSize

		// Signal: KVUtilization → KVCacheUsagePercent / 100.0
		// Production KVCacheUsagePercent is 0-100; sim expects 0.0-1.0.
		kvUtil := metrics.KVCacheUsagePercent / 100.0
		if kvUtil < 0.0 {
			kvUtil = 0.0
		}
		if kvUtil > 1.0 {
			kvUtil = 1.0
		}

		data[i] = endpointData{
			inFlightRequests: inFlight,
			kvUtilization:    kvUtil,
			hasMetrics:       true,
		}
	}

	// Adaptive weight computation:
	//
	// Since prefix affinity is not available in production:
	//   bestPrefixID = "" and bestPrefixScore = 0.0
	// The condition (bestPrefixScore > 0.1) is NEVER true.
	// Therefore the decay branch is NEVER entered and weights remain at base values.
	aw0 := s.prefixWeight // prefix-affinity weight (contribution is always 0)
	aw1 := s.loadWeight   // load-balance weight

	// Compute final scores.
	scoredEndpoints := make(map[scheduling.Endpoint]float64, len(endpoints))

	for i, endpoint := range endpoints {
		d := data[i]
		if !d.hasMetrics {
			scoredEndpoints[endpoint] = 0.0
			continue
		}

		// Load-balance sub-score: 1.0 / (1.0 + float64(inFlightRequests))
		loadScore := 1.0 / (1.0 + float64(d.inFlightRequests))

		// Prefix-affinity sub-score: 0.0 (not available in production)
		prefixScore := 0.0

		// Clamp each sub-score to [0, 1] per EVOLVE-BLOCK
		loadScore = clampSubScore(loadScore)
		prefixScore = clampSubScore(prefixScore)

		// Combined score with adaptive weights.
		combined := aw0*prefixScore + aw1*loadScore

		// KV pressure penalty
		if d.kvUtilization > s.kvPressureThreshold {
			kvPenalty := s.kvPenaltyCoefficient * (d.kvUtilization - s.kvPressureThreshold) / s.kvPenaltyDenominator
			combined -= kvPenalty
		}

		// Tiebreaker bonus using inFlightRequests
		combined += s.tiebreakerCoefficient / (1.0 + float64(d.inFlightRequests))

		// Do NOT clamp — the EVOLVE-BLOCK allows negative scores.
		scoredEndpoints[endpoint] = combined

		logger.V(logutil.DEBUG).Info("BlisWeightedScoring score",
			"endpoint", endpoint.GetMetadata().NamespacedName.String(),
			"inFlightRequests", d.inFlightRequests,
			"kvUtilization", d.kvUtilization,
			"loadScore", loadScore,
			"aw0", aw0,
			"aw1", aw1,
			"combined", combined,
		)
	}

	return scoredEndpoints
}

// ScoreEndpoints is a test helper that runs the scorer and returns results
// keyed by endpoint name (string) instead of scheduling.Endpoint.
//
// This function is called by the Go test harness (tools/harness/) during
// equivalence testing (Stage 5). It provides a stable interface that doesn't
// depend on the scheduling.Endpoint type's identity semantics.
//
// WARNING — CycleState constraint: This helper passes nil for cycleState.
// Generated scorers MUST NOT dereference or depend on cycleState in their
// Score() implementation.
func ScoreEndpoints(
	ctx context.Context,
	s scheduling.Scorer,
	request *scheduling.LLMRequest,
	endpoints []scheduling.Endpoint,
) map[string]float64 {
	raw := s.Score(ctx, nil, request, endpoints)
	if raw == nil {
		return nil
	}

	result := make(map[string]float64, len(raw))
	for endpoint, score := range raw {
		name := endpoint.GetMetadata().NamespacedName.String()
		if _, exists := result[name]; exists {
			panic(fmt.Sprintf("ScoreEndpoints: duplicate endpoint name %q — input contains two endpoints with the same NamespacedName", name))
		}
		result[name] = score
	}
	return result
}
