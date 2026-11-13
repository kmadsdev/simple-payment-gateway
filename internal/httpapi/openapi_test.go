package httpapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/kmadsdev/simple-payment-gateway/internal/httpapi"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return doc
}

// TestOpenAPISpecIsValid validates api/openapi.yaml (runs in CI).
func TestOpenAPISpecIsValid(t *testing.T) {
	if err := loadSpec(t).Validate(context.Background()); err != nil {
		t.Fatalf("invalid spec: %v", err)
	}
}

// TestOpenAPIMatchesRoutes fails when the router and the spec drift apart,
// in either direction, or when the Idempotency-Key header is documented
// on the wrong operations.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	doc := loadSpec(t)
	inSpec := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			inSpec[strings.ToUpper(method)+" "+path] = true
		}
	}
	for _, rt := range httpapi.Routes {
		key := rt.Method + " " + rt.Path
		if !inSpec[key] {
			t.Errorf("route %s is not in the spec", key)
			continue
		}
		delete(inSpec, key)
		op := doc.Paths.Find(rt.Path).GetOperation(rt.Method)
		hasIdem := false
		for _, p := range op.Parameters {
			if p.Value != nil && p.Value.In == "header" && p.Value.Name == "Idempotency-Key" && p.Value.Required {
				hasIdem = true
			}
		}
		if hasIdem != rt.Idem {
			t.Errorf("%s: Idempotency-Key documented=%v, enforced=%v", key, hasIdem, rt.Idem)
		}
		public := op.Security != nil && len(*op.Security) == 0
		if public == rt.Auth {
			t.Errorf("%s: spec public=%v but route auth=%v", key, public, rt.Auth)
		}
	}
	for key := range inSpec {
		t.Errorf("spec documents %s but no route serves it", key)
	}
}
