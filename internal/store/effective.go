package store

import "github.com/passstream/passstream/internal/hostview"

// Effective contains only allowlisted sender facts for authenticated owner APIs.
type Effective struct {
	Phase  string           `json:"phase"`
	Mode   string           `json:"mode"`
	Access hostview.Access  `json:"access"`
	E2EE   hostview.E2EE    `json:"e2ee"`
	Health *hostview.Health `json:"health"`
}
