package micom

// CarClimate is the climate state the car has reported, in its own units, as
// its variant's state frames give it (on the EV6 the panel frames, 43 c2 00 c3,
// and the seat heat frames, 55 ca 00 d5 and 44 c1 00 c4). A nil field has not
// been reported yet.
type CarClimate struct {
	// Set points in degrees Fahrenheit, and in half degrees Celsius (the
	// car sends both; the Celsius ones are exact): 0 is the zone off (driver
	// only, for the passenger's), SetPointLo and SetPointHi the panel's LO
	// and HI.
	DriverSetF, PassengerSetF         *int
	DriverSetHalfC, PassengerSetHalfC *int
	// Airflow: 0 off, 1 face, 2 face and feet, 3 feet, 4 feet and defrost,
	// 5 defrost, 8 auto.
	Airflow *byte
	// Switches; recirculation off is the car's 2, the others' 0.
	Auto, AC, DriverOnly, Recirculation, ZonesSynced *bool
	// FanLevel 0 is off, 1 to 8 the levels; FanAuto when the fan runs on auto.
	FanLevel *int
	FanAuto  *bool
	// Seat heat 0 (off) to 3.
	DriverSeatHeat, PassengerSeatHeat *int
}

// SetPointLo and SetPointHi are a set point at the panel's LO and HI ends.
const (
	SetPointLo = -1
	SetPointHi = -2
)

// ClimateEvent is what a frame did to a CarClimate.
type ClimateEvent int

const (
	// ClimateNone: not a climate frame, or one whose value is not known.
	ClimateNone ClimateEvent = iota
	// ClimateChanged: a field took a new value.
	ClimateChanged
	// ClimateBegin and ClimateEnd bracket a burst of panel fields (cmd 1b
	// 01 ... 1b 00): one change of the panel arrives as several frames.
	ClimateBegin
	ClimateEnd
)

// Apply folds f into c, as the default variant reads it.
func (c *CarClimate) Apply(f Frame) ClimateEvent {
	return defaultVariant().ApplyClimate(c, f)
}

func set[T comparable](field **T, v T) ClimateEvent {
	if *field != nil && **field == v {
		return ClimateNone
	}
	*field = &v
	return ClimateChanged
}
