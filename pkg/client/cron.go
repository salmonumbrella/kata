package client

import "go.kenn.io/kata/internal/uid"

// NewCronUID generates a normalized ULID for a definition, operation or
// launch request. Generate it once, persist it with the intended request, and
// reuse both after a timeout. Client calls never replace request identities or
// treat a conflict as a successful create. A new logical operation needs a new
// UID; this function itself does not submit a request or grant execution.
func NewCronUID() (string, error) { return uid.New() }
