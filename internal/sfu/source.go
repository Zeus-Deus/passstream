package sfu

// Publisher-only diagnostics distinguish encoded-source gaps from forwarding
// stalls without inspecting captured pixels or media content.
func (r *Room) sourceFrameTimes() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	times := make([]int64, r.frameN)
	for i := range times {
		times[i] = r.frameTimes[(r.frameHead-r.frameN+i+len(r.frameTimes))%len(r.frameTimes)]
	}
	return times
}
