// Package apidoc describes the gateway's `/api/v1/` operations and builds
// the OpenAPI document (`docs/dev/openapi.yaml`) from them. Each API package
// exports its operations next to its handlers.
package apidoc

import (
	"fmt"

	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"
)

// Response is one status code of an operation.
type Response struct {
	Status int
	// Body is a value of the response's Go type, or nil for no body.
	Body any
	// ContentType is the body's media type; empty means `application/json`.
	ContentType string
}

// Operation is one route: its method and path, the Go type of its request
// (path and query parameters, tagged `path:` and `query:`) and its
// responses.
type Operation struct {
	Method  string
	Path    string
	Summary string
	// Request is a value of the request type, or nil.
	Request   any
	Responses []Response
}

// Pattern is the operation's `net/http` mux pattern.
func (o Operation) Pattern() string {
	return o.Method + " " + o.Path
}

// TextError is the body type of the plain-text error responses.
type TextError string

// Build returns the OpenAPI 3.1 document of operations, as YAML.
func Build(title, version string, operations []Operation) ([]byte, error) {
	reflector := openapi31.NewReflector()
	reflector.SpecEns().WithInfo(*new(openapi31.Info).WithTitle(title).WithVersion(version))

	for _, operation := range operations {
		context, err := reflector.NewOperationContext(operation.Method, operation.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation.Pattern(), err)
		}

		context.SetSummary(operation.Summary)

		if operation.Request != nil {
			context.AddReqStructure(operation.Request)
		}

		for _, response := range operation.Responses {
			options := []openapi.ContentOption{func(content *openapi.ContentUnit) { content.HTTPStatus = response.Status }}

			if response.ContentType != "" {
				contentType := response.ContentType

				options = append(options, func(content *openapi.ContentUnit) { content.ContentType = contentType })
			}

			context.AddRespStructure(response.Body, options...)
		}

		if err := reflector.AddOperation(context); err != nil {
			return nil, fmt.Errorf("%s: %w", operation.Pattern(), err)
		}
	}

	document, err := reflector.Spec.MarshalYAML()
	if err != nil {
		return nil, err
	}

	return document, nil
}
