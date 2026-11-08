package micom

// CarClimate is the climate state the car has reported, in its own units, as
// the panel frames (43 c2 00 c3) and the seat heat frames (55 ca 00 d5, 44 c1
// 00 c4) give it; see car.go for how each was read off live captures. A nil
// field has not been reported yet.
type CarClimate struct {
	// Set points in degrees Fahrenheit; 0 is the zone off (driver only, for
	// the passenger's).
	DriverSetF, PassengerSetF *int
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

var (
	hdrClimatePanel = [4]byte{0x43, 0xc2, 0x00, 0xc3}
	hdrSeatHeat     = [4]byte{0x55, 0xca, 0x00, 0xd5}
	hdrSeatRepeat   = [4]byte{0x44, 0xc1, 0x00, 0xc4}
)

// Apply folds f into c.
func (c *CarClimate) Apply(f Frame) ClimateEvent {
	if len(f.Payload) != 1 {
		return ClimateNone
	}
	v := f.Payload[0]
	switch f.Hdr {
	case hdrSeatHeat:
		switch f.Cmd {
		case 0x20:
			return setSeat(&c.DriverSeatHeat, v)
		case 0x21:
			return setSeat(&c.PassengerSeatHeat, v)
		}
	case hdrSeatRepeat:
		switch f.Cmd {
		case 0x19:
			return setSeat(&c.DriverSeatHeat, v)
		case 0x1b:
			return setSeat(&c.PassengerSeatHeat, v)
		}
	case hdrClimatePanel:
		return c.applyPanel(f.Cmd, v)
	}
	return ClimateNone
}

func (c *CarClimate) applyPanel(cmd, v byte) ClimateEvent {
	flag := func(field **bool, on, off byte) ClimateEvent {
		switch v {
		case on:
			return set(field, true)
		case off:
			return set(field, false)
		}
		return ClimateNone
	}
	switch cmd {
	case 0x1b:
		if v == 1 {
			return ClimateBegin
		}
		return ClimateEnd
	case 0x05, 0x07:
		return set(&c.DriverSetF, setPointF(v))
	case 0x09, 0x0b:
		return set(&c.PassengerSetF, setPointF(v))
	case 0x0d:
		if _, ok := airflow[v]; ok {
			return set(&c.Airflow, v)
		}
	case 0x0f:
		return flag(&c.Auto, 1, 0)
	case 0x11:
		return flag(&c.AC, 1, 0)
	case 0x15:
		switch {
		case v == 0x0a:
			return set(&c.FanAuto, true)
		case v == 0:
			return max(set(&c.FanLevel, 0), set(&c.FanAuto, false))
		case v >= 2 && v <= 9:
			return max(set(&c.FanLevel, int(v)-1), set(&c.FanAuto, false))
		}
	case 0x19:
		return flag(&c.DriverOnly, 1, 0)
	case 0x1e, 0x43:
		return flag(&c.Recirculation, 1, 2)
	case 0x27:
		return set(&c.ZonesSynced, v == 1)
	}
	return ClimateNone
}

// setPointF: degrees Fahrenheit less 56, 0 with the zone off.
func setPointF(v byte) int {
	if v == 0 {
		return 0
	}
	return int(v) + 56
}

// setSeat: 02 off, 06 to 08 levels 1 to 3.
func setSeat(field **int, v byte) ClimateEvent {
	switch {
	case v == 2:
		return set(field, 0)
	case v >= 6 && v <= 8:
		return set(field, int(v)-5)
	}
	return ClimateNone
}

func set[T comparable](field **T, v T) ClimateEvent {
	if *field != nil && **field == v {
		return ClimateNone
	}
	*field = &v
	return ClimateChanged
}
