package service

import (
	"sync/atomic"
	"time"
)

type RouteCircuitTransition string

const (
	RouteCircuitOpened   RouteCircuitTransition = "opened"
	RouteCircuitHalfOpen RouteCircuitTransition = "half_open"
	RouteCircuitClosed   RouteCircuitTransition = "closed"
)

type APIKeyRouteMetricsSnapshot struct {
	RouteRequests               uint64
	FallbackSuccesses           uint64
	CandidateSkips              map[RouteFailureClass]uint64
	CircuitTransitions          map[RouteCircuitTransition]uint64
	StickyHits                  uint64
	InvalidStickyRecords        uint64
	ConfigValidationFailures    uint64
	StoreErrors                 uint64
	SwitchDurationCount         uint64
	SwitchDurationTotal         time.Duration
	BillingIdempotencyConflicts uint64
}

type APIKeyRouteMetrics struct {
	routeRequests               atomic.Uint64
	fallbackSuccesses           atomic.Uint64
	skipCapacity                atomic.Uint64
	skipConnection              atomic.Uint64
	skipTimeout                 atomic.Uint64
	skipUpstream429             atomic.Uint64
	skipUpstream5xx             atomic.Uint64
	skipUnsupportedModel        atomic.Uint64
	skipAdmission               atomic.Uint64
	skipBusiness                atomic.Uint64
	skipCanceled                atomic.Uint64
	circuitOpened               atomic.Uint64
	circuitHalfOpen             atomic.Uint64
	circuitClosed               atomic.Uint64
	stickyHits                  atomic.Uint64
	invalidStickyRecords        atomic.Uint64
	configValidationFailures    atomic.Uint64
	storeErrors                 atomic.Uint64
	switchDurationCount         atomic.Uint64
	switchDurationNanos         atomic.Uint64
	billingIdempotencyConflicts atomic.Uint64
}

var defaultAPIKeyRouteMetrics APIKeyRouteMetrics

func NewAPIKeyRouteMetrics() *APIKeyRouteMetrics { return &APIKeyRouteMetrics{} }

func GetAPIKeyRouteMetricsSnapshot() APIKeyRouteMetricsSnapshot {
	return defaultAPIKeyRouteMetrics.Snapshot()
}

func RecordAPIKeyRouteRequest() { defaultAPIKeyRouteMetrics.RecordRouteRequest() }

func RecordAPIKeyRouteFallbackSuccess() { defaultAPIKeyRouteMetrics.RecordFallbackSuccess() }

func RecordAPIKeyRouteCandidateSkip(reason RouteFailureClass) {
	defaultAPIKeyRouteMetrics.RecordCandidateSkip(reason)
}

func RecordAPIKeyRouteCircuitTransition(transition RouteCircuitTransition) {
	defaultAPIKeyRouteMetrics.RecordCircuitTransition(transition)
}

func RecordAPIKeyRouteStickyHit() { defaultAPIKeyRouteMetrics.RecordStickyHit() }

func RecordAPIKeyRouteInvalidSticky() { defaultAPIKeyRouteMetrics.RecordInvalidSticky() }

func RecordAPIKeyRouteConfigValidationFailure() {
	defaultAPIKeyRouteMetrics.RecordConfigValidationFailure()
}

func RecordAPIKeyRouteStoreError() { defaultAPIKeyRouteMetrics.RecordStoreError() }

func ObserveAPIKeyRouteSwitchDuration(duration time.Duration) {
	defaultAPIKeyRouteMetrics.ObserveSwitchDuration(duration)
}

func RecordAPIKeyRouteBillingIdempotencyConflict() {
	defaultAPIKeyRouteMetrics.RecordBillingIdempotencyConflict()
}

func (m *APIKeyRouteMetrics) RecordRouteRequest() {
	if m != nil {
		m.routeRequests.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordFallbackSuccess() {
	if m != nil {
		m.fallbackSuccesses.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordCandidateSkip(reason RouteFailureClass) {
	if m == nil {
		return
	}
	switch reason {
	case RouteFailureCapacity:
		m.skipCapacity.Add(1)
	case RouteFailureConnection:
		m.skipConnection.Add(1)
	case RouteFailureTimeout:
		m.skipTimeout.Add(1)
	case RouteFailureUpstream429:
		m.skipUpstream429.Add(1)
	case RouteFailureUpstream5xx:
		m.skipUpstream5xx.Add(1)
	case RouteFailureUnsupportedModel:
		m.skipUnsupportedModel.Add(1)
	case RouteFailureAdmission:
		m.skipAdmission.Add(1)
	case RouteFailureBusiness:
		m.skipBusiness.Add(1)
	case RouteFailureCanceled:
		m.skipCanceled.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordCircuitTransition(transition RouteCircuitTransition) {
	if m == nil {
		return
	}
	switch transition {
	case RouteCircuitOpened:
		m.circuitOpened.Add(1)
	case RouteCircuitHalfOpen:
		m.circuitHalfOpen.Add(1)
	case RouteCircuitClosed:
		m.circuitClosed.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordStickyHit() {
	if m != nil {
		m.stickyHits.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordInvalidSticky() {
	if m != nil {
		m.invalidStickyRecords.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordConfigValidationFailure() {
	if m != nil {
		m.configValidationFailures.Add(1)
	}
}

func (m *APIKeyRouteMetrics) RecordStoreError() {
	if m != nil {
		m.storeErrors.Add(1)
	}
}

func (m *APIKeyRouteMetrics) ObserveSwitchDuration(duration time.Duration) {
	if m == nil {
		return
	}
	if duration < 0 {
		duration = 0
	}
	m.switchDurationCount.Add(1)
	m.switchDurationNanos.Add(uint64(duration))
}

func (m *APIKeyRouteMetrics) RecordBillingIdempotencyConflict() {
	if m != nil {
		m.billingIdempotencyConflicts.Add(1)
	}
}

func (m *APIKeyRouteMetrics) Snapshot() APIKeyRouteMetricsSnapshot {
	if m == nil {
		return APIKeyRouteMetricsSnapshot{}
	}
	return APIKeyRouteMetricsSnapshot{
		RouteRequests:               m.routeRequests.Load(),
		FallbackSuccesses:           m.fallbackSuccesses.Load(),
		CandidateSkips:              m.candidateSkipSnapshot(),
		CircuitTransitions:          m.circuitTransitionSnapshot(),
		StickyHits:                  m.stickyHits.Load(),
		InvalidStickyRecords:        m.invalidStickyRecords.Load(),
		ConfigValidationFailures:    m.configValidationFailures.Load(),
		StoreErrors:                 m.storeErrors.Load(),
		SwitchDurationCount:         m.switchDurationCount.Load(),
		SwitchDurationTotal:         time.Duration(m.switchDurationNanos.Load()),
		BillingIdempotencyConflicts: m.billingIdempotencyConflicts.Load(),
	}
}

func (m *APIKeyRouteMetrics) candidateSkipSnapshot() map[RouteFailureClass]uint64 {
	values := []struct {
		key   RouteFailureClass
		value uint64
	}{
		{RouteFailureCapacity, m.skipCapacity.Load()},
		{RouteFailureConnection, m.skipConnection.Load()},
		{RouteFailureTimeout, m.skipTimeout.Load()},
		{RouteFailureUpstream429, m.skipUpstream429.Load()},
		{RouteFailureUpstream5xx, m.skipUpstream5xx.Load()},
		{RouteFailureUnsupportedModel, m.skipUnsupportedModel.Load()},
		{RouteFailureAdmission, m.skipAdmission.Load()},
		{RouteFailureBusiness, m.skipBusiness.Load()},
		{RouteFailureCanceled, m.skipCanceled.Load()},
	}
	result := make(map[RouteFailureClass]uint64)
	for _, value := range values {
		if value.value > 0 {
			result[value.key] = value.value
		}
	}
	return result
}

func (m *APIKeyRouteMetrics) circuitTransitionSnapshot() map[RouteCircuitTransition]uint64 {
	result := make(map[RouteCircuitTransition]uint64)
	if value := m.circuitOpened.Load(); value > 0 {
		result[RouteCircuitOpened] = value
	}
	if value := m.circuitHalfOpen.Load(); value > 0 {
		result[RouteCircuitHalfOpen] = value
	}
	if value := m.circuitClosed.Load(); value > 0 {
		result[RouteCircuitClosed] = value
	}
	return result
}
