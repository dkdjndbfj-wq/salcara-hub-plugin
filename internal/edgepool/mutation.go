package edgepool

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

func decodeJSON(raw []byte, value any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}

func decodeMutation(action string, raw json.RawMessage) (*int64, func(*savedConfig) string, string) {
	switch action {
	case "nodes":
		var body struct {
			ExpectedRevision *int64    `json:"expected_revision"`
			Node             nodeInput `json:"node"`
		}
		if !decodeJSON(raw, &body) {
			return nil, nil, "invalid_request"
		}
		if body.Node.Enabled == nil {
			return nil, nil, "enabled_required"
		}
		return body.ExpectedRevision, func(s *savedConfig) string {
			input := body.Node
			if input.Region != nil {
				region := trimRegionInput(*input.Region)
				if !validRegion(region) {
					return "invalid_node_region"
				}
				input.Region = &region
			}
			for _, limit := range []*int{input.MaxRequests, input.MaxStreams} {
				if limit != nil && (*limit < 1 || *limit > 4096) {
					return "invalid_node_limits"
				}
			}
			if input.Weight != nil && (*input.Weight < 1 || *input.Weight > 100) {
				return "invalid_node_limits"
			}
			if input.ID == "" {
				if len(s.Nodes) >= maxNodes {
					return "node_limit"
				}
				var bytes [8]byte
				if _, err := rand.Read(bytes[:]); err != nil {
					return "node_id_unavailable"
				}
				input.ID = "node-" + hex.EncodeToString(bytes[:])
				url := ""
				if input.ProxyURL != nil {
					url = *input.ProxyURL
				}
				s.Nodes = append(s.Nodes, nodeConfig{ID: input.ID, Name: strings.TrimSpace(input.Name), ProxyURL: url, Enabled: *input.Enabled})
			} else {
				index := -1
				for i := range s.Nodes {
					if s.Nodes[i].ID == input.ID {
						index = i
						break
					}
				}
				if index < 0 {
					return "node_not_found"
				}
				n := &s.Nodes[index]
				n.Name = strings.TrimSpace(input.Name)
				n.Enabled = *input.Enabled
				if input.ProxyURL != nil {
					n.ProxyURL = *input.ProxyURL
					n.Imported = false
					n.SourceID, n.SourceKey = "", ""
				}
			}
			for i := range s.Nodes {
				n := &s.Nodes[i]
				if n.ID == input.ID {
					if input.Region != nil {
						n.Region = *input.Region
					}
					if input.Weight != nil {
						n.Weight = *input.Weight
					}
					if input.MaxRequests != nil {
						n.MaxRequests = *input.MaxRequests
					}
					if input.MaxStreams != nil {
						n.MaxStreams = *input.MaxStreams
					}
					if input.Draining != nil {
						n.Draining = *input.Draining
					}
					break
				}
			}
			if s.SelectedID == "" || !enabledNode(*s, s.SelectedID) {
				s.SelectedID = firstEnabled(*s)
			}
			return ""
		}, ""
	case "nodes/delete":
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			ID               string `json:"id"`
		}
		if !decodeJSON(raw, &body) {
			return nil, nil, "invalid_request"
		}
		return body.ExpectedRevision, func(s *savedConfig) string {
			index := -1
			for i, n := range s.Nodes {
				if n.ID == body.ID {
					index = i
					break
				}
			}
			if index < 0 {
				return "node_not_found"
			}
			s.Nodes = append(s.Nodes[:index], s.Nodes[index+1:]...)
			if s.SelectedID == body.ID {
				s.SelectedID = firstEnabled(*s)
			}
			return ""
		}, ""
	case "policy":
		var body struct {
			ExpectedRevision *int64                `json:"expected_revision"`
			Mode             *string               `json:"mode"`
			SelectedID       *string               `json:"selected_id"`
			Limits           *limitsInput          `json:"limits"`
			HealthPolicy     *healthPolicyInput    `json:"health_policy"`
			SelectionPolicy  *selectionPolicyInput `json:"selection_policy"`
			SourceLimits     *[]SourceLimit        `json:"source_limits"`
		}
		if !decodeJSON(raw, &body) {
			return nil, nil, "invalid_request"
		}
		return body.ExpectedRevision, func(s *savedConfig) string {
			if body.SourceLimits != nil {
				if err := validateSourceLimits(*body.SourceLimits); err != nil {
					return "invalid_source_limits"
				}
				s.SourceLimits = append([]SourceLimit{}, (*body.SourceLimits)...)
			}
			if body.Mode != nil {
				s.Mode = *body.Mode
			}
			if body.SelectedID != nil {
				s.SelectedID = *body.SelectedID
			}
			body.HealthPolicy.apply(&s.HealthPolicy)
			body.SelectionPolicy.apply(&s.SelectionPolicy)
			if err := validatePolicies(s.HealthPolicy, s.SelectionPolicy); err != nil {
				return "invalid_policy_settings"
			}
			if body.Limits != nil {
				v := body.Limits
				for _, value := range []*int{v.MaxRequests, v.MaxStreams} {
					if value != nil && (*value < 1 || *value > 4096) {
						return "invalid_pool_limits"
					}
				}
				if v.QueueLimit != nil && (*v.QueueLimit < 0 || *v.QueueLimit > 4096) {
					return "invalid_pool_limits"
				}
				if v.QueueTimeoutMS != nil && (*v.QueueTimeoutMS < 1 || *v.QueueTimeoutMS > 30000) {
					return "invalid_pool_limits"
				}
				if v.MaxRequests != nil {
					s.Limits.MaxRequests = *v.MaxRequests
				}
				if v.MaxStreams != nil {
					s.Limits.MaxStreams = *v.MaxStreams
				}
				if v.QueueLimit != nil {
					s.Limits.QueueLimit = *v.QueueLimit
				}
				if v.QueueTimeoutMS != nil {
					s.Limits.QueueTimeoutMS = *v.QueueTimeoutMS
				}
			}
			return ""
		}, ""
	}
	return nil, nil, "invalid_action"
}
