package prometheus

import (
	"regexp"
	"strings"

	"github.com/teslamotors/fleet-telemetry/protos"
)

// fieldKind describes how a field's value becomes one or more Prometheus
// series. See the comment on fieldRegistry for the reasoning behind each kind.
type fieldKind int

const (
	// kindExcluded means this field is intentionally not exposed. reason
	// documents why (free text/unbounded cardinality, reserved/placeholder,
	// documented-broken, or deferred to a follow-up PR).
	kindExcluded fieldKind = iota
	// kindNumeric is a plain scalar value (bool, int, or float) set directly
	// on a gauge, optionally scaled to preserve sub-unit precision within
	// adapter.Gauge's int64 constraint (see scale).
	kindNumeric
	// kindBoolean is a bool value set as 0/1.
	kindBoolean
	// kindEnum is a small, bounded set of named states (e.g. charge state,
	// gear, cable type) modeled as one gauge series per possible value, set
	// to 1 for whichever is current. This requires setExclusiveState -- see
	// the package-level comment on that function for why.
	kindEnum
	// kindBoolMap is a fixed set of independently-reported booleans (doors,
	// tire positions) that arrive together as a single field. Unlike
	// kindEnum, these are not mutually exclusive, so no exclusive-state
	// bookkeeping is needed -- the full set is simply re-set every time the
	// field is present.
	kindBoolMap
	// kindScheduledTime is a scheduled time, which Tesla reports in either
	// of two encodings that are not interchangeable, so each gets its own
	// metric suffixed onto spec.metric:
	//
	//   int64 Unix timestamp  -> "<metric>_timestamp_seconds"
	//   "HH:MM:SS" wall clock -> "<metric>_seconds_since_midnight"
	//
	// ScheduledChargingStartTime was observed sending the former
	// (2026-09-21, firmware 2026.32.3); the proto's TimeValue produces the
	// latter. Both carry a documented Tesla quirk: the hour is expressed in
	// Pacific Time wherever the vehicle actually is. A schedule set for
	// 20:00 in Zurich arrives as an epoch reading 20:00 PDT -- 05:00 the
	// next day locally -- so the timestamp is NOT a true instant and must
	// not be plotted as one. The wall-clock time in Pacific is the part
	// that carries the user's intent.
	kindScheduledTime
	// kindLocation is a lat/lon pair, reported in degrees across two
	// float-valued gauges.
	kindLocation
	// kindInfo contributes one label to the single, rarely-changing
	// vehicle_info gauge (CarType, Trim, firmware version, etc.) rather than
	// its own time series.
	kindInfo
)

// fieldSpec describes how one protos.Field becomes Prometheus output.
type fieldSpec struct {
	kind fieldKind

	// metric is the Prometheus metric name. Several fields may share one
	// metric name as a "family" distinguished by extraLabel/extraValue (e.g.
	// all four TpmsPressure* fields share vehicle_tire_pressure_pascals,
	// with extraLabel "tire"). For kindLocation, metric is a prefix and
	// "_latitude_degrees"/"_longitude_degrees" is appended.
	metric string

	// extraLabel/extraValue add one fixed label identifying which member of
	// a family this field is (e.g. extraLabel "tire", extraValue
	// "front_left"). Leave both empty for a field with no family.
	extraLabel string
	extraValue string

	// valueLabel names the label that holds the field's own reported value.
	// Required for kindEnum (e.g. "state", "mode", "status").
	valueLabel string

	// boolMapLabel names the label that identifies each element of a
	// kindBoolMap field's value (e.g. "door", "tire").
	boolMapLabel string

	// infoLabel names the label this field contributes on the shared
	// vehicle_info gauge. Required for kindInfo.
	infoLabel string

	// scale converts a kindNumeric value to the metric unit (e.g. 1000
	// for kW->W / kWh->Wh, or 100000 for bar->Pa). Zero means 1.
	scale int64

	// touchKey overrides the automatically-derived (from the field's proto
	// name) "field" label used on vehicle_telemetry_updated_timestamp_seconds.
	// Only set where the derived value would differ from what's already
	// shipped in dashboards (see camelToSnake below).
	touchKey string

	// reason documents, for kindExcluded fields, why this field isn't
	// exposed. Surfaced in tests so an exclusion can't be accidental.
	reason string
}

func (s fieldSpec) touchKeyOrDefault(field protos.Field) string {
	if s.touchKey != "" {
		return s.touchKey
	}
	return camelToSnake(field.String())
}

var (
	acronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	wordBoundary    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// camelToSnake converts a Go-style field name (as produced by protos.Field's
// String(), e.g. "ACChargingPower", "BMSState") into snake_case, handling
// acronym runs. Used to derive registry defaults; a handful of already-shipped
// values are pinned via fieldSpec.touchKey instead of relying on this.
func camelToSnake(s string) string {
	s = acronymBoundary.ReplaceAllString(s, "${1}_${2}")
	s = wordBoundary.ReplaceAllString(s, "${1}_${2}")
	return strings.ToLower(s)
}

// excluded is a shorthand for building a documented kindExcluded entry.
func excluded(reason string) fieldSpec {
	return fieldSpec{kind: kindExcluded, reason: reason}
}

const (
	reasonReserved       = "reserved/placeholder field name in the proto, never carries real data"
	reasonFreeText       = "unbounded free-text value -- modeling it as a label would leak content into the monitoring stack and grow cardinality forever"
	reasonSemiOnly       = "Tesla Semi-only field, deferred to a follow-up PR"
	reasonDrivetrainDiag = "low-level drivetrain diagnostic field, deferred to a follow-up PR"
	reasonBroken         = "documented by Tesla as broken and not returning data (see developer.tesla.com fleet-telemetry available-data docs)"
	reasonBlob           = "not a scalar value (encoded route geometry) -- wrong shape for a Prometheus label or gauge"
)

// fieldRegistry maps known protos.Field entries to how each becomes Prometheus
// output. Excluded fields document a specific reason why they are not exported;
// unmapped fields not present in this registry are ignored.
//
// Renaming a metric that has already shipped silently breaks every dashboard
// and alert built on it, so entries here are append-only in practice: add
// fields, but do not rename existing metrics or labels.
var fieldRegistry = map[protos.Field]fieldSpec{
	protos.Field_Unknown: excluded(reasonReserved),

	// --- Charging & energy ---
	protos.Field_Soc:                                {kind: kindNumeric, metric: "vehicle_battery_soc", touchKey: "soc"},
	protos.Field_ChargeState:                        {kind: kindEnum, metric: "vehicle_charge_state", valueLabel: "state", touchKey: "charge_state"},
	protos.Field_ChargeLimitSoc:                     {kind: kindNumeric, metric: "vehicle_charge_limit_soc", touchKey: "charge_limit"},
	protos.Field_ChargingCableType:                  {kind: kindEnum, metric: "vehicle_charging_cable", valueLabel: "cable", touchKey: "charging_cable"},
	protos.Field_DetailedChargeState:                {kind: kindEnum, metric: "vehicle_detailed_charge_state", valueLabel: "state"},
	protos.Field_BatteryLevel:                       {kind: kindNumeric, metric: "vehicle_battery_level_percent"},
	protos.Field_ChargeAmps:                         {kind: kindNumeric, metric: "vehicle_charge_current_amperes"},
	protos.Field_ChargeCurrentRequest:               {kind: kindNumeric, metric: "vehicle_charge_current_request_amperes", extraLabel: "kind", extraValue: "requested"},
	protos.Field_ChargeCurrentRequestMax:            {kind: kindNumeric, metric: "vehicle_charge_current_request_amperes", extraLabel: "kind", extraValue: "max"},
	protos.Field_ChargeEnableRequest:                {kind: kindBoolean, metric: "vehicle_charge_enable_request"},
	protos.Field_ChargePort:                         {kind: kindEnum, metric: "vehicle_charge_port_state", valueLabel: "state"},
	protos.Field_ChargePortColdWeatherMode:          {kind: kindBoolean, metric: "vehicle_charge_port_cold_weather_mode"},
	protos.Field_ChargePortDoorOpen:                 {kind: kindBoolean, metric: "vehicle_charge_port_door_open"},
	protos.Field_ChargePortLatch:                    {kind: kindEnum, metric: "vehicle_charge_port_latch_state", valueLabel: "state"},
	protos.Field_ChargeRateMilePerHour:              {kind: kindNumeric, metric: "vehicle_charge_rate_miles_per_hour"},
	protos.Field_ChargerPhases:                      {kind: kindNumeric, metric: "vehicle_charger_phases"},
	protos.Field_ChargerVoltage:                     {kind: kindNumeric, metric: "vehicle_charger_voltage_volts"},
	protos.Field_ACChargingPower:                    {kind: kindNumeric, metric: "vehicle_charging_power_watts", extraLabel: "source", extraValue: "ac", scale: 1000},
	protos.Field_DCChargingPower:                    {kind: kindNumeric, metric: "vehicle_charging_power_watts", extraLabel: "source", extraValue: "dc", scale: 1000},
	protos.Field_ACChargingEnergyIn:                 {kind: kindNumeric, metric: "vehicle_charging_energy_in_watt_hours", extraLabel: "source", extraValue: "ac", scale: 1000},
	protos.Field_DCChargingEnergyIn:                 {kind: kindNumeric, metric: "vehicle_charging_energy_in_watt_hours", extraLabel: "source", extraValue: "dc", scale: 1000},
	protos.Field_FastChargerPresent:                 {kind: kindBoolean, metric: "vehicle_fast_charger_present"},
	protos.Field_FastChargerType:                    {kind: kindEnum, metric: "vehicle_fast_charger_type", valueLabel: "type"},
	protos.Field_TimeToFullCharge:                   {kind: kindNumeric, metric: "vehicle_time_to_full_charge_minutes", scale: 60},
	protos.Field_EstimatedHoursToChargeTermination:  {kind: kindNumeric, metric: "vehicle_estimated_time_to_charge_termination_minutes", scale: 60},
	protos.Field_EstBatteryRange:                    {kind: kindNumeric, metric: "vehicle_battery_range_miles", extraLabel: "kind", extraValue: "estimated"},
	protos.Field_IdealBatteryRange:                  {kind: kindNumeric, metric: "vehicle_battery_range_miles", extraLabel: "kind", extraValue: "ideal"},
	protos.Field_RatedRange:                         {kind: kindNumeric, metric: "vehicle_battery_range_miles", extraLabel: "kind", extraValue: "rated"},
	protos.Field_EnergyRemaining:                    {kind: kindNumeric, metric: "vehicle_energy_remaining_watt_hours", scale: 1000},
	protos.Field_ExpectedEnergyPercentAtTripArrival: {kind: kindNumeric, metric: "vehicle_expected_energy_percent_at_trip_arrival"},
	protos.Field_NotEnoughPowerToHeat:               {kind: kindBoolean, metric: "vehicle_not_enough_power_to_heat"},
	protos.Field_SuperchargerSessionTripPlanner:     {kind: kindBoolean, metric: "vehicle_supercharger_session_trip_planner"},
	protos.Field_ScheduledChargingMode:              {kind: kindEnum, metric: "vehicle_scheduled_charging_mode", valueLabel: "mode"},
	protos.Field_ScheduledChargingPending:           {kind: kindBoolean, metric: "vehicle_scheduled_charging_pending"},
	protos.Field_ScheduledChargingStartTime:         {kind: kindScheduledTime, metric: "vehicle_scheduled_charging_start"},
	protos.Field_ScheduledDepartureTime:             {kind: kindScheduledTime, metric: "vehicle_scheduled_departure"},
	protos.Field_LifetimeEnergyUsed:                 {kind: kindNumeric, metric: "vehicle_lifetime_energy_used_watt_hours", scale: 1000},
	protos.Field_LifetimeEnergyGainedRegen:          {kind: kindNumeric, metric: "vehicle_lifetime_energy_gained_regen_watt_hours", scale: 1000},
	protos.Field_LifetimeEnergyChargedKwh:           {kind: kindNumeric, metric: "vehicle_lifetime_energy_charged_watt_hours", scale: 1000},
	protos.Field_PowershareHoursLeft:                {kind: kindNumeric, metric: "vehicle_powershare_hours_left"},
	protos.Field_PowershareInstantaneousPowerKW:     {kind: kindNumeric, metric: "vehicle_powershare_instantaneous_power_watts", scale: 1000},
	protos.Field_PowershareStatus:                   {kind: kindEnum, metric: "vehicle_powershare_status", valueLabel: "status"},
	protos.Field_PowershareStopReason:               {kind: kindEnum, metric: "vehicle_powershare_stop_reason", valueLabel: "reason"},
	protos.Field_PowershareType:                     {kind: kindEnum, metric: "vehicle_powershare_type", valueLabel: "type"},

	// --- Battery / BMS internals ---
	protos.Field_PackVoltage:              {kind: kindNumeric, metric: "vehicle_pack_voltage_volts"},
	protos.Field_PackCurrent:              {kind: kindNumeric, metric: "vehicle_pack_current_amperes"},
	protos.Field_BrickVoltageMax:          {kind: kindNumeric, metric: "vehicle_brick_voltage_volts", extraLabel: "kind", extraValue: "max"},
	protos.Field_BrickVoltageMin:          {kind: kindNumeric, metric: "vehicle_brick_voltage_volts", extraLabel: "kind", extraValue: "min"},
	protos.Field_NumBrickVoltageMax:       {kind: kindNumeric, metric: "vehicle_brick_index", extraLabel: "extreme", extraValue: "max_voltage"},
	protos.Field_NumBrickVoltageMin:       {kind: kindNumeric, metric: "vehicle_brick_index", extraLabel: "extreme", extraValue: "min_voltage"},
	protos.Field_ModuleTempMax:            {kind: kindNumeric, metric: "vehicle_module_temp_celsius", extraLabel: "kind", extraValue: "max"},
	protos.Field_ModuleTempMin:            {kind: kindNumeric, metric: "vehicle_module_temp_celsius", extraLabel: "kind", extraValue: "min"},
	protos.Field_NumModuleTempMax:         {kind: kindNumeric, metric: "vehicle_module_index", extraLabel: "extreme", extraValue: "max_temp"},
	protos.Field_NumModuleTempMin:         {kind: kindNumeric, metric: "vehicle_module_index", extraLabel: "extreme", extraValue: "min_temp"},
	protos.Field_IsolationResistance:      {kind: kindNumeric, metric: "vehicle_isolation_resistance", reason: "unit not documented by Tesla; exposed unscaled as reported"},
	protos.Field_BMSState:                 {kind: kindEnum, metric: "vehicle_bms_state", valueLabel: "state"},
	protos.Field_BmsFullchargecomplete:    {kind: kindBoolean, metric: "vehicle_bms_full_charge_complete"},
	protos.Field_BrickSocMinPercent:       {kind: kindNumeric, metric: "vehicle_brick_soc_min_percent"},
	protos.Field_NominalFullPackEnergyKwh: {kind: kindNumeric, metric: "vehicle_nominal_full_pack_energy_watt_hours", scale: 1000},
	protos.Field_BatteryHeaterOn:          {kind: kindBoolean, metric: "vehicle_battery_heater_on"},

	// --- Location & navigation ---
	protos.Field_Location:                      {kind: kindLocation, metric: "vehicle_location"},
	protos.Field_OriginLocation:                {kind: kindLocation, metric: "vehicle_trip_location", extraLabel: "kind", extraValue: "origin"},
	protos.Field_DestinationLocation:           {kind: kindLocation, metric: "vehicle_trip_location", extraLabel: "kind", extraValue: "destination"},
	protos.Field_GpsHeading:                    {kind: kindNumeric, metric: "vehicle_gps_heading_degrees"},
	protos.Field_GpsState:                      {kind: kindBoolean, metric: "vehicle_gps_locked"},
	protos.Field_GpsAccuracyMeters:             {kind: kindNumeric, metric: "vehicle_gps_accuracy_meters"},
	protos.Field_LocatedAtHome:                 {kind: kindBoolean, metric: "vehicle_located_at", extraLabel: "place", extraValue: "home"},
	protos.Field_LocatedAtWork:                 {kind: kindBoolean, metric: "vehicle_located_at", extraLabel: "place", extraValue: "work"},
	protos.Field_LocatedAtFavorite:             {kind: kindBoolean, metric: "vehicle_located_at", extraLabel: "place", extraValue: "favorite"},
	protos.Field_MilesToArrival:                {kind: kindNumeric, metric: "vehicle_trip_remaining", extraLabel: "unit", extraValue: "miles"},
	protos.Field_MinutesToArrival:              {kind: kindNumeric, metric: "vehicle_trip_remaining", extraLabel: "unit", extraValue: "minutes"},
	protos.Field_RouteTrafficMinutesDelay:      {kind: kindNumeric, metric: "vehicle_route_traffic_minutes_delay"},
	protos.Field_MaxSpeedToReachDestinationMph: {kind: kindNumeric, metric: "vehicle_max_speed_to_reach_destination_mph"},
	protos.Field_RouteLastUpdated:              excluded(reasonBroken),
	protos.Field_RouteLine:                     excluded(reasonBlob),
	protos.Field_DestinationName:               excluded(reasonFreeText),

	// --- Climate / HVAC ---
	protos.Field_InsideTemp:                              {kind: kindNumeric, metric: "vehicle_temp_celsius", extraLabel: "location", extraValue: "inside"},
	protos.Field_OutsideTemp:                             {kind: kindNumeric, metric: "vehicle_temp_celsius", extraLabel: "location", extraValue: "outside"},
	protos.Field_HvacACEnabled:                           {kind: kindBoolean, metric: "vehicle_hvac_ac_enabled"},
	protos.Field_HvacAutoMode:                            {kind: kindEnum, metric: "vehicle_hvac_auto_mode", valueLabel: "mode"},
	protos.Field_HvacFanSpeed:                            {kind: kindNumeric, metric: "vehicle_hvac_fan_speed"},
	protos.Field_HvacFanStatus:                           {kind: kindNumeric, metric: "vehicle_hvac_fan_status_segment"},
	protos.Field_HvacPower:                               {kind: kindEnum, metric: "vehicle_hvac_power_state", valueLabel: "state"},
	protos.Field_HvacLeftTemperatureRequest:              {kind: kindNumeric, metric: "vehicle_hvac_temperature_request_celsius", extraLabel: "side", extraValue: "left"},
	protos.Field_HvacRightTemperatureRequest:             {kind: kindNumeric, metric: "vehicle_hvac_temperature_request_celsius", extraLabel: "side", extraValue: "right"},
	protos.Field_HvacSteeringWheelHeatAuto:               {kind: kindBoolean, metric: "vehicle_hvac_steering_wheel_heat_auto"},
	protos.Field_HvacSteeringWheelHeatLevel:              {kind: kindNumeric, metric: "vehicle_hvac_steering_wheel_heat_level"},
	protos.Field_DefrostMode:                             {kind: kindEnum, metric: "vehicle_defrost_mode", valueLabel: "mode"},
	protos.Field_DefrostForPreconditioning:               {kind: kindBoolean, metric: "vehicle_defrost_for_preconditioning"},
	protos.Field_RearDefrostEnabled:                      {kind: kindBoolean, metric: "vehicle_rear_defrost_enabled"},
	protos.Field_RearDisplayHvacEnabled:                  {kind: kindBoolean, metric: "vehicle_rear_display_hvac_enabled"},
	protos.Field_PreconditioningEnabled:                  {kind: kindBoolean, metric: "vehicle_preconditioning_enabled"},
	protos.Field_CabinOverheatProtectionMode:             {kind: kindEnum, metric: "vehicle_cabin_overheat_protection_mode", valueLabel: "mode"},
	protos.Field_CabinOverheatProtectionTemperatureLimit: {kind: kindEnum, metric: "vehicle_cabin_overheat_protection_temperature_limit", valueLabel: "limit"},
	protos.Field_ClimateKeeperMode:                       {kind: kindEnum, metric: "vehicle_climate_keeper_mode", valueLabel: "mode"},
	protos.Field_SeatHeaterLeft:                          {kind: kindNumeric, metric: "vehicle_seat_heater_level", extraLabel: "seat", extraValue: "front_left"},
	protos.Field_SeatHeaterRight:                         {kind: kindNumeric, metric: "vehicle_seat_heater_level", extraLabel: "seat", extraValue: "front_right"},
	protos.Field_SeatHeaterRearLeft:                      {kind: kindNumeric, metric: "vehicle_seat_heater_level", extraLabel: "seat", extraValue: "rear_left"},
	protos.Field_SeatHeaterRearRight:                     {kind: kindNumeric, metric: "vehicle_seat_heater_level", extraLabel: "seat", extraValue: "rear_right"},
	protos.Field_SeatHeaterRearCenter:                    {kind: kindNumeric, metric: "vehicle_seat_heater_level", extraLabel: "seat", extraValue: "rear_center"},
	protos.Field_ClimateSeatCoolingFrontLeft:             {kind: kindNumeric, metric: "vehicle_seat_cooling_level", extraLabel: "seat", extraValue: "front_left"},
	protos.Field_ClimateSeatCoolingFrontRight:            {kind: kindNumeric, metric: "vehicle_seat_cooling_level", extraLabel: "seat", extraValue: "front_right"},
	protos.Field_AutoSeatClimateLeft:                     {kind: kindBoolean, metric: "vehicle_auto_seat_climate_enabled", extraLabel: "seat", extraValue: "left"},
	protos.Field_AutoSeatClimateRight:                    {kind: kindBoolean, metric: "vehicle_auto_seat_climate_enabled", extraLabel: "seat", extraValue: "right"},
	protos.Field_SeatVentEnabled:                         {kind: kindBoolean, metric: "vehicle_seat_vent_enabled"},
	protos.Field_WiperHeatEnabled:                        {kind: kindBoolean, metric: "vehicle_wiper_heat_enabled"},

	// --- Doors / windows / locks / security ---
	protos.Field_DoorState:                  {kind: kindBoolMap, metric: "vehicle_door_open", boolMapLabel: "door"},
	protos.Field_Locked:                     {kind: kindBoolean, metric: "vehicle_locked"},
	protos.Field_FdWindow:                   {kind: kindEnum, metric: "vehicle_window_state", valueLabel: "state", extraLabel: "window", extraValue: "front_driver"},
	protos.Field_FpWindow:                   {kind: kindEnum, metric: "vehicle_window_state", valueLabel: "state", extraLabel: "window", extraValue: "front_passenger"},
	protos.Field_RdWindow:                   {kind: kindEnum, metric: "vehicle_window_state", valueLabel: "state", extraLabel: "window", extraValue: "rear_driver"},
	protos.Field_RpWindow:                   {kind: kindEnum, metric: "vehicle_window_state", valueLabel: "state", extraLabel: "window", extraValue: "rear_passenger"},
	protos.Field_SentryMode:                 {kind: kindEnum, metric: "vehicle_sentry_mode_state", valueLabel: "state"},
	protos.Field_ValetModeEnabled:           {kind: kindBoolean, metric: "vehicle_valet_mode_enabled"},
	protos.Field_PinToDriveEnabled:          {kind: kindBoolean, metric: "vehicle_pin_to_drive_enabled"},
	protos.Field_GuestModeEnabled:           {kind: kindBoolean, metric: "vehicle_guest_mode_enabled"},
	protos.Field_GuestModeMobileAccessState: {kind: kindEnum, metric: "vehicle_guest_mode_mobile_access_state", valueLabel: "state"},
	protos.Field_HomelinkNearby:             {kind: kindBoolean, metric: "vehicle_homelink_nearby"},
	protos.Field_HomelinkDeviceCount:        {kind: kindNumeric, metric: "vehicle_homelink_device_count"},
	protos.Field_PairedPhoneKeyAndKeyFobQty: {kind: kindNumeric, metric: "vehicle_paired_phone_key_and_key_fob_qty"},
	protos.Field_DriverSeatBelt:             {kind: kindEnum, metric: "vehicle_seat_belt_status", valueLabel: "status", extraLabel: "seat", extraValue: "driver"},
	protos.Field_PassengerSeatBelt:          {kind: kindEnum, metric: "vehicle_seat_belt_status", valueLabel: "status", extraLabel: "seat", extraValue: "passenger"},
	protos.Field_DriverSeatOccupied:         {kind: kindBoolean, metric: "vehicle_driver_seat_occupied"},
	protos.Field_LightsHazardsActive:        {kind: kindBoolean, metric: "vehicle_lights_active", extraLabel: "kind", extraValue: "hazards"},
	protos.Field_LightsHighBeams:            {kind: kindBoolean, metric: "vehicle_lights_active", extraLabel: "kind", extraValue: "high_beams"},
	protos.Field_LightsTurnSignal:           {kind: kindEnum, metric: "vehicle_lights_turn_signal_state", valueLabel: "state"},
	protos.Field_RemoteStartEnabled:         {kind: kindBoolean, metric: "vehicle_remote_start_enabled"},
	protos.Field_RemoteStartActive:          {kind: kindBoolean, metric: "vehicle_remote_start_active"},
	protos.Field_ServiceMode:                {kind: kindBoolean, metric: "vehicle_service_mode"},

	// --- Tires ---
	protos.Field_TpmsPressureFl:             {kind: kindNumeric, metric: "vehicle_tire_pressure_pascals", extraLabel: "tire", extraValue: "front_left", scale: 100000},
	protos.Field_TpmsPressureFr:             {kind: kindNumeric, metric: "vehicle_tire_pressure_pascals", extraLabel: "tire", extraValue: "front_right", scale: 100000},
	protos.Field_TpmsPressureRl:             {kind: kindNumeric, metric: "vehicle_tire_pressure_pascals", extraLabel: "tire", extraValue: "rear_left", scale: 100000},
	protos.Field_TpmsPressureRr:             {kind: kindNumeric, metric: "vehicle_tire_pressure_pascals", extraLabel: "tire", extraValue: "rear_right", scale: 100000},
	protos.Field_TpmsLastSeenPressureTimeFl: {kind: kindNumeric, metric: "vehicle_tire_pressure_last_seen_timestamp_seconds", extraLabel: "tire", extraValue: "front_left"},
	protos.Field_TpmsLastSeenPressureTimeFr: {kind: kindNumeric, metric: "vehicle_tire_pressure_last_seen_timestamp_seconds", extraLabel: "tire", extraValue: "front_right"},
	protos.Field_TpmsLastSeenPressureTimeRl: {kind: kindNumeric, metric: "vehicle_tire_pressure_last_seen_timestamp_seconds", extraLabel: "tire", extraValue: "rear_left"},
	protos.Field_TpmsLastSeenPressureTimeRr: {kind: kindNumeric, metric: "vehicle_tire_pressure_last_seen_timestamp_seconds", extraLabel: "tire", extraValue: "rear_right"},
	protos.Field_TpmsHardWarnings:           {kind: kindBoolMap, metric: "vehicle_tire_pressure_warning", boolMapLabel: "tire", extraLabel: "severity", extraValue: "hard"},
	protos.Field_TpmsSoftWarnings:           {kind: kindBoolMap, metric: "vehicle_tire_pressure_warning", boolMapLabel: "tire", extraLabel: "severity", extraValue: "soft"},

	// --- Drive state ---
	protos.Field_Gear:                       {kind: kindEnum, metric: "vehicle_gear", valueLabel: "gear"},
	protos.Field_VehicleSpeed:               {kind: kindNumeric, metric: "vehicle_speed_mph"},
	protos.Field_Odometer:                   {kind: kindNumeric, metric: "vehicle_odometer_miles"},
	protos.Field_MilesSinceReset:            {kind: kindNumeric, metric: "vehicle_trip_miles_since_reset", extraLabel: "kind", extraValue: "drive"},
	protos.Field_SelfDrivingMilesSinceReset: {kind: kindNumeric, metric: "vehicle_trip_miles_since_reset", extraLabel: "kind", extraValue: "self_driving"},
	protos.Field_BrakePedal:                 {kind: kindBoolean, metric: "vehicle_brake_pedal_pressed"},
	protos.Field_BrakePedalPos:              {kind: kindNumeric, metric: "vehicle_brake_pedal_position", scale: 1000, reason: "unit not documented by Tesla; exposed unscaled x1000 for sub-unit precision"},
	protos.Field_PedalPosition:              {kind: kindNumeric, metric: "vehicle_accelerator_pedal_position", scale: 1000, reason: "unit not documented by Tesla; exposed unscaled x1000 for sub-unit precision"},
	protos.Field_LateralAcceleration:        {kind: kindNumeric, metric: "vehicle_acceleration_mm_per_s2", extraLabel: "axis", extraValue: "lateral", scale: 1000},
	protos.Field_LongitudinalAcceleration:   {kind: kindNumeric, metric: "vehicle_acceleration_mm_per_s2", extraLabel: "axis", extraValue: "longitudinal", scale: 1000},
	protos.Field_GradeEstimatePercent:       {kind: kindNumeric, metric: "vehicle_grade_estimate_percent"},
	protos.Field_CruiseSetSpeed:             {kind: kindNumeric, metric: "vehicle_cruise_set_speed_mph"},
	protos.Field_CruiseFollowDistance:       {kind: kindEnum, metric: "vehicle_cruise_follow_distance", valueLabel: "level"},
	protos.Field_CurrentLimitMph:            {kind: kindNumeric, metric: "vehicle_current_limit_mph"},
	protos.Field_DriveRail:                  {kind: kindBoolean, metric: "vehicle_drive_rail_enabled"},
	protos.Field_DCDCEnable:                 {kind: kindBoolean, metric: "vehicle_dcdc_enable"},
	protos.Field_Hvil:                       {kind: kindEnum, metric: "vehicle_hvil_status", valueLabel: "status"},

	// --- Safety / ADAS ---
	protos.Field_ForwardCollisionWarning:         {kind: kindEnum, metric: "vehicle_forward_collision_warning_sensitivity", valueLabel: "level"},
	protos.Field_LaneDepartureAvoidance:          {kind: kindEnum, metric: "vehicle_lane_departure_avoidance_level", valueLabel: "level"},
	protos.Field_EmergencyLaneDepartureAvoidance: {kind: kindBoolean, metric: "vehicle_emergency_lane_departure_avoidance_enabled"},
	protos.Field_AutomaticEmergencyBrakingOff:    {kind: kindBoolean, metric: "vehicle_automatic_emergency_braking_off"},
	protos.Field_AutomaticBlindSpotCamera:        {kind: kindBoolean, metric: "vehicle_automatic_blind_spot_camera_enabled"},
	protos.Field_BlindSpotCollisionWarningChime:  {kind: kindBoolean, metric: "vehicle_blind_spot_collision_warning_chime_enabled"},
	protos.Field_SpeedLimitMode:                  {kind: kindBoolean, metric: "vehicle_speed_limit_mode_enabled"},
	protos.Field_SpeedLimitWarning:               {kind: kindEnum, metric: "vehicle_speed_limit_warning_level", valueLabel: "level"},

	// --- Software / settings ---
	protos.Field_SoftwareUpdateDownloadPercentComplete:     {kind: kindNumeric, metric: "vehicle_software_update_percent_complete", extraLabel: "phase", extraValue: "download"},
	protos.Field_SoftwareUpdateInstallationPercentComplete: {kind: kindNumeric, metric: "vehicle_software_update_percent_complete", extraLabel: "phase", extraValue: "installation"},
	protos.Field_SoftwareUpdateExpectedDurationMinutes:     {kind: kindNumeric, metric: "vehicle_software_update_expected_duration_minutes"},
	protos.Field_SoftwareUpdateScheduledStartTime:          {kind: kindScheduledTime, metric: "vehicle_software_update_scheduled_start"},
	protos.Field_SoftwareUpdateAvailable:                   {kind: kindBoolean, metric: "vehicle_software_update_available"},
	protos.Field_SoftwareUpdateInProgress:                  {kind: kindBoolean, metric: "vehicle_software_update_in_progress"},
	protos.Field_SettingDistanceUnit:                       {kind: kindEnum, metric: "vehicle_setting_unit", valueLabel: "unit", extraLabel: "setting", extraValue: "distance"},
	protos.Field_SettingTemperatureUnit:                    {kind: kindEnum, metric: "vehicle_setting_unit", valueLabel: "unit", extraLabel: "setting", extraValue: "temperature"},
	protos.Field_SettingTirePressureUnit:                   {kind: kindEnum, metric: "vehicle_setting_unit", valueLabel: "unit", extraLabel: "setting", extraValue: "tire_pressure"},
	protos.Field_SettingChargeUnit:                         {kind: kindEnum, metric: "vehicle_setting_unit", valueLabel: "unit", extraLabel: "setting", extraValue: "charge"},
	protos.Field_Setting24HourTime:                         {kind: kindBoolean, metric: "vehicle_setting_24_hour_time"},

	// --- Vehicle identity (static info metric) ---
	protos.Field_CarType:                {kind: kindInfo, infoLabel: "car_type"},
	protos.Field_Trim:                   {kind: kindInfo, infoLabel: "trim"},
	protos.Field_ExteriorColor:          {kind: kindInfo, infoLabel: "exterior_color"},
	protos.Field_RoofColor:              {kind: kindInfo, infoLabel: "roof_color"},
	protos.Field_WheelType:              {kind: kindInfo, infoLabel: "wheel_type"},
	protos.Field_EfficiencyPackage:      {kind: kindInfo, infoLabel: "efficiency_package"},
	protos.Field_RightHandDrive:         {kind: kindInfo, infoLabel: "right_hand_drive"},
	protos.Field_EuropeVehicle:          {kind: kindInfo, infoLabel: "europe_vehicle"},
	protos.Field_SunroofInstalled:       {kind: kindInfo, infoLabel: "sunroof_installed"},
	protos.Field_OffroadLightbarPresent: {kind: kindInfo, infoLabel: "offroad_lightbar_present"},
	protos.Field_RearSeatHeaters:        {kind: kindInfo, infoLabel: "rear_seat_heaters"},
	protos.Field_Version:                {kind: kindInfo, infoLabel: "version"},
	protos.Field_SoftwareUpdateVersion:  {kind: kindInfo, infoLabel: "pending_update_version"},

	// --- Media (excluding free text) ---
	protos.Field_MediaPlaybackStatus:       {kind: kindEnum, metric: "vehicle_media_playback_status", valueLabel: "status"},
	protos.Field_MediaAudioVolume:          {kind: kindNumeric, metric: "vehicle_media_audio_volume"},
	protos.Field_MediaAudioVolumeIncrement: {kind: kindNumeric, metric: "vehicle_media_audio_volume_increment"},
	protos.Field_MediaAudioVolumeMax:       {kind: kindNumeric, metric: "vehicle_media_audio_volume_max"},
	protos.Field_MediaNowPlayingDuration:   {kind: kindNumeric, metric: "vehicle_media_now_playing_milliseconds", extraLabel: "kind", extraValue: "duration"},
	protos.Field_MediaNowPlayingElapsed:    {kind: kindNumeric, metric: "vehicle_media_now_playing_milliseconds", extraLabel: "kind", extraValue: "elapsed"},
	protos.Field_MediaPlaybackSource:       excluded(reasonFreeText),
	protos.Field_MediaNowPlayingArtist:     excluded(reasonFreeText),
	protos.Field_MediaNowPlayingTitle:      excluded(reasonFreeText),
	protos.Field_MediaNowPlayingAlbum:      excluded(reasonFreeText),
	protos.Field_MediaNowPlayingStation:    excluded(reasonFreeText),

	// --- Excluded: free text ---
	protos.Field_VehicleName: excluded(reasonFreeText),

	// --- Excluded: reserved/placeholder ---
	protos.Field_Deprecated_1:    excluded(reasonReserved),
	protos.Field_Deprecated_2:    excluded(reasonReserved),
	protos.Field_Deprecated_3:    excluded(reasonReserved),
	protos.Field_Experimental_1:  excluded(reasonReserved),
	protos.Field_Experimental_2:  excluded(reasonReserved),
	protos.Field_Experimental_3:  excluded(reasonReserved),
	protos.Field_Experimental_4:  excluded(reasonReserved),
	protos.Field_Experimental_5:  excluded(reasonReserved),
	protos.Field_Experimental_6:  excluded(reasonReserved),
	protos.Field_Experimental_7:  excluded(reasonReserved),
	protos.Field_Experimental_8:  excluded(reasonReserved),
	protos.Field_Experimental_9:  excluded(reasonReserved),
	protos.Field_Experimental_10: excluded(reasonReserved),
	protos.Field_Experimental_11: excluded(reasonReserved),
	protos.Field_Experimental_12: excluded(reasonReserved),
	protos.Field_Experimental_13: excluded(reasonReserved),
	protos.Field_Experimental_14: excluded(reasonReserved),
	protos.Field_Experimental_15: excluded(reasonReserved),

	// --- Excluded: Tesla Semi-only fields, deferred ---
	protos.Field_SemiCruiseSpeedLimitMph:            excluded(reasonSemiOnly),
	protos.Field_SemitruckPassengerSeatFoldPosition: excluded(reasonSemiOnly),
	protos.Field_SemitruckTractorParkBrakeStatus:    excluded(reasonSemiOnly),
	protos.Field_SemitruckTrailerParkBrakeStatus:    excluded(reasonSemiOnly),
	protos.Field_LifetimeEnergyUsedDrive:            excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe1L0:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe1L1:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe1R0:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe1R1:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe2L0:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe2L1:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe2R0:         excluded(reasonSemiOnly),
	protos.Field_SemitruckTpmsPressureRe2R1:         excluded(reasonSemiOnly),

	// --- Excluded: low-level drivetrain diagnostics, deferred ---
	protos.Field_DiAxleSpeedF:      excluded(reasonDrivetrainDiag),
	protos.Field_DiAxleSpeedR:      excluded(reasonDrivetrainDiag),
	protos.Field_DiAxleSpeedREL:    excluded(reasonDrivetrainDiag),
	protos.Field_DiAxleSpeedRER:    excluded(reasonDrivetrainDiag),
	protos.Field_DiHeatsinkTF:      excluded(reasonDrivetrainDiag),
	protos.Field_DiHeatsinkTR:      excluded(reasonDrivetrainDiag),
	protos.Field_DiHeatsinkTREL:    excluded(reasonDrivetrainDiag),
	protos.Field_DiHeatsinkTRER:    excluded(reasonDrivetrainDiag),
	protos.Field_DiInverterTF:      excluded(reasonDrivetrainDiag),
	protos.Field_DiInverterTR:      excluded(reasonDrivetrainDiag),
	protos.Field_DiInverterTREL:    excluded(reasonDrivetrainDiag),
	protos.Field_DiInverterTRER:    excluded(reasonDrivetrainDiag),
	protos.Field_DiMotorCurrentF:   excluded(reasonDrivetrainDiag),
	protos.Field_DiMotorCurrentR:   excluded(reasonDrivetrainDiag),
	protos.Field_DiMotorCurrentREL: excluded(reasonDrivetrainDiag),
	protos.Field_DiMotorCurrentRER: excluded(reasonDrivetrainDiag),
	protos.Field_DiSlaveTorqueCmd:  excluded(reasonDrivetrainDiag),
	protos.Field_DiStateF:          excluded(reasonDrivetrainDiag),
	protos.Field_DiStateR:          excluded(reasonDrivetrainDiag),
	protos.Field_DiStateREL:        excluded(reasonDrivetrainDiag),
	protos.Field_DiStateRER:        excluded(reasonDrivetrainDiag),
	protos.Field_DiStatorTempF:     excluded(reasonDrivetrainDiag),
	protos.Field_DiStatorTempR:     excluded(reasonDrivetrainDiag),
	protos.Field_DiStatorTempREL:   excluded(reasonDrivetrainDiag),
	protos.Field_DiStatorTempRER:   excluded(reasonDrivetrainDiag),
	protos.Field_DiTorqueActualF:   excluded(reasonDrivetrainDiag),
	protos.Field_DiTorqueActualR:   excluded(reasonDrivetrainDiag),
	protos.Field_DiTorqueActualREL: excluded(reasonDrivetrainDiag),
	protos.Field_DiTorqueActualRER: excluded(reasonDrivetrainDiag),
	protos.Field_DiTorquemotor:     excluded(reasonDrivetrainDiag),
	protos.Field_DiVBatF:           excluded(reasonDrivetrainDiag),
	protos.Field_DiVBatR:           excluded(reasonDrivetrainDiag),
	protos.Field_DiVBatREL:         excluded(reasonDrivetrainDiag),
	protos.Field_DiVBatRER:         excluded(reasonDrivetrainDiag),

	// --- Cybertruck-specific (kept: harmless no-op on other vehicles) ---
	protos.Field_TonneauOpenPercent: {kind: kindNumeric, metric: "vehicle_tonneau_open_percent"},
	protos.Field_TonneauPosition:    {kind: kindEnum, metric: "vehicle_tonneau_position_state", valueLabel: "state"},
	protos.Field_TonneauTentMode:    {kind: kindEnum, metric: "vehicle_tonneau_tent_mode", valueLabel: "mode"},

	// --- Center display ---
	protos.Field_CenterDisplay: {kind: kindEnum, metric: "vehicle_center_display_state", valueLabel: "state"},
}
