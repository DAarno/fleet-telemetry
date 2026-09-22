package prometheus

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	"github.com/teslamotors/fleet-telemetry/protos"
)

// TestRegistryCoversEveryField ensures every real protos.Field constant has a
// registry entry -- implemented or explicitly excluded with a reason -- so a
// newly-added upstream field can't silently vanish from this producer without
// review. protos.Field_name is the generated map from field number to name;
// walking it (rather than a hardcoded count) means this test stays correct as
// Tesla adds fields upstream.
func TestRegistryCoversEveryField(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	for number := range protos.Field_name {
		field := protos.Field(number)
		spec, ok := fieldRegistry[field]
		g.Expect(ok).To(gomega.BeTrue(), "protos.Field %s (%d) has no fieldRegistry entry", field, number)

		if spec.kind == kindExcluded {
			g.Expect(spec.reason).ToNot(gomega.BeEmpty(), "excluded field %s must document why", field)
		}
	}
}

// TestRegistrySpecsAreWellFormed catches authoring mistakes that would
// otherwise only surface as a panic during gauge registration or a silent
// no-op at Produce() time.
func TestRegistrySpecsAreWellFormed(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	for field, spec := range fieldRegistry {
		switch spec.kind {
		case kindExcluded:
			g.Expect(spec.metric).To(gomega.BeEmpty(), "%s: excluded fields shouldn't set metric", field)
		case kindEnum:
			g.Expect(spec.metric).ToNot(gomega.BeEmpty(), "%s: kindEnum needs metric", field)
			g.Expect(spec.valueLabel).ToNot(gomega.BeEmpty(), "%s: kindEnum needs valueLabel", field)
		case kindBoolMap:
			g.Expect(spec.metric).ToNot(gomega.BeEmpty(), "%s: kindBoolMap needs metric", field)
			g.Expect(spec.boolMapLabel).ToNot(gomega.BeEmpty(), "%s: kindBoolMap needs boolMapLabel", field)
		case kindInfo:
			g.Expect(spec.infoLabel).ToNot(gomega.BeEmpty(), "%s: kindInfo needs infoLabel", field)
		case kindNumeric, kindBoolean, kindScheduledTime, kindLocation:
			g.Expect(spec.metric).ToNot(gomega.BeEmpty(), "%s: needs metric", field)
		}

		if spec.extraLabel == "" {
			g.Expect(spec.extraValue).To(gomega.BeEmpty(), "%s: extraValue set without extraLabel", field)
		} else {
			g.Expect(spec.extraValue).ToNot(gomega.BeEmpty(), "%s: extraLabel set without extraValue", field)
		}
	}
}

// TestFamilyMembersAgreeOnLabelSchema catches the mistake of two fields
// sharing a metric name with different label shapes, which would panic at
// Prometheus registration time (or worse, silently pick whichever label set
// won the registration race) rather than fail a fast, readable test.
func TestFamilyMembersAgreeOnLabelSchema(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	type schema struct {
		kind         fieldKind
		extraLabel   string
		valueLabel   string
		boolMapLabel string
	}
	seen := map[string]schema{}

	for field, spec := range fieldRegistry {
		if spec.kind == kindExcluded || spec.kind == kindInfo {
			continue
		}
		got := schema{kind: spec.kind, extraLabel: spec.extraLabel, valueLabel: spec.valueLabel, boolMapLabel: spec.boolMapLabel}
		metricNames := []string{spec.metric}
		if spec.kind == kindLocation {
			metricNames = []string{spec.metric + "_latitude_millidegrees", spec.metric + "_longitude_millidegrees"}
		}
		for _, name := range metricNames {
			if want, ok := seen[name]; ok {
				g.Expect(got).To(gomega.Equal(want), "%s: metric %q label schema disagrees with an earlier field", field, name)
			} else {
				seen[name] = got
			}
		}
	}
}

// TestTouchKeysAreUnique guards against camelToSnake collisions between two
// different fields, which would make vehicle_telemetry_updated_timestamp_seconds
// ambiguous between them.
func TestTouchKeysAreUnique(t *testing.T) {
	seenBy := map[string]protos.Field{}
	for number := range protos.Field_name {
		field := protos.Field(number)
		spec, ok := fieldRegistry[field]
		if !ok || spec.kind == kindExcluded {
			continue
		}
		key := spec.touchKeyOrDefault(field)
		if other, exists := seenBy[key]; exists {
			t.Errorf("touch key %q used by both %s and %s", key, other, field)
		}
		seenBy[key] = field
	}
}

func TestCamelToSnake(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	cases := map[string]string{
		"Soc":                        "soc",
		"ChargeState":                "charge_state",
		"ACChargingPower":            "ac_charging_power",
		"BMSState":                   "bms_state",
		"GpsState":                   "gps_state",
		"TpmsPressureFl":             "tpms_pressure_fl",
		"SelfDrivingMilesSinceReset": "self_driving_miles_since_reset",
	}
	for in, want := range cases {
		g.Expect(camelToSnake(in)).To(gomega.Equal(want), fmt.Sprintf("camelToSnake(%q)", in))
	}
}
