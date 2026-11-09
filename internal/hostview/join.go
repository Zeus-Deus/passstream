package hostview

import "sort"

const WindowMS int64 = 300000

func ptr[T any](v T) *T { return &v }
func flag(v *Viewer, f string) {
	for _, old := range v.Flags {
		if old == f {
			return
		}
	}
	v.Flags = append(v.Flags, f)
}
func blank(id, status string) Viewer {
	return Viewer{RowID: id, Status: status, AdmissionIDs: []string{}, Connections: []Connection{}, Flags: []string{}, Provenance: map[string]string{"viewer_id": "sender", "row_code": "sender", "admission_ids": "sender", "knock_id": "sender", "sender": "sender", "connections.*": "server-observed", "connections.*.reported.*": "viewer-reported", "status": "derived", "flags": "derived", "left_at": "derived", "row_id": "derived"}}
}

// Join never uses connection metadata to choose the identity targeted by commands.
func Join(in JoinInput, mem JoinMemory) (ViewerList, JoinMemory, []HistoryEntry) {
	out := ViewerList{Source: in.Source, SenderOnline: in.PublisherOnline, SenderStale: !in.PublisherOnline || in.StateAt == nil || in.Now-*in.StateAt > 10000, StateAt: in.StateAt, BuiltAt: in.Now, Viewers: []Viewer{}}
	rows := map[string]*Viewer{}
	groups := map[string][]Admission{}
	knocks := map[string]Knock{}
	ends := map[string]End{}
	for _, e := range mem.Ends {
		ends[e.Kind+e.AdmissionID+e.KnockID] = e
	}
	if in.State != nil {
		for _, e := range in.State.Ends {
			ends[e.Kind+e.AdmissionID+e.KnockID] = e
		}
		for _, a := range in.State.Admissions {
			groups[a.ViewerID] = append(groups[a.ViewerID], a)
		}
		for _, k := range in.State.Knocks {
			knocks[k.KnockID] = k
		}
	}
	type claim struct {
		row  string
		at   int64
		live bool
	}
	claims := map[string][]claim{}
	for id, aa := range groups {
		sort.Slice(aa, func(i, j int) bool {
			if aa[i].AdmittedAt == aa[j].AdmittedAt {
				return aa[i].AdmissionID > aa[j].AdmissionID
			}
			return aa[i].AdmittedAt > aa[j].AdmittedAt
		})
		v := blank("v:"+id, "left")
		v.ViewerID = ptr(id)
		v.RowCode = ptr(aa[0].RowCode)
		v.Sender = &ViewerSender{ViaGrant: aa[0].ViaGrant, AdmittedAt: aa[len(aa)-1].AdmittedAt}
		for _, a := range aa {
			v.AdmissionIDs = append(v.AdmissionIDs, a.AdmissionID)
			v.Sender.Connected = v.Sender.Connected || a.Connected
			v.Sender.KickIncomplete = v.Sender.KickIncomplete || a.KickIncomplete
			claims[a.PeerID] = append(claims[a.PeerID], claim{v.RowID, a.AdmittedAt, a.Connected})
		}
		rows[v.RowID] = &v
	}
	conns := map[string]Connection{}
	attached := map[string]bool{}
	for _, c := range in.Conns {
		if c.LeftAt == nil || in.Now-*c.LeftAt < WindowMS {
			conns[c.ConnectionID] = c
		}
	}
	for peer, cc := range claims {
		live := map[string]bool{}
		for _, c := range cc {
			if c.live {
				live[c.row] = true
				if conn, ok := conns[peer]; !ok || conn.LeftAt != nil {
					flag(rows[c.row], "no_connection")
				}
			}
		}
		target := ""
		if len(live) > 1 {
			for id := range live {
				flag(rows[id], "shared_connection")
			}
		} else if len(live) == 1 {
			for id := range live {
				target = id
			}
		} else {
			latest := int64(-1)
			tie := false
			for _, c := range cc {
				if c.at > latest {
					latest = c.at
					target = c.row
					tie = false
				} else if c.at == latest && c.row != target {
					tie = true
				}
			}
			if tie {
				target = ""
			}
		}
		if c, ok := conns[peer]; ok && target != "" {
			rows[target].Connections = append(rows[target].Connections, c)
			attached[peer] = true
		}
	}
	for id, k := range knocks {
		v := blank("k:"+id, "knocking")
		v.KnockID = ptr(id)
		v.RowCode = ptr(k.RowCode)
		live := false
		for _, c := range claims[k.PeerID] {
			live = live || c.live
		}
		if c, ok := conns[k.PeerID]; ok && !live {
			v.Connections = append(v.Connections, c)
			attached[k.PeerID] = true
		}
		rows[v.RowID] = &v
	}
	joining := []Connection{}
	for id, c := range conns {
		if c.LeftAt == nil && !attached[id] && len(claims[id]) == 0 {
			joining = append(joining, c)
		}
	}
	sort.Slice(joining, func(i, j int) bool {
		if joining[i].JoinedAt == joining[j].JoinedAt {
			return joining[i].ConnectionID < joining[j].ConnectionID
		}
		return joining[i].JoinedAt > joining[j].JoinedAt
	})
	if len(joining) > 100 {
		joining = joining[:100]
	}
	for _, c := range joining {
		v := blank("c:"+c.ConnectionID, "joining")
		v.Connections = append(v.Connections, c)
		rows[v.RowID] = &v
	}
	old := map[string]MemoryRow{}
	for _, m := range mem.Rows {
		old[m.RowID] = m
		if rows[m.RowID] == nil {
			v := m.Snapshot
			v.Status = "left"
			v.Flags = append([]string{}, v.Flags...)
			v.Connections = append([]Connection{}, v.Connections...)
			for i, c := range v.Connections {
				if fresh, ok := conns[c.ConnectionID]; ok {
					v.Connections[i] = fresh
				}
			}
			rows[m.RowID] = &v
		}
	}
	history := []HistoryEntry{}
	next := JoinMemory{Rows: []MemoryRow{}, Ends: []End{}}
	ids := []string{}
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v := rows[id]
		firstAt := old[id].FirstAt
		if k, ok := knocks[value(v.KnockID)]; ok {
			firstAt = k.At
		}
		if v.Sender != nil && groups[value(v.ViewerID)] != nil {
			if v.Sender.Connected {
				v.Status = "watching"
			} else {
				for _, c := range v.Connections {
					if c.LeftAt == nil {
						v.Status = "reconnecting"
					}
				}
			}
		}
		if out.SenderStale && (v.Sender != nil || v.KnockID != nil) {
			flag(v, "sender_stale")
		}
		sort.Slice(v.Connections, func(i, j int) bool {
			if v.Connections[i].JoinedAt == v.Connections[j].JoinedAt {
				return v.Connections[i].ConnectionID > v.Connections[j].ConnectionID
			}
			return v.Connections[i].JoinedAt > v.Connections[j].JoinedAt
		})
		if len(v.Connections) > 3 {
			v.Connections = v.Connections[:3]
		}
		if v.Status == "left" {
			v.LeftAt = old[id].LeftAt
			if v.LeftAt == nil {
				v.LeftAt = ptr(in.Now)
			}
		} else {
			v.LeftAt = nil
		}
		sort.Strings(v.Flags)
		removedKnock := v.KnockID != nil && knocks[*v.KnockID].KnockID == ""
		already := old[id].Archived
		sameAdmissions := len(v.AdmissionIDs) == len(old[id].Snapshot.AdmissionIDs)
		if sameAdmissions {
			for i, a := range v.AdmissionIDs {
				if a != old[id].Snapshot.AdmissionIDs[i] {
					sameAdmissions = false
					break
				}
			}
		}
		if already && sameAdmissions && (in.StreamEnded || v.Status == "left") {
			if groups[value(v.ViewerID)] != nil || in.StreamEnded {
				next.Rows = append(next.Rows, MemoryRow{FirstAt: firstAt, RowID: id, Snapshot: *v, LeftAt: v.LeftAt, Archived: true})
			}
			continue
		}
		if in.StreamEnded || removedKnock || (v.LeftAt != nil && in.Now-*v.LeftAt >= WindowMS) {
			h := HistoryEntry{RowID: id, ViewerID: v.ViewerID, RowCode: v.RowCode, AdmissionIDs: v.AdmissionIDs, FirstAt: in.Now, LastAt: in.Now, End: "left", Connections: []HistoryConnection{}}
			if v.KnockID != nil {
				h.FirstAt = firstAt
			}
			if v.Sender != nil {
				h.FirstAt = v.Sender.AdmittedAt
			}
			if v.LeftAt != nil {
				h.LastAt = *v.LeftAt
			}
			if in.StreamEnded {
				h.End = "stream_ended"
			}
			if groups[value(v.ViewerID)] == nil {
				for _, aid := range v.AdmissionIDs {
					e := ends["admission"+aid]
					if e.Cause == "kicked" || e.Cause == "revoked" && h.End != "kicked" || e.Cause == "stream_ended" && h.End == "left" {
						h.End = e.Cause
					}
				}
			}
			if v.KnockID != nil {
				switch ends["knock"+*v.KnockID].Reason {
				case "denied", "pin_rotated", "commit_failed":
					h.End = "denied"
				case "timeout":
					h.End = "knock_timeout"
				case "stopped":
					h.End = "stream_ended"
				}
			}
			for _, c := range v.Connections {
				h.Connections = append(h.Connections, HistoryConnection{c.ConnectionID, c.JoinedAt, c.LeftAt, c.IP, c.Geo, c.Device})
				if v.KnockID == nil && c.JoinedAt < h.FirstAt {
					h.FirstAt = c.JoinedAt
				}
			}
			history = append(history, h)
			if groups[value(v.ViewerID)] != nil || in.StreamEnded {
				next.Rows = append(next.Rows, MemoryRow{FirstAt: firstAt, RowID: id, Snapshot: *v, LeftAt: v.LeftAt, Archived: true})
			}
			continue
		}
		if v.Status == "watching" {
			out.Watching++
		}
		if v.Status == "knocking" {
			out.Knocking++
		}
		out.Viewers = append(out.Viewers, *v)
		next.Rows = append(next.Rows, MemoryRow{FirstAt: firstAt, RowID: id, Snapshot: *v, LeftAt: v.LeftAt})
	}
	// Ends have no timestamp on the wire. Retain only those still referenced by the bounded memory/state.
	refs := map[string]bool{}
	for _, m := range next.Rows {
		for _, id := range m.Snapshot.AdmissionIDs {
			refs["admission"+id] = true
		}
		if m.Snapshot.KnockID != nil {
			refs["knock"+*m.Snapshot.KnockID] = true
		}
	}
	keys := []string{}
	for k := range ends {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if refs[k] {
			next.Ends = append(next.Ends, ends[k])
		}
	}
	rank := map[string]int{"watching": 0, "knocking": 1, "reconnecting": 2, "joining": 3, "left": 4}
	sort.SliceStable(out.Viewers, func(i, j int) bool { return rank[out.Viewers[i].Status] < rank[out.Viewers[j].Status] })
	return out, next, history
}
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
