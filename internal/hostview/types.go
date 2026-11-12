// Package hostview joins sender admissions with untrusted relay telemetry.
package hostview

type PinPolicy struct {
	Kind   string  `json:"kind"`
	Length *uint64 `json:"length,omitempty"`
}
type Latches struct {
	Locked bool `json:"locked"`
	Paused bool `json:"paused"`
	Frozen bool `json:"frozen"`
}

// Or merges two copies field by field (CONTRACT §4.2).
func (l Latches) Or(o Latches) Latches {
	return Latches{l.Locked || o.Locked, l.Paused || o.Paused, l.Frozen || o.Frozen}
}

type PinState struct {
	Required bool    `json:"required"`
	Kind     string  `json:"kind"`
	Length   *uint64 `json:"length"`
}
type Failures struct {
	Stream uint64 `json:"stream"`
	Limit  uint64 `json:"limit"`
}
type Access struct {
	Pin             PinState `json:"pin"`
	PinVersion      uint64   `json:"pin_version"`
	RotationPending bool     `json:"rotation_pending"`
	Reconciling     bool     `json:"reconciling"`
	Latches         Latches  `json:"latches"`
	Knock           bool     `json:"knock"`
	Failures        Failures `json:"failures"`
}
type E2EE struct {
	Status               string `json:"status"`
	Epoch                uint64 `json:"epoch"`
	Suspended            bool   `json:"suspended"`
	RevocationNotDurable bool   `json:"revocation_not_durable"`
}
type Admission struct {
	AdmissionID    string `json:"admission_id"`
	ViewerID       string `json:"viewer_id"`
	RowCode        string `json:"row_code"`
	PeerID         string `json:"peer_id"`
	Connected      bool   `json:"connected"`
	ViaGrant       bool   `json:"via_grant"`
	AdmittedAt     int64  `json:"admitted_at"`
	KickIncomplete bool   `json:"kick_incomplete"`
}
type Knock struct {
	KnockID   string `json:"knock_id"`
	PeerID    string `json:"peer_id"`
	RowCode   string `json:"row_code"`
	At        int64  `json:"at"`
	ExpiresAt int64  `json:"expires_at"`
}
type End struct {
	Kind        string `json:"kind"`
	AdmissionID string `json:"admission_id,omitempty"`
	ViewerID    string `json:"viewer_id,omitempty"`
	Cause       string `json:"cause,omitempty"`
	KnockID     string `json:"knock_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
type SenderState struct {
	V          uint64      `json:"v"`
	StreamID   string      `json:"stream_id"`
	Instance   string      `json:"instance"`
	Seq        uint64      `json:"seq"`
	Phase      string      `json:"phase"`
	EndReason  string      `json:"end_reason,omitempty"`
	Mode       string      `json:"mode"`
	Safe       bool        `json:"safe"`
	E2EE       E2EE        `json:"e2ee"`
	Access     Access      `json:"access"`
	Admissions []Admission `json:"admissions"`
	Knocks     []Knock     `json:"knocks"`
	Ends       []End       `json:"ends"`
	Health     *Health     `json:"health"`
}
type Geo struct {
	Country *string `json:"country"`
	City    *string `json:"city"`
	ASN     *uint64 `json:"asn"`
	ASOrg   *string `json:"as_org"`
}
type Stats struct {
	RTTMS         *float64 `json:"rtt_ms"`
	LossPct       *float64 `json:"loss_pct"`
	BitrateKbps   *uint64  `json:"bitrate_kbps"`
	Freezes       uint64   `json:"freezes"`
	TemporalLayer *uint64  `json:"temporal_layer"`
}
type Reported struct {
	Label     *string `json:"label"`
	TabHidden *bool   `json:"tab_hidden"`
}
type Connection struct {
	ConnectionID string   `json:"connection_id"`
	JoinedAt     int64    `json:"joined_at"`
	LeftAt       *int64   `json:"left_at"`
	IP           *string  `json:"ip"`
	Geo          *Geo     `json:"geo"`
	UserAgent    *string  `json:"user_agent"`
	Device       *string  `json:"device"`
	Path         *string  `json:"path"`
	Stats        *Stats   `json:"stats"`
	StatsAt      *int64   `json:"stats_at"`
	Reported     Reported `json:"reported"`
}
type ViewerSender struct {
	Connected      bool  `json:"connected"`
	ViaGrant       bool  `json:"via_grant"`
	AdmittedAt     int64 `json:"admitted_at"`
	KickIncomplete bool  `json:"kick_incomplete"`
}
type Viewer struct {
	RowID        string            `json:"row_id"`
	Status       string            `json:"status"`
	ViewerID     *string           `json:"viewer_id"`
	RowCode      *string           `json:"row_code"`
	AdmissionIDs []string          `json:"admission_ids"`
	KnockID      *string           `json:"knock_id"`
	Sender       *ViewerSender     `json:"sender"`
	Connections  []Connection      `json:"connections"`
	Flags        []string          `json:"flags"`
	LeftAt       *int64            `json:"left_at"`
	Provenance   map[string]string `json:"provenance,omitempty"`
}
type ViewerList struct {
	Meta         map[string]string `json:"meta,omitempty"`
	Source       string            `json:"source"`
	SenderOnline bool              `json:"sender_online"`
	SenderStale  bool              `json:"sender_stale"`
	StateAt      *int64            `json:"state_at"`
	BuiltAt      int64             `json:"built_at"`
	Watching     uint64            `json:"watching"`
	Knocking     uint64            `json:"knocking"`
	Viewers      []Viewer          `json:"viewers"`
}
type HistoryConnection struct {
	ConnectionID string  `json:"connection_id"`
	JoinedAt     int64   `json:"joined_at"`
	LeftAt       *int64  `json:"left_at"`
	IP           *string `json:"ip"`
	Geo          *Geo    `json:"geo"`
	Device       *string `json:"device"`
}
type HistoryEntry struct {
	RowID        string              `json:"row_id"`
	ViewerID     *string             `json:"viewer_id"`
	RowCode      *string             `json:"row_code"`
	AdmissionIDs []string            `json:"admission_ids"`
	FirstAt      int64               `json:"first_at"`
	LastAt       int64               `json:"last_at"`
	End          string              `json:"end"`
	Connections  []HistoryConnection `json:"connections"`
}
type JoinInput struct {
	Source          string
	Now             int64
	State           *SenderState
	StateAt         *int64
	PublisherOnline bool
	StreamEnded     bool
	Conns           []Connection
}
type MemoryRow struct {
	Archived bool   `json:"archived,omitempty"`
	FirstAt  int64  `json:"first_at,omitempty"`
	RowID    string `json:"row_id"`
	Snapshot Viewer `json:"snapshot"`
	LeftAt   *int64 `json:"left_at"`
}
type JoinMemory struct {
	Rows []MemoryRow `json:"rows"`
	Ends []End       `json:"ends"`
}
