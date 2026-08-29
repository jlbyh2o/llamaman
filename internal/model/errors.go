package model

// ErrorCode is the closed enum of machine-readable API error codes (DESIGN
// section 3). Codes are mirrored into TypeScript by the generated OpenAPI
// schema, so a code that is not listed here cannot appear on the wire.
type ErrorCode string

// Error is the body of every non-2xx API response:
//
//	{"error":{"code":"model_in_use","message":"…","details":{…}}}
//
// The HTTP status is chosen by the handler to match the code.
type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorEnvelope is the top-level wrapper the API marshals.
type ErrorEnvelope struct {
	Error Error `json:"error"`
}

// Error implements the error interface so a model.Error can be returned from
// service code and rendered by the API layer without translation.
func (e Error) Error() string { return string(e.Code) + ": " + e.Message }
