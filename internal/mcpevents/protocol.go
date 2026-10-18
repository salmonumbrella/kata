package mcpevents

// Error carries only a fixed protocol code and safe categorized reason.
type Error struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return "MCP Events: " + e.Reason }

type Delivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type SubscribeRequest struct {
	Name      string    `json:"name"`
	Arguments Arguments `json:"arguments"`
	Delivery  Delivery  `json:"delivery"`
	Cursor    *string   `json:"cursor,omitempty"`
	TTLMS     *int64    `json:"ttlMs,omitempty"`
}

type SubscribeResult struct {
	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
	Cursor        string `json:"cursor"`
	Truncated     bool   `json:"truncated"`
}

type UnsubscribeRequest struct {
	Name      string    `json:"name"`
	Arguments Arguments `json:"arguments"`
	Delivery  Delivery  `json:"delivery"`
}

type Payload struct {
	Recipient      string `json:"recipient"`
	Reason         string `json:"reason"`
	ProjectUID     string `json:"project_uid,omitempty"`
	IssueUID       string `json:"issue_uid,omitempty"`
	SourceEventUID string `json:"source_event_uid,omitempty"`
	ResyncRequired bool   `json:"resync_required"`
}

type Envelope struct {
	EventID   string  `json:"eventId"`
	Name      string  `json:"name"`
	Timestamp string  `json:"timestamp"`
	Data      Payload `json:"data"`
	Cursor    string  `json:"cursor"`
}

type Definition struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Delivery      []string       `json:"delivery"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}

type ListResult struct {
	Events []Definition `json:"events"`
}

// SubscriptionStatus excludes destinations, keys and envelope contents.
type SubscriptionStatus struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Recipient            string         `json:"recipient"`
	State                string         `json:"state"`
	RefreshBefore        string         `json:"refreshBefore"`
	Cursor               string         `json:"cursor"`
	ContinuityGeneration uint64         `json:"continuity_generation"`
	ContinuityLost       bool           `json:"continuity_lost"`
	Delivery             DeliveryStatus `json:"delivery"`
}
