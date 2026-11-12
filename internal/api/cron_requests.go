package api

import (
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// CronRequest is advisory work for optional runtimes, with current read state.
type CronRequest db.CronRequest

// TransformSchema describes retained values as scalars rather than byte arrays.
func (CronRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.Properties["inputs"] = &huma.Schema{Type: huma.TypeObject, AdditionalProperties: &huma.Schema{OneOf: []*huma.Schema{{Type: huma.TypeString}, {Type: huma.TypeInteger, Format: "int64"}, {Type: huma.TypeBoolean}}}}
	return schema
}

// CronRequestBody binds a caller-retained fresh UID to its retry identity.
type CronRequestBody struct {
	UID        string `json:"uid" minLength:"26" maxLength:"26"`
	Actor      string `json:"actor,omitempty"`
	TTLSeconds *int   `json:"ttl_seconds,omitempty" minimum:"60" maximum:"86400"`
}

// CronJobRequestRunBody extends run admission with per-request values.
type CronJobRequestRunBody struct {
	UID        string `json:"uid" minLength:"26" maxLength:"26"`
	Actor      string `json:"actor,omitempty"`
	TTLSeconds *int `json:"ttl_seconds,omitempty" minimum:"60" maximum:"86400"`
	Inputs     JSONRawMap `json:"inputs,omitempty"`
}

// CronJobRequestRun addresses an adapter execute job.
type CronJobRequestRun struct {
	CronDefinitionRequest
	Body CronJobRequestRunBody
}

// CronRunRequestCancel addresses a currently running observation.
type CronRunRequestCancel struct {
	CronRunRequest
	Body CronRequestBody
}

// CronRequestResponse returns the original pinned request on an exact retry.
type CronRequestResponse struct {
	Body struct {
		Request  CronRequest `json:"request"`
		Replayed bool        `json:"replayed"`
		Event    *db.Event   `json:"event,omitempty"`
	}
}

// CronRequestListRequest reads retained requests in one project.
type CronRequestListRequest struct {
	CronProjectRequest
	State string `query:"state" enum:"pending,answered,expired"`
}

// CronRequestListResponse contains locally known retained requests.
type CronRequestListResponse struct {
	Body struct {
		Requests []CronRequest `json:"requests"`
	}
}
