package prometheus

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/teslamotors/fleet-telemetry/datastore/simple/transformers"
	logrus "github.com/teslamotors/fleet-telemetry/logger"
	"github.com/teslamotors/fleet-telemetry/metrics"
	"github.com/teslamotors/fleet-telemetry/metrics/adapter"
	"github.com/teslamotors/fleet-telemetry/protos"
	"github.com/teslamotors/fleet-telemetry/telemetry"
)

// Producer publishes select vehicle fields from 'V' records as Prometheus
// gauges. Which fields, and how each becomes a metric, is declared in
// fields.go's fieldRegistry rather than hardcoded here -- this loop just
// dispatches on fieldSpec.kind.
type Producer struct {
	logger      *logrus.Logger
	gauges      gaugeSet
	allowedVINs map[string]struct{}
	retention   time.Duration
	lastSeen    map[string]time.Time
	series      map[string]map[string]seriesRef
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once

	// exclusiveState and infoState are both instances of the same underlying
	// problem: a Prometheus series identity (a metric name plus its full
	// label set) can only ever be *set*, never removed, so switching to a
	// new value/combination requires explicitly zeroing the old series or it
	// stays stuck at 1 forever. See setExclusiveState and updateInfo.
	mu             sync.Mutex
	exclusiveState map[string]string
	infoState      map[string]map[string]string
}

type seriesRef struct {
	metric string
	labels adapter.Labels
}

// gaugeSet holds the set of gauges this producer reports on.
type gaugeSet struct {
	byMetric       map[string]adapter.Gauge
	updated        adapter.Gauge
	info           adapter.Gauge
	infoLabelNames []string
}

// NewProducer creates a bounded Prometheus metrics producer. An explicit VIN
// allowlist prevents a large fleet from accidentally exporting every car.
func NewProducer(metricsCollector metrics.MetricCollector, logger *logrus.Logger, vins []string, retention time.Duration) (telemetry.Producer, error) {
	if len(vins) == 0 || len(vins) > 20 {
		return nil, fmt.Errorf("prometheus dispatcher requires 1 to 20 prometheus_export_vins")
	}
	if retention <= 0 {
		return nil, fmt.Errorf("prometheus retention must be positive")
	}
	allowed := make(map[string]struct{}, len(vins))
	for _, vin := range vins {
		if vin == "" {
			return nil, fmt.Errorf("prometheus_export_vins cannot contain an empty VIN")
		}
		if _, exists := allowed[vin]; exists {
			return nil, fmt.Errorf("prometheus_export_vins contains duplicate VIN %q", vin)
		}
		allowed[vin] = struct{}{}
	}
	p := &Producer{
		logger:         logger,
		gauges:         registerGauges(metricsCollector),
		allowedVINs:    allowed,
		retention:      retention,
		lastSeen:       make(map[string]time.Time),
		series:         make(map[string]map[string]seriesRef),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		exclusiveState: make(map[string]string),
		infoState:      make(map[string]map[string]string),
	}
	go p.runSweep()
	return p, nil
}

// helpText carries over the exact wording already shipped for fields that
// predate the fieldRegistry, so dashboards relying on HELP output (rare, but
// possible via some scrapers/docs tooling) don't see it change.
var helpText = map[string]string{
	"vehicle_battery_soc":                         "Battery state of charge (percentage)",
	"vehicle_charge_limit_soc":                    "Configured battery charge limit (percentage)",
	"vehicle_location_latitude_degrees":           "Last known vehicle latitude, in degrees",
	"vehicle_location_longitude_degrees":          "Last known vehicle longitude, in degrees",
	"vehicle_charge_state":                        "Current charging state as an info metric (1 = current)",
	"vehicle_charging_cable":                      "Current charging cable type as an info metric (1 = current)",
	"vehicle_telemetry_updated_timestamp_seconds": "Unix time this field was last reported by the vehicle",
	"vehicle_info":                                "Static vehicle identity/configuration fields; always 1 for the vin's currently-known combination",
}

func help(metric string) string {
	if h, ok := helpText[metric]; ok {
		return h
	}
	// The scheduled-time metrics need their Pacific-Time caveat visible to
	// anyone reading the exposition, not just fields.go.
	switch {
	case strings.HasSuffix(metric, "_timestamp_seconds"):
		return "Unix time of a vehicle-scheduled event. Tesla expresses the hour in Pacific Time regardless of the vehicle's location, so this is not a true instant"
	case strings.HasSuffix(metric, "_seconds_since_midnight"):
		return "Wall-clock time of a vehicle-scheduled event, in seconds since midnight Pacific Time (Tesla reports the hour in Pacific regardless of the vehicle's location)"
	}
	return "Tesla vehicle telemetry field -- see datastore/prometheus/fields.go for the source field and unit"
}

// registerGauges computes, from fieldRegistry, the full set of distinct
// Prometheus metrics needed (several fields may share one metric name as a
// "family", e.g. all four TPMS sensors under vehicle_tire_pressure_pascals)
// and registers each exactly once.
func registerGauges(m metrics.MetricCollector) gaugeSet {
	labelSets := map[string]map[string]bool{}
	infoLabels := map[string]bool{}

	ensure := func(metric, extraLabel string) {
		if labelSets[metric] == nil {
			labelSets[metric] = map[string]bool{"vin": true}
		}
		if extraLabel != "" {
			labelSets[metric][extraLabel] = true
		}
	}

	for _, spec := range fieldRegistry {
		switch spec.kind {
		case kindExcluded:
			continue
		case kindInfo:
			infoLabels[spec.infoLabel] = true
		case kindLocation:
			ensure(spec.metric+"_latitude_degrees", spec.extraLabel)
			ensure(spec.metric+"_longitude_degrees", spec.extraLabel)
		case kindScheduledTime:
			ensure(spec.metric+"_timestamp_seconds", spec.extraLabel)
			ensure(spec.metric+"_seconds_since_midnight", spec.extraLabel)
		case kindEnum:
			ensure(spec.metric, spec.extraLabel)
			labelSets[spec.metric][spec.valueLabel] = true
		case kindBoolMap:
			ensure(spec.metric, spec.extraLabel)
			labelSets[spec.metric][spec.boolMapLabel] = true
		default: // kindNumeric, kindBoolean
			ensure(spec.metric, spec.extraLabel)
		}
	}

	byMetric := make(map[string]adapter.Gauge, len(labelSets))
	for name, labels := range labelSets {
		names := make([]string, 0, len(labels))
		for l := range labels {
			names = append(names, l)
		}
		sort.Strings(names)
		byMetric[name] = m.RegisterGauge(adapter.CollectorOptions{Name: name, Help: help(name), Labels: names})
	}

	infoLabelNames := make([]string, 0, len(infoLabels))
	for l := range infoLabels {
		infoLabelNames = append(infoLabelNames, l)
	}
	sort.Strings(infoLabelNames)
	infoAllLabels := append([]string{"vin"}, infoLabelNames...)

	return gaugeSet{
		byMetric: byMetric,
		updated: m.RegisterGauge(adapter.CollectorOptions{
			Name:   "vehicle_telemetry_updated_timestamp_seconds",
			Help:   help("vehicle_telemetry_updated_timestamp_seconds"),
			Labels: []string{"vin", "field"},
		}),
		info:           m.RegisterGauge(adapter.CollectorOptions{Name: "vehicle_info", Help: help("vehicle_info"), Labels: infoAllLabels}),
		infoLabelNames: infoLabelNames,
	}
}

// Close stops the expiry worker.
func (p *Producer) Close() error {
	p.closeOnce.Do(func() { close(p.stop) })
	<-p.done
	return nil
}

func (p *Producer) runSweep() {
	defer close(p.done)
	interval := p.retention / 2
	if interval > time.Hour {
		interval = time.Hour
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			p.sweep(now)
		case <-p.stop:
			return
		}
	}
}

// sweep removes all output series and state for VINs that have stopped
// reporting, including timestamps and previously active enum/info labels.
// A scrape may run concurrently; Gauge.Delete is safe for that case.
func (p *Producer) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for vin, seen := range p.lastSeen {
		if now.Sub(seen) < p.retention {
			continue
		}
		for _, ref := range p.series[vin] {
			p.gauge(ref.metric).Delete(ref.labels)
		}
		delete(p.series, vin)
		delete(p.lastSeen, vin)
		delete(p.infoState, vin)
		for key := range p.exclusiveState {
			if strings.Contains(key, "|"+vin+"|") {
				delete(p.exclusiveState, key)
			}
		}
	}
}

func (p *Producer) gauge(metric string) adapter.Gauge {
	switch metric {
	case "vehicle_telemetry_updated_timestamp_seconds":
		return p.gauges.updated
	case "vehicle_info":
		return p.gauges.info
	default:
		return p.gauges.byMetric[metric]
	}
}

// set remembers every exact label set so expiry can delete it later.
// Produce and sweep hold p.mu, so tracking and gauge updates are atomic to
// each other. JSON encodes map keys in stable order for deduplication.
func (p *Producer) set(metric string, value float64, labels adapter.Labels) {
	encoded, _ := json.Marshal(labels)
	vin := labels["vin"]
	if p.series[vin] == nil {
		p.series[vin] = make(map[string]seriesRef)
	}
	key := metric + "|" + string(encoded)
	p.series[vin][key] = seriesRef{metric: metric, labels: cloneLabels(labels)}
	p.gauge(metric).SetFloat64(value, labels)
}

// ProcessReliableAck noop
func (p *Producer) ProcessReliableAck(_ *telemetry.Record) {}

// ReportError noop
func (p *Producer) ReportError(_ string, _ error, _ logrus.LogInfo) {}

// Produce extracts every registered field present on the record and updates
// the corresponding Prometheus gauge(s). Iterating the fields actually
// present on the payload (rather than the whole ~260-entry registry) keeps
// this cheap regardless of how much of the registry is populated.
func (p *Producer) Produce(entry *telemetry.Record) {
	payload, ok := entry.GetProtoMessage().(*protos.Payload)
	if !ok {
		return
	}
	if _, allowed := p.allowedVINs[entry.Vin]; !allowed {
		return
	}

	fields := transformers.PayloadToMap(payload, false, entry.Vin, p.logger)
	invalid := invalidFields(payload)
	vin := entry.Vin
	observed := time.Now()
	now := observed.Unix()
	infoChanges := map[string]string{}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSeen[vin] = observed

	for name, val := range fields {
		number, ok := protos.Field_value[name]
		if !ok {
			continue // "Vin", "CreatedAt", "IsResend" -- payload metadata, not a telemetry field
		}
		field := protos.Field(number)
		spec, ok := fieldRegistry[field]
		if !ok || spec.kind == kindExcluded {
			continue
		}
		if invalid[name] {
			// The vehicle is reporting that this field does not currently
			// apply -- an unset scheduled charging time, an option the car
			// doesn't have. That is neither an error nor data: leave the
			// gauge and its freshness timestamp alone rather than inventing
			// a value. Skipping it here also keeps "<invalid>" from becoming
			// an enum label, which the string-typed kinds would otherwise
			// accept as a perfectly good value.
			continue
		}

		var handled bool
		switch spec.kind {
		case kindNumeric:
			handled = p.produceNumeric(spec, vin, val)
		case kindBoolean:
			handled = p.produceBoolean(spec, vin, val)
		case kindEnum:
			handled = p.produceEnum(spec, vin, val)
		case kindBoolMap:
			handled = p.produceBoolMap(spec, vin, val)
		case kindScheduledTime:
			handled = p.produceScheduledTime(spec, vin, val)
		case kindLocation:
			handled = p.produceLocation(spec, vin, val)
		case kindInfo:
			if s, ok := infoValue(val); ok {
				infoChanges[spec.infoLabel] = s
				handled = true
			}
		}

		if !handled {
			// Says "value", not "type": the type is often fine and the
			// content unusable (a time that doesn't parse, say).
			p.logger.ErrorLog("prometheus_unhandled_field_value", fmt.Errorf("unhandled value for %s (type %T)", name, val), logrus.LogInfo{"vin": vin})
			continue
		}
		if spec.kind != kindInfo {
			p.set("vehicle_telemetry_updated_timestamp_seconds", float64(now), adapter.Labels{"vin": vin, "field": spec.touchKeyOrDefault(field)})
		}
	}

	if len(infoChanges) > 0 {
		p.updateInfo(vin, infoChanges)
		for label := range infoChanges {
			p.set("vehicle_telemetry_updated_timestamp_seconds", float64(now), adapter.Labels{"vin": vin, "field": "info_" + label})
		}
	}
}

// invalidFields names the fields the vehicle explicitly reported as invalid.
//
// Tesla uses Value_Invalid to mean "this field does not currently apply",
// which is common and entirely normal -- ScheduledChargingStartTime reports
// it whenever nothing is scheduled. PayloadToMap flattens that to the literal
// string "<invalid>", indistinguishable at that layer from a real string
// value, so the distinction has to be recovered from the raw payload.
func invalidFields(payload *protos.Payload) map[string]bool {
	var out map[string]bool
	for _, datum := range payload.Data {
		if datum == nil || datum.Value == nil {
			continue
		}
		if _, ok := datum.Value.Value.(*protos.Value_Invalid); ok {
			if out == nil {
				out = make(map[string]bool, 1)
			}
			out[protos.Field_name[int32(datum.Key.Number())]] = true
		}
	}
	return out
}

func (p *Producer) produceNumeric(spec fieldSpec, vin string, val interface{}) bool {
	v, ok := numericValue(val)
	if !ok {
		return false
	}
	scale := spec.scale
	if scale == 0 {
		scale = 1
	}
	p.set(spec.metric, v*float64(scale), familyLabels(vin, spec))
	return true
}

func (p *Producer) produceBoolean(spec fieldSpec, vin string, val interface{}) bool {
	if b, ok := val.(bool); ok {
		p.set(spec.metric, float64(boolToInt(b)), familyLabels(vin, spec))
		return true
	}
	// Also accept a 0/1 numeric. Tesla does not document which encoding each
	// field uses, and a handful arrive as an int where the name reads like a
	// flag, so a strict bool assertion would drop the field and log an error
	// on every payload. Anything other than exactly 0 or 1 is left unhandled
	// rather than coerced, since that would mean the field is not a flag at
	// all and the registry entry is simply wrong.
	if v, ok := numericValue(val); ok && (v == 0 || v == 1) {
		p.set(spec.metric, v, familyLabels(vin, spec))
		return true
	}
	return false
}

func (p *Producer) produceEnum(spec fieldSpec, vin string, val interface{}) bool {
	s, ok := val.(string)
	if !ok {
		return false
	}
	key := strings.Join([]string{spec.metric, vin, spec.extraValue}, "|")
	p.setExclusiveState(spec.metric, key, familyLabels(vin, spec), spec.valueLabel, s)
	return true
}

func (p *Producer) produceBoolMap(spec fieldSpec, vin string, val interface{}) bool {
	m, ok := val.(map[string]bool)
	if !ok {
		return false
	}
	for k, v := range m {
		labels := familyLabels(vin, spec)
		labels[spec.boolMapLabel] = camelToSnake(k)
		p.set(spec.metric, float64(boolToInt(v)), labels)
	}
	return true
}

// produceScheduledTime handles both encodings Tesla uses for a scheduled
// time. See the kindScheduledTime comment in fields.go for why they go to
// separate metrics and why neither is a true instant.
func (p *Producer) produceScheduledTime(spec fieldSpec, vin string, val interface{}) bool {
	labels := familyLabels(vin, spec)

	if s, ok := val.(string); ok {
		t, err := time.Parse("15:04:05", s)
		if err != nil {
			return false
		}
		seconds := t.Hour()*3600 + t.Minute()*60 + t.Second()
		p.set(spec.metric+"_seconds_since_midnight", float64(seconds), labels)
		return true
	}

	// An absolute Unix timestamp. Observed on ScheduledChargingStartTime,
	// which the registry originally assumed was a clock string -- the
	// mismatch was silently dropping the field and logging an error on
	// every payload once a schedule was actually set.
	if v, ok := numericValue(val); ok {
		p.set(spec.metric+"_timestamp_seconds", v, labels)
		return true
	}
	return false
}

func (p *Producer) produceLocation(spec fieldSpec, vin string, val interface{}) bool {
	loc, ok := val.(map[string]float64)
	if !ok {
		return false
	}
	labels := familyLabels(vin, spec)
	p.set(spec.metric+"_latitude_degrees", loc["latitude"], labels)
	p.set(spec.metric+"_longitude_degrees", loc["longitude"], labels)
	return true
}

func familyLabels(vin string, spec fieldSpec) adapter.Labels {
	labels := adapter.Labels{"vin": vin}
	if spec.extraLabel != "" {
		labels[spec.extraLabel] = spec.extraValue
	}
	return labels
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func infoValue(val interface{}) (string, bool) {
	switch v := val.(type) {
	case string:
		return v, true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// setExclusiveState sets newValue's series to 1 and, if a different value was
// previously active for this key (typically metric+vin, or metric+vin+family
// member for a field like window state that has several independent slots),
// resets that old series back to 0. Without this, a stale series from before
// the last transition stays at 1 forever, and a consumer picking "whichever
// series is 1" can pick the wrong one -- this is exactly the bug that once
// caused a car to keep charging after "current state" reported stopped.
func (p *Producer) setExclusiveState(metric, key string, baseLabels adapter.Labels, valueLabel, newValue string) {
	if old, ok := p.exclusiveState[key]; ok && old != newValue {
		oldLabels := cloneLabels(baseLabels)
		oldLabels[valueLabel] = old
		p.set(metric, 0, oldLabels)
	}
	newLabels := cloneLabels(baseLabels)
	newLabels[valueLabel] = newValue
	p.set(metric, 1, newLabels)
	p.exclusiveState[key] = newValue
}

// updateInfo applies changes to the vin's running set of vehicle_info labels
// and, if the resulting combination differs from what's currently emitted,
// zeroes the old combination's series and sets the new one. This is the same
// problem setExclusiveState solves, generalized from "one value among many"
// to "one combination of several labels among many" -- vehicle_info's series
// identity is its full label set, so changing even one label (e.g. a
// firmware update) means the old combination must be zeroed just the same.
func (p *Producer) updateInfo(vin string, changes map[string]string) {
	old := p.infoState[vin]
	next := make(map[string]string, len(p.gauges.infoLabelNames))
	for _, name := range p.gauges.infoLabelNames {
		next[name] = ""
	}
	for k, v := range old {
		next[k] = v
	}
	for k, v := range changes {
		next[k] = v
	}

	if old != nil && labelsEqual(old, next) {
		return
	}
	if old != nil {
		p.set("vehicle_info", 0, labelsWithVin(vin, old))
	}
	p.set("vehicle_info", 1, labelsWithVin(vin, next))
	p.infoState[vin] = next
}

func labelsWithVin(vin string, m map[string]string) adapter.Labels {
	out := make(adapter.Labels, len(m)+1)
	out["vin"] = vin
	for k, v := range m {
		out[k] = v
	}
	return out
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func cloneLabels(l adapter.Labels) adapter.Labels {
	out := make(adapter.Labels, len(l)+1)
	for k, v := range l {
		out[k] = v
	}
	return out
}

// numericValue normalizes the numeric proto value kinds PayloadToMap can produce
// (float32/float64/int32/int64) into a single float64
func numericValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
