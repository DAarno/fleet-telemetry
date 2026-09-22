package prometheus

import (
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
	logger *logrus.Logger

	// exclusiveState and infoState are both instances of the same underlying
	// problem: a Prometheus series identity (a metric name plus its full
	// label set) can only ever be *set*, never removed, so switching to a
	// new value/combination requires explicitly zeroing the old series or it
	// stays stuck at 1 forever. See setExclusiveState and updateInfo.
	mu             sync.Mutex
	exclusiveState map[string]string
	infoState      map[string]map[string]string
}

// gaugeSet holds the process-wide set of gauges this producer reports on.
// Prometheus collectors must be registered exactly once per process, so
// they're kept separate from Producer and initialized lazily the first time a
// producer is created.
type gaugeSet struct {
	byMetric       map[string]adapter.Gauge
	updated        adapter.Gauge
	info           adapter.Gauge
	infoLabelNames []string
}

var (
	registeredGauges gaugeSet
	registerOnce     sync.Once
)

// NewProducer creates a new Prometheus metrics producer
func NewProducer(metricsCollector metrics.MetricCollector, logger *logrus.Logger) (telemetry.Producer, error) {
	registerOnce.Do(func() { registeredGauges = registerGauges(metricsCollector) })
	return &Producer{
		logger:         logger,
		exclusiveState: make(map[string]string),
		infoState:      make(map[string]map[string]string),
	}, nil
}

// helpText carries over the exact wording already shipped for fields that
// predate the fieldRegistry, so dashboards relying on HELP output (rare, but
// possible via some scrapers/docs tooling) don't see it change.
var helpText = map[string]string{
	"vehicle_battery_soc":                         "Battery state of charge (percentage)",
	"vehicle_charge_limit_soc":                    "Configured battery charge limit (percentage)",
	"vehicle_location_latitude_millidegrees":      "Last known vehicle latitude, in thousandths of a degree",
	"vehicle_location_longitude_millidegrees":     "Last known vehicle longitude, in thousandths of a degree",
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
// "family", e.g. all four TPMS sensors under vehicle_tire_pressure_millibar)
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
			ensure(spec.metric+"_latitude_millidegrees", spec.extraLabel)
			ensure(spec.metric+"_longitude_millidegrees", spec.extraLabel)
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

// Close noop
func (p *Producer) Close() error { return nil }

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

	fields := transformers.PayloadToMap(payload, false, entry.Vin, p.logger)
	invalid := invalidFields(payload)
	vin := entry.Vin
	now := time.Now().Unix()
	infoChanges := map[string]string{}

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
			registeredGauges.updated.Set(now, adapter.Labels{"vin": vin, "field": spec.touchKeyOrDefault(field)})
		}
	}

	if len(infoChanges) > 0 {
		p.updateInfo(vin, infoChanges)
		for label := range infoChanges {
			registeredGauges.updated.Set(now, adapter.Labels{"vin": vin, "field": "info_" + label})
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
	registeredGauges.byMetric[spec.metric].Set(int64(v*float64(scale)), familyLabels(vin, spec))
	return true
}

func (p *Producer) produceBoolean(spec fieldSpec, vin string, val interface{}) bool {
	if b, ok := val.(bool); ok {
		registeredGauges.byMetric[spec.metric].Set(boolToInt(b), familyLabels(vin, spec))
		return true
	}
	// Also accept a 0/1 numeric. Tesla does not document which encoding each
	// field uses, and a handful arrive as an int where the name reads like a
	// flag, so a strict bool assertion would drop the field and log an error
	// on every payload. Anything other than exactly 0 or 1 is left unhandled
	// rather than coerced, since that would mean the field is not a flag at
	// all and the registry entry is simply wrong.
	if v, ok := numericValue(val); ok && (v == 0 || v == 1) {
		registeredGauges.byMetric[spec.metric].Set(int64(v), familyLabels(vin, spec))
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
	p.setExclusiveState(registeredGauges.byMetric[spec.metric], key, familyLabels(vin, spec), spec.valueLabel, s)
	return true
}

func (p *Producer) produceBoolMap(spec fieldSpec, vin string, val interface{}) bool {
	m, ok := val.(map[string]bool)
	if !ok {
		return false
	}
	gauge := registeredGauges.byMetric[spec.metric]
	for k, v := range m {
		labels := familyLabels(vin, spec)
		labels[spec.boolMapLabel] = camelToSnake(k)
		gauge.Set(boolToInt(v), labels)
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
		registeredGauges.byMetric[spec.metric+"_seconds_since_midnight"].Set(int64(seconds), labels)
		return true
	}

	// An absolute Unix timestamp. Observed on ScheduledChargingStartTime,
	// which the registry originally assumed was a clock string -- the
	// mismatch was silently dropping the field and logging an error on
	// every payload once a schedule was actually set.
	if v, ok := numericValue(val); ok {
		registeredGauges.byMetric[spec.metric+"_timestamp_seconds"].Set(int64(v), labels)
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
	registeredGauges.byMetric[spec.metric+"_latitude_millidegrees"].Set(int64(loc["latitude"]*1000), labels)
	registeredGauges.byMetric[spec.metric+"_longitude_millidegrees"].Set(int64(loc["longitude"]*1000), labels)
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
func (p *Producer) setExclusiveState(gauge adapter.Gauge, key string, baseLabels adapter.Labels, valueLabel, newValue string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.exclusiveState[key]; ok && old != newValue {
		oldLabels := cloneLabels(baseLabels)
		oldLabels[valueLabel] = old
		gauge.Set(0, oldLabels)
	}
	newLabels := cloneLabels(baseLabels)
	newLabels[valueLabel] = newValue
	gauge.Set(1, newLabels)
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
	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.infoState[vin]
	next := make(map[string]string, len(registeredGauges.infoLabelNames))
	for _, name := range registeredGauges.infoLabelNames {
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
		registeredGauges.info.Set(0, labelsWithVin(vin, old))
	}
	registeredGauges.info.Set(1, labelsWithVin(vin, next))
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
