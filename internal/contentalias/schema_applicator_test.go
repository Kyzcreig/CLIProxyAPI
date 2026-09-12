package contentalias

import "testing"

func TestSchemaUnsupportedReferenceAndItemApplicators(t *testing.T) {
	for _, fragment := range []string{`"unevaluatedItems":{"properties":{"command":{}}}`, `"additionalItems":{"properties":{"command":{}}}`, `"$dynamicRef":"#node"`, `"$recursiveRef":"#"`, `"$id":"https://example.invalid/new-root"`} {
		if _, _, err := Prepare([]byte(`{"tools":[{"name":"T","input_schema":{`+fragment+`}}]}`), testSession(t)); err == nil {
			t.Errorf("unsupported applicator accepted: %s", fragment)
		}
	}
}
