package prometheus_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	promclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	promproducer "github.com/teslamotors/fleet-telemetry/datastore/prometheus"
	logrus "github.com/teslamotors/fleet-telemetry/logger"
	"github.com/teslamotors/fleet-telemetry/messages"
	promadapter "github.com/teslamotors/fleet-telemetry/metrics/adapter/prometheus"
	"github.com/teslamotors/fleet-telemetry/protos"
	"github.com/teslamotors/fleet-telemetry/telemetry"
)

var testVINs = []string{
	"TESTVIN", "TESTVIN2", "TRANSITIONVIN", "CABLETRANSITIONVIN", "SCALEVIN",
	"DOORVIN", "WINDOWVIN", "SCHEDVIN", "TIMEVIN", "TRIPVIN", "INFOVIN",
	"INVALIDVIN", "INVALIDENUMVIN", "BOOLNUMVIN", "BOOLBADVIN",
}

func newTestProducer(t *testing.T, collector *promadapter.Collector, logger *logrus.Logger) (telemetry.Producer, error) {
	t.Helper()
	producer, err := promproducer.NewProducer(collector, logger, testVINs, time.Hour)
	if err == nil {
		t.Cleanup(func() { _ = producer.Close() })
	}
	return producer, err
}

func newTestCollector(t *testing.T) *promadapter.Collector {
	t.Helper()
	collector := promadapter.NewCollector()
	t.Cleanup(collector.Shutdown)
	return collector
}

func TestProducerValidatesNonemptyVINFilter(t *testing.T) {
	logger, _ := logrus.NoOpLogger()
	for _, vins := range [][]string{{""}, {"DUP", "DUP"}} {
		_, err := promproducer.NewProducer(newTestCollector(t), logger, vins, time.Hour)
		if err == nil {
			t.Fatalf("accepted invalid VIN allowlist %v", vins)
		}
	}
	tooMany := make([]string, 21)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("VIN%02d", i)
	}
	if _, err := promproducer.NewProducer(newTestCollector(t), logger, tooMany, time.Hour); err == nil {
		t.Fatal("accepted allowlist with more than 20 vehicles")
	}
}

func TestProducerExportsAllVINsWithoutFilter(t *testing.T) {
	for _, test := range []struct {
		name string
		vins []string
	}{
		{name: "omitted"},
		{name: "empty", vins: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			logger, _ := logrus.NoOpLogger()
			producer, err := promproducer.NewProducer(newTestCollector(t), logger, test.vins, time.Hour)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			t.Cleanup(func() { _ = producer.Close() })
			// Unfiltered exports have no 20-vehicle cap.
			for i := 0; i < 25; i++ {
				send(t, producer, fmt.Sprintf("VIN%02d", i), &protos.Datum{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: int32(i)}}})
			}
			mfs, err := promclient.DefaultGatherer.Gather()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			for i := 0; i < 25; i++ {
				vin := fmt.Sprintf("VIN%02d", i)
				g.Expect(gaugeValue(mfs, "vehicle_battery_soc", vin)).To(gomega.Equal(float64(i)))
				g.Expect(gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds", map[string]string{"vin": vin, "field": "soc"})).To(gomega.BeNumerically(">", 0))
			}
		})
	}
}

func TestProducerFiltersAndExpiresAllVINSeries(t *testing.T) {
	for _, test := range []struct {
		name string
		vins []string
	}{
		{name: "filtered", vins: []string{"ACTIVEVIN"}},
		{name: "omitted"},
		{name: "empty", vins: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			logger, _ := logrus.NoOpLogger()
			collector := newTestCollector(t)
			producer, err := promproducer.NewProducer(collector, logger, test.vins, 500*time.Millisecond)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			t.Cleanup(func() { _ = producer.Close() })

			send(t, producer, "OTHER", &protos.Datum{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 20}}})
			send(t, producer, "ACTIVEVIN", &protos.Datum{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 80}}})
			send(t, producer, "ACTIVEVIN", &protos.Datum{Key: protos.Field_ChargeState, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "Charging"}}})
			send(t, producer, "ACTIVEVIN", &protos.Datum{Key: protos.Field_CarType, Value: &protos.Value{Value: &protos.Value_CarTypeValue{CarTypeValue: protos.CarTypeValue_CarTypeModelY}}})

			read := func(metric string, labels map[string]string) float64 {
				mfs, err := promclient.DefaultGatherer.Gather()
				g.Expect(err).ToNot(gomega.HaveOccurred())
				return gaugeValueForLabels(mfs, metric, labels)
			}
			otherValue := float64(20)
			if len(test.vins) > 0 {
				otherValue = -1
			}
			g.Expect(read("vehicle_battery_soc", map[string]string{"vin": "OTHER"})).To(gomega.Equal(otherValue))
			g.Expect(read("vehicle_battery_soc", map[string]string{"vin": "ACTIVEVIN"})).To(gomega.Equal(float64(80)))
			g.Expect(read("vehicle_charge_state", map[string]string{"vin": "ACTIVEVIN", "state": "Charging"})).To(gomega.Equal(float64(1)))
			g.Expect(read("vehicle_telemetry_updated_timestamp_seconds", map[string]string{"vin": "ACTIVEVIN", "field": "soc"})).To(gomega.BeNumerically(">", 0))
			// The info label set is long; a VIN lookup is enough to prove the series exists.
			g.Expect(read("vehicle_info", map[string]string{"vin": "ACTIVEVIN"})).To(gomega.Equal(float64(1)))

			g.Eventually(func() bool {
				return read("vehicle_battery_soc", map[string]string{"vin": "OTHER"}) == -1 &&
					read("vehicle_battery_soc", map[string]string{"vin": "ACTIVEVIN"}) == -1 &&
					read("vehicle_charge_state", map[string]string{"vin": "ACTIVEVIN", "state": "Charging"}) == -1 &&
					read("vehicle_telemetry_updated_timestamp_seconds", map[string]string{"vin": "ACTIVEVIN", "field": "soc"}) == -1 &&
					read("vehicle_info", map[string]string{"vin": "ACTIVEVIN"}) == -1
			}, 2*time.Second, 20*time.Millisecond).Should(gomega.BeTrue())

			// A reconnect starts with fresh enum state, not a predecessor from the old session.
			send(t, producer, "ACTIVEVIN", &protos.Datum{Key: protos.Field_ChargeState, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "Idle"}}})
			g.Expect(read("vehicle_charge_state", map[string]string{"vin": "ACTIVEVIN", "state": "Idle"})).To(gomega.Equal(float64(1)))
			g.Expect(read("vehicle_charge_state", map[string]string{"vin": "ACTIVEVIN", "state": "Charging"})).To(gomega.Equal(float64(-1)))

		})
	}
}

func TestProduceSetsMetrics(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()

	metricsCollector := newTestCollector(t)
	producer, err := newTestProducer(t, metricsCollector, logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	payload := &protos.Payload{
		Vin: "TESTVIN",
		Data: []*protos.Datum{
			{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 84.71}}},
			{Key: protos.Field_ChargeLimitSoc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 90}}},
			{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 37.4, Longitude: -122.1}}}},
		},
	}
	payloadBytes, err := proto.Marshal(payload)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	msg := messages.StreamMessage{TXID: []byte("tx1"), DeviceID: []byte("TESTVIN"), SenderID: []byte("vehicle_device.TESTVIN"), MessageTopic: []byte("V"), Payload: payloadBytes}
	recordMsg, err := msg.ToBytes()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: "TESTVIN", SenderID: "vehicle_device.TESTVIN"}, map[string][]telemetry.Producer{"V": nil}, logger)
	record, err := telemetry.NewRecord(serializer, recordMsg, "1", true)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	producer.Produce(record)

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	g.Expect(gaugeValue(mfs, "vehicle_battery_soc", "TESTVIN")).To(gomega.BeNumerically("~", 84.71, 0.01))
	g.Expect(gaugeValue(mfs, "vehicle_charge_limit_soc", "TESTVIN")).To(gomega.Equal(float64(90)))
	g.Expect(gaugeValue(mfs, "vehicle_location_latitude_degrees", "TESTVIN")).To(gomega.Equal(float64(37.4)))
	g.Expect(gaugeValue(mfs, "vehicle_location_longitude_degrees", "TESTVIN")).To(gomega.Equal(float64(-122.1)))
}

// TestProduceSetsFieldUpdateTimestamps checks that vehicle_telemetry_updated_timestamp_seconds
// is set only for fields actually present on the record, so a consumer can
// tell a field that hasn't been reported in a while from one just scraped.
func TestProduceSetsFieldUpdateTimestamps(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()

	metricsCollector := newTestCollector(t)
	producer, err := newTestProducer(t, metricsCollector, logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	before := float64(time.Now().Unix())
	payload := &protos.Payload{
		Vin: "TESTVIN2",
		Data: []*protos.Datum{
			{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 55}}},
			{Key: protos.Field_ChargeState, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "Charging"}}},
		},
	}
	payloadBytes, err := proto.Marshal(payload)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	msg := messages.StreamMessage{TXID: []byte("tx2"), DeviceID: []byte("TESTVIN2"), SenderID: []byte("vehicle_device.TESTVIN2"), MessageTopic: []byte("V"), Payload: payloadBytes}
	recordMsg, err := msg.ToBytes()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: "TESTVIN2", SenderID: "vehicle_device.TESTVIN2"}, map[string][]telemetry.Producer{"V": nil}, logger)
	record, err := telemetry.NewRecord(serializer, recordMsg, "1", true)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	producer.Produce(record)
	after := float64(time.Now().Unix())

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	soc := gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds",
		map[string]string{"vin": "TESTVIN2", "field": "soc"})
	g.Expect(soc).To(gomega.SatisfyAll(gomega.BeNumerically(">=", before), gomega.BeNumerically("<=", after)))

	chargeState := gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds",
		map[string]string{"vin": "TESTVIN2", "field": "charge_state"})
	g.Expect(chargeState).To(gomega.SatisfyAll(gomega.BeNumerically(">=", before), gomega.BeNumerically("<=", after)))

	// Not present on this record, so no timestamp should have been set.
	location := gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds",
		map[string]string{"vin": "TESTVIN2", "field": "location"})
	g.Expect(location).To(gomega.Equal(float64(-1)))
}

// TestChargeStateTransitionZeroesPreviousValue is a regression test: a stale
// "Idle" series stuck at 1 from before a charge started previously caused a
// consumer that picks "whichever series is 1" to see the car as idle while
// it was actually charging.
func TestChargeStateTransitionZeroesPreviousValue(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()

	metricsCollector := newTestCollector(t)
	producer, err := newTestProducer(t, metricsCollector, logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	send := func(vin, state string) {
		payload := &protos.Payload{
			Vin: vin,
			Data: []*protos.Datum{
				{Key: protos.Field_ChargeState, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: state}}},
			},
		}
		payloadBytes, err := proto.Marshal(payload)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		msg := messages.StreamMessage{TXID: []byte("tx"), DeviceID: []byte(vin), SenderID: []byte("vehicle_device." + vin), MessageTopic: []byte("V"), Payload: payloadBytes}
		recordMsg, err := msg.ToBytes()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: vin, SenderID: "vehicle_device." + vin}, map[string][]telemetry.Producer{"V": nil}, logger)
		record, err := telemetry.NewRecord(serializer, recordMsg, "1", true)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		producer.Produce(record)
	}

	const vin = "TRANSITIONVIN"
	send(vin, "Idle")
	send(vin, "Enable") // car starts charging

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	g.Expect(gaugeValueForLabels(mfs, "vehicle_charge_state",
		map[string]string{"vin": vin, "state": "Enable"})).To(gomega.Equal(float64(1)),
		"the current state must read as active")
	g.Expect(gaugeValueForLabels(mfs, "vehicle_charge_state",
		map[string]string{"vin": vin, "state": "Idle"})).To(gomega.Equal(float64(0)),
		"the previous state must be reset, not left stuck at 1")
}

// TestChargingCableTransitionZeroesPreviousValue mirrors
// TestChargeStateTransitionZeroesPreviousValue for vehicle_charging_cable --
// the other enum-style gauge in this producer, sharing the same
// setExclusiveState fix. Nothing previously exercised ChargingCableType at
// all, so this also closes that coverage gap.
func TestChargingCableTransitionZeroesPreviousValue(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()

	metricsCollector := newTestCollector(t)
	producer, err := newTestProducer(t, metricsCollector, logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	send := func(vin, cable string) {
		payload := &protos.Payload{
			Vin: vin,
			Data: []*protos.Datum{
				{Key: protos.Field_ChargingCableType, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: cable}}},
			},
		}
		payloadBytes, err := proto.Marshal(payload)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		msg := messages.StreamMessage{TXID: []byte("tx"), DeviceID: []byte(vin), SenderID: []byte("vehicle_device." + vin), MessageTopic: []byte("V"), Payload: payloadBytes}
		recordMsg, err := msg.ToBytes()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: vin, SenderID: "vehicle_device." + vin}, map[string][]telemetry.Producer{"V": nil}, logger)
		record, err := telemetry.NewRecord(serializer, recordMsg, "1", true)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		producer.Produce(record)
	}

	const vin = "CABLETRANSITIONVIN"
	send(vin, "IEC")
	send(vin, "SAE")

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	g.Expect(gaugeValueForLabels(mfs, "vehicle_charging_cable",
		map[string]string{"vin": vin, "cable": "SAE"})).To(gomega.Equal(float64(1)),
		"the current cable must read as active")
	g.Expect(gaugeValueForLabels(mfs, "vehicle_charging_cable",
		map[string]string{"vin": vin, "cable": "IEC"})).To(gomega.Equal(float64(0)),
		"the previous cable must be reset, not left stuck at 1")

	touchedAt := gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds",
		map[string]string{"vin": vin, "field": "charging_cable"})
	g.Expect(touchedAt).To(gomega.BeNumerically(">", 0), "charging_cable's update timestamp must also be touched")
}

// send builds and produces a single-field 'V' record, for the tests below
// that don't need TestChargeStateTransitionZeroesPreviousValue's multi-field
// shape.
func send(t *testing.T, producer telemetry.Producer, vin string, datum *protos.Datum) {
	t.Helper()
	g := gomega.NewGomegaWithT(t)
	payload := &protos.Payload{Vin: vin, Data: []*protos.Datum{datum}}
	payloadBytes, err := proto.Marshal(payload)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	logger, _ := logrus.NoOpLogger()
	msg := messages.StreamMessage{TXID: []byte("tx"), DeviceID: []byte(vin), SenderID: []byte("vehicle_device." + vin), MessageTopic: []byte("V"), Payload: payloadBytes}
	recordMsg, err := msg.ToBytes()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: vin, SenderID: "vehicle_device." + vin}, map[string][]telemetry.Producer{"V": nil}, logger)
	record, err := telemetry.NewRecord(serializer, recordMsg, "1", true)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	producer.Produce(record)
}

// TestProducePreservesFractionalMeasurements checks that fractional telemetry
// values survive gauge dispatch in their named physical unit.
func TestProducePreservesFractionalMeasurements(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "SCALEVIN"
	send(t, producer, vin, &protos.Datum{Key: protos.Field_InsideTemp, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 21.5}}})
	send(t, producer, vin, &protos.Datum{Key: protos.Field_ChargerVoltage, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 231.725}}})
	send(t, producer, vin, &protos.Datum{Key: protos.Field_TpmsPressureFl, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 2.5}}})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_temp_celsius",
		map[string]string{"vin": vin, "location": "inside"})).To(gomega.Equal(float64(21.5)))
	g.Expect(gaugeValue(mfs, "vehicle_charger_voltage_volts", vin)).To(gomega.BeNumerically("~", 231.725, 0.00001))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_tire_pressure_pascals",
		map[string]string{"vin": vin, "tire": "front_left"})).To(gomega.Equal(float64(250000)))
}

// TestProduceHandlesDoorBooleans checks the independent-booleans path (no
// exclusive-state bookkeeping needed, unlike enum fields): every door in the
// fixed set is set directly from the payload each time it's present.
func TestProduceHandlesDoorBooleans(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "DOORVIN"
	send(t, producer, vin, &protos.Datum{Key: protos.Field_DoorState, Value: &protos.Value{Value: &protos.Value_DoorValue{
		DoorValue: &protos.Doors{DriverFront: true, TrunkRear: false},
	}}})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_door_open", map[string]string{"vin": vin, "door": "driver_front"})).To(gomega.Equal(float64(1)))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_door_open", map[string]string{"vin": vin, "door": "trunk_rear"})).To(gomega.Equal(float64(0)))
}

// TestProduceWindowFamilyTransitionZeroesPreviousValue exercises a family of
// four independent exclusive-enum fields (FdWindow/FpWindow/RdWindow/RpWindow)
// sharing one metric name -- each window's own state transition must only
// zero *that* window's previous value, not affect the others.
func TestProduceWindowFamilyTransitionZeroesPreviousValue(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "WINDOWVIN"
	sendWindow := func(field protos.Field, state protos.WindowState) {
		send(t, producer, vin, &protos.Datum{Key: field, Value: &protos.Value{Value: &protos.Value_WindowStateValue{WindowStateValue: state}}})
	}
	sendWindow(protos.Field_FdWindow, protos.WindowState_WindowStateClosed)
	sendWindow(protos.Field_FpWindow, protos.WindowState_WindowStateClosed)
	sendWindow(protos.Field_FdWindow, protos.WindowState_WindowStateOpened) // only the driver window opens

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_window_state",
		map[string]string{"vin": vin, "window": "front_driver", "state": "WindowStateOpened"})).To(gomega.Equal(float64(1)))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_window_state",
		map[string]string{"vin": vin, "window": "front_driver", "state": "WindowStateClosed"})).To(gomega.Equal(float64(0)),
		"the driver window's previous state must be zeroed")
	g.Expect(gaugeValueForLabels(mfs, "vehicle_window_state",
		map[string]string{"vin": vin, "window": "front_passenger", "state": "WindowStateClosed"})).To(gomega.Equal(float64(1)),
		"the passenger window must be unaffected by the driver window's transition")
}

// TestProduceConvertsTimeOfDay checks the HH:MM:SS -> seconds-since-midnight
// conversion used for scheduling fields, chosen specifically to avoid
// modeling an effectively-unbounded string as a label (see fields.go).
// TestProduceScheduledTimeFromTimestamp covers the encoding actually observed
// in the field: ScheduledChargingStartTime arrives as an int64 Unix
// timestamp, not the clock string the registry first assumed. Before this was
// handled the field was dropped and an error logged on every payload -- but
// only once a charging schedule was actually set on the car, which is why it
// survived the first round of production testing.
//
// 1790046000 is a real captured value: a 20:00 Mon-Fri schedule on a car in
// Zurich, which Tesla encodes as 20:00 Pacific (2026-09-21 20:00 PDT).
func TestProduceScheduledTimeFromTimestamp(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, hook := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "SCHEDVIN"
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_ScheduledChargingStartTime,
		Value: &protos.Value{Value: &protos.Value_LongValue{LongValue: 1790046000}},
	})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_scheduled_charging_start_timestamp_seconds",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(1790046000)))
	// The clock-string metric must stay untouched: the two encodings mean
	// different things and sharing a series would silently mix units.
	g.Expect(gaugeValueForLabels(mfs, "vehicle_scheduled_charging_start_seconds_since_midnight",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(-1)))
	for _, entry := range hook.AllEntries() {
		g.Expect(entry.Message).ToNot(gomega.Equal("prometheus_unhandled_field_value"))
	}
}

func TestProduceConvertsTimeOfDay(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "TIMEVIN"
	send(t, producer, vin, &protos.Datum{Key: protos.Field_ScheduledChargingStartTime, Value: &protos.Value{Value: &protos.Value_TimeValue{
		TimeValue: &protos.Time{Hour: 14, Minute: 30, Second: 0},
	}}})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_scheduled_charging_start_seconds_since_midnight",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(14*3600 + 30*60)))
}

// TestProduceLocationFamily checks that OriginLocation/DestinationLocation
// share a metric family distinct from the vehicle_location_* metrics, which
// must keep their own names rather than being folded into the family.
func TestProduceLocationFamily(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "TRIPVIN"
	send(t, producer, vin, &protos.Datum{Key: protos.Field_OriginLocation, Value: &protos.Value{Value: &protos.Value_LocationValue{
		LocationValue: &protos.LocationValue{Latitude: 47.39, Longitude: 8.63},
	}}})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_trip_location_latitude_degrees",
		map[string]string{"vin": vin, "kind": "origin"})).To(gomega.Equal(float64(47.39)))
}

// TestProduceInfoGaugeTracksChanges is the vehicle_info analogue of
// TestChargeStateTransitionZeroesPreviousValue: vehicle_info's series
// identity is its *entire* label combination, so a single field changing
// (e.g. a firmware update) must zero the old combination, not leave it stuck
// at 1 alongside the new one.
func TestProduceInfoGaugeTracksChanges(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "INFOVIN"
	send(t, producer, vin, &protos.Datum{Key: protos.Field_CarType, Value: &protos.Value{Value: &protos.Value_CarTypeValue{CarTypeValue: protos.CarTypeValue_CarTypeModelY}}})
	send(t, producer, vin, &protos.Datum{Key: protos.Field_Version, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "2024.44.25"}}})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_info",
		map[string]string{"vin": vin, "car_type": "CarTypeModelY", "version": "2024.44.25"})).To(gomega.Equal(float64(1)),
		"the current combination (both fields known) must read as active")
	g.Expect(gaugeValueForLabels(mfs, "vehicle_info",
		map[string]string{"vin": vin, "car_type": "CarTypeModelY", "version": ""})).To(gomega.Equal(float64(0)),
		"the combination from before Version was known must be zeroed, not left stuck at 1")

	// A later firmware update must zero the old combination the same way.
	send(t, producer, vin, &protos.Datum{Key: protos.Field_Version, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "2024.44.32"}}})
	mfs, err = promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_info",
		map[string]string{"vin": vin, "car_type": "CarTypeModelY", "version": "2024.44.32"})).To(gomega.Equal(float64(1)))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_info",
		map[string]string{"vin": vin, "car_type": "CarTypeModelY", "version": "2024.44.25"})).To(gomega.Equal(float64(0)),
		"the pre-update version combination must be zeroed")
}

// gaugeValue finds the value of the gauge named metricName with a "vin" label
// matching vin, failing the calling test if it isn't found.
func gaugeValue(mfs []*dto.MetricFamily, metricName, vin string) float64 {
	return gaugeValueForLabels(mfs, metricName, map[string]string{"vin": vin})
}

// gaugeValueForLabels finds the value of the gauge named metricName whose
// labels are a superset of wantLabels, or -1 if no such series exists.
func gaugeValueForLabels(mfs []*dto.MetricFamily, metricName string, wantLabels map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, label := range m.GetLabel() {
				got[label.GetName()] = label.GetValue()
			}
			matches := true
			for k, v := range wantLabels {
				if got[k] != v {
					matches = false
					break
				}
			}
			if matches {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

// TestInvalidValueProducesNothing covers the case that sent this to
// production: ScheduledChargingStartTime reports Value_Invalid whenever
// nothing is scheduled, which PayloadToMap renders as the string "<invalid>".
// That is normal vehicle behaviour, not a malformed payload -- it must not
// produce a metric, must not stamp a freshness timestamp (no value arrived),
// and must not be logged as an error.
func TestInvalidValueProducesNothing(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, hook := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "INVALIDVIN"
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_ScheduledChargingStartTime,
		Value: &protos.Value{Value: &protos.Value_Invalid{Invalid: true}},
	})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_scheduled_charging_start_seconds_since_midnight",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(-1)))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_telemetry_updated_timestamp_seconds",
		map[string]string{"vin": vin, "field": "scheduled_charging_start_time"})).To(gomega.Equal(float64(-1)))

	// The observable difference between handling this and merely falling
	// through the unhandled-value path is the log line, so assert on it --
	// otherwise this test passes with or without the fix. A car with no
	// charging schedule reports this on every payload, which is a lot of
	// noise to pour into an error log for normal behaviour.
	for _, entry := range hook.AllEntries() {
		g.Expect(entry.Message).ToNot(gomega.Equal("prometheus_unhandled_field_value"))
	}
}

// TestInvalidValueDoesNotBecomeAnEnumLabel guards the sharper edge of the
// same bug. The string-typed kinds would accept "<invalid>" as a perfectly
// good value, so an invalid report would mint a bogus series and -- via
// setExclusiveState -- zero the real one, making the car look like it had
// changed state when it had not.
func TestInvalidValueDoesNotBecomeAnEnumLabel(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "INVALIDENUMVIN"
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_ChargeState,
		Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "Enable"}},
	})
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_ChargeState,
		Value: &protos.Value{Value: &protos.Value_Invalid{Invalid: true}},
	})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_charge_state",
		map[string]string{"vin": vin, "state": "<invalid>"})).To(gomega.Equal(float64(-1)))
	g.Expect(gaugeValueForLabels(mfs, "vehicle_charge_state",
		map[string]string{"vin": vin, "state": "Enable"})).To(gomega.Equal(float64(1)))
}

// TestProduceBooleanAcceptsNumericFlag covers fields whose name reads like a
// flag but whose wire encoding Tesla does not document. A strict bool
// assertion would drop such a field and log an error on every payload, which
// is how the ScheduledChargingStartTime encoding mismatch first surfaced.
func TestProduceBooleanAcceptsNumericFlag(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, hook := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "BOOLNUMVIN"
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_SoftwareUpdateAvailable,
		Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 1}},
	})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_software_update_available",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(1)))
	for _, entry := range hook.AllEntries() {
		g.Expect(entry.Message).ToNot(gomega.Equal("prometheus_unhandled_field_value"))
	}
}

// TestProduceBooleanRejectsNonFlagNumeric is the other half: a value outside
// {0,1} means the registry entry is wrong about the field being a flag, and
// coercing it would silently publish a bogus 0/1 series.
func TestProduceBooleanRejectsNonFlagNumeric(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	logger, _ := logrus.NoOpLogger()
	producer, err := newTestProducer(t, newTestCollector(t), logger)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	const vin = "BOOLBADVIN"
	send(t, producer, vin, &protos.Datum{
		Key:   protos.Field_SoftwareUpdateInProgress,
		Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 42}},
	})

	mfs, err := promclient.DefaultGatherer.Gather()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(gaugeValueForLabels(mfs, "vehicle_software_update_in_progress",
		map[string]string{"vin": vin})).To(gomega.Equal(float64(-1)))
}
