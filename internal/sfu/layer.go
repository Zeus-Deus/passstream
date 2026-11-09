package sfu

// selectLayer preserves VP9 reference dependencies across temporal upswitches.
// RFC 9628 section 4.2: U on a retained frame guarantees subsequent higher TIDs
// do not depend on older pictures above that frame's TID. PIDs and TL0PICIDX must
// remain unchanged so the decoder can interpret the original reference deltas.
// Flexible mode instead names every reference explicitly; the pacer checks
// their delivery before sending each picture, without waiting for a U bit.
func selectLayer(current, target, tid uint8, key, up, flexible bool) uint8 {
	if target < current || key || flexible || (up && tid <= current) {
		return target
	}
	return current
}
